package provenance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHarnessIdentityCapturesCleanAndReproducibleDirtyTrees(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.Mkdir(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	runHarnessGit(t, repository, "init", "--quiet")
	runHarnessGit(t, repository, "config", "user.name", "Harness Test")
	runHarnessGit(t, repository, "config", "user.email", "harness@example.invalid")
	tracked := filepath.Join(repository, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("clean\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runHarnessGit(t, repository, "add", "tracked.txt")
	runHarnessGit(t, repository, "commit", "--quiet", "-m", "initial")
	binary := filepath.Join(t.TempDir(), "codexos")
	if err := os.WriteFile(binary, []byte("binary one"), 0o755); err != nil {
		t.Fatal(err)
	}
	build := testHarnessBuildIdentity()
	clean, err := CaptureHarnessIdentity(repository, binary, build)
	if err != nil {
		t.Fatal(err)
	}
	if clean.RepositoryDirty || clean.DirtyTreeSHA256 != nil {
		t.Fatalf("clean identity = %#v", clean)
	}
	if err := os.WriteFile(tracked, []byte("dirty contents\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "untracked.txt"), []byte("untracked\x00bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := CaptureHarnessIdentity(repository, binary, build)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CaptureHarnessIdentity(repository, binary, build)
	if err != nil {
		t.Fatal(err)
	}
	if !first.RepositoryDirty || first.DirtyTreeSHA256 == nil || !first.Equal(second) {
		t.Fatalf("dirty identities are not stable: %#v %#v", first, second)
	}
	if err := os.WriteFile(tracked, []byte("different dirty contents\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := CaptureHarnessIdentity(repository, binary, build)
	if err != nil {
		t.Fatal(err)
	}
	if changed.DirtyTreeSHA256 == nil || *changed.DirtyTreeSHA256 == *first.DirtyTreeSHA256 {
		t.Fatal("dirty-tree identity did not change with tracked bytes")
	}
}

func TestHarnessIdentityDistinguishesBinaryReplacement(t *testing.T) {
	repository, binary := cleanHarnessFixture(t)
	first, err := CaptureHarnessIdentity(repository, binary, testHarnessBuildIdentity())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("replacement binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	second, err := CaptureHarnessIdentity(repository, binary, testHarnessBuildIdentity())
	if err != nil {
		t.Fatal(err)
	}
	if first.Executable.SHA256 == second.Executable.SHA256 || first.Equal(second) {
		t.Fatal("binary replacement retained the same harness identity")
	}
}

func TestHarnessIdentityStoreRecordsChangedIdentityAtQuiescentGate(t *testing.T) {
	repository, binary := cleanHarnessFixture(t)
	initial, err := CaptureHarnessIdentity(repository, binary, testHarnessBuildIdentity())
	if err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(t.TempDir(), "run")
	store := NewHarnessIdentityStore(run)
	if err := store.RecordRunCreation(initial); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyCurrent(initial); err != nil {
		t.Fatalf("same identity verification: %v", err)
	}
	same, err := store.PrepareGateTransition(initial, 4)
	if err != nil || same.RequiresRecord {
		t.Fatalf("same identity gate = %#v, %v", same, err)
	}
	if err := os.WriteFile(binary, []byte("new binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	replacement, err := CaptureHarnessIdentity(repository, binary, testHarnessBuildIdentity())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyCurrent(replacement); err == nil {
		t.Fatal("replacement identity verified before its gate transition")
	}
	transition, err := store.PrepareGateTransition(replacement, 4)
	if err != nil || !transition.RequiresRecord || transition.Previous == nil || !transition.Previous.Equal(initial) {
		t.Fatalf("gate transition = %#v, %v", transition, err)
	}
	if err := store.RecordGateTransition(transition); err != nil {
		t.Fatal(err)
	}
	accepted, err := store.PrepareGateTransition(replacement, 4)
	if err != nil || accepted.RequiresRecord {
		t.Fatalf("recorded replacement was not accepted: %#v, %v", accepted, err)
	}
	legacy := NewHarnessIdentityStore(filepath.Join(t.TempDir(), "legacy"))
	legacyTransition, err := legacy.PrepareGateTransition(initial, 9)
	if err != nil || !legacyTransition.RequiresRecord || legacyTransition.Previous != nil {
		t.Fatalf("legacy gate transition = %#v, %v", legacyTransition, err)
	}
	if err := os.MkdirAll(legacy.run, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := legacy.RecordGateTransition(legacyTransition); err != nil {
		t.Fatal(err)
	}
}

func TestHarnessIdentityStoreRejectsInconsistentTransitionAncestry(t *testing.T) {
	run := filepath.Join(t.TempDir(), "run")
	store := NewHarnessIdentityStore(run)
	initial := testHarnessIdentity()
	if err := store.RecordRunCreation(initial); err != nil {
		t.Fatal(err)
	}
	replacement := initial
	replacement.Executable.SHA256 = strings.Repeat("d", 64)
	transition, err := store.PrepareGateTransition(replacement, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordGateTransition(transition); err != nil {
		t.Fatal(err)
	}
	inconsistent := initial
	inconsistent.RepositoryCommit = strings.Repeat("e", 40)
	encoded, err := EncodeHarnessIdentity(inconsistent)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, RunHarnessIdentityFilename), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PrepareGateTransition(replacement, 3); err == nil || !strings.Contains(err.Error(), "does not descend") {
		t.Fatalf("inconsistent ancestry error = %v", err)
	}
}

func TestHarnessIdentityStoreRejectsNonQuiescentGateTransitionSchema(t *testing.T) {
	run := filepath.Join(t.TempDir(), "run")
	store := NewHarnessIdentityStore(run)
	initial := testHarnessIdentity()
	if err := store.RecordRunCreation(initial); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(run, harnessTransitionDirectory)
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeHarnessJSON(map[string]any{
		"acknowledged": true, "after_generation": uint64(3),
		"current": initial.AsJSON(), "previous": initial.AsJSON(),
		"schema_version": HarnessIdentitySchemaVersion, "transition": "gate_reopen",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "transition-000001.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PrepareGateTransition(initial, 3); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("non-quiescent transition schema error = %v", err)
	}
}

func TestGenerationHarnessIdentityCannotBeReplaced(t *testing.T) {
	repository, binary := cleanHarnessFixture(t)
	identity, err := CaptureHarnessIdentity(repository, binary, testHarnessBuildIdentity())
	if err != nil {
		t.Fatal(err)
	}
	store := NewHarnessIdentityStore(filepath.Join(t.TempDir(), "run"))
	if err := store.RecordRunCreation(identity); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordGenerationStart(3, identity); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordGenerationStart(3, identity); err != nil {
		t.Fatalf("same generation identity was not idempotent: %v", err)
	}
	if err := os.WriteFile(binary, []byte("different"), 0o755); err != nil {
		t.Fatal(err)
	}
	different, err := CaptureHarnessIdentity(repository, binary, testHarnessBuildIdentity())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordGenerationStart(3, different); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("generation identity replacement error = %v", err)
	}
}

func cleanHarnessFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	if err := os.Mkdir(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	runHarnessGit(t, repository, "init", "--quiet")
	runHarnessGit(t, repository, "config", "user.name", "Harness Test")
	runHarnessGit(t, repository, "config", "user.email", "harness@example.invalid")
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("tracked"), 0o644); err != nil {
		t.Fatal(err)
	}
	runHarnessGit(t, repository, "add", "tracked")
	runHarnessGit(t, repository, "commit", "--quiet", "-m", "initial")
	binary := filepath.Join(root, "codexos")
	if err := os.WriteFile(binary, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return repository, binary
}

func testHarnessBuildIdentity() HarnessBuildIdentity {
	return HarnessBuildIdentity{
		GoVersion: "go-test", ModulePath: "codexos", ModuleVersion: "v-test",
		SettingsSHA256: strings.Repeat("1", 64),
	}
}

func testHarnessIdentity() HarnessIdentity {
	return HarnessIdentity{
		SchemaVersion:    HarnessIdentitySchemaVersion,
		RepositoryCommit: strings.Repeat("a", 40),
		Executable:       FileIdentity{SHA256: strings.Repeat("b", 64), Size: 123},
		Build:            testHarnessBuildIdentity(),
	}
}

func runHarnessGit(t *testing.T, repository string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repository}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
}
