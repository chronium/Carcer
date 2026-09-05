#!/bin/sh
# Integration fixture build; compile.sh is the general arbitrary-source command.
set -eu
src=${1:-/inputs/source/seed}
out=${2:-/work/out}
mkdir -p "$out"
obj=$(mktemp -d /work/cx-test.XXXXXX)
trap 'rm -rf "$obj"' EXIT HUP INT TERM
cc -std=c11 -O2 -Wall -Wextra -Werror -x c "$src/sdk/elf2cxe.c.inc" -o "$obj/elf2cxe"
"$obj/elf2cxe" --self-test
cc -std=c11 -O2 -Wall -Wextra -Werror -DSHA256_SELFTEST -I "$src/sdk" -x c "$src/user/sha256.c.inc" -o "$obj/sha-test"
"$obj/sha-test"
for name in spin child report launch sha256 argchild argtest; do
    sh "$src/sdk/compile.sh" "$src" "$out/$name" "$src/user/$name.c.inc"
    readelf -l "$out/$name.elf"
done
sha256sum "$out/"*.cxe
