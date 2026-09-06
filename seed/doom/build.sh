#!/bin/sh
# Reproducible source port. Immutable archive is read only; adaptations live in /work.
set -eu
seed=/inputs/source/seed
mkdir -p /work/doom /work/out
tar --warning=no-unknown-keyword -xf /inputs/assets/doomgeneric-src -C /work/doom
cd /work/doom/doomgeneric-master/doomgeneric
# DoomGeneric's upstream non-SDL quit routine omits process termination.
# Add that userland adaptation without changing the supplied archive.
# Patch only the I_Quit function, using its following conditional as boundary.
awk '
/^void I_Quit / {quit=1}
quit && /^#if ORIGCODE/ {print "    exit(0);"; quit=0}
{print}
' i_system.c > i_system.cx
mv i_system.cx i_system.c
sources=$(sed -n 's/^SRC_DOOM = //p' Makefile)
set --
for name in $sources; do
    [ "$name" = doomgeneric_xlib.o ] && continue
    set -- "$@" "${name%.o}.c"
done
CX_LIBC=1 CX_USER_FLAGS="-Os -fwrapv -fno-strict-aliasing -Wno-error -I /work/doom/doomgeneric-master/doomgeneric -DDOOMGENERIC_RESX=320 -DDOOMGENERIC_RESY=200" \
    sh "$seed/sdk/compile.sh" "$seed" /work/out/doom "$@" "$seed/doom/platform.c.inc"
for program in libtest inputtest keylog concurrent; do
    CX_LIBC=1 sh "$seed/sdk/compile.sh" "$seed" "/work/out/$program" "$seed/user/$program.c.inc"
done
sh "$seed/sdk/compile.sh" "$seed" /work/out/cpubench "$seed/user/cpubench.c.inc"
cc -std=c11 -O2 -Wall -Wextra -Werror -I "$seed/sdk" -I "$seed/doom" -I . \
    -x c "$seed/doom/keytest.c.inc" -o /work/keytest
/work/keytest
printf 'Doom key translation tests passed\n'

