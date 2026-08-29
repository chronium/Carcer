# CodexOS source snapshot, version 1

A source snapshot serializes the bounded guest-owned tree used as input to the
trusted host build procedure. It is not a general archive format. All integer
fields are unsigned and little-endian.

The payload begins with a `uint16_t file_count`, followed by exactly that many
file records:

| Field | Type | Meaning |
| --- | --- | --- |
| `path_length` | `uint16_t` | Encoded path length in bytes |
| `path` | `uint8_t[path_length]` | UTF-8 path bytes |
| `content_length` | `uint32_t` | File content length in bytes |
| `content` | `uint8_t[content_length]` | Arbitrary file bytes |

Version 1 permits at most 128 files, at most 255 encoded bytes per non-empty
path, and at most 64 KiB of file content across the complete snapshot. Paths
must be valid UTF-8 and unique. Truncated fields, duplicate paths, and bytes
after the final file record are invalid.

For a build snapshot, every path must be relative and have the form
`seed/<remaining components>`. Empty, `.`, and `..` components, embedded NUL,
and absolute paths are invalid. Paths are rejected rather than rewritten.

Snapshots contain no directories, permissions, timestamps, ownership,
compression, checksums, or other metadata.
