package experiment

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"codexos/internal/guest"
	"codexos/internal/provenance"
	"codexos/internal/qemu"
)

func TestGenerationArchiveCarriesHarnessIdentityWithoutChangingLegacyMetadata(t *testing.T) {
	run := t.TempDir()
	identity := experimentHarnessIdentity()
	archive, err := WriteCompletedArchive(run, CompletedArchive{
		Generation: 0, Transition: "initial", Hardware: testHardware(t), BootISO: []byte("boot"),
		Handoff: "handoff", SourceSnapshot: testSnapshot(t, "source\n"), KernelELF: []byte("kernel"),
		SuccessorISO: []byte("iso"), HarnessIdentity: &identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	if archive.HarnessIdentity == nil || !archive.HarnessIdentity.Equal(identity) {
		t.Fatalf("archive harness identity = %#v", archive.HarnessIdentity)
	}
	metadata, err := os.ReadFile(filepath.Join(archive.ArchivePath, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(metadata, []byte("harness")) {
		t.Fatalf("legacy archive metadata was changed: %s", metadata)
	}
}

func TestCompletedArchiveGateAndContinue(t *testing.T) {
	run := t.TempDir()
	hardware := testHardware(t)
	snapshot := testSnapshot(t, "generation zero\n")
	archive, err := WriteCompletedArchive(run, CompletedArchive{
		Generation:     0,
		Transition:     "initial",
		Hardware:       hardware,
		BootISO:        []byte("boot-0"),
		Handoff:        "handoff zero λ",
		SourceSnapshot: snapshot,
		KernelELF:      []byte("kernel-0"),
		SuccessorISO:   []byte("successor-0"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if archive.HarnessIdentity != nil {
		t.Fatalf("legacy archive acquired a fabricated harness identity: %#v", archive.HarnessIdentity)
	}
	before := archiveBytes(t, archive.ArchivePath)
	runState, err := NewCodexOSRun(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := runState.ReopenAtGate(); err != nil {
		t.Fatal(err)
	}
	if runState.State() != RuntimeStateAwaitingNextGeneration {
		t.Fatalf("state = %q", runState.State())
	}
	if generation, ok := runState.GenerationNumber(); !ok || generation != 0 {
		t.Fatalf("generation = %d, %t", generation, ok)
	}
	pending, ok := runState.PendingGenerationFinish()
	if !ok || pending.HandoffMessage != "handoff zero λ" || !bytes.Equal(pending.SourceSnapshot, snapshot) {
		t.Fatalf("pending = %#v, %t", pending, ok)
	}
	if !runState.GenerationFinishFrozen() {
		t.Fatal("completed gate did not expose its frozen finish invariant")
	}
	if !runState.RetainGenerationFinish(0) || !runState.GenerationFinishRetained(0) {
		t.Fatal("completed gate could not be retained atomically")
	}
	if runState.RetainGenerationFinish(0) {
		t.Fatal("completed gate accepted a second retention lease")
	}
	if err := runState.ContinueGeneration(); err == nil || !strings.Contains(err.Error(), "retained for an exit interview") {
		t.Fatalf("continue under retained gate error = %v", err)
	}
	runState.ReleaseGenerationFinish(0)
	if runState.GenerationFinishRetained(0) {
		t.Fatal("released gate remains retained")
	}
	if err := runState.ContinueGeneration(); err != nil {
		t.Fatal(err)
	}
	if runState.State() != RuntimeStateRunning {
		t.Fatalf("continued state = %q", runState.State())
	}
	if generation, ok := runState.GenerationNumber(); !ok || generation != 1 {
		t.Fatalf("continued generation = %d, %t", generation, ok)
	}
	if got := archiveBytes(t, archive.ArchivePath); !sameArchiveBytes(got, before) {
		t.Fatal("continue changed immutable archive")
	}
	if _, ok := runState.PendingGenerationFinish(); ok {
		t.Fatal("continue retained selected successor")
	}
	if runState.GenerationFinishFrozen() {
		t.Fatal("running generation still reported a frozen finish")
	}
}

func TestAbortedGateHasNoSuccessor(t *testing.T) {
	run := t.TempDir()
	archive, err := WriteAbortedArchive(run, AbortedArchive{
		Generation: 0,
		Transition: "initial",
		Hardware:   testHardware(t),
		BootISO:    []byte("boot"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if archive.Outcome != "aborted" {
		t.Fatalf("outcome = %q", archive.Outcome)
	}
	runState, err := NewCodexOSRun(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := runState.ReopenAtGate(); err != nil {
		t.Fatal(err)
	}
	if runState.State() != RuntimeStateAwaitingNextGeneration {
		t.Fatalf("state = %q", runState.State())
	}
	if _, ok := runState.PendingGenerationFinish(); ok {
		t.Fatal("aborted gate selected a successor")
	}
	if runState.GenerationFinishFrozen() {
		t.Fatal("aborted gate reported a frozen completed finish")
	}
	if err := runState.ContinueGeneration(); err == nil || !strings.Contains(err.Error(), "no selected successor") {
		t.Fatalf("continue error = %v", err)
	}
}

func TestAbortRunningProcessFreeGenerationPublishesArchive(t *testing.T) {
	run := t.TempDir()
	hardware := testHardware(t)
	snapshot := testSnapshot(t, "source\n")
	if _, err := WriteCompletedArchive(run, CompletedArchive{
		Generation: 0, Transition: "initial", Hardware: hardware, BootISO: []byte("boot"),
		Handoff: "handoff", SourceSnapshot: snapshot, KernelELF: []byte("kernel"), SuccessorISO: []byte("iso"),
	}); err != nil {
		t.Fatal(err)
	}
	runState, err := NewCodexOSRun(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := runState.ReopenAtGate(); err != nil {
		t.Fatal(err)
	}
	if err := runState.ContinueGeneration(); err != nil {
		t.Fatal(err)
	}
	if err := runState.AbortGeneration(); err != nil {
		t.Fatal(err)
	}
	if runState.State() != RuntimeStateAwaitingNextGeneration {
		t.Fatalf("state = %q", runState.State())
	}
	aborted, err := InspectGeneration(run, 1)
	if err != nil {
		t.Fatal(err)
	}
	if aborted.Outcome != "aborted" || aborted.Handoff != nil {
		t.Fatalf("aborted archive = %#v", aborted)
	}
}

func TestRollbackSelectionRequiresEarlierCompletedArchive(t *testing.T) {
	run := t.TempDir()
	hardware := testHardware(t)
	snapshot := testSnapshot(t, "zero\n")
	if _, err := WriteCompletedArchive(run, CompletedArchive{
		Generation: 0, Transition: "initial", Hardware: hardware, BootISO: []byte("boot-0"),
		Handoff: "handoff-0", SourceSnapshot: snapshot, KernelELF: []byte("kernel-0"), SuccessorISO: []byte("iso-0"),
	}); err != nil {
		t.Fatal(err)
	}
	parent := uint64(0)
	if _, err := WriteCompletedArchive(run, CompletedArchive{
		Generation: 1, ParentGeneration: &parent, Transition: "successor", Hardware: hardware, BootISO: []byte("boot-1"),
		Handoff: "handoff-1", SourceSnapshot: snapshot, KernelELF: []byte("kernel-1"), SuccessorISO: []byte("iso-1"),
	}); err != nil {
		t.Fatal(err)
	}
	runState, err := NewCodexOSRun(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := runState.ReopenAtGate(); err != nil {
		t.Fatal(err)
	}
	if err := runState.ForkFromGeneration(1); err == nil || !strings.Contains(err.Error(), "earlier") {
		t.Fatalf("same-generation fork error = %v", err)
	}
	if !runState.RetainGenerationFinish(1) {
		t.Fatal("latest completed gate could not be retained")
	}
	if err := runState.ForkFromGeneration(0); err == nil || !strings.Contains(err.Error(), "retained for an exit interview") {
		t.Fatalf("fork under retained gate error = %v", err)
	}
	runState.ReleaseGenerationFinish(1)
	if err := runState.ForkFromGeneration(0); err != nil {
		t.Fatal(err)
	}
	if runState.State() != RuntimeStateRunning {
		t.Fatalf("forked state = %q", runState.State())
	}
	if generation, ok := runState.GenerationNumber(); !ok || generation != 2 {
		t.Fatalf("forked generation = %d, %t", generation, ok)
	}
	if transition, ok := runState.CurrentTransition(); !ok || transition != "rollback" {
		t.Fatalf("transition = %q, %t", transition, ok)
	}
	if handoff, ok := runState.PreviousHandoff(); !ok || handoff != "handoff-0" {
		t.Fatalf("handoff = %q, %t", handoff, ok)
	}
}

func TestHistoryAndPartialStateFailClosed(t *testing.T) {
	run := t.TempDir()
	hardware := testHardware(t)
	snapshot := testSnapshot(t, "source\n")
	parent := uint64(0)
	if _, err := WriteCompletedArchive(run, CompletedArchive{
		Generation: 0, Transition: "initial", Hardware: hardware, BootISO: []byte("boot"),
		Handoff: "handoff", SourceSnapshot: snapshot, KernelELF: []byte("kernel"), SuccessorISO: []byte("iso"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteCompletedArchive(run, CompletedArchive{
		Generation: 2, ParentGeneration: &parent, Transition: "rollback", Hardware: hardware, BootISO: []byte("boot"),
		Handoff: "handoff", SourceSnapshot: snapshot, KernelELF: []byte("kernel"), SuccessorISO: []byte("iso"),
	}); err != nil {
		t.Fatal(err)
	}
	runState, err := NewCodexOSRun(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := runState.ReopenAtGate(); err == nil || !strings.Contains(err.Error(), "not contiguous") {
		t.Fatalf("history error = %v", err)
	}
	partial := filepath.Join(run, ".generation-0003-active")
	if err := os.Mkdir(partial, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadArchivedGenerations(run); err != nil {
		t.Fatal(err)
	}
	if err := runState.ReopenAtGate(); err == nil || !strings.Contains(err.Error(), "partial generation state") {
		t.Fatalf("partial-state error = %v", err)
	}
}

func TestSymlinkAndCollisionAreRejectedWithoutMutation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink test requires Unix semantics")
	}
	run := t.TempDir()
	hardware := testHardware(t)
	snapshot := testSnapshot(t, "source\n")
	if _, err := WriteCompletedArchive(run, CompletedArchive{
		Generation: 0, Transition: "initial", Hardware: hardware, BootISO: []byte("boot"),
		Handoff: "handoff", SourceSnapshot: snapshot, KernelELF: []byte("kernel"), SuccessorISO: []byte("iso"),
	}); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(run, "generation-0000")
	before := archiveBytes(t, archive)
	if _, err := WriteAbortedArchive(run, AbortedArchive{
		Generation: 0, Transition: "initial", Hardware: hardware,
	}); err == nil {
		t.Fatal("collision publication succeeded")
	}
	if got := archiveBytes(t, archive); !sameArchiveBytes(got, before) {
		t.Fatal("collision changed existing archive")
	}
	sourceFile := filepath.Join(archive, "source", "seed", "kernel.c")
	if err := os.Remove(sourceFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(run, "outside"), sourceFile); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadArchivedGenerations(run); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("nested source symlink error = %v", err)
	}
	if err := os.Remove(sourceFile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourceFile, []byte("source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(archive, filepath.Join(run, "generation-0001")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadArchivedGenerations(run); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("symlink archive error = %v", err)
	}
}

func TestGoArchiveIsReadableByPython(t *testing.T) {
	repository := experimentRepositoryRoot(t)
	run := filepath.Join(t.TempDir(), "run")
	snapshot := testSnapshot(t, "Go source\n")
	if _, err := WriteCompletedArchive(run, CompletedArchive{
		Generation: 0, Transition: "initial", Hardware: testHardware(t), BootISO: []byte("boot"),
		Handoff: "Go handoff λ.\n", SourceSnapshot: snapshot, KernelELF: []byte("kernel"), SuccessorISO: []byte("successor"),
	}); err != nil {
		t.Fatal(err)
	}
	script := "import json, pathlib, sys\n" +
		"archive = pathlib.Path(sys.argv[1]) / 'generation-0000'\n" +
		"assert json.loads((archive / 'metadata.json').read_text()) == {'generation': 0, 'outcome': 'completed', 'parent_generation': None, 'transition': 'initial'}\n" +
		"assert (archive / 'handoff.txt').read_text() == 'Go handoff λ.\\n'\n" +
		"assert (archive / 'source' / 'seed' / 'kernel.c').read_bytes() == b'Go source\\n'\n" +
		"assert (archive / 'successor' / 'codexos.iso').read_bytes() == b'successor'\n"
	command := exec.Command("python3", "-c", script, run)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python archive verification failed: %v\n%s", err, output)
	}
	_ = repository
}

func TestPythonArchiveIsReadableByGo(t *testing.T) {
	repository := experimentRepositoryRoot(t)
	run := filepath.Join(t.TempDir(), "run")
	script := "import importlib.util, json, pathlib, sys, types\n" +
		"root = pathlib.Path(sys.argv[1]); run = pathlib.Path(sys.argv[2])\n" +
		"package = types.ModuleType('harness'); package.__path__ = [str(root / 'harness')]; sys.modules['harness'] = package\n" +
		"modules = {}\n" +
		"for name in ('source_snapshot', 'hardware'):\n" +
		"  spec = importlib.util.spec_from_file_location('harness.' + name, root / 'harness' / (name + '.py')); module = importlib.util.module_from_spec(spec); sys.modules[spec.name] = module; spec.loader.exec_module(module); modules[name] = module\n" +
		"archive = run / 'generation-0000'; (archive / 'boot').mkdir(parents=True); (archive / 'source').mkdir(); (archive / 'successor').mkdir()\n" +
		"(archive / 'metadata.json').write_text(json.dumps({'generation': 0, 'outcome': 'completed', 'parent_generation': None, 'transition': 'initial'}, indent=2, sort_keys=True) + chr(10))\n" +
		"manifest = modules['hardware'].TEST_HARDWARE_PROFILE.manifest('QEMU emulator version test'); (archive / 'hardware.json').write_text(json.dumps(manifest.as_json_object(), indent=2, sort_keys=True) + chr(10))\n" +
		"snapshot = modules['source_snapshot'].encode_source_snapshot((modules['source_snapshot'].SnapshotFile('seed/kernel.c', b'Python source\\n'),)); (archive / 'source.snapshot').write_bytes(snapshot)\n" +
		"(archive / 'source' / 'seed').mkdir(); (archive / 'source' / 'seed' / 'kernel.c').write_bytes(b'Python source\\n'); (archive / 'handoff.txt').write_text('Python handoff λ.\\n')\n" +
		"(archive / 'boot' / 'codexos.iso').write_bytes(b'boot'); (archive / 'successor' / 'kernel.elf').write_bytes(b'kernel'); (archive / 'successor' / 'codexos.iso').write_bytes(b'successor'); (archive / 'qemu.stdout').write_bytes(b''); (archive / 'qemu.stderr').write_bytes(b'')\n"
	command := exec.Command("python3", "-c", script, repository, run)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python fixture failed: %v\n%s", err, output)
	}
	archives, err := LoadArchivedGenerations(run)
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 1 || archives[0].Handoff == nil || *archives[0].Handoff != "Python handoff λ.\n" {
		t.Fatalf("archives = %#v", archives)
	}
}

func testHardware(t *testing.T) qemu.HardwareManifest {
	t.Helper()
	hardware, err := qemu.TestHardwareProfile.Manifest("QEMU emulator version test")
	if err != nil {
		t.Fatal(err)
	}
	return hardware
}

func experimentHarnessIdentity() provenance.HarnessIdentity {
	return provenance.HarnessIdentity{
		SchemaVersion:    provenance.HarnessIdentitySchemaVersion,
		RepositoryCommit: strings.Repeat("a", 40),
		Executable:       provenance.FileIdentity{SHA256: strings.Repeat("b", 64), Size: 42},
		Build: provenance.HarnessBuildIdentity{
			GoVersion: "go-test", ModulePath: "codexos", ModuleVersion: "v-test",
			SettingsSHA256: strings.Repeat("c", 64),
		},
	}
}

func testSnapshot(t *testing.T, source string) []byte {
	t.Helper()
	snapshot, err := guest.EncodeSourceSnapshot([]guest.SnapshotFile{{Path: "seed/kernel.c", Content: []byte(source)}})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func archiveBytes(t *testing.T, root string) map[string][]byte {
	t.Helper()
	result := make(map[string][]byte)
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			value, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			result[filepath.ToSlash(relative)] = value
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

func sameArchiveBytes(left, right map[string][]byte) bool {
	if len(left) != len(right) {
		return false
	}
	for path, value := range left {
		if !bytes.Equal(value, right[path]) {
			return false
		}
	}
	return true
}

func experimentRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate experiment test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(current), "../.."))
}
