#!/usr/bin/env bash
# Feasibility only: fixed fixtures, disposable state, no production service wiring.
set -euo pipefail
here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
source "$here/pins.env"
inputs=${1:?usage: run.sh PREPARED_INPUT_DIRECTORY NEW_EVIDENCE_DIRECTORY}
output=${2:?usage: run.sh PREPARED_INPUT_DIRECTORY NEW_EVIDENCE_DIRECTORY}
mkdir -- "$output"
output=$(cd -- "$output" && pwd)
printf '%s  %s\n' "$TCC_ARCHIVE_SHA256" "$inputs/tcc-source.tar" | sha256sum -c -
test "$(podman image inspect "$IMAGE" --format '{{.Id}}')" = "$IMAGE_ID"
test "$(podman info --format '{{.Host.Security.Rootless}}')" = true
scratch=$(mktemp -d /tmp/codexos-tcc-fixture-XXXXXX)
tag="codexos-tcc-feas-$$"
slice="$tag.slice"
anchor="$tag-anchor.service"
attach_pid=
cleanup() {
    status=$?
    trap - EXIT
    mapfile -t ids < <(podman ps -aq --filter "label=codexos.feasibility=$tag")
    if ((${#ids[@]})); then podman rm -f "${ids[@]}" >/dev/null; fi
    if [[ -n "$attach_pid" ]]; then wait "$attach_pid" 2>/dev/null || true; fi
    systemctl --user stop "$anchor" "$slice" >/dev/null 2>&1 || true
    systemctl --user revert "$slice" > "$output/slice-cleanup.txt" 2>&1 || true
    rm -rf -- "$scratch"
    podman ps -aq --filter "label=codexos.feasibility=$tag" > "$output/remaining-containers.txt"
    test ! -s "$output/remaining-containers.txt" || status=1
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
podman ps -aq > "$output/baseline-containers.txt"
podman info --format json | jq '{host: (.host | {arch,os,cgroupVersion,cgroupManager,cgroupControllers,security,ociRuntime}), version}' > "$output/environment.json"
podman image inspect "$IMAGE" --format '{{.Id}} {{.Digest}} {{.Size}}' > "$output/image.txt"
cp "$here/pins.env" "$output/pins.env"
cp "$inputs/tcc-source.tar" "$here/job.sh" "$here/hello.c" "$here/freestanding.c" "$here/converter.c" "$here/limits.c" "$scratch/"
chmod 755 "$scratch"
chmod 644 "$scratch/"*
systemd-run --user --unit="$anchor" --slice="$slice" --property=RuntimeMaxSec=600 /usr/bin/sleep 600
systemctl --user set-property --runtime "$slice" MemoryMax=805306368 MemorySwapMax=0 CPUQuota=100% TasksMax=96
parent=$(systemctl --user show "$slice" --property=ControlGroup --value)
for f in memory.max memory.swap.max cpu.max pids.max; do
    printf '%s=%s\n' "$f" "$(cat "/sys/fs/cgroup$parent/$f")"
done > "$output/aggregate-controls.txt"
common=(--pull=never --label "codexos.feasibility=$tag" --cgroup-parent="$slice"
    --network=none --read-only --read-only-tmpfs=false --ipc=none
    --tmpfs /work:rw,nosuid,nodev,exec,size=256m,mode=1777
    --tmpfs /tmp:rw,nosuid,nodev,exec,size=16m,mode=1777
    --cap-drop=all --security-opt=no-new-privileges --user=65534:65534
    --pids-limit=64 --memory=512m --memory-swap=512m --cpus=1
    --ulimit nofile=128:128 --ulimit core=0:0 --ulimit fsize=67108864:67108864
    --log-driver=none --http-proxy=false --unsetenv-all --env=HOME=/work
    --env=PATH=/usr/local/bin:/usr/bin:/bin --env=LANG=C --workdir=/work
    --timeout=180 --stop-timeout=2)
# All actual argv, including host-selected mount paths, are recorded for audit.
printf '%q ' podman create "${common[@]}" > "$output/job-argv.txt"
printf '\n' >> "$output/job-argv.txt"
name="$tag-build"
podman create "${common[@]}" --name "$name" -v "$scratch:/inputs:ro,Z" "$IMAGE" sh /inputs/job.sh > "$output/build-id.txt"
# Byte-sized writes keep readiness visible while imposing a hard log bound.
(podman start -a "$name" 2>&1 | dd bs=1 count=65537 status=none > "$output/build.log") &
attach_pid=$!
ready=false
for ((attempt=0; attempt<360; attempt++)); do
    if grep -q '^FEASIBILITY_READY$' "$output/build.log"; then ready=true; break; fi
    if (($(wc -c < "$output/build.log") > 65536)); then echo 'diagnostic limit exceeded' >&2; exit 1; fi
    if [[ $(podman inspect "$name" --format '{{.State.Status}}') == exited ]]; then break; fi
    sleep 0.5
done
if [[ $ready != true ]]; then cat "$output/build.log"; exit 1; fi
podman pause "$name" > "$output/pause.txt"
mkdir "$output/artifacts"
# podman cp reads the storage mount, which omits the running tmpfs. This known
# fixture uses the paused process's mount view. A production hostile-output
# collector must instead validate identity and use bounded, beneath-only opens.
job_pid=$(podman inspect "$name" --format '{{.State.Pid}}')
cat "/proc/$job_pid/cgroup" > "$output/build-cgroup.txt"
grep -Fq "/$slice/" "$output/build-cgroup.txt"
timeout --signal=TERM --kill-after=2s 15s podman unshare tar -C "/proc/$job_pid/root/work/export" -cf - . |
    tar --no-same-owner -C "$output/artifacts" -xf -
podman inspect "$name" | jq '.[0] | {State, HostConfig: (.HostConfig | {CgroupParent,NetworkMode,ReadonlyRootfs,CapDrop,SecurityOpt,Memory,MemorySwap,PidsLimit,CpuPeriod,CpuQuota}), Mounts}' > "$output/build-inspect.json"
podman unpause "$name" >/dev/null
podman rm -f "$name" >/dev/null
wait "$attach_pid" || true
attach_pid=
# Linux tooling retrieved from the first job is an immutable input to later jobs.
run_probe() {
    local suffix=$1; shift
    local name="$tag-$suffix"
    podman create "${common[@]}" --name "$name" --memory=64m --memory-swap=64m --cpus=0.5 --pids-limit=16 --timeout=15 \
        -v "$output/artifacts:/tools:ro,Z" "$IMAGE" "$@" >/dev/null
    set +e
    timeout --signal=TERM --kill-after=2s 20s podman start -a "$name" > "$output/$suffix.log" 2>&1
    local status=$?
    set -e
    podman inspect "$name" --format '{{.State.ExitCode}} {{.State.OOMKilled}}' > "$output/$suffix-state.txt"
    podman rm -f "$name" >/dev/null
    printf '%s\n' "$status" > "$output/$suffix-client-status.txt"
}
run_probe isolation sh -ec '
    id
    for f in memory.max memory.swap.max pids.max cpu.max; do echo "$f=$(cat /sys/fs/cgroup/$f)"; done
    grep -E "^(CapEff|NoNewPrivs|Seccomp):" /proc/self/status
    grep -E "^Max (file size|core file size|open files)" /proc/self/limits
    ls /sys/class/net
    test "$(ls /sys/class/net)" = lo
    test -z "${TCC_FEASIBILITY_SECRET:-}"
    for p in /shared /srv/codexos /run/podman/podman.sock /run/user/1000/podman/podman.sock /root/.codex /root/.ssh; do test ! -e "$p"; done
    test ! -w /tools
    /tools/hello
    /tools/tool/tcc -B/tools/tool -run - <<EOF_C
#include <stdio.h>
int main(void) { puts("retrieved-tcc-executes"); return 0; }
EOF_C
'
test "$(cat "$output/isolation-client-status.txt")" = 0
run_probe pids /tools/limits pids
test "$(cat "$output/pids-client-status.txt")" = 0
run_probe memory /tools/limits memory
grep -q '^137 true$' "$output/memory-state.txt"
run_probe cpu sh -ec '/tools/limits cpu; cat /sys/fs/cgroup/cpu.stat; test "$(awk '\''$1=="nr_throttled" {print $2}'\'' /sys/fs/cgroup/cpu.stat)" -gt 0'
test "$(cat "$output/cpu-client-status.txt")" = 0
# Smaller tmpfs/ulimit probes keep failure tests cheap and separate from the build.
name="$tag-disk"
podman run --rm "${common[@]}" --name "$name" --tmpfs /limited:rw,nosuid,nodev,noexec,size=8m,mode=1777 "$IMAGE" \
    sh -c 'dd if=/dev/zero of=/limited/full bs=1M count=9; test $? -ne 0' > "$output/disk.log" 2>&1
name="$tag-cancel"
podman create "${common[@]}" --name "$name" --timeout=3 "$IMAGE" sh -c 'trap "" TERM; sleep 60 & wait' >/dev/null
started=$SECONDS
podman start -a "$name" > "$output/cancel.log" 2>&1 || true
elapsed=$((SECONDS-started))
podman inspect "$name" --format '{{.State.ExitCode}} {{.State.Running}}' > "$output/cancel-state.txt"
printf 'elapsed_seconds=%s\n' "$elapsed" >> "$output/cancel-state.txt"
test "$elapsed" -le 10
test "$(podman inspect "$name" --format '{{.State.Running}}')" = false
podman rm -f "$name" >/dev/null
# A noisy fixed fixture reaches the sentinel byte without growing a log file.
name="$tag-noise"
podman create "${common[@]}" --name "$name" --timeout=5 "$IMAGE" sh -c 'exec yes diagnostic' >/dev/null
(podman start -a "$name" 2>&1 | dd bs=1 count=65537 status=none > "$output/noise.log") &
attach_pid=$!
for ((attempt=0; attempt<50; attempt++)); do
    if [[ -f "$output/noise.log" ]] && (($(wc -c < "$output/noise.log") == 65537)); then break; fi
    sleep 0.1
done
test "$(wc -c < "$output/noise.log")" = 65537
podman rm -f "$name" >/dev/null
wait "$attach_pid" || true
attach_pid=
printf 'captured_bytes=65537\nretained_diagnostic_bytes=65536\noverflow_cancelled=true\n' > "$output/noise-state.txt"
truncate -s 65536 "$output/noise.log"
find "$output/artifacts" -type f -printf '%P %s bytes\n' | sort > "$output/artifact-sizes.txt"
printf '%s\n' 'PASS: offline build, execution, paused output capture, rootless controls, cancellation.' > "$output/result.txt"
