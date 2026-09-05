# Go Linux bootstrap service: operator setup and provisioning

The Go harness can run guest-authored Linux compilers, modified compilers and
converters in disposable rootless Podman jobs. Pinned upstream TCC source is a
provided asset; successful outputs are opaque persistent artifacts. They may
contain source as well as binaries. This capability is **disabled by default**.
Approval of feature request #3 neither enables it nor supplies a complete
guest-runnable compilation pipeline. Compiler adaptations, executable packaging,
SDK/runtime and any native guest port remain implementor-owned.

This document describes post-review operator actions. Development/acceptance uses
only disposable state. Installing the worker, provisioning a run and starting a
generation are separate actions; none is performed by the PR or by feature
approval. Python, permanent seed and live experiment state are unchanged.

## Prerequisites and dedicated account

The initial implementation supports **Linux amd64**, rootless Podman with crun,
a user systemd manager with delegated cgroup v2 cpu/memory/pids controllers,
enforcing SELinux, seccomp and kernel `openat2`/pidfd support. The tested host has
Podman 5.8.1 and crun 1.27. Accepted flags alone are insufficient: the worker
checks aggregate membership/limits and actual job controller values. It refuses
an unavailable/mismatched image or missing isolation controls.

Use the following setup on the tested Fedora/systemd family of hosts, from the
reviewed repository checkout. Administrator access is required. The example
harness account is `chronium`; substitute the account that actually owns runs.
Do not add the job account to the harness account's groups or grant it access to
experiment storage, the checkout, credentials or container control sockets.

```bash
bootstrap_harness_user=chronium
bootstrap_harness_group=$(id -gn "$bootstrap_harness_user")
bootstrap_setup=$(mktemp -d /tmp/codexos-bootstrap-setup-XXXXXX)

GOMAXPROCS=2 CGO_ENABLED=0 go build -p 1 \
  -o "$bootstrap_setup/codexos-bootstrap" ./cmd/codexos-bootstrap
GOMAXPROCS=2 go build -p 1 -o "$bootstrap_setup/codexos" ./cmd/codexos

# Create only if this dedicated account does not already exist.
sudo useradd --create-home --home-dir /var/lib/codexos-bootstrap \
  --shell /usr/sbin/nologin codexos-bootstrap
bootstrap_job_uid=$(id -u codexos-bootstrap)
getsubids codexos-bootstrap
getsubids -g codexos-bootstrap
```

Both subordinate-ID listings must provide at least 65,536 IDs. Regular `useradd`
allocates these on the tested host family; if local policy does not, an
administrator must allocate **non-overlapping** ranges in `/etc/subuid` and
`/etc/subgid` before continuing. Do not copy another account's mappings. The
worker explicitly maps container IDs 0–65535 through the rootless intermediate
namespace; UID 65534 must be representable.

```bash
sudo install -D -o root -g root -m 0755 \
  "$bootstrap_setup/codexos-bootstrap" /usr/local/libexec/codexos-bootstrap
sudo install -D -o root -g root -m 0755 \
  "$bootstrap_setup/codexos" /usr/local/bin/codexos-go
sudo install -d -o "$bootstrap_harness_user" -g "$bootstrap_harness_group" \
  -m 0700 /var/lib/codexos-bootstrap-artifacts
sudo chmod 0700 /var/lib/codexos-bootstrap
sudo loginctl enable-linger codexos-bootstrap
sudo systemctl start "user@${bootstrap_job_uid}.service"
sudo install -d -o codexos-bootstrap -g codexos-bootstrap -m 0700 \
  /var/lib/codexos-bootstrap/.config/systemd/user

cat > "$bootstrap_setup/codexosbootstrap.slice" <<'UNIT'
[Unit]
Description=CodexOS isolated bootstrap jobs

[Slice]
CPUQuota=100%
MemoryMax=768M
MemorySwapMax=0
TasksMax=96
UNIT
sudo install -o root -g root -m 0644 "$bootstrap_setup/codexosbootstrap.slice" \
  /var/lib/codexos-bootstrap/.config/systemd/user/codexosbootstrap.slice
sudo -u codexos-bootstrap env HOME=/var/lib/codexos-bootstrap \
  XDG_RUNTIME_DIR="/run/user/$bootstrap_job_uid" \
  DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$bootstrap_job_uid/bus" \
  systemctl --user daemon-reload
sudo -u codexos-bootstrap env HOME=/var/lib/codexos-bootstrap \
  XDG_RUNTIME_DIR="/run/user/$bootstrap_job_uid" \
  DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$bootstrap_job_uid/bus" \
  systemctl --user start codexosbootstrap.slice

printf '%s ALL=(codexos-bootstrap) NOPASSWD: /usr/local/libexec/codexos-bootstrap ""\n' \
  "$bootstrap_harness_user" > "$bootstrap_setup/sudoers"
sudo visudo -cf "$bootstrap_setup/sudoers"
sudo install -o root -g root -m 0440 "$bootstrap_setup/sudoers" \
  /etc/sudoers.d/codexos-bootstrap
```

