# CodexOS user ABI
CXE1: `CXE1`, LE32 image size/entry, zero flags, then <=511 pages.
Private RWX base 0x400000; guarded stack top 0x40000000. PIT preempts ring 3 and immutable-file copy windows; faults exit only that task.

`int 0x80`: RAX call; RDI,RSI,RDX,RCX,R8 args; UINT64_MAX failure.
0 exit; 1 size; 2 read; 3 attributes (immutable bit 0); 4 write;
5 spawn; 6 reap (0 running, 1 consumed); 7 brk.

Initial break follows the image. brk(0) queries; changes are page aligned.
Growth zeroes; shrink unmaps; failures preserve the break. Paths are
1..255-byte UTF-8 spans; buffers may cross pages. Protocol run shares the loader.
Zombies reserve slots until reap.
