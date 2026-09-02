package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCrossRunBootstrapPythonSourceAndGoDestinationConformance(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "experiment-source")
	repositoryRoot := repositoryRootForCrossRunTest(t)
	if err := createPythonCrossRunFixture(source, repositoryRoot); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	createCrossRunGitRepository(t, repository, filepath.Base(source)+"/generation-0000")
	initialISO := filepath.Join(source, "generation-0000", "successor", "codexos.iso")
	destination := filepath.Join(root, "experiment-destination")

	if _, err := InitializeCrossRunBootstrap(
		destination,
		initialISO,
		source,
		0,
		repository,
		filepath.Base(source)+"/generation-0000",
	); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadCrossRunBootstrap(destination)
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || loaded.SourceRun != filepath.Base(source) || loaded.SourceGeneration != 0 || loaded.Handoff != "Python handoff λ.\n" {
		t.Fatalf("loaded bootstrap = %#v", loaded)
	}
	if !equalCrossRunRequestIDsFromValues(loaded.InheritedRequestIDs, []uint64{1, 2}) {
		t.Fatalf("inherited request IDs = %v", loaded.InheritedRequestIDs)
	}
	store, err := NewFeatureRequestStore(destination)
	if err != nil {
		t.Fatal(err)
	}
	requests, err := store.Requests()
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[0].Status != FeaturePending || requests[1].Status != FeatureApproved {
		t.Fatalf("inherited requests = %#v", requests)
	}

	// The Python reader accepts Go's exact manifest, handoff, and ledger bytes.
	verify := `
import importlib.util, pathlib, sys, types
root = pathlib.Path(sys.argv[1])
run = pathlib.Path(sys.argv[2])
package = types.ModuleType("harness")
package.__path__ = [str(root / "harness")]
sys.modules["harness"] = package
for name in ("feature_requests", "cross_run_bootstrap"):
    spec = importlib.util.spec_from_file_location("harness." + name, root / "harness" / (name + ".py"))
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
from harness.cross_run_bootstrap import load_cross_run_bootstrap
from harness.feature_requests import FeatureRequestStore
bootstrap = load_cross_run_bootstrap(run)
assert bootstrap.source_run == "experiment-source"
assert bootstrap.source_generation == 0
assert bootstrap.handoff == "Python handoff λ.\n"
assert bootstrap.inherited_request_ids == (1, 2)
assert [(request.id, request.status) for request in FeatureRequestStore(run).requests()] == [(1, "pending"), (2, "approved")]
`
	if output, err := exec.Command("python3", "-c", verify, repositoryRoot, destination).CombinedOutput(); err != nil {
		t.Fatalf("Python destination verification failed: %v\n%s", err, output)
	}
}

