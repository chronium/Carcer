import json
import tempfile
import unittest
from pathlib import Path

from harness import CodexOSRun, FeatureRequest, RuntimeState
from harness.feature_requests import FeatureRequestError, FeatureRequestStore


class FeatureRequestStoreTests(unittest.TestCase):
    def test_import_preserves_sparse_identities_and_future_allocation(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            store = FeatureRequestStore(temporary)
            inherited = (
                FeatureRequest(2, 8, "Pending", "Exact text", "pending"),
                FeatureRequest(7, 10, "Denied", "Exact denial", "denied"),
            )
            store.import_requests(inherited)
            self.assertEqual(store.requests(), inherited)
            created = store.create(0, "New run", "No collision")
            self.assertEqual(created.id, 8)
            with self.assertRaisesRegex(FeatureRequestError, "not empty"):
                store.import_requests(inherited)

    def test_persists_monotonic_unicode_requests_and_durable_decisions(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            run_directory = Path(temporary) / "run"
            store = FeatureRequestStore(run_directory)
            self.assertEqual(store.requests(), ())

            first = store.create(3, "Δυνατότητα", "Line one.\nLinia a doua: λ")
            second = store.create(4, "Another capability", "")
            self.assertEqual(first.id, 1)
            self.assertEqual(second.id, 2)
            self.assertEqual(first.status, "pending")

            reconstructed = FeatureRequestStore(run_directory)
            self.assertEqual(reconstructed.requests(), (first, second))
            approved = reconstructed.approve(first.id)
            denied = reconstructed.deny(second.id)
            self.assertEqual(approved.status, "approved")
            self.assertEqual(denied.status, "denied")

            again = FeatureRequestStore(run_directory)
            self.assertEqual(again.request(1), approved)
            self.assertEqual(again.request(2), denied)
            third = again.create(5, "Third", "IDs survive reconstruction.")
            self.assertEqual(third.id, 3)
            with self.assertRaisesRegex(FeatureRequestError, "already approved"):
                again.deny(1)
            with self.assertRaisesRegex(FeatureRequestError, "already denied"):
                again.approve(2)

            paths = sorted(
                path.name
                for path in (run_directory / "feature-requests").iterdir()
            )
            self.assertEqual(
                paths,
                [
                    "request-000001.json",
                    "request-000002.json",
                    "request-000003.json",
                ],
            )

    def test_rejects_malformed_or_conflicting_persisted_records(self) -> None:
        malformed_cases = {
            "bad filename": ("request-title.json", b"{}"),
            "bad JSON": ("request-000001.json", b"{"),
            "wrong ID": (
                "request-000001.json",
                _record(id=2),
            ),
            "extra field": (
                "request-000001.json",
                _record(extra=True),
            ),
            "invalid status": (
                "request-000001.json",
                _record(status="reopened"),
            ),
        }
        for label, (name, contents) in malformed_cases.items():
            with self.subTest(label=label), tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary) / "feature-requests"
                directory.mkdir()
                (directory / name).write_bytes(contents)
                with self.assertRaises(FeatureRequestError):
                    FeatureRequestStore(temporary)

    def test_validates_bounds_and_never_uses_request_text_as_a_path(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            run_directory = Path(temporary)
            store = FeatureRequestStore(run_directory)
            with self.assertRaisesRegex(FeatureRequestError, "must not be empty"):
                store.create(0, "", "description")
            with self.assertRaisesRegex(FeatureRequestError, "256 bytes"):
                store.create(0, "é" * 129, "description")
            with self.assertRaisesRegex(FeatureRequestError, "16 KiB"):
                store.create(0, "title", "x" * (16 * 1024 + 1))

            request = store.create(
                0,
                "../../outside.json",
                "../description is inert text",
            )
            self.assertEqual(request.id, 1)
            self.assertFalse((run_directory / "outside.json").exists())
            self.assertEqual(
                [path.name for path in (run_directory / "feature-requests").iterdir()],
                ["request-000001.json"],
            )

    def test_reads_authoritative_current_state_without_modifying_records(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            run_directory = Path(temporary) / "run"
            reader = FeatureRequestStore(run_directory)
            writer = FeatureRequestStore(run_directory)
            first = writer.create(4, "Pending λ", "Exact pending text.")
            second = writer.create(5, "Decision", "Exact decision text.")

            self.assertEqual(reader.requests(), (first, second))
            approved = writer.approve(first.id)
            denied = writer.deny(second.id)
            self.assertEqual(reader.request(first.id), approved)
            self.assertEqual(reader.request(second.id), denied)
            self.assertEqual(reader.requests(), (approved, denied))

            records = run_directory / "feature-requests"
            before = {
                path.name: path.read_bytes()
                for path in records.iterdir()
            }
            self.assertEqual(reader.requests(), (approved, denied))
            self.assertEqual(reader.requests(), (approved, denied))
            self.assertEqual(
                {path.name: path.read_bytes() for path in records.iterdir()},
                before,
            )
            (records / "unexpected.json").write_text("{}", encoding="utf-8")
            with self.assertRaisesRegex(
                FeatureRequestError,
                "invalid feature-request filename",
            ):
                reader.requests()

    def test_runtime_decisions_are_gate_only_and_survive_reconstruction(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            runtime = CodexOSRun(temporary)
            request = runtime._feature_request_store.create(2, "Capability", "Text")
            with self.assertRaisesRegex(RuntimeError, "only while awaiting"):
                runtime.approve_feature_request(request.id)

            runtime._state = RuntimeState.AWAITING_NEXT_GENERATION
            approved = runtime.approve_feature_request(request.id)
            self.assertEqual(approved.status, "approved")
            self.assertEqual(runtime.feature_request(request.id), approved)

            reconstructed = CodexOSRun(temporary)
            self.assertEqual(reconstructed.feature_requests(), (approved,))
            reconstructed._state = RuntimeState.AWAITING_NEXT_GENERATION
            with self.assertRaisesRegex(FeatureRequestError, "already approved"):
                reconstructed.deny_feature_request(request.id)


def _record(
    *,
    id: int = 1,
    generation: int = 0,
    title: str = "title",
    description: str = "description",
    status: str = "pending",
    extra: bool = False,
) -> bytes:
    value: dict[str, object] = {
        "id": id,
        "generation": generation,
        "title": title,
        "description": description,
        "status": status,
    }
    if extra:
        value["unexpected"] = True
    return (json.dumps(value) + "\n").encode("utf-8")


if __name__ == "__main__":
    unittest.main()
