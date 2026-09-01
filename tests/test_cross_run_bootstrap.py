from __future__ import annotations

import hashlib
import io
import json
import tempfile
import unittest
from contextlib import redirect_stderr
from pathlib import Path
from unittest.mock import patch

import harness.cross_run_bootstrap as cross_run_bootstrap_module
from harness import (
    CROSS_RUN_BOOTSTRAP_FEATURE_LEDGER,
    CROSS_RUN_BOOTSTRAP_HANDOFF,
    CROSS_RUN_BOOTSTRAP_MANIFEST,
    TEST_HARDWARE_PROFILE,
    CodexOSRun,
    CrossRunBootstrapError,
    FeatureRequestError,
    FeatureRequestStore,
    GenerationGitRecorder,
    RuntimeState,
    SnapshotFile,
    initialize_cross_run_bootstrap,
    load_cross_run_bootstrap,
)
from harness.codex_generation_worker import _planning_prompt
from harness.operator_console import main
from tests.test_generation_git import (
    _archive_aborted,
    _archive_completed,
    _create_repository,
    _generation_tag,
    _git,
)


class CrossRunBootstrapTests(unittest.TestCase):
    def test_ordinary_initial_run_has_no_inherited_state(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            run = root / "ordinary"
            image = root / "initial.iso"
            image.write_bytes(b"ordinary image")
            runtime = CodexOSRun(run, hardware_profile=TEST_HARDWARE_PROFILE)
            with patch.object(runtime, "_configure_provided_assets"), patch.object(
                runtime, "_boot_generation"
            ):
                runtime.start(image)

            self.assertIsNone(runtime.previous_handoff)
            self.assertFalse((run / CROSS_RUN_BOOTSTRAP_MANIFEST).exists())
            self.assertFalse((run / CROSS_RUN_BOOTSTRAP_HANDOFF).exists())

    def test_generation_fifteen_successor_becomes_generation_zero_context(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "experiment-002"
            for generation in range(16):
                _archive_completed(
                    source,
                    generation,
                    generation - 1 if generation else None,
                    "successor" if generation else "initial",
                    [
                        SnapshotFile(
                            "seed/kernel.c",
                            f"generation {generation}\n".encode("ascii"),
                        )
                    ],
                    handoff=f"Handoff from Experiment 2 G{generation}.",
                )
            source_store = FeatureRequestStore(source)
            inherited = source_store.create(
                10,
                "Inherited request",
                "Exact cross-experiment state.",
            )
            inherited = source_store.approve(inherited.id)
            repository, _ = _create_repository(root / "repository")
            source_record = GenerationGitRecorder(
                repository, source, "test-base"
            ).reconcile()[-1]
            image = source / "generation-0015" / "successor" / "codexos.iso"
            destination = root / "experiment-003"

            initialize_cross_run_bootstrap(
                destination,
                image,
                source,
                15,
                repository,
                source_record.tag,
            )
            runtime = CodexOSRun(
                destination,
                hardware_profile=TEST_HARDWARE_PROFILE,
            )
            with patch.object(runtime, "_configure_provided_assets"), patch.object(
                runtime, "_boot_generation"
            ):
                runtime.start(image)

            self.assertEqual(runtime.generation_number, 0)
            self.assertEqual(
                runtime.previous_handoff,
                "Handoff from Experiment 2 G15.",
            )
            self.assertIn(
                "Previous generation handoff:\nHandoff from Experiment 2 G15.",
                _planning_prompt(runtime, None),
            )
            self.assertEqual(runtime.feature_requests(), (inherited,))

    def test_imports_handoff_feature_ledger_and_immutable_identities(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "experiment-source"
            destination = root / "experiment-destination"
            handoff = "Exact predecessor handoff λ.\nSecond line."
            _archive_completed(
                source,
                0,
                None,
                "initial",
                [SnapshotFile("seed/kernel.c", b"inherited source\n")],
                handoff=handoff,
            )
            source_store = FeatureRequestStore(source)
            pending = source_store.create(0, "Pending λ", "Exact pending text.")
            approved = source_store.create(0, "Approved", "Provisioned exactly.")
            denied = source_store.create(0, "Denied", "Unavailable exactly.")
            approved = source_store.approve(approved.id)
            denied = source_store.deny(denied.id)
            expected_requests = (pending, approved, denied)
            (source / "events.jsonl").write_text(
                '{"source":"event"}\n', encoding="utf-8"
            )
            (source / "provided-assets.json").write_text(
                '{"source":"asset provenance"}\n', encoding="utf-8"
            )
            source_before = _tree_bytes(source)

            repository, _ = _create_repository(root / "repository")
            source_record = GenerationGitRecorder(
                repository, source, "test-base"
            ).reconcile()[0]
            initial_iso = source / "generation-0000" / "successor" / "codexos.iso"

            wrong_destination = root / "wrong-git-base"
            with self.assertRaisesRegex(
                CrossRunBootstrapError, "inherited generation tag"
            ):
                initialize_cross_run_bootstrap(
                    wrong_destination,
                    initial_iso,
                    source,
                    0,
                    repository,
                    "test-base",
                )
            self.assertFalse(wrong_destination.exists())

            initialized = initialize_cross_run_bootstrap(
                destination,
                initial_iso,
                source,
                0,
                repository,
                source_record.tag,
            )

            self.assertEqual(_tree_bytes(source), source_before)
            self.assertEqual(initialized.source_run, source.name)
            self.assertEqual(initialized.source_generation, 0)
            self.assertEqual(initialized.handoff, handoff)
            self.assertEqual(
                FeatureRequestStore(destination).requests(),
                expected_requests,
            )
            manifest = json.loads(
                (destination / CROSS_RUN_BOOTSTRAP_MANIFEST).read_text(
                    encoding="utf-8"
                )
            )
            self.assertEqual(
                manifest["source"],
                {"generation": 0, "run": source.name},
            )
            self.assertEqual(
                manifest["successor_iso"],
                {
                    "sha256": hashlib.sha256(initial_iso.read_bytes()).hexdigest(),
                    "size": len(initial_iso.read_bytes()),
                },
            )
            self.assertEqual(manifest["feature_requests"]["ids"], [1, 2, 3])
            ledger_bytes = (
                destination / CROSS_RUN_BOOTSTRAP_FEATURE_LEDGER
            ).read_bytes()
            self.assertEqual(
                manifest["feature_requests"]["sha256"],
                hashlib.sha256(ledger_bytes).hexdigest(),
            )
            self.assertEqual(
                manifest["feature_requests"]["size"], len(ledger_bytes)
            )
            self.assertEqual(
                [
                    record["id"]
                    for record in json.loads(ledger_bytes)["requests"]
                ],
                [1, 2, 3],
            )
            self.assertEqual(manifest["git_base"]["ref"], source_record.tag)
            self.assertEqual(manifest["git_base"]["commit"], source_record.commit)
            self.assertNotIn(str(source), json.dumps(manifest))
            self.assertEqual(
                (destination / CROSS_RUN_BOOTSTRAP_HANDOFF).read_text(
                    encoding="utf-8"
                ),
                handoff,
            )
            self.assertFalse((destination / "events.jsonl").exists())
            self.assertFalse((destination / "provided-assets.json").exists())

            runtime = CodexOSRun(
                destination,
                hardware_profile=TEST_HARDWARE_PROFILE,
            )
            with patch.object(runtime, "_configure_provided_assets"), patch.object(
                runtime, "_boot_generation"
            ) as boot:
                runtime.start(initial_iso)
            boot.assert_called_once_with(0, initial_iso.resolve(), None, "initial")
            self.assertEqual(runtime.previous_handoff, handoff)
            prompt = _planning_prompt(runtime, None)
            self.assertIn("Previous generation handoff:\n" + handoff, prompt)
            self.assertIn("This first turn is a planning phase", prompt)
            next_request = runtime._feature_request_store.create(
                0, "Destination request", "Collision-free identity."
            )
            self.assertEqual(next_request.id, 4)

    def test_rejects_non_latest_source_generation(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "experiment-source"
            files = [SnapshotFile("seed/kernel.c", b"source\n")]
            _archive_completed(source, 0, None, "initial", files, handoff="G0")
            _archive_completed(source, 1, 0, "successor", files, handoff="G1")
            FeatureRequestStore(source).create(
                1, "Later request", "Must not contaminate a G0 fork."
            )
            repository, _ = _create_repository(root / "repository")
            source_records = GenerationGitRecorder(
                repository, source, "test-base"
            ).reconcile()
            destination = root / "destination"

            with self.assertRaisesRegex(
                CrossRunBootstrapError, "latest source-run archive"
            ):
                initialize_cross_run_bootstrap(
                    destination,
                    source / "generation-0000" / "successor" / "codexos.iso",
                    source,
                    0,
                    repository,
                    source_records[0].tag,
                )

            self.assertFalse(destination.exists())

    def test_rejects_requests_from_an_unarchived_successor(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "experiment-002"
            files = [SnapshotFile("seed/kernel.c", b"source\n")]
            for generation in range(16):
                _archive_completed(
                    source,
                    generation,
                    generation - 1 if generation else None,
                    "successor" if generation else "initial",
                    files,
                    handoff=f"G{generation}",
                )
            FeatureRequestStore(source).create(
                16,
                "Unarchived successor request",
                "Created while the next generation was active.",
            )
            repository, _ = _create_repository(root / "repository")
            source_record = GenerationGitRecorder(
                repository, source, "test-base"
            ).reconcile()[-1]
            destination = root / "experiment-003"

            with self.assertRaisesRegex(
                CrossRunBootstrapError, "newer than the inherited generation"
            ):
                initialize_cross_run_bootstrap(
                    destination,
                    source / "generation-0015" / "successor" / "codexos.iso",
                    source,
                    15,
                    repository,
                    source_record.tag,
                )

            self.assertFalse(destination.exists())

    def test_empty_feature_request_description_round_trips(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "source"
            _archive_completed(
                source,
                0,
                None,
                "initial",
                [SnapshotFile("seed/kernel.c", b"source\n")],
                handoff="source",
            )
            request = FeatureRequestStore(source).create(0, "Title only", "")
            repository, _ = _create_repository(root / "repository")
            source_record = GenerationGitRecorder(
                repository, source, "test-base"
            ).reconcile()[0]
            destination = root / "destination"

            initialize_cross_run_bootstrap(
                destination,
                source / "generation-0000" / "successor" / "codexos.iso",
                source,
                0,
                repository,
                source_record.tag,
            )

            self.assertEqual(
                load_cross_run_bootstrap(destination).inherited_request_ids,
                (request.id,),
            )
            self.assertEqual(
                FeatureRequestStore(destination).request(request.id).description,
                "",
            )

    def test_validates_immutable_inherited_feature_ledger(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "source"
            files = [SnapshotFile("seed/kernel.c", b"source\n")]
            _archive_completed(source, 0, None, "initial", files, handoff="source")
            inherited = FeatureRequestStore(source).create(
                0, "Inherited", "Exact inherited request."
            )
            repository, _ = _create_repository(root / "repository")
            source_record = GenerationGitRecorder(
                repository, source, "test-base"
            ).reconcile()[0]
            image = source / "generation-0000" / "successor" / "codexos.iso"

            missing = root / "missing"
            initialize_cross_run_bootstrap(
                missing, image, source, 0, repository, source_record.tag
            )
            (missing / "feature-requests" / "request-000001.json").unlink()
            with self.assertRaisesRegex(
                CrossRunBootstrapError, "request #1 is missing"
            ):
                load_cross_run_bootstrap(missing)

            altered = root / "altered"
            initialize_cross_run_bootstrap(
                altered, image, source, 0, repository, source_record.tag
            )
            request_path = (
                altered / "feature-requests" / "request-000001.json"
            )
            record = json.loads(request_path.read_text(encoding="utf-8"))
            record["title"] = "Altered"
            request_path.write_text(
                json.dumps(record, indent=2, sort_keys=True) + "\n",
                encoding="utf-8",
            )
            with self.assertRaisesRegex(
                CrossRunBootstrapError, "request #1 was altered"
            ):
                load_cross_run_bootstrap(altered)

            status_changed_before_start = root / "status-before-start"
            initialize_cross_run_bootstrap(
                status_changed_before_start,
                image,
                source,
                0,
                repository,
                source_record.tag,
            )
            FeatureRequestStore(status_changed_before_start).approve(inherited.id)
            with self.assertRaisesRegex(
                CrossRunBootstrapError, "request #1 was altered"
            ):
                load_cross_run_bootstrap(status_changed_before_start)

            ledger_changed = root / "ledger-changed"
            initialize_cross_run_bootstrap(
                ledger_changed, image, source, 0, repository, source_record.tag
            )
            ledger_path = ledger_changed / CROSS_RUN_BOOTSTRAP_FEATURE_LEDGER
            ledger_path.write_bytes(ledger_path.read_bytes() + b" ")
            with self.assertRaisesRegex(
                CrossRunBootstrapError, "feature ledger"
            ):
                load_cross_run_bootstrap(ledger_changed)

            invalid_status = root / "invalid-status"
            initialize_cross_run_bootstrap(
                invalid_status, image, source, 0, repository, source_record.tag
            )
            invalid_ledger_path = (
                invalid_status / CROSS_RUN_BOOTSTRAP_FEATURE_LEDGER
            )
            invalid_ledger = json.loads(invalid_ledger_path.read_bytes())
            invalid_ledger["requests"][0]["status"] = []
            invalid_ledger_bytes = (
                json.dumps(
                    invalid_ledger,
                    ensure_ascii=False,
                    separators=(",", ":"),
                    sort_keys=True,
                )
                + "\n"
            ).encode("utf-8")
            invalid_ledger_path.write_bytes(invalid_ledger_bytes)
            invalid_manifest_path = (
                invalid_status / CROSS_RUN_BOOTSTRAP_MANIFEST
            )
            invalid_manifest = json.loads(invalid_manifest_path.read_bytes())
            invalid_manifest["feature_requests"]["sha256"] = hashlib.sha256(
                invalid_ledger_bytes
            ).hexdigest()
            invalid_manifest["feature_requests"]["size"] = len(
                invalid_ledger_bytes
            )
            invalid_manifest_path.write_text(
                json.dumps(invalid_manifest, indent=2, sort_keys=True) + "\n",
                encoding="utf-8",
            )
            with self.assertRaisesRegex(
                CrossRunBootstrapError, "ledger record is invalid"
            ):
                load_cross_run_bootstrap(invalid_status)

            evolved = root / "evolved"
            initialize_cross_run_bootstrap(
                evolved, image, source, 0, repository, source_record.tag
            )
            _archive_completed(
                evolved, 0, None, "initial", files, handoff="destination"
            )
            store = FeatureRequestStore(evolved)
            approved = store.approve(inherited.id)
            added = store.create(0, "New request", "Created by this run.")
            loaded = load_cross_run_bootstrap(evolved)
            self.assertIsNotNone(loaded)
            self.assertEqual(
                FeatureRequestStore(evolved).requests(), (approved, added)
            )
            immutable_ledger = json.loads(
                (evolved / CROSS_RUN_BOOTSTRAP_FEATURE_LEDGER).read_text(
                    encoding="utf-8"
                )
            )
            self.assertEqual(
                immutable_ledger["requests"][0]["status"], "pending"
            )

    def test_rejects_iso_mismatch_and_invalid_source_generations_without_output(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            repository, _ = _create_repository(root / "repository")
            valid = root / "valid"
            _archive_completed(
                valid,
                0,
                None,
                "initial",
                [SnapshotFile("seed/kernel.c", b"source\n")],
            )
            valid_iso = valid / "generation-0000" / "successor" / "codexos.iso"
            wrong_iso = root / "wrong.iso"
            wrong_iso.write_bytes(valid_iso.read_bytes() + b"wrong")
            with self.assertRaisesRegex(CrossRunBootstrapError, "byte-identical"):
                initialize_cross_run_bootstrap(
                    root / "mismatch-destination",
                    wrong_iso,
                    valid,
                    0,
                    repository,
                    "test-base",
                )
            self.assertFalse((root / "mismatch-destination").exists())

            aborted = root / "aborted"
            _archive_aborted(aborted, 0, None, "initial")
            malformed = root / "malformed"
            _archive_completed(
                malformed,
                0,
                None,
                "initial",
                [SnapshotFile("seed/kernel.c", b"source\n")],
            )
            (malformed / "generation-0000" / "metadata.json").write_text(
                "not JSON", encoding="utf-8"
            )
            incomplete = root / "incomplete"
            _archive_completed(
                incomplete,
                0,
                None,
                "initial",
                [SnapshotFile("seed/kernel.c", b"source\n")],
            )
            (incomplete / "generation-0000" / "successor" / "codexos.iso").unlink()
            cases = (
                (
                    "aborted",
                    aborted,
                    0,
                    aborted / "generation-0000" / "boot" / "codexos.iso",
                ),
                ("missing", valid, 1, valid_iso),
                ("malformed", malformed, 0, valid_iso),
                ("incomplete", incomplete, 0, valid_iso),
            )
            for label, source, generation, image in cases:
                destination = root / f"destination-{label}"
                with self.subTest(label=label), self.assertRaises(
                    CrossRunBootstrapError
                ):
                    initialize_cross_run_bootstrap(
                        destination,
                        image,
                        source,
                        generation,
                        repository,
                        "test-base",
                    )
                self.assertFalse(destination.exists())

    def test_requires_fresh_destination_and_cleans_failed_staging(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "source"
            _archive_completed(
                source,
                0,
                None,
                "initial",
                [SnapshotFile("seed/kernel.c", b"source\n")],
            )
            FeatureRequestStore(source).create(0, "Request", "Description")
            image = source / "generation-0000" / "successor" / "codexos.iso"
            repository, _ = _create_repository(root / "repository")
            source_record = GenerationGitRecorder(
                repository, source, "test-base"
            ).reconcile()[0]
            occupied = root / "occupied"
            occupied.mkdir()
            (occupied / "keep").write_bytes(b"unchanged")
            with self.assertRaisesRegex(CrossRunBootstrapError, "fresh destination"):
                initialize_cross_run_bootstrap(
                    occupied, image, source, 0, repository, source_record.tag
                )
            self.assertEqual((occupied / "keep").read_bytes(), b"unchanged")

            destination = root / "failed"
            with patch.object(
                FeatureRequestStore,
                "import_requests",
                side_effect=FeatureRequestError("injected persistence failure"),
            ), self.assertRaises(FeatureRequestError):
                initialize_cross_run_bootstrap(
                    destination,
                    image,
                    source,
                    0,
                    repository,
                    source_record.tag,
                )
            self.assertFalse(destination.exists())
            self.assertEqual(
                [
                    path.name
                    for path in root.iterdir()
                    if path.name.startswith(".cross-run")
                ],
                [],
            )

    def test_post_publication_failure_removes_destination(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "source"
            _archive_completed(
                source,
                0,
                None,
                "initial",
                [SnapshotFile("seed/kernel.c", b"source\n")],
            )
            image = source / "generation-0000" / "successor" / "codexos.iso"
            repository, _ = _create_repository(root / "repository")
            source_record = GenerationGitRecorder(
                repository, source, "test-base"
            ).reconcile()[0]
            destination = root / "destination"
            real_fsync_directory = cross_run_bootstrap_module._fsync_directory

            def fail_after_publication(
                path: Path, *, optional: bool = False
            ) -> None:
                if destination.exists():
                    raise OSError("injected parent-directory fsync failure")
                real_fsync_directory(path, optional=optional)

            with patch.object(
                cross_run_bootstrap_module,
                "_fsync_directory",
                side_effect=fail_after_publication,
            ), self.assertRaisesRegex(OSError, "injected parent-directory"):
                initialize_cross_run_bootstrap(
                    destination,
                    image,
                    source,
                    0,
                    repository,
                    source_record.tag,
                )

            self.assertFalse(destination.exists())
            self.assertEqual(
                [
                    path.name
                    for path in root.iterdir()
                    if path.name.startswith(".cross-run")
                ],
                [],
            )

    def test_rejects_tampered_persisted_handoff(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "source"
            _archive_completed(
                source,
                0,
                None,
                "initial",
                [SnapshotFile("seed/kernel.c", b"source\n")],
                handoff="Exact handoff",
            )
            image = source / "generation-0000" / "successor" / "codexos.iso"
            repository, _ = _create_repository(root / "repository")
            source_record = GenerationGitRecorder(
                repository, source, "test-base"
            ).reconcile()[0]
            destination = root / "destination"
            initialize_cross_run_bootstrap(
                destination,
                image,
                source,
                0,
                repository,
                source_record.tag,
            )
            (destination / CROSS_RUN_BOOTSTRAP_HANDOFF).write_text(
                "Changed handoff", encoding="utf-8"
            )

            with self.assertRaisesRegex(
                CrossRunBootstrapError, "handoff identity"
            ):
                CodexOSRun(destination)

    def test_resume_needs_no_inheritance_flags_and_validates_persisted_state(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "source"
            files = [SnapshotFile("seed/kernel.c", b"source\n")]
            _archive_completed(source, 0, None, "initial", files, handoff="source")
            image = source / "generation-0000" / "successor" / "codexos.iso"
            repository, _ = _create_repository(root / "repository")
            source_record = GenerationGitRecorder(
                repository, source, "test-base"
            ).reconcile()[0]
            destination = root / "destination"
            initialize_cross_run_bootstrap(
                destination,
                image,
                source,
                0,
                repository,
                source_record.tag,
            )
            _archive_completed(
                destination, 0, None, "initial", files, handoff="destination"
            )

            output = io.StringIO()
            result = main(
                [
                    "--run-directory",
                    str(destination),
                    "--resume-at-gate",
                    "--git-repository",
                    str(repository),
                    "--git-base-ref",
                    source_record.tag,
                    "--plain",
                ],
                io.StringIO("quit\n"),
                output,
            )
            self.assertEqual(result, 0, output.getvalue())
            self.assertEqual(
                load_cross_run_bootstrap(destination).source_run,
                source.name,
            )

            (repository / "README.md").write_text(
                "different base\n", encoding="utf-8"
            )
            _git(repository, "add", "README.md")
            _git(repository, "commit", "-m", "Different base")
            _git(
                repository,
                "tag",
                "--no-sign",
                "--no-annotate",
                "different-base",
            )
            wrong_git_output = io.StringIO()
            wrong_git = main(
                [
                    "--run-directory",
                    str(destination),
                    "--resume-at-gate",
                    "--git-repository",
                    str(repository),
                    "--git-base-ref",
                    "different-base",
                    "--plain",
                ],
                io.StringIO("quit\n"),
                wrong_git_output,
            )
            self.assertEqual(wrong_git, 1)
            self.assertIn(
                "does not match cross-run bootstrap provenance",
                wrong_git_output.getvalue(),
            )

            missing_git_output = io.StringIO()
            missing_git = main(
                [
                    "--run-directory",
                    str(destination),
                    "--resume-at-gate",
                    "--plain",
                ],
                io.StringIO("quit\n"),
                missing_git_output,
            )
            self.assertEqual(missing_git, 1)
            self.assertIn(
                "requires its recorded Git provenance options",
                missing_git_output.getvalue(),
            )

    def test_generation_zero_git_commit_descends_from_source_generation(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            repository, _ = _create_repository(root / "repository")
            files = [SnapshotFile("seed/kernel.c", b"exact inherited source\n")]
            source = root / "experiment-002"
            _archive_completed(source, 0, None, "initial", files, handoff="source")
            source_record = GenerationGitRecorder(
                repository, source, "test-base"
            ).reconcile()[0]
            image = source / "generation-0000" / "successor" / "codexos.iso"
            destination = root / "experiment-003"
            initialize_cross_run_bootstrap(
                destination,
                image,
                source,
                0,
                repository,
                source_record.tag,
            )
            _archive_completed(
                destination, 0, None, "initial", files, handoff="destination"
            )
            destination_record = GenerationGitRecorder(
                repository, destination, source_record.tag
            ).reconcile()[0]
            ancestry = _git(
                repository,
                "rev-list",
                "--parents",
                "-n",
                "1",
                destination_record.commit,
            ).split()
            self.assertEqual(
                ancestry,
                [destination_record.commit, source_record.commit],
            )
            self.assertEqual(
                destination_record.tag,
                _generation_tag(destination, 0),
            )

    def test_cli_requires_paired_initial_inheritance_and_git_options(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            cases = (
                [
                    "--run-directory", str(root / "one"),
                    "--initial-iso", str(root / "image"),
                    "--inherit-from-run", str(root / "source"),
                ],
                [
                    "--run-directory", str(root / "two"),
                    "--resume-at-gate",
                    "--inherit-from-run", str(root / "source"),
                    "--inherit-from-generation", "0",
                ],
                [
                    "--run-directory", str(root / "three"),
                    "--initial-iso", str(root / "image"),
                    "--inherit-from-run", str(root / "source"),
                    "--inherit-from-generation", "0",
                ],
            )
            for arguments in cases:
                with self.subTest(arguments=arguments), redirect_stderr(io.StringIO()):
                    with self.assertRaises(SystemExit) as error:
                        main(arguments, io.StringIO(), io.StringIO())
                    self.assertEqual(error.exception.code, 2)


def _tree_bytes(root: Path) -> dict[str, bytes]:
    return {
        path.relative_to(root).as_posix(): path.read_bytes()
        for path in sorted(root.rglob("*"))
        if path.is_file()
    }


if __name__ == "__main__":
    unittest.main()
