# CodexOS host-service protocol version 1

This protocol lets a CodexOS guest request a named service from the external
harness. It uses the version 1 serial framing defined in
[`serial-framing.md`](serial-framing.md); all integers below are unsigned and
little-endian. The framing header supplies the message type, request ID, and
payload length.

Version 1 defines exactly two host-service message types:

| Value | Name |
| --- | --- |
| `0x0003` | `HOST_SERVICE_REQUEST` |
| `0x8003` | `HOST_SERVICE_RESPONSE` |

## Request

A `HOST_SERVICE_REQUEST` payload is encoded without padding:

```
uint16_t service_name_length
uint8_t  service_name[service_name_length]
uint16_t argument_count

repeat argument_count times:
    uint32_t argument_length
    uint8_t  argument[argument_length]
```

`service_name_length` is the encoded byte length, not a character count. It
must be from 1 through 255, and `service_name` must be valid UTF-8. Version 1
allows at most 64 arguments. Argument contents are arbitrary bytes; an
argument may be empty and may contain zero bytes or invalid UTF-8.

The request ID in the frame header is assigned by the guest and must be a
non-zero `uint32_t`. Version 1 permits one outstanding guest-originated
host-service request. Guest-originated request IDs and harness-originated tool
request IDs are independent namespaces, so the same numeric ID may be in use
in both directions at once.

## Response

The harness replies with `HOST_SERVICE_RESPONSE`, using exactly the request ID
from the corresponding request. Its payload is:

```
uint32_t status
uint8_t  output[remaining payload bytes]
```

Status zero means success. Any non-zero value means failure; version 1 assigns
no individual failure meanings. Output is arbitrary bytes and may be empty.

Both payloads are subject to the framing protocol's 16 MiB maximum. A decoder
must reject the wrong message type, request ID zero, truncated fields, an empty
or invalid UTF-8 service name, a service name longer than 255 encoded bytes,
more than 64 arguments, trailing request bytes, and oversized payloads. Version
1 does not permit pipelining, concurrent requests, or out-of-order responses.
