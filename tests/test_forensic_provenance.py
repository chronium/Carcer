import hashlib
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

from opentelemetry.sdk.metrics.export import InMemoryMetricReader

from harness import (
    BuildHostService,
    BuildReviewProvenance,
    BuildResult,
    BuildStatus,
    CandidateBootResult,
    HostServiceRequest,
    SnapshotFile,
    ToolResult,
    decode_source_snapshot,
    encode_source_snapshot,
)
from harness.codex_review_worker import _dispatch_read_only_tool
from harness.observability import ExperimentObservability
from harness.forensic_provenance import (
    ForensicProvenanceError,
    _atomic_bytes,
    _atomic_json,
)


class BuildProvenanceTests(unittest.TestCase):
    def test_structured_identity_events_do_not_create_metric_labels(self) -> None:
        snapshot = b"\x00\x00"
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            reader = InMemoryMetricReader()
            observability = ExperimentObservability(
                root / "run",
                metric_readers=[reader],
            )
            evidence = BuildReviewProvenance(
                root / "run", observability
            ).begin_build(2, snapshot)
            evidence.record_decoded(0, 0)
            observability.close()

            events = [
                json.loads(line)
                for line in (root / "run/events.jsonl").read_text().splitlines()
            ]
            received = next(
                event for event in events if event["event"] == "build_attempt_received"
            )
            self.assertEqual(received["data"]["build_attempt_id"], "build-000001")
            self.assertEqual(
                received["data"]["source_snapshot_sha256"],
                hashlib.sha256(snapshot).hexdigest(),
            )
            metrics = reader.get_metrics_data()
            for resource in metrics.resource_metrics:
                for scope in resource.scope_metrics:
                    for metric in scope.metrics:
                        for point in metric.data.data_points:
                            self.assertNotIn("build_attempt_id", point.attributes)
                            self.assertNotIn("source_snapshot_sha256", point.attributes)

    def test_build_attempts_are_durable_and_preserve_latest_success(self) -> None:
        snapshot = encode_source_snapshot(
            (SnapshotFile("seed/kernel.c", b"exact source bytes"),)
        )
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            provenance = BuildReviewProvenance(root / "run")
            validator = _SuccessfulValidator()
            service = BuildHostService(
                root / "staging",
                validator,
                generation=12,
                provenance=provenance,
            )

            with patch(
                "harness.build_host_service.build_source_snapshot",
                side_effect=_synthetic_successful_build,
            ):
                first = service.handle_request(
                    HostServiceRequest(1, "build", (snapshot,))
                )
                second = service.handle_request(
                    HostServiceRequest(2, "build", (snapshot,))
                )

            self.assertEqual(int.from_bytes(first.payload[:4], "little"), 0)
            self.assertEqual(int.from_bytes(second.payload[:4], "little"), 0)
            attempts = sorted(
                (root / "run" / "build-review-provenance" / "generation-0012").glob(
                    "build-*"
                )
            )
            self.assertEqual(
                [path.name for path in attempts],
                ["build-000001", "build-000002"],
            )
            for attempt in attempts:
                manifest = json.loads((attempt / "manifest.json").read_text())
                self.assertEqual(manifest["outcome"], "success")
                self.assertEqual(
                    manifest["source_snapshot"],
                    {
                        "sha256": hashlib.sha256(snapshot).hexdigest(),
                        "size": len(snapshot),
                        "decoded": True,
                        "file_count": 1,
                        "content_size": len(b"exact source bytes"),
                    },
                )
                self.assertEqual((attempt / "source.snapshot").read_bytes(), snapshot)
                self.assertEqual(
                    decode_source_snapshot((attempt / "source.snapshot").read_bytes()),
                    decode_source_snapshot(snapshot),
                )
                self.assertTrue(manifest["latest_success"]["ready"])
                self.assertTrue(
                    manifest["latest_success"]["protocol_validated"]
                )
                self.assertEqual(
                    manifest["artifacts"]["kernel"],
                    {
                        "sha256": hashlib.sha256(b"synthetic kernel").hexdigest(),
                        "size": len(b"synthetic kernel"),
                    },
                )
                self.assertEqual(
                    manifest["artifacts"]["iso"],
                    {
                        "sha256": hashlib.sha256(b"synthetic iso").hexdigest(),
                        "size": len(b"synthetic iso"),
                    },
                )
            latest = service.latest_successful_build
            self.assertIsNotNone(latest)
            self.assertEqual(latest.build_attempt_id, "build-000002")
            self.assertEqual(latest.source_snapshot, snapshot)

            reopened = BuildReviewProvenance(root / "run")
            third = reopened.begin_build(12, snapshot)
            self.assertEqual(third.attempt_id, "build-000003")

    def test_malformed_and_later_failed_builds_do_not_claim_or_erase_success(
        self,
    ) -> None:
        snapshot = encode_source_snapshot(
            (SnapshotFile("seed/kernel.c", b"source"),)
        )
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            service = BuildHostService(
                root / "staging",
                _SuccessfulValidator(),
                generation=4,
                provenance=BuildReviewProvenance(root / "run"),
            )
            with patch(
                "harness.build_host_service.build_source_snapshot",
                side_effect=_synthetic_successful_build,
            ):
                service.handle_request(HostServiceRequest(1, "build", (snapshot,)))
            latest = service.latest_successful_build

            with patch(
                "harness.build_host_service.build_source_snapshot",
                return_value=BuildResult(BuildStatus.HARNESS_FAILURE, "malformed"),
            ):
                response = service.handle_request(
                    HostServiceRequest(2, "build", (b"\x01",))
                )

            self.assertEqual(int.from_bytes(response.payload[:4], "little"), 2)
            self.assertIs(service.latest_successful_build, latest)
            manifest = json.loads(
                (
                    root
                    / "run/build-review-provenance/generation-0004"
                    / "build-000002/manifest.json"
                ).read_text()
            )
            self.assertFalse(manifest["source_snapshot"]["decoded"])
            self.assertEqual(manifest["outcome"], "harness_failure")
            self.assertNotIn("latest_success", manifest)

    def test_failed_candidate_never_advances_latest_success(self) -> None:
        snapshot = encode_source_snapshot(
            (SnapshotFile("seed/kernel.c", b"source"),)
        )
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            service = BuildHostService(
                root / "staging",
                _FailedValidator(),
                generation=5,
                provenance=BuildReviewProvenance(root / "run"),
            )
            with patch(
                "harness.build_host_service.build_source_snapshot",
                side_effect=_synthetic_successful_build,
            ):
                response = service.handle_request(
                    HostServiceRequest(1, "build", (snapshot,))
                )

            self.assertEqual(int.from_bytes(response.payload[:4], "little"), 1)
            self.assertIsNone(service.latest_successful_build)
            attempt = (
                root
                / "run/build-review-provenance/generation-0005/build-000001"
            )
            manifest = json.loads((attempt / "manifest.json").read_text())
            self.assertEqual(manifest["outcome"], "build_failure")
            self.assertNotIn("latest_success", manifest)
            self.assertFalse((attempt / "source.snapshot").exists())

    def test_atomic_failure_never_replaces_a_success_manifest(self) -> None:
        snapshot = encode_source_snapshot(
            (SnapshotFile("seed/kernel.c", b"source"),)
        )
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            evidence = BuildReviewProvenance(root).begin_build(1, snapshot)
            manifest_path = (
                root
                / "build-review-provenance/generation-0001/build-000001/manifest.json"
            )
            before = manifest_path.read_bytes()
            with patch(
                "harness.forensic_provenance.os.replace",
                side_effect=OSError("synthetic replace failure"),
            ):
                with self.assertRaisesRegex(RuntimeError, "cannot write"):
                    evidence.record_final("success")
            self.assertEqual(manifest_path.read_bytes(), before)
            self.assertFalse(list(manifest_path.parent.glob(".manifest.json-*")))

    def test_build_provenance_failure_is_fail_closed_and_preserves_latest(
        self,
    ) -> None:
        snapshot = encode_source_snapshot(
            (SnapshotFile("seed/kernel.c", b"source"),)
        )
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            service = BuildHostService(
                root / "staging",
                _SuccessfulValidator(),
                generation=7,
                provenance=BuildReviewProvenance(root / "run"),
            )
            with patch(
                "harness.build_host_service.build_source_snapshot",
                side_effect=_synthetic_successful_build,
            ):
                first = service.handle_request(
                    HostServiceRequest(1, "build", (snapshot,))
                )
                latest = service.latest_successful_build
                original_atomic_bytes = _atomic_bytes

                def fail_successful_snapshot(path: Path, value: bytes) -> None:
                    if path.name == "source.snapshot":
                        raise ForensicProvenanceError("synthetic storage failure")
                    original_atomic_bytes(path, value)

                with patch(
                    "harness.forensic_provenance._atomic_bytes",
                    side_effect=fail_successful_snapshot,
                ):
                    second = service.handle_request(
                        HostServiceRequest(2, "build", (snapshot,))
                    )

            self.assertEqual(int.from_bytes(first.payload[:4], "little"), 0)
            self.assertEqual(int.from_bytes(second.payload[:4], "little"), 2)
            self.assertIs(service.latest_successful_build, latest)
            self.assertIsNotNone(latest)
            self.assertEqual(latest.build_attempt_id, "build-000001")
            second_manifest = json.loads(
                (
                    root
                    / "run/build-review-provenance/generation-0007"
                    / "build-000002/manifest.json"
                ).read_text()
            )
            self.assertNotEqual(second_manifest["outcome"], "success")
            self.assertNotIn("latest_success", second_manifest)

    def test_incomplete_ids_remain_reserved_and_future_schema_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            provenance = BuildReviewProvenance(root)
            first = provenance.begin_build(6, b"\x00\x00")
            self.assertEqual(first.attempt_id, "build-000001")
            generation = root / "build-review-provenance/generation-0006"
            (generation / "build-000003").mkdir()
            next_attempt = provenance.begin_build(6, b"\x00\x00")
            self.assertEqual(next_attempt.attempt_id, "build-000004")

            future = generation / "build-000005"
            future.mkdir()
            (future / "manifest.json").write_text(
                json.dumps(
                    {
                        "schema_version": 2,
                        "kind": "build_attempt",
                        "generation": 6,
                        "attempt_id": "build-000005",
                    }
                )
            )
            with self.assertRaisesRegex(RuntimeError, "unsupported or inconsistent"):
                provenance.begin_build(6, b"\x00\x00")