The empty-argument restriction in sudoers matters: the harness may invoke only
this fixed worker with **no command-line arguments**. Its bounded stdin/stdout
protocol carries captured bytes, never a host command. The entry point verifies
its account, clears its environment, and enters the dedicated aggregate slice.
Container commands are interpreted only inside the sandbox. Installing this
binary does not give guests a generic sudo, shell or Podman API.

Before provisioning, verify filesystem denial as the job account for the actual
run root, checkout and harness-owned artifact store, for example:

```bash
sudo -u codexos-bootstrap test ! -r /srv/codexos
sudo -u codexos-bootstrap test ! -r /home/chronium/src/CodexOS-go
sudo -u codexos-bootstrap test ! -r /var/lib/codexos-bootstrap-artifacts
```

If any check fails, correct host permissions/ACLs before proceeding. Keep runtime
configuration and installed executables operator-controlled. Jobs have no network,
extra host binds, hooks, host namespaces, registry credentials or control sockets;
pre-start inspection rejects unexpected input mounts. Container isolation still
shares the host kernel. Dedicated-account installation and these permission
checks require operator verification: development had no passwordless admin
access and tested the same worker in a disposable scope under the current UID.

## Acquire the pinned image and immutable source

Acquisition is an operator-controlled **network-enabled** setup step. Jobs always
use `--pull=never --network=none`. The image must exist in the dedicated account's
rootless storage, not merely in the operator's image cache.

```bash
bootstrap_image=docker.io/library/gcc@sha256:a689e29bc3adf4663ef9a141d23081252764d1319c63f591a027bd6fd676f4c1
printf '{}\n' > "$bootstrap_setup/auth.json"
sudo install -o codexos-bootstrap -g codexos-bootstrap -m 0600 \
  "$bootstrap_setup/auth.json" /var/lib/codexos-bootstrap/acquisition-auth.json
sudo -u codexos-bootstrap env HOME=/var/lib/codexos-bootstrap \
  XDG_RUNTIME_DIR="/run/user/$bootstrap_job_uid" \
  podman pull --authfile /var/lib/codexos-bootstrap/acquisition-auth.json \
  --platform linux/amd64 "$bootstrap_image"
sudo rm /var/lib/codexos-bootstrap/acquisition-auth.json
sudo -u codexos-bootstrap env HOME=/var/lib/codexos-bootstrap \
  XDG_RUNTIME_DIR="/run/user/$bootstrap_job_uid" \
  podman image inspect "$bootstrap_image" --format '{{.Id}}'
# Required configuration ID:
# f3d916a4884034b89cb6148781f07d8c92d94b6c6dc1b74dcbec3475d16400da

bootstrap_tcc_commit=0fb54300b56512754221d80adda85ddb9815bceb
git init -q "$bootstrap_setup/tcc"
git -C "$bootstrap_setup/tcc" fetch --depth=1 \
  https://repo.or.cz/tinycc.git "$bootstrap_tcc_commit"
git -C "$bootstrap_setup/tcc" archive --format=tar --prefix=tinycc/ \
  "$bootstrap_tcc_commit" > "$bootstrap_setup/tcc-source.tar"
printf '%s  %s\n' \
  e696d12b9429faf08a08aeeaffe96769370e5ea50cf98218f45c74956b3b3f18 \
  "$bootstrap_setup/tcc-source.tar" | sha256sum -c -

# A new provided-asset ID; never replace an existing asset's bytes.
mkdir /shared/assets/tcc-0fb54300
install -m 0444 "$bootstrap_setup/tcc-source.tar" \
  /shared/assets/tcc-0fb54300/tcc-source.tar
```

The tar is 4,997,120 bytes, containing 4,544,205 content bytes in 546 regular
files. It is an immutable provided asset, not mutable snapshot content. The
current 64 KiB / provisioned 1 MiB source limit and 128-file limit still apply to
captured guest source, including handwritten SDK sources and patches. Generated
sysroots or modified source trees may also be retained as explicitly bounded,
opaque artifacts. Importing their bytes into snapshot-backed guest storage makes
those bytes subject to the source budget again.

## Provision an existing run at its inactive gate

Stop the old harness at an archived gate and close any retained exit interview.
Use the new Go binary, preserving the run's existing Git arguments. For the
previously shown Experiment 4 invocation:

```bash
/usr/local/bin/codexos-go \
  --run-directory /srv/codexos/experiment-004 \
  --resume-at-gate \
  --provided-assets /shared/assets \
  --git-repository /home/chronium/src/CodexOS \
  --git-base-ref experiment-003/generation-0000 \
  --tui
```

At the operator prompt:

```text
bootstrap
bootstrap provision tcc-0fb54300
bootstrap
```

Provisioning validates archive history, inactivity, absence of partial generation
state/interview retention, the frozen TCC asset, and worker/image/resource
availability before atomically recording `bootstrap-service.json`. It does not
start a VM or Codex session, approve a request, or change request #3's status.
An unavailable worker or incorrect pin fails without enabling the run. The
`bootstrap` command reports configuration, pins, limits and authorized job count;
`inspect GENERATION` reports archived artifact references and limits.

