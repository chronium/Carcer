#!/bin/sh
# Usage: sh compile.sh SDK-parent output-base source.c [source.S ...]
# Both .c/.S and persistence-safe .c.inc/.S.inc names are accepted.
set -eu
if [ "$#" -lt 3 ]; then
    echo "usage: compile.sh seed-root output-base source.c [source.S ...]" >&2
    exit 2
fi
src=$1
base=$2
shift 2
mkdir -p "$(dirname "$base")"
obj=$(mktemp -d /work/cx-build.XXXXXX)
trap 'rm -rf "$obj"' EXIT HUP INT TERM
cc -std=c11 -O2 -Wall -Wextra -Werror -x c "$src/sdk/elf2cxe.c.inc" -o "$obj/elf2cxe"
flags="-std=c11 -O2 -Wall -Wextra -Werror -ffreestanding -fno-builtin -fno-stack-protector -fno-pie -fno-pic -fno-asynchronous-unwind-tables -fno-unwind-tables -fcf-protection=none -march=x86-64 -mtune=generic -mno-red-zone"
if [ "${CX_LIBC:-0}" = 1 ]; then
    flags="$flags -I $src/sdk/libc"
    for lib in "$src/sdk/libc/"*.c.inc; do
        cc $flags -I "$src/sdk" -x c -c "$lib" -o "$obj/lib-$(basename "$lib").o"
    done
fi
# Optional caller flags apply only to workload sources, not SDK/runtime code.
userflags="${CX_USER_FLAGS:-}"
cc $flags -I "$src/sdk" -x c -c "$src/sdk/runtime.c.inc" -o "$obj/runtime.o"
cc -march=x86-64 -fno-pie -x assembler-with-cpp -c "$src/sdk/start.S.inc" -o "$obj/start.o"
i=0
for input do
    case "$input" in
        *.c|*.c.inc)
            cc $flags $userflags -I "$src/sdk" -x c -c "$input" -o "$obj/input$i.o"
            ;;
        *.S|*.S.inc)
            cc -march=x86-64 -fno-pie -I "$src/sdk" -x assembler-with-cpp -c "$input" -o "$obj/input$i.o"
            ;;
        *) echo "unsupported input suffix: $input" >&2; exit 2 ;;
    esac
    i=$((i+1))
done
ld -nostdlib --build-id=none -z max-page-size=4096 -z noexecstack --orphan-handling=error -T "$src/sdk/user.ld" "$obj/"*.o -o "$base.elf"
"$obj/elf2cxe" "$base.elf" "$base.cxe"
