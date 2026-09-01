# CodexOS user ABI

CXE1: `CXE1`, LE image size, entry offset, zero flags, then <=511 pages.
`run(path)` privately maps it RWX at 0x400000; stack top is 0x600000.
PIT ticks preempt ring 3. User faults exit that task with UINT64_MAX.

`int 0x80`: RAX call; args RDI, RSI, RDX, RCX, R8; failure UINT64_MAX.

- 0 exit(status)
- 1 size(path,len)
- 2 read(path,len,offset,destination,count)
- 3 attributes(path,len); immutable bit 0
- 4 write(path,len,offset,source,count)
- 5 spawn(path,len), returns task ID
- 6 reap(id,status_pointer): 0 running; 1 stores status and frees slot

Paths are 1..255-byte UTF-8 spans; buffers stay in image or stack.
Sealed files reject mutation.

`import_provided_asset` creates an ephemeral sealed ordinary file up to
64 MiB. Protocol `run` shares the loader. Protocol `reap` returns
`running` or consumes exact decimal status. Exited slots stay reserved
until reap.