class ReviewProvenanceTests(unittest.TestCase):
    def test_exact_reviewer_source_result_is_recorded_without_changing_it(self) -> None:
        output = b"source\x00bytes\xff"
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            evidence = BuildReviewProvenance(root).begin_review(9)
            runtime = Mock()
            runtime.invoke_tool.return_value = ToolResult(0, output)
            runtime.observability = None

            result = _dispatch_read_only_tool(
                runtime,
                "read",
                {"path": "seed/tasks.c", "offset": 7, "length": 14},
                evidence,
            )
            evidence.complete("completed")

            self.assertEqual(result, ToolResult(0, output))
            runtime.invoke_tool.assert_called_once_with(
                "read", [b"seed/tasks.c", b"7", b"14"]
            )
            review = root / "build-review-provenance/generation-0009/review-000001"
            manifest = json.loads((review / "manifest.json").read_text())
            self.assertEqual(manifest["review_id"], "review-000001")
            self.assertEqual(manifest["review_outcome"], "completed")
            self.assertEqual(manifest["capture_outcome"], "complete")
            self.assertTrue(manifest["evidence_complete"])
            read = manifest["source_reads"][0]
            self.assertEqual(read["path"], "seed/tasks.c")
            self.assertEqual(read["offset"], 7)
            self.assertEqual(read["length"], 14)
            self.assertEqual(read["status"], 0)
            self.assertEqual(read["returned_bytes"], len(output))
            self.assertEqual(read["sha256"], hashlib.sha256(output).hexdigest())
            self.assertEqual((review / read["content_file"]).read_bytes(), output)

            with self.assertRaisesRegex(ValueError, "unsupported reviewer"):
                _dispatch_read_only_tool(
                    runtime,
                    "read_provided_asset",
                    {"id": "private", "offset": 0, "length": 1},
                    evidence,
                )
            self.assertEqual(len(list(review.glob("read-*.bin"))), 1)

    def test_failed_content_write_keeps_review_result_and_incomplete_evidence(
        self,
    ) -> None:
        output = b"exact reviewer bytes"
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            evidence = BuildReviewProvenance(root).begin_review(10)
            runtime = Mock()
            runtime.invoke_tool.return_value = ToolResult(0, output)
            runtime.observability = None

            with patch(
                "harness.forensic_provenance._atomic_bytes",
                side_effect=ForensicProvenanceError("synthetic byte failure"),
            ):
                result = _dispatch_read_only_tool(
                    runtime,
                    "read",
                    {"path": "seed/tasks.c", "offset": 0, "length": len(output)},
                    evidence,
                )
            evidence.complete("completed")

            self.assertEqual(result, ToolResult(0, output))
            review = root / "build-review-provenance/generation-0010/review-000001"
            manifest = json.loads((review / "manifest.json").read_text())
            self.assertEqual(manifest["review_outcome"], "completed")
            self.assertEqual(manifest["capture_outcome"], "incomplete")
            self.assertFalse(manifest["evidence_complete"])
            self.assertEqual(manifest["source_reads"], [])
            self.assertFalse(list(review.glob("read-*.bin")))

    def test_unwritable_incomplete_marker_leaves_safe_initial_manifest(self) -> None:
        output = b"uncaptured reviewer bytes"
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            evidence = BuildReviewProvenance(root).begin_review(10)
            runtime = Mock()
            runtime.invoke_tool.return_value = ToolResult(0, output)
            runtime.observability = None

            with patch(
                "harness.forensic_provenance._atomic_bytes",
                side_effect=ForensicProvenanceError("persistent storage failure"),
            ):
                result = _dispatch_read_only_tool(
                    runtime,
                    "read",
                    {"path": "seed/tasks.c", "offset": 0, "length": len(output)},
                    evidence,
                )
                with self.assertRaisesRegex(RuntimeError, "storage failure"):
                    evidence.complete("completed")

            self.assertEqual(result, ToolResult(0, output))
            manifest = json.loads(
                (
                    root
                    / "build-review-provenance/generation-0010"
                    / "review-000001/manifest.json"
                ).read_text()
            )
            self.assertEqual(manifest["review_outcome"], "incomplete")
            self.assertEqual(manifest["capture_outcome"], "in_progress")
            self.assertFalse(manifest["evidence_complete"])
            self.assertEqual(manifest["source_reads"], [])

    def test_manifest_failure_recovers_only_from_verified_exact_content(self) -> None:
        output = b"recoverable exact bytes"
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            evidence = BuildReviewProvenance(root).begin_review(11)
            runtime = Mock()
            runtime.invoke_tool.return_value = ToolResult(0, output)
            runtime.observability = None
            original_atomic_json = _atomic_json
            failed = False

            def fail_first_manifest(path: Path, value: object) -> None:
                nonlocal failed
                if not failed:
                    failed = True
                    raise ForensicProvenanceError("synthetic manifest failure")
                original_atomic_json(path, value)

            with patch(
                "harness.forensic_provenance._atomic_json",
                side_effect=fail_first_manifest,
            ):
                result = _dispatch_read_only_tool(
                    runtime,
                    "read",
                    {"path": "seed/kernel.c", "offset": 2, "length": len(output)},
                    evidence,
                )
                durable_before = json.loads(
                    (
                        root
                        / "build-review-provenance/generation-0011"
                        / "review-000001/manifest.json"
                    ).read_text()
                )
                self.assertFalse(durable_before["evidence_complete"])
                self.assertEqual(durable_before["source_reads"], [])
                evidence.complete("completed")

            self.assertEqual(result, ToolResult(0, output))
            review = root / "build-review-provenance/generation-0011/review-000001"
            manifest = json.loads((review / "manifest.json").read_text())
            self.assertEqual(manifest["review_outcome"], "completed")
            self.assertTrue(manifest["evidence_complete"])
            self.assertEqual(manifest["capture_outcome"], "complete")
            entry = manifest["source_reads"][0]
            self.assertEqual((review / entry["content_file"]).read_bytes(), output)

    def test_missing_or_corrupt_content_prevents_complete_evidence(self) -> None:
        for corruption in ("missing", "size", "hash", "orphan"):
            with self.subTest(corruption=corruption):
                with tempfile.TemporaryDirectory() as temporary:
                    root = Path(temporary)
                    evidence = BuildReviewProvenance(root).begin_review(12)
                    evidence.record_source_read(
                        "seed/tasks.c", 0, 5, 0, b"exact"
                    )
                    review = (
                        root
                        / "build-review-provenance/generation-0012/review-000001"
                    )
                    content = review / "read-000001.bin"
                    if corruption == "missing":
                        content.unlink()
                    elif corruption == "size":
                        content.write_bytes(b"different-size")
                    elif corruption == "orphan":
                        (review / "read-999999.bin").write_bytes(b"orphan")
                    else:
                        content.write_bytes(b"other")
                    evidence.complete("completed")
                    manifest = json.loads((review / "manifest.json").read_text())
                    self.assertEqual(manifest["review_outcome"], "completed")
                    self.assertFalse(manifest["evidence_complete"])
                    self.assertEqual(manifest["capture_outcome"], "incomplete")

    def test_failed_and_cancelled_review_outcomes_are_separate_from_capture(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            provenance = BuildReviewProvenance(root)
            for expected, evidence in (
                ("failed", provenance.begin_review(13)),
                ("cancelled", provenance.begin_review(13)),
            ):
                evidence.complete(expected)
            manifests = sorted(
                (root / "build-review-provenance/generation-0013").glob(
                    "review-*/manifest.json"
                )
            )
            self.assertEqual(len(manifests), 2)
            for expected, path in zip(("failed", "cancelled"), manifests):
                manifest = json.loads(path.read_text())
                self.assertEqual(manifest["review_outcome"], expected)
                self.assertTrue(manifest["evidence_complete"])
                self.assertEqual(manifest["capture_outcome"], "complete")

    def test_historical_manifest_without_completeness_is_not_rewritten(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            generation = root / "build-review-provenance/generation-0014"
            legacy = generation / "review-000001"
            legacy.mkdir(parents=True)
            legacy_manifest = {
                "schema_version": 1,
                "kind": "review",
                "generation": 14,
                "review_id": "review-000001",
                "stage": "completed",
                "outcome": "completed",
                "source_reads": [],
            }
            encoded = (json.dumps(legacy_manifest, sort_keys=True) + "\n").encode()
            (legacy / "manifest.json").write_bytes(encoded)

            current = BuildReviewProvenance(root).begin_review(14)

            self.assertEqual((legacy / "manifest.json").read_bytes(), encoded)
            self.assertNotIn("evidence_complete", legacy_manifest)
            current_manifest = json.loads(
                (
                    generation / "review-000002/manifest.json"
                ).read_text()
            )
            self.assertFalse(current_manifest["evidence_complete"])
            self.assertEqual(current_manifest["capture_outcome"], "in_progress")


class _SuccessfulValidator:
    def validate(self, iso, *, evidence=None, iso_identity=None):
        if evidence is not None:
            evidence.record_candidate_stage(
                "build_candidate_validation_started",
                "candidate_started",
                expected_iso_sha256=iso_identity.sha256,
            )
            evidence.record_candidate_stage(
                "build_candidate_qemu_started", "candidate_qemu_started"
            )
            evidence.record_candidate_stage(
                "build_candidate_ready_observed", "ready_observed", ready=True
            )
            evidence.record_candidate_stage(
                "build_protocol_validation_started", "protocol_validation_started"
            )
            evidence.record_candidate_stage(
                "build_protocol_validation_completed",
                "protocol_validated",
                outcome="success",
                protocol_validated=True,
            )
            evidence.record_candidate_stage(
                "build_candidate_validation_completed",
                "candidate_completed",
                outcome="success",
            )
        return CandidateBootResult(BuildStatus.SUCCESS, "")


class _FailedValidator:
    def validate(self, iso, *, evidence=None, iso_identity=None):
        if evidence is not None:
            evidence.record_candidate_stage(
                "build_candidate_validation_started",
                "candidate_started",
                expected_iso_sha256=iso_identity.sha256,
            )
            evidence.record_candidate_stage(
                "build_candidate_validation_completed",
                "candidate_completed",
                outcome="build_failure",
            )
        return CandidateBootResult(BuildStatus.BUILD_FAILURE, "candidate failed")


def _synthetic_successful_build(snapshot: bytes, output_directory: Path) -> BuildResult:
    output_directory.mkdir(parents=True, exist_ok=True)
    kernel = output_directory / "kernel.elf"
    iso = output_directory / "codexos.iso"
    kernel.write_bytes(b"synthetic kernel")
    iso.write_bytes(b"synthetic iso")
    return BuildResult(BuildStatus.SUCCESS, "", kernel, iso)


if __name__ == "__main__":
    unittest.main()
