import struct
import unittest

from harness import (
    SnapshotFile,
    SourceSnapshotError,
    decode_source_snapshot,
    encode_source_snapshot,
)


class SourceSnapshotTests(unittest.TestCase):
    def test_round_trip_preserves_paths_and_binary_contents(self) -> None:
        files = (
            SnapshotFile("seed/kernel.c", b"source\x00bytes\xff"),
            SnapshotFile("seed/empty.bin", b""),
        )

        self.assertEqual(decode_source_snapshot(encode_source_snapshot(files)), files)

    def test_rejects_malformed_and_duplicate_entries(self) -> None:
        valid = encode_source_snapshot((SnapshotFile("seed/a", b"data"),))
        duplicate_record = struct.pack("<H", 6) + b"seed/a" + struct.pack("<I", 0)

        with self.assertRaisesRegex(SourceSnapshotError, "truncated"):
            decode_source_snapshot(b"\x01")
        with self.assertRaisesRegex(SourceSnapshotError, "trailing"):
            decode_source_snapshot(valid + b"x")
        with self.assertRaisesRegex(SourceSnapshotError, "duplicate"):
            decode_source_snapshot(struct.pack("<H", 2) + duplicate_record * 2)

    def test_rejects_unsafe_build_paths(self) -> None:
        for path in (
            b"seed/../outside",
            b"/seed/kernel.c",
            b"seed//kernel.c",
            b"seed/bad\x00name",
        ):
            with self.subTest(path=path):
                snapshot = (
                    struct.pack("<HH", 1, len(path))
                    + path
                    + struct.pack("<I", 0)
                )
                with self.assertRaisesRegex(SourceSnapshotError, "unsafe"):
                    decode_source_snapshot(snapshot)


if __name__ == "__main__":
    unittest.main()
