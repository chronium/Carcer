# CodexOS tool protocol, version 1

This protocol uses the message type, request ID, and payload fields of the
[CodexOS serial framing protocol](serial-framing.md). All integers in payloads
are unsigned and encoded in little-endian byte order.

## Message types

| Value | Name |
| ---: | --- |
| `0x0001` | `LIST_TOOLS_REQUEST` |
| `0x8001` | `LIST_TOOLS_RESPONSE` |
| `0x0002` | `INVOKE_TOOL_REQUEST` |
| `0x8002` | `INVOKE_TOOL_RESPONSE` |

Every harness request uses a non-zero `uint32_t` request ID. Its response must
use exactly the same request ID and the matching response message type shown
above. Request ID 0 is reserved. Version 1 permits only one outstanding
request; it does not support pipelining, concurrency, or out-of-order
responses.

## List tools

`LIST_TOOLS_REQUEST` has an empty payload.

`LIST_TOOLS_RESPONSE` begins with a little-endian `uint16_t tool_count`, then
contains exactly `tool_count` entries. Each entry is a little-endian
`uint16_t name_length` followed immediately by `name_length` bytes of UTF-8.
The length counts encoded bytes, not characters. No trailing bytes are
permitted after the final entry.

Version 1 allows at most 256 entries. Each name must contain 1 through 255
bytes and must be valid UTF-8. Names have no description, schema, category,
version, or other metadata.

## Invoke tool

`INVOKE_TOOL_REQUEST` has this payload, in order:

1. little-endian `uint16_t tool_name_length`;
2. exactly `tool_name_length` bytes of UTF-8 tool name;
3. little-endian `uint16_t argument_count`;
4. exactly `argument_count` arguments, each consisting of a little-endian
   `uint32_t argument_length` followed by exactly `argument_length` bytes.

The tool name contains 1 through 255 encoded bytes and must be valid UTF-8.
Version 1 allows at most 64 arguments. Argument data is opaque binary data;
it may be empty, need not be UTF-8, and may contain zero bytes. No trailing
bytes are permitted after the final argument. Individual arguments and the
complete payload remain subject to the framing protocol's 16 MiB payload
limit.

`INVOKE_TOOL_RESPONSE` begins with a little-endian `uint32_t status`. Every
remaining payload byte is opaque output. Status 0 means success; non-zero
values mean tool failure but have no assigned version 1 meaning. Empty output
is valid.

Receivers must reject truncated fields, empty or invalid UTF-8 names, counts
or name lengths above the version 1 limits, and unexpected trailing data in
fully length-specified payloads. The harness also rejects responses whose
request ID or message type does not match its outstanding request.
