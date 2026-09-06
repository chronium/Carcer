# Keyboard events

The generic PS/2 driver configures a set-2 keyboard with controller translation
to set1 and polls at most16 controller bytes on each100Hz timer interrupt.
All initialization waits and retries are bounded. Absence/failure leaves input
unavailable and does not prevent boot. It introduces no new interrupt return
path. Kernel FP/SIMD entry invariants are unchanged.

Syscall15 with RSI=0 queries availability (0 or1), ignoring other arguments.
Otherwise RDI is a writable array of1..64 events, RSI is its capacity, and RDX
points to a readable+writable uint64 cursor. Returns event count or UINT64_MAX.
Both ranges must be valid and must not overlap; failure writes neither range.
Successful empty reads update the cursor but do not touch the event array.

Each24-byte event is LE64 sequence, LE64 boot tick, LE16 code, byte pressed,
byte flags and LE32 reserved=0. Set1 make codes identify physical keys, with
0x100 added for E0 extended keys. Pause is0x145 and emits press+release.
PrintScreen is0x137; its fake shift bytes are discarded. Code0 is ignored.
Flags: bit0 repeated press; bit1 history loss before this reader's first returned
event. No text, keyboard layout, focus or process ownership is implied.

Set cursor0 to begin with the oldest retained event. Subsequent calls pass the
returned next-sequence cursor. A256-event history is shared read-only; independent
cursors do not consume one another's events. Stale cursors advance to the oldest
retained event with the loss flag. Future cursors fail. Readers should release
their remembered held keys after loss to avoid stuck input. There is no current
key-state snapshot and no blocking input wait. The driver stops on64-bit
sequence exhaustion; no wraparound alias is supported.

Decoder/history boot tests use synthetic bytes, including press/release/repeat,
extended keys, Pause, PrintScreen, invalid prefixes, independent readers and
overflow. Such tests do not establish physical input delivery. keylog.c.inc is
an ordinary independent user program: argv1 output path, argv2 duration1..3600
seconds. It writes sequence,tick,code,pressed,flags records through stdio.

Standard q35 controller availability is probed, not inferred from future goals.
Trusted observation and input injection remain pending request2. Long existing
interrupt-disabled kernel sections can delay polling and lose controller bytes.
No broad kernel preemptibility, mouse support or input isolation is claimed.

input_tests.h also runs an ordinary compiled inputtest.cxe through the production
syscall15 with deterministic boot-only key_fixture state. Begin/end are under CLI;
the complete controller-decoder/history state is restored, and hardware polling
is suppressed only while this fixture is active. It includes rejected read-only
second pages and checks resource recovery under a non-syscalling spin competitor.
This fixture neither accepts external key injection nor claims physical delivery.

Post-transition live check: keylog returned0 after5 seconds, with an empty log.
Doom subsequently reported input=1 during a successful600-frame concurrent run.
Physical key delivery remains unobserved. console/README.md documents the
ordinary userland US keyboard line editor and its bounded history-drain policy.
Doom's adapter also starts after the retained launch-era history at DG_Init.
Neither policy grants exclusive input ownership or a current-held-key snapshot.
