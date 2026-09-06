# Optional user C library

Enable CX_LIBC=1 when invoking sdk/compile.sh. All code in sdk/libc is guest-owned
userland, statically linked into ordinary CXE2 programs. It is a documented C
subset, not a complete libc, POSIX implementation or guest-native compiler.

Implemented:
- malloc/free/calloc/realloc with 16-byte alignment, block splitting and
  coalescing; freed blocks are reused. Heap pages remain until process exit.
  cx_alloc is the underlying break manager; direct brk mutation remains unsafe.
- ASCII strings, ctype, case-insensitive comparisons and decimal/integer parsing.
- Unbuffered FILE streams with r/w/a, optional b/+, and exclusive wx modes.
  fread/fwrite count whole items; partial final items advance the byte offset.
  Seek permits positions through UINT32_MAX and writes zero-fill a preceding gap.
  w truncates; a selects the current EOF for each write.
- Integer/string/character/pointer printf conversions, width and precision,
  h/hh/l/ll/z/t/j integer lengths, and finite fixed-point f/F formatting with
  precision 0..9 and magnitude <=1e18 (also inf/nan output). Other conversions,
  including e/g/a, wide strings, positional arguments and %n output fail.
  snprintf returns the would-have-written length and terminates when capacity>0.
- scanf integer/decimal floating, c/s/scanset and n, field widths and assignment
  suppression. Numeric input items are width-bounded before conversion; incomplete
  exponents/signs/base prefixes fail without assigning destinations. No locale, wide characters, hexadecimal floats or inf/nan input.
  strtod is a limited decimal conversion, not guaranteed correctly rounded.
- exit and up to32 atexit callbacks. abort exits134. getenv returns NULL (empty
  environment); system(NULL) returns0, execution and mkdir fail ENOSYS.
  No shell/directory implementation is implied.

Standard streams start disconnected. cx_stdio(output_path,error_path) explicitly
connects stdout/stderr to append streams; NULL leaves that stream unchanged.
stdio errors use a small documented errno set, not fine-grained kernel error
codes. There is no terminal, input stream connection API, TLS, signals, or threads.

Streams own stable kernel handles and independent positions. Rename/removal
does not redirect an open stream; removed/replaced objects remain available
until their last handle closes. Append and seek-gap writes are single kernel
operations. The kernel closes leaked handles on exit/fault/forced termination.
The limit is16 handles per task, including streams and direct cx_open calls.
Each I/O call is bounded by UINT32_MAX bytes and positions/sizes by UINT32_MAX.
Immutable files reject write opens and all later mutations, including through
a previously opened write handle. See USER_ABI.md calls19/20. Standard streams
are still disconnected unless explicitly connected; this adds no terminal or
inherited stream redirection. a+ retains the prior initial-EOF position policy.

libtest's default mode additionally exercises stable identities, stale/foreign
handles, invalid flags/buffers, EOF and gap rules, read-only/immutable failures,
handle saturation, unaligned/cross-page I/O and info, libc rename/unlink, and two
independent append writers with complete record-integrity/uniqueness checks.
Boot observers check full-table rename with pinned endpoints, allocation/serial
failure, exit/fault/stop/tree cleanup and immutable-copy cancellation with exact
page/file/reference recovery. Explicit leak-*, copy-handle, copy-resume, namespace-move and open-fail modes
are observer fixtures: do not launch them standalone. Default libtest can run
normally when test/immutable is present and test/rt-a,b are absent.

Build flags CX_USER_FLAGS apply only to workload sources; the SDK/library remains
compiled with warnings as errors and x86-64 baseline ISA. The existing SDK mode
and its seven previously compiled fixtures remain supported.

libtest.c.inc is an independent ordinary program covering allocation reuse,
overflow, data preservation, formatting/parsing, seeks, partial-item EOF,
truncation, zero gaps, append, exclusive creation, rename replacement, invalid
syscall arguments and sealed inputs. libc_tests.h runs it while the inherited
non-syscalling spin counter advances, then checks page/file recovery.

Successful immutable-read resumption is also boot tested: after observing real
kernel-context preemption, another ordinary user task reorders the namespace
while the observer holds the reader suspended. The resumed reader checks all
32MiB-2 bytes, unchanged tail sentinels, count, position, EOF and a later seek/read.
Reference, handle, file and page counts recover exactly.
