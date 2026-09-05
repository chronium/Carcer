#!/bin/sh
# Disposable workload fixture, not a guest compiler port or service entry point.
set -eu
export TMPDIR=/work/tmp
mkdir -p "$TMPDIR" /work/export/tool
cd /work
tar xf /inputs/tcc-source.tar
cd tinycc
./configure --prefix=/work/installed --enable-cross --with-selinux --extra-cflags=-O0
make -j1 tcc libtcc1.a i386-tcc arm64-tcc riscv64-tcc
./tcc -v
./tcc -B. -std=c99 -run /inputs/hello.c
./tcc -B. -std=c11 -o /work/export/hello /inputs/hello.c
/work/export/hello
./tcc -B. -c -o /work/export/x86_64.o /inputs/freestanding.c
./i386-tcc -c -o /work/export/i386.o /inputs/freestanding.c
./arm64-tcc -c -o /work/export/arm64.o /inputs/freestanding.c
./riscv64-tcc -c -o /work/export/riscv64.o /inputs/freestanding.c
readelf -h /work/export/*.o | grep -E 'File:|Class:|Machine:'
./tcc -B. -o /work/export/converter /inputs/converter.c
printf 'bootstrap bytes\n' > /work/input.bin
/work/export/converter /work/input.bin /work/export/converted.bin
cmp /work/input.bin /work/export/converted.bin
cp tcc libtcc1.a runmain.o /work/export/tool/
cp -r include /work/export/tool/
gcc -O2 -Wall -Wextra -Werror /inputs/limits.c -o /work/export/limits
find /work/export -type f -print0 | sort -z | xargs -0 sha256sum
printf 'FEASIBILITY_READY\n'
# Keep the tmpfs mounted so the host can freeze all processes before capture.
exec sleep 180
