# CodexOS command console

The console is an ordinary compiled CXE2 user program. It uses existing display,
keyboard, file and task calls plus syscall16 namespace enumeration. Its command
parser, glyphs, editing, rendering and foreground policy are entirely userland.

## Launch
Run seed/user/console.cxe without arguments for the keyboard console.
It appends a transcript to runtime/console.log. Controller availability is
required. It is not automatically started by the kernel.

For reproducible scripted execution, use these LF-terminated launcher lines:
```
seed/user/console.cxe
--script
runtime/commands
runtime/transcript
runtime/console.xrgb
```
The final capture argument is optional. Run seed/user/launch.cxe and reap the
returned task. Transcript and capture paths must be new; existing files fail.
The capture is raw XRGB8888, tightly packed, width min(display.width,800)
rounded down to a multiple of6, height min(display.height,480) rounded down
to a multiple of8. It is the userland render buffer, not trusted VGA observation.

## Commands
- help: command reference.
- ls [prefix]: list the entire namespace, optionally filtered by literal byte
  prefix. Names are not directories; slash has no special filesystem meaning.
- stat path: size and immutable attribute.
- cat path: show up to4096 bytes, escaping nonprintable bytes and quotes/backslash.
- write path text: exclusively create a new file with exactly the supplied text.
  No newline is appended. A failed write can leave the newly created empty file.
- mv old new: existing file-control rename, including replacement of a mutable
  destination. Both endpoints must permit modification.
- rm path: remove a mutable file.
- run executable [args...]: spawn with argv[0]=executable, block until it exits,
  consume its result and print the full unsigned64-bit status.
- ticks: monotonic100Hz boot ticks.
- clear: clear the visible text area.
- exit [status]: exit with an unsigned decimal64-bit status, default0.

Whitespace separates tokens. Single and double quotes preserve spaces and empty
arguments. Backslash quotes the following byte except inside single quotes.
Quotes may appear within a token. There are no operators, pipes, expansions,
environment substitution, comments, response files or hexadecimal escape syntax.
At most32 tokens per line, including the command; run therefore passes <=31
arguments. C-string paths cannot address names containing embedded NUL. ls
preserves their byte identity as escaped output but is not a round-trip encoder.

The renderer has ASCII glyphs in6x8 cells. The final row shows the prompt and the
tail of the edited line. US physical-key mapping supports Shift, CapsLock,
Backspace, Enter, Escape and Ctrl-C/Ctrl-U to clear the line. Independent Shift
and Ctrl keys are combined. These are line-editing controls, not task signals.
Maximum line content is1023 bytes. Overflow is sticky until Enter rejects the
whole command or a clear-line key resets it; backspace cannot undo discarded
bytes. A lost input-history event clears the partial line and modifier state.

Scripts contain <=65536 bytes, no NUL, and every line must end in LF and fit the
same1023-byte limit. Framing is validated before command execution. Syntax or
command failure aborts the script with125; prior successful effects remain.
An empty script succeeds. Child nonzero/fault status is reported and does not
abort the script. Interactive command errors return to the prompt.
The transcript has a1MiB per-invocation output ceiling; exceeding it fails125.

## Foreground and resource semantics
The console draws before launching a child, then does not draw or read keyboard
events until the child completes. It redraws its own buffer afterward. At start
and after each submitted interactive command it discards old keyboard history,
so launch/game keystrokes do not become another command. A bounded drain covers
the256-event retained history; modifier state resets, and fresh presses are needed.

There is no exclusive display/input ownership. Unrelated programs may still
draw or observe keyboard history. There is no terminal stream redirection:
children keep their own file-based output conventions. Inspect those with cat.
There are no background jobs, cancellation, timeout or process ownership APIs.
An indefinitely running foreground child keeps this session waiting; other
independent tasks and the development bridge retain ordinary scheduling.

All files are RAM files. Runtime transcripts, scripts, captures, saves and imports
disappear across generations. FILE streams remain path-backed; concurrent rename,
removal, replacement and writers retain the limitations in sdk/libc/README.md.

Startup errors120 invalid arguments,121 invalid paths/transcript equal to script,
122 display/allocation,123 keyboard unavailable,124 transcript open,125 script
or output/capture failure. Explicit exit statuses can coincide with these.

## Build and checks
sh /inputs/source/seed/console/build.sh
uses the approved bootstrap executor and guest SDK; no external assets needed.
Requested bootstrap output names are relative to /work/out (console.cxe,
enumtest.cxe, consoletest.cxe). Do not include an extra out/ prefix.

coretest exercises the production lexer, editing bounds, modifier/loss handling,
scrolling and glyph bounds. enum_tests.h runs enumtest.cxe through syscall16.
console_tests.h runs consoletest.cxe, which starts the real console, checks command
effects and transcripts, and exercises child37, full-width status and fault.
The kernel observer compares the exported final renderer bytes with mapped
framebuffer memory. Both suites check progress of an unrelated non-syscalling
spin program and recover file/page counts. These are guest-side tests, not
trusted physical display or keyboard validation. See VALIDATION.md.

Review hardening: every command's echo is flushed and checked before dispatch.
A detected transcript write error or exhausted output ceiling prevents that
command's effects. A later failure while recording results can still follow
an already executed command; there is no command/file transaction mechanism.
Production regressions cover both ceiling exhaustion and real transcript write
failure. The substantive command script's capture is the framebuffer fixture.

console/verify.sh rebuilds every persisted user binary with declared immutable
Doom source input and compares all16 byte-for-byte; rebuild.txt and VALIDATION.md
record the observed successful run and final artifact hashes.
