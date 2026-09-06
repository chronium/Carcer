#!/bin/sh
# Independent reconstruction of every persisted user executable.
set -eu
src=/inputs/source/seed
mkdir -p /work/out
sh "$src/sdk/build.sh" "$src" /work/out > /work/sdk-verify.log 2>&1 || {
    tail -c 8192 /work/sdk-verify.log; exit 1;
}
sh "$src/doom/build.sh" > /work/doom-verify.log 2>&1 || {
    tail -c 8192 /work/doom-verify.log; exit 1;
}
sh "$src/console/build.sh" "$src" /work/out > /work/console-verify.log 2>&1 || {
    tail -c 8192 /work/console-verify.log; exit 1;
}
count=0
for file in "$src/user/"*.cxe; do
    name=$(basename "$file")
    cmp "$file" "/work/out/$name"
    count=$((count+1))
done
{
    printf 'All %s persisted CXE2 files matched reconstructed output.\n' "$count"
    printf 'SDK packager/SHA, Doom keys, console core tests passed.\n'
    find "$src" -type f -printf '%s\n' | awk '{n++;s+=$1}END{printf "Source files=%d content bytes=%d\n",n,s}'
    sha256sum /work/out/*.cxe
} > /work/out/verification.txt
cat /work/out/verification.txt
