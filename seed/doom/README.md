# DoomGeneric source port

The authoritative operator clarification permits a source-built Doom port.
There is no requirement to execute the supplied DOS/4G executable. All supplied
assets remain immutable; this source route does not modify or replace them.

doom/build.sh consumes the approved immutable doomgeneric-src archive through
the provisioned bounded bootstrap executor, compiles the ordinary upstream
source with guest-owned SDK/libc and doom/platform.c.inc, then packages CXE2.
No loader, scheduler, driver or syscall tests workload identity. The only source
adaptation beyond the platform adapter adds process exit to upstream I_Quit,
whose non-SDL configuration omits it. The supplied archive itself is unchanged.
The included upstream source is GPL-2.0-or-later; preserve its notices and archive
identity when redistributing the linked executable.

Asset identities:
doomgeneric-src: 4849664 bytes
93ca655ebfb9cccd2f02e05bf70d5bf1502bef21d09d3d353b9ed7aaceb61fb7
doom-wad: 12408292 bytes
6fdf361847b46228cfebd9f3af09cd844282ac75f3edbb61ca4cb27103ce2e7f

Only adaptation/build/library source and the linked CXE2 are persisted in seed/.
The 4.8 MB upstream archive and 12 MB WAD exceed the 1 MiB source budget. They remain
available as supplied immutable assets. Runtime imports, logs, screenshots and
save files do not persist across generations. Reimport the WAD when needed.
No network download, writable block storage or future provisioning is assumed.

Invocation uses the ordinary file-driven launcher:
seed/user/doom.cxe
-iwad
runtime/doom.wad
-playdemo
demo1
-cx-frames
600
-cx-capture
runtime/frame.xrgb

Write those LF-terminated lines to runtime/launch.txt, run seed/user/launch.cxe,
then reap its returned task ID. First import doom-wad to runtime/doom.wad.
-cx-frames bounds execution by presented frames; omit it for the interactive
loop with controls when input is available. -cx-log chooses an append log (default runtime/doom.log). -cx-capture
writes the final 320x200 XRGB8888 buffer at the frame limit: 256000 raw bytes.
These are userland diagnostic options, not kernel behavior.

The adapter presents 320x200 pixels through the generic display ABI, sleeps and
measures time through existing 100 Hz calls. Audio is absent. Physical interactive
validation requires the pending trusted display observation/input injection
request 2; userland frame diagnostics do not establish that milestone.

Source-built rendering and interactive play are separate validation claims.
See ENGINEERING.md for exact observed results, limitations and final artifacts.

Keyboard handling lives entirely in platform.c.inc and keys.h. It uses a persistent
syscall15 cursor, US set1 key mapping, combined left/right modifiers and releases
all remembered keys when history loss is reported. Unknown keys and typematic
duplicates are ignored. keytest.c.inc validates mapping and state transitions;
this is not a physical input test. There is no mouse or sound driver.

The build uses -fwrapv and -fno-strict-aliasing for the upstream engine's historical
integer/pointer conventions; these are workload compiler choices, not kernel
changes. Normal compilation of other user programs is unaffected.
