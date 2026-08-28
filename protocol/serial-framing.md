# CodexOS serial framing, version 1

The harness and guest exchange a sequence of frames over a raw byte stream.
Each frame is a 16-byte header followed immediately by its payload. There is
no delimiter, alignment padding, checksum, or compression.

All integer fields are unsigned and encoded in little-endian byte order. The
four magic bytes are the ASCII string `CXOS` (`43 58 4f 53` in hexadecimal).

| Offset | Size | Field | Version 1 encoding |
| ---: | ---: | --- | --- |
| 0 | 4 | magic | bytes `43 58 4f 53` (`CXOS`) |
| 4 | 2 | protocol version | little-endian `uint16_t`, value 1 |
| 6 | 2 | message type | little-endian `uint16_t` |
| 8 | 4 | request ID | little-endian `uint32_t` |
| 12 | 4 | payload length | little-endian `uint32_t` |

The payload starts at offset 16 and contains exactly `payload length` bytes.
A zero-length payload is valid. Frames may follow one another with no bytes
between them.

Message type and request ID are opaque values at this layer. Version 1 does
not assign message semantics or request/response behavior.

Version 1 permits payload lengths from 0 through 16,777,216 bytes (16 MiB),
inclusive. A receiver must reject a frame whose magic is not `CXOS`, whose
version is not 1, or whose advertised payload length exceeds that limit. The
length must be validated before allocating storage for the payload.
