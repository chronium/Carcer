# CodexOS provided-assets host services, version 1

The provided-assets services use the request and response envelope defined in
[`host-service-protocol.md`](host-service-protocol.md). They expose only the
immutable asset snapshot explicitly supplied by the trusted operator when the
harness process started.

## `list_provided_assets`

The request has no arguments. Success status is `0` and the response is a
deterministic UTF-8 descriptor list ordered by asset ID. Each record is:

```text
<id>\t<filename>\t<size-decimal>\t<sha256-hex>\n
```

The digest is lowercase SHA-256 over the exact transported bytes. A configured
empty asset set returns an empty successful payload. Arguments or other invalid
guest input return status `1`; a trusted harness failure returns status `2`.

## `read_provided_asset`

The request has exactly three arguments:

1. asset ID as UTF-8;
2. offset as canonical unsigned ASCII decimal;
3. length as canonical unsigned ASCII decimal.

Success status is `0` and the response body is the exact requested raw byte
range. Offset and length are unsigned 64-bit values, length is at most 1 MiB,
and the complete range must satisfy `offset + length <= size`. An offset equal
to size is valid only with zero length. Invalid encodings, unknown IDs,
overflow, and out-of-range requests return status `1` with a bounded textual
diagnostic. Reads are never silently truncated. Trusted harness failures use
status `2`.

These services expose no host path and never reopen external files. The active
generation and its ephemeral candidate-validation VMs use the same frozen
in-memory snapshot. The provided-assets dispatcher remains available whenever
each VM is running, both while the guest is approaching canonical READY and
after READY, independently of harness-initiated development-tool traffic.
Other trusted development host services retain their scoped invocation
semantics and are not thereby exposed to idle guest code. The interface does
not define a guest filesystem, installation path, extraction behavior,
executable format, or use policy.