When the operator separately chooses to proceed, the existing `continue` and
`agent` commands retain their normal explicit approval/session semantics. No
special restart, automatic generation, or guest-source adaptation is performed.
Guest helpers must still expose invocation/source capture and binary-safe import.
The Go tool bridge recognizes advertised `bootstrap_job` and
`read_bootstrap_artifact` helpers; it never invents missing guest tools.

## Limits, retention and recovery

The baseline is fixed for this implementation, not a general resource framework:

| Boundary | Enforced limit |
| --- | --- |
| Admission | One job per run and globally; shared storage lock plus dedicated-account worker lock; busy requests are rejected |
| Job | 1 CPU, 512 MiB RAM, zero swap, 64 tasks, 180-second deadline |
| Aggregate worker/job scope | 1 CPU, 768 MiB RAM, zero swap, 96 tasks |
| Scratch | 256 MiB `/work`, 16 MiB `/tmp`, 1 MiB root-only supervisor control tmpfs; all charged to job RAM |
| Rlimits | 128 FDs, 64 MiB regular file, zero core dumps |
| Inputs | Current source budget, plus at most 8 selected assets/artifacts totaling 64 MiB |
| Outputs | 32 regular files, 16 MiB each, 32 MiB total; no symlinks, hardlinks or special files |
| Diagnostics | 64 KiB combined; stop on overflow; invalid UTF-8 becomes bounded display text |
| Collection / teardown | 15 seconds; TERM grace 2 seconds, bounded cleanup verification; no success without confirmed teardown |
| Retention | 128 MiB/run including manifests/logs, 64 successful jobs and 256 artifact entries across retained lineages |
| Failures | 32 records, at most 64 KiB each; retained diagnostic prefix capped at 8 KiB within each record |
| Shared artifact storage | 512 MiB logical file bytes including metadata and staging; one pinned image is separate (about 1.45 GB unpacked) |

Admission reserves worst-case output/metadata and worker input/helper/transfer
staging before execution. Empty-output jobs still need the reservation. The shared
store additionally bounds traversed entries to 65,536; filesystem allocation
metadata is not described as content capacity. Large manifests have a separate
1 MiB read bound to allow JSON framing/escaping. Existing invocation, path,
source-snapshot and v1 transport limits are preserved.

The supervisor is container PID 1/UID 0 in the **rootless** namespace. Only it
retains SETUID/SETGID so its child can become UID/GID 65534 with zero effective
capabilities. The guest cannot signal/ptrace the different-UID supervisor or
write its root-only completion record. This is why the production runner differs
from the feasibility script's single unprivileged process. The worker reads
completion separately from stdout, freezes the whole cgroup, binds collection to
a live pidfd/start time, uses `openat2` beneath a pinned directory, validates file
type/link count/size, and kills/reaps the container before publishing. It does
not use tar or `podman cp` to collect hostile outputs.

Successful immutable job directories and their hash-bound reference index have
separate commit points. Store initialization publishes ownership metadata and the jobs directory together.
Interrupted initializer, metadata-write and job staging are removed on recovery; a published
job lacking a committed reference remains unauthorized. Archives freeze references,
pins and limits. Restart/continuation select the parent's references; rollback
excludes later lineage. All referenced historical jobs remain immutable. Quotas
can therefore exhaust: **GC does not evict archived compilers/sysroots** and is
not a way around the 64-success limit for retained history.

```text
bootstrap recover
bootstrap gc
```

Recovery cancels any local active job, joins it, and asks the worker to reap owned
orphan containers before clearing the cleanup block. It starts no guest. GC is
allowed only at a validated inactive gate and removes successes unreferenced by
all archives/imported parents. Failed-record rotation never removes successful
referenced data. A cleanup failure blocks successful publication and retirement;
keep ownership/state for recovery rather than deleting files by hand. At restart,
the worker verifies/reaps its labeled incomplete jobs before admitting another.

Back up both the run directory **and** `/var/lib/codexos-bootstrap-artifacts`.
Run metadata alone does not contain the opaque blob bytes. Missing/corrupt blobs
fail archive reopening/inspection; a source or artifact quota never expands
silently. Do not relocate the shared store or mutate pins with manual JSON edits;
those changes require a separately reviewed migration.

## Cross-run inheritance

The existing `--inherit-from-run` / `--inherit-from-generation` operation copies
only the selected completed archive's authorized jobs, validates content hashes
and destination/global quotas, and holds its reservation through atomic fresh-run
publication. Copied blobs do not depend on the source store afterward. Source
capacity remains explicit through `--inherit-source-capacity` when needed; artifact
storage is separate. No additional artifact inheritance flag is required.

The destination retains artifacts but starts **unprovisioned**. Inheriting bytes
is not permission to execute jobs. Provision it at its first validated inactive
gate with the pinned provided asset and worker setup. A fresh seed-only experiment
has no bootstrap service or inherited artifacts. Historical references survive
source continuation and destination rollback independently.

For reproducible verification and measured limitations, see
[bootstrap acceptance evidence](bootstrap-service-acceptance.md) and the
[original design](tcc-bootstrap-service-design.md).
