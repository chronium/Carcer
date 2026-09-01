# CodexOS user ABI

CXE1 files have a 16-byte little-endian header: `CXE1`, image size,
entry offset, and zero flags, followed by the exact image. Generic `run`
loads any valid CXE privately: a nonempty, at-most-511-page RWX image at
`0x400000`, with an in-image entry and a zeroed stack page ending at
`0x600000`. Exit reclaims all task pages. The PIT preempts user code;
independent CPU-bound ring-3 tasks make progress without voluntary yields.
User CPU exceptions terminate only the faulting task with status UINT64_MAX;
the scheduler reclaims its pages and continues other workloads.

Ring 3 uses `int 0x80`: RAX is the call; arguments are RDI, RSI, RDX, RCX,
R8. Failure or an unknown call returns `UINT64_MAX`.

- 0: `exit(status)`, no return.
- 1: `file_size(path,path_length)`.
- 2: `file_read(path,path_length,offset,destination,count)`.
- 3: `file_attributes(path,path_length)`; bit 0 means immutable.
- 4: `file_write(path,path_length,offset,source,count)`.

Paths are explicit 1..255-byte spans. User buffers must stay wholly in the
image or stack and may cross image pages. Overflow, holes, and boundary
crossings fail. Reads return through EOF; offset past EOF fails. Offset-zero
writes may create files. Zero-length I/O ignores its data pointer.

Sealing is irreversible: writes, truncations, and removal all fail, including
zero-length/no-op mutations. The development protocol's generic
`import_provided_asset(id,path)` looks up the trusted advertised size,
copies an asset up to 64 MiB in bounded range reads into a newly created
ordinary file, removes partial files on failure, and seals the exact completed
file. Protocol-side file transactions are scheduler-atomic. Assets
are not automatically imported across boots. `run(path)` launches any valid
CXE through the same loader and scheduler and returns its task identifier.
