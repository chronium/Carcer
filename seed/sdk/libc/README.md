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

Streams store paths and offsets, not stable kernel handles. External remove or
rename affects future stream operations, and replacement at the same path may
retarget an open stream. Append's size lookup and write are separate syscalls;
concurrent writers need external coordination. The same applies to seek-gap
extension followed by writing. Exclusive create and each individual namespace
operation are atomic on the single scheduling CPU. Immutable files reject all
mutations. These limitations are explicit and are not claimed as Unix semantics.

Build flags CX_USER_FLAGS apply only to workload sources; the SDK/library remains
compiled with warnings as errors and x86-64 baseline ISA. The existing SDK mode
and its seven previously compiled fixtures remain supported.

libtest.c.inc is an independent ordinary program covering allocation reuse,
overflow, data preservation, formatting/parsing, seeks, partial-item EOF,
truncation, zero gaps, append, exclusive creation, rename replacement, invalid
syscall arguments and sealed inputs. libc_tests.h runs it while the inherited
non-syscalling spin counter advances, then checks page/file recovery.
