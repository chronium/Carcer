# CodexOS feature-request host service, version 1

The `request_feature` host service uses the request and response envelope
defined in [`host-service-protocol.md`](host-service-protocol.md). This
document defines only its service-specific arguments and result semantics.

The request contains exactly two arguments:

1. `title`: valid UTF-8 text from 1 through 256 encoded bytes.
2. `description`: valid UTF-8 text from zero through 16 KiB encoded bytes.

The trusted harness assigns a monotonically increasing positive request ID and
durably records the request with the current generation number and `pending`
status. Recording a request does not grant, provision, or otherwise change any
external capability.

Response status has these service-specific meanings:

| Status | Meaning |
| --- | --- |
| `0` | The feature request was recorded |
| `2` | The request was malformed, invalid for the session, or the trusted harness failed |

Success output is the assigned request ID encoded as canonical ASCII decimal
with no surrounding text. Failure output is a short UTF-8 explanation.

Once `finish_generation` has been accepted, the host-service session rejects
later feature requests. Approval and denial are trusted operator actions at a
generation gate and are not part of this service.
