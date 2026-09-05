#!/usr/bin/env bash
# Trusted acquisition only. Network is allowed here, never in the job runner.
set -euo pipefail
here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
source "$here/pins.env"
output=${1:?usage: prepare.sh NEW_INPUT_DIRECTORY}
mkdir -- "$output"
output=$(cd -- "$output" && pwd)
temporary=$(mktemp -d)
trap 'rm -rf -- "$temporary"' EXIT
printf '{}\n' > "$temporary/auth.json"
git init -q "$temporary/source"
timeout --signal=TERM --kill-after=5s 120s git -C "$temporary/source" fetch --depth=1 "$TCC_REPOSITORY" "$TCC_COMMIT"
git -C "$temporary/source" archive --format=tar --prefix=tinycc/ "$TCC_COMMIT" > "$output/tcc-source.tar"
printf '%s  %s\n' "$TCC_ARCHIVE_SHA256" "$output/tcc-source.tar" | sha256sum -c -
if ! podman image exists "$IMAGE"; then
    timeout --signal=TERM --kill-after=10s 300s podman pull --authfile "$temporary/auth.json" --platform linux/amd64 "$IMAGE"
fi
test "$(podman image inspect "$IMAGE" --format '{{.Id}}')" = "$IMAGE_ID"
podman image inspect "$IMAGE" --format '{{.Id}} {{.Digest}} {{.Size}}' > "$output/image.txt"
git -C "$temporary/source" show -s --format='%H%n%ci%n%s' "$TCC_COMMIT" > "$output/source.txt"
printf '%s\n' 'Acquisition complete; no container job or run has been started.'