func TestCrossRunBootstrapAllowsChainedInheritedRequestFromHigherGeneration(t *testing.T) {
	root := t.TempDir()
	repositoryRoot := repositoryRootForCrossRunTest(t)
	predecessor := filepath.Join(root, "experiment-002")
	if err := createPythonCrossRunHistory(predecessor, repositoryRoot, 10, 10); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	createCrossRunGitRepository(t, repository, "experiment-002/generation-0010")

	middle := filepath.Join(root, "experiment-003")
	predecessorISO := filepath.Join(predecessor, "generation-0010", "successor", "codexos.iso")
	if _, err := InitializeCrossRunBootstrap(
		middle,
		predecessorISO,
		predecessor,
		10,
		repository,
		"experiment-002/generation-0010",
	); err != nil {
		t.Fatal(err)
	}
	if err := createPythonCrossRunHistory(middle, repositoryRoot, 0, -1); err != nil {
		t.Fatal(err)
	}
	runCrossRunGit(t, repository, "tag", "--annotate", "--no-sign", "--cleanup=verbatim", "-m", "Cross-run base", "experiment-003/generation-0000")

	destination := filepath.Join(root, "experiment-004")
	middleISO := filepath.Join(middle, "generation-0000", "successor", "codexos.iso")
	if _, err := InitializeCrossRunBootstrap(
		destination,
		middleISO,
		middle,
		0,
		repository,
		"experiment-003/generation-0000",
	); err != nil {
		t.Fatal(err)
	}
	requests, err := NewFeatureRequestStore(destination)
	if err != nil {
		t.Fatal(err)
	}
	inherited, err := requests.Requests()
	if err != nil {
		t.Fatal(err)
	}
	if len(inherited) != 2 || inherited[0].Generation != 10 || inherited[1].Generation != 10 {
		t.Fatalf("chained inherited requests = %#v", inherited)
	}

	middleRequests, err := NewFeatureRequestStore(middle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := middleRequests.Create(1, "Unarchived local request", "Must still be rejected."); err != nil {
		t.Fatal(err)
	}
	invalidDestination := filepath.Join(root, "invalid-destination")
	if _, err := InitializeCrossRunBootstrap(
		invalidDestination,
		middleISO,
		middle,
		0,
		repository,
		"experiment-003/generation-0000",
	); err == nil || !strings.Contains(err.Error(), "newer than the inherited generation") {
		t.Fatalf("unarchived local request error = %v", err)
	}
	if _, err := os.Lstat(invalidDestination); !os.IsNotExist(err) {
		t.Fatalf("failed initialization published destination: %v", err)
	}
}

func TestCrossRunBootstrapRejectsIdentityAndLedgerTampering(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := createPythonCrossRunFixture(source, repositoryRootForCrossRunTest(t)); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	createCrossRunGitRepository(t, repository, "source/generation-0000")
	image := filepath.Join(source, "generation-0000", "successor", "codexos.iso")
	base := filepath.Join(root, "base")
	if _, err := InitializeCrossRunBootstrap(base, image, source, 0, repository, "source/generation-0000"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, CrossRunBootstrapHandoff), []byte("tampered\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCrossRunBootstrap(base); err == nil || !strings.Contains(err.Error(), "handoff identity") {
		t.Fatalf("tampered handoff error = %v", err)
	}

	ledger := filepath.Join(root, "ledger")
	if _, err := InitializeCrossRunBootstrap(ledger, image, source, 0, repository, "source/generation-0000"); err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(ledger, CrossRunBootstrapFeatureLedger)
	contents, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledgerPath, append(contents, ' '), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCrossRunBootstrap(ledger); err == nil || !strings.Contains(err.Error(), "feature ledger") {
		t.Fatalf("tampered ledger error = %v", err)
	}

	wrongImage := filepath.Join(root, "wrong.iso")
	if err := os.WriteFile(wrongImage, []byte("different"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeCrossRunBootstrap(filepath.Join(root, "mismatch"), wrongImage, source, 0, repository, "source/generation-0000"); err == nil || !strings.Contains(err.Error(), "byte-identical") {
		t.Fatalf("ISO mismatch error = %v", err)
	}
}

func TestCrossRunBootstrapRejectsCollisionsAndInvalidSourceSelectionWithoutPublishing(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := createPythonCrossRunFixture(source, repositoryRootForCrossRunTest(t)); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	createCrossRunGitRepository(t, repository, "source/generation-0000")
	image := filepath.Join(source, "generation-0000", "successor", "codexos.iso")

	destination := filepath.Join(root, "existing")
	if err := os.Mkdir(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(destination, "operator-state")
	if err := os.WriteFile(marker, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeCrossRunBootstrap(destination, image, source, 0, repository, "source/generation-0000"); err == nil || !strings.Contains(err.Error(), "fresh destination") {
		t.Fatalf("destination collision error = %v", err)
	}
	if contents, err := os.ReadFile(marker); err != nil || string(contents) != "preserve" {
		t.Fatalf("destination marker = %q, %v", contents, err)
	}

	nonexistent := filepath.Join(root, "nonexistent-generation")
	if _, err := InitializeCrossRunBootstrap(nonexistent, image, source, 1, repository, "source/generation-0001"); err == nil || !strings.Contains(err.Error(), "latest source-run archive") {
		t.Fatalf("source generation error = %v", err)
	}
	if _, err := os.Lstat(nonexistent); !os.IsNotExist(err) {
		t.Fatalf("failed initialization published destination: %v", err)
	}
}

func TestCrossRunBootstrapRequiresAnnotatedGenerationTagAndCompleteTriple(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := createPythonCrossRunFixture(source, repositoryRootForCrossRunTest(t)); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	createCrossRunGitRepository(t, repository, "source/generation-0000")
	runCrossRunGit(t, repository, "tag", "--delete", "source/generation-0000")
	runCrossRunGit(t, repository, "update-ref", "refs/tags/source/generation-0000", "HEAD")
	image := filepath.Join(source, "generation-0000", "successor", "codexos.iso")
	destination := filepath.Join(root, "destination")
	if _, err := InitializeCrossRunBootstrap(destination, image, source, 0, repository, "source/generation-0000"); err == nil || !strings.Contains(err.Error(), "not annotated") {
		t.Fatalf("lightweight-tag error = %v", err)
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatalf("Git validation failure published destination: %v", err)
	}

	partial := filepath.Join(root, "partial")
	if err := os.Mkdir(partial, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partial, CrossRunBootstrapManifest), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCrossRunBootstrap(partial); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("partial provenance error = %v", err)
	}
}

func createPythonCrossRunFixture(source, repositoryRoot string) error {
	return createPythonCrossRunHistory(source, repositoryRoot, 0, 0)
}

func createPythonCrossRunHistory(source, repositoryRoot string, latestGeneration, requestGeneration int) error {
	script := `
import importlib.util, json, pathlib, sys, types
root = pathlib.Path(sys.argv[1])
source = pathlib.Path(sys.argv[2])
latest = int(sys.argv[3])
request_generation = int(sys.argv[4])
package = types.ModuleType("harness")
package.__path__ = [str(root / "harness")]
sys.modules["harness"] = package
for name in ("source_snapshot", "hardware", "feature_requests"):
    spec = importlib.util.spec_from_file_location("harness." + name, root / "harness" / (name + ".py"))
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
snapshot_module = sys.modules["harness.source_snapshot"]
hardware_module = sys.modules["harness.hardware"]
feature_module = sys.modules["harness.feature_requests"]
snapshot = snapshot_module.encode_source_snapshot((snapshot_module.SnapshotFile("seed/kernel.c", b"source\n"),))
for generation in range(latest + 1):
    archive = source / f"generation-{generation:04d}"
    (archive / "boot").mkdir(parents=True)
    (archive / "source" / "seed").mkdir(parents=True)
    (archive / "successor").mkdir()
    metadata = {
        "generation": generation,
        "outcome": "completed",
        "parent_generation": generation - 1 if generation else None,
        "transition": "successor" if generation else "initial",
    }
    (archive / "metadata.json").write_text(json.dumps(metadata, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    hardware = hardware_module.TEST_HARDWARE_PROFILE.manifest("QEMU emulator version test")
    (archive / "hardware.json").write_text(json.dumps(hardware.as_json_object(), indent=2, sort_keys=True) + "\n", encoding="utf-8")
    (archive / "source.snapshot").write_bytes(snapshot)
    (archive / "source" / "seed" / "kernel.c").write_bytes(b"source\n")
    (archive / "handoff.txt").write_text("Python handoff λ.\n", encoding="utf-8")
    (archive / "boot" / "codexos.iso").write_bytes(b"boot")
    (archive / "successor" / "kernel.elf").write_bytes(b"kernel")
    (archive / "successor" / "codexos.iso").write_bytes(b"successor")
    (archive / "qemu.stdout").write_bytes(b"")
    (archive / "qemu.stderr").write_bytes(b"")
if request_generation >= 0:
    store = feature_module.FeatureRequestStore(source)
    store.create(
        request_generation,
        "Pending λ",
        "Pending source request",
    )
    approved = store.create(
        request_generation,
        "Approved",
        "Approved source request",
    )
    store.approve(approved.id)
`
	command := exec.Command(
		"python3",
		"-c",
		script,
		repositoryRoot,
		source,
		fmt.Sprintf("%d", latestGeneration),
		fmt.Sprintf("%d", requestGeneration),
	)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Python cross-run history fixture failed: %w\n%s", err, output)
	}
	return nil
}

func createCrossRunGitRepository(t *testing.T, repository, tag string) {
	t.Helper()
	if err := os.MkdirAll(repository, 0o777); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{
		{"init", "-q"},
		{"config", "user.name", "Cross-run test"},
		{"config", "user.email", "cross-run@example.invalid"},
	} {
		runCrossRunGit(t, repository, arguments...)
	}
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("trusted base\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	runCrossRunGit(t, repository, "add", "README.md")
	runCrossRunGit(t, repository, "commit", "-q", "-m", "Trusted experiment base")
	runCrossRunGit(t, repository, "tag", "--annotate", "--no-sign", "--cleanup=verbatim", "-m", "Cross-run base", tag)
}

func runCrossRunGit(t *testing.T, repository string, arguments ...string) {
	t.Helper()
	commandArguments := append([]string{"-C", repository}, arguments...)
	command := exec.Command("git", commandArguments...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", arguments, err, output)
	}
}

func equalCrossRunRequestIDsFromValues(left, right []uint64) bool {
	return bytes.Equal(mustJSONBytes(left), mustJSONBytes(right))
}

func mustJSONBytes(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

func repositoryRootForCrossRunTest(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate cross-run conformance test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFile), "../.."))
}
