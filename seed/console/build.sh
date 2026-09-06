#!/bin/sh
set -eu
src=${1:-/inputs/source/seed}
out=${2:-/work/out}
mkdir -p "$out"
cc -std=c11 -O2 -Wall -Wextra -Werror -x c "$src/console/coretest.c.inc" -o "$out/console-coretest"
"$out/console-coretest"
sh "$src/sdk/compile.sh" "$src" "$out/enumtest" "$src/user/enumtest.c.inc"
for name in console consoletest; do
    CX_LIBC=1 sh "$src/sdk/compile.sh" "$src" "$out/$name" "$src/user/$name.c.inc"
done
sha256sum "$out/enumtest.cxe" "$out/console.cxe" "$out/consoletest.cxe"

sh "$src/sdk/compile.sh" "$src" "$out/jobtest" "$src/user/jobtest.c.inc"
sha256sum "$out/jobtest.cxe"
