package provenance

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"codexos/internal/guest"
	"codexos/internal/qemu"
)

func TestGenerationGitRecorderCreatesImmutableTagsAndLineages(t *testing.T) {
	t.Setenv("GIT_AUTHOR_DATE", "2026-01-02T03:04:05+0000")
	t.Setenv("GIT_COMMITTER_DATE", "2026-01-02T03:04:05+0000")
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	base := createGenerationGitRepository(t, repository)
	run := filepath.Join(root, "experiment-002")
	archiveGenerationGitCompleted(t, run, 0, nil, "initial", []guest.SnapshotFile{
		{Path: "seed/kernel.c", Content: []byte("generation zero\n")},
		{Path: "seed/deleted.c", Content: []byte("removed later\n")},
	}, "Handoff λ from generation zero.\nNext line.")
	archiveGenerationGitCompleted(t, run, 1, generationGitUint64Pointer(0), "successor", []guest.SnapshotFile{
		{Path: "seed/kernel.c", Content: []byte("generation zero\none\n")},
	}, "Handoff from generation 1.")
	archiveGenerationGitCompleted(t, run, 2, generationGitUint64Pointer(1), "successor", []guest.SnapshotFile{
		{Path: "seed/kernel.c", Content: []byte("generation zero\none\n")},
	}, "Handoff from generation 2.")
	archiveGenerationGitAborted(t, run, 3, generationGitUint64Pointer(2), "successor")
	archiveGenerationGitCompleted(t, run, 4, generationGitUint64Pointer(0), "rollback", []guest.SnapshotFile{
		{Path: "seed/kernel.c", Content: []byte("generation zero\nrollback\n")},
	}, "Handoff from generation 4.")

	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("uncommitted developer change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	statusBefore := generationGitCommand(t, repository, "status", "--porcelain")
	headBefore := generationGitCommand(t, repository, "rev-parse", "HEAD")
	branchBefore := generationGitCommand(t, repository, "symbolic-ref", "--short", "HEAD")

	recorder, err := NewGenerationGitRecorder(repository, run, "test-base")
	if err != nil {
		t.Fatal(err)
	}
	records, err := recorder.Reconcile()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(records), 4; got != want {
		t.Fatalf("record count = %d, want %d", got, want)
	}
	if got, want := recorder.BaseCommit(), base; got != want {
		t.Fatalf("base commit = %s, want %s", got, want)
	}
	for _, record := range records {
		if record.AlreadyRecorded {
			t.Fatalf("first record for generation %d was already recorded", record.Generation)
		}
		if got, want := record.Tag, generationTag("experiment-002", record.Generation); got != want {
			t.Fatalf("tag = %q, want %q", got, want)
		}
	}
	commits := make(map[uint64]string)
	for _, generation := range []uint64{0, 1, 2, 4} {
		commits[generation] = strings.TrimSpace(generationGitCommand(t, repository, "rev-parse", generationTag("experiment-002", generation)+"^{commit}"))
	}
	if got := generationGitCommand(t, repository, "rev-list", "--parents", "-n", "1", commits[0]); strings.TrimSpace(got) != commits[0]+" "+base {
		t.Fatalf("generation zero ancestry = %q", got)
	}
	if got := generationGitCommand(t, repository, "rev-list", "--parents", "-n", "1", commits[1]); strings.TrimSpace(got) != commits[1]+" "+commits[0] {
		t.Fatalf("generation one ancestry = %q", got)
	}
	if got := generationGitCommand(t, repository, "rev-list", "--parents", "-n", "1", commits[2]); strings.TrimSpace(got) != commits[2]+" "+commits[1] {
		t.Fatalf("generation two ancestry = %q", got)
	}
	if got := generationGitCommand(t, repository, "rev-list", "--parents", "-n", "1", commits[4]); strings.TrimSpace(got) != commits[4]+" "+commits[0] {
		t.Fatalf("generation four ancestry = %q", got)
	}
	if got := strings.TrimSpace(generationGitCommand(t, repository, "rev-parse", "refs/heads/experiment-002/lineage-0000")); got != commits[2] {
		t.Fatalf("lineage zero = %s, want %s", got, commits[2])
	}
	if got := strings.TrimSpace(generationGitCommand(t, repository, "rev-parse", "refs/heads/experiment-002/lineage-0001")); got != commits[4] {
		t.Fatalf("lineage one = %s, want %s", got, commits[4])
	}
	if got := generationGitCommand(t, repository, "cat-file", "-t", "refs/tags/experiment-002/generation-0000"); got != "tag\n" {
		t.Fatalf("generation tag type = %q", got)
	}
	if got := strings.TrimSpace(generationGitCommand(t, repository, "show", "-s", "--format=%an <%ae>", commits[0])); got != "Existing Developer <developer@example.invalid>" {
		t.Fatalf("generation author = %q", got)
	}
	commitObject := generationGitCommand(t, repository, "cat-file", "commit", commits[0])
	if !strings.Contains(commitObject, "Recorded-By: CodexOS harness\n") {
		t.Fatalf("generation commit message = %q", commitObject)
	}
	tagObject := generationGitCommand(t, repository, "cat-file", "tag", "refs/tags/experiment-002/generation-0000")
	if !strings.HasSuffix(tagObject, "Handoff λ from generation zero.\nNext line.") {
		t.Fatalf("generation tag annotation = %q", tagObject)
	}

	second, err := recorder.Reconcile()
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range second {
		if !record.AlreadyRecorded || record.Commit != commits[record.Generation] {
			t.Fatalf("second reconciliation record = %#v", record)
		}
	}
	if got := generationGitCommand(t, repository, "status", "--porcelain"); got != statusBefore {
		t.Fatalf("worktree status changed: before %q after %q", statusBefore, got)
	}
	if got := generationGitCommand(t, repository, "rev-parse", "HEAD"); got != headBefore {
		t.Fatalf("HEAD changed: before %q after %q", headBefore, got)
	}
	if got := generationGitCommand(t, repository, "symbolic-ref", "--short", "HEAD"); got != branchBefore {
		t.Fatalf("current branch changed: before %q after %q", branchBefore, got)
	}
	if got := strings.Count(generationGitCommand(t, repository, "worktree", "list", "--porcelain"), "worktree "); got != 1 {
		t.Fatalf("temporary worktree count = %d", got)
	}
}

func TestGenerationGitRecorderReconcilesFreshRunWithoutArchives(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repository")
	createGenerationGitRepository(t, repository)
	run := filepath.Join(t.TempDir(), "fresh-run")
	if err := os.Mkdir(run, 0o755); err != nil {
		t.Fatal(err)
	}
	recorder, err := NewGenerationGitRecorder(repository, run, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	records, err := recorder.Reconcile()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("fresh run records = %#v, want none", records)
	}
}

func TestGenerationGitRecorderRejectsImmutableConflicts(t *testing.T) {
	t.Setenv("GIT_AUTHOR_DATE", "2026-01-02T03:04:05+0000")
	t.Setenv("GIT_COMMITTER_DATE", "2026-01-02T03:04:05+0000")
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	createGenerationGitRepository(t, repository)
	run := filepath.Join(root, "experiment-002")
	archiveGenerationGitCompleted(t, run, 0, nil, "initial", []guest.SnapshotFile{{Path: "seed/kernel.c", Content: []byte("G0\n")}}, "handoff")
	recorder, err := NewGenerationGitRecorder(repository, run, "test-base")
	if err != nil {
		t.Fatal(err)
	}
	firstRecords, err := recorder.Reconcile()
	if err != nil {
		t.Fatal(err)
	}
	commit := firstRecords[0].Commit
	tag := generationTag("experiment-002", 0)
	if err := generationGitRun(repository, "tag", "-d", tag); err != nil {
		t.Fatal(err)
	}
	if err := generationGitRun(repository, "tag", "--annotate", "--no-sign", "--message", "modified annotation", tag, commit); err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.Reconcile(); err == nil || !strings.Contains(err.Error(), "conflicting annotation") {
		t.Fatalf("annotation conflict = %v", err)
	}
	if got := strings.TrimSpace(generationGitCommand(t, repository, "rev-parse", tag+"^{commit}")); got != commit {
		t.Fatalf("conflicting tag target changed to %s", got)
	}
}

func TestGenerationGitRecorderBaseRefIsResolvedOnce(t *testing.T) {
	t.Setenv("GIT_AUTHOR_DATE", "2026-01-02T03:04:05+0000")
	t.Setenv("GIT_COMMITTER_DATE", "2026-01-02T03:04:05+0000")
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	base := createGenerationGitRepository(t, repository)
	if err := generationGitRun(repository, "branch", "base-ref"); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(root, "experiment-002")
	archiveGenerationGitCompleted(t, run, 0, nil, "initial", []guest.SnapshotFile{{Path: "seed/kernel.c", Content: []byte("G0\n")}}, "handoff")
	recorder, err := NewGenerationGitRecorder(repository, run, "base-ref")
	if err != nil {
		t.Fatal(err)
	}
	if err := generationGitRun(repository, "commit", "--allow-empty", "-m", "later"); err != nil {
		t.Fatal(err)
	}
	if err := generationGitRun(repository, "branch", "-f", "base-ref", "HEAD"); err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.Reconcile(); err != nil {
		t.Fatal(err)
	}
	commit := strings.TrimSpace(generationGitCommand(t, repository, "rev-parse", generationTag("experiment-002", 0)+"^{commit}"))
	if got := strings.TrimSpace(generationGitCommand(t, repository, "rev-list", "--parents", "-n", "1", commit)); got != commit+" "+base {
		t.Fatalf("generation parent changed with base ref: %q", got)
	}
}

func TestGenerationGitMessagesMatchPythonReference(t *testing.T) {
	root := provenanceRepositoryRoot(t)
	pythonScript := `
import base64, importlib.util, pathlib, sys, types
root = pathlib.Path(sys.argv[1])
runtime = types.ModuleType("harness.generation_runtime")
runtime.ArchivedGeneration = object
runtime.CodexOSRun = object
sys.modules[runtime.__name__] = runtime
source_spec = importlib.util.spec_from_file_location("harness.source_snapshot", root / "harness" / "source_snapshot.py")
source = importlib.util.module_from_spec(source_spec)
sys.modules[source_spec.name] = source
source_spec.loader.exec_module(source)
git_spec = importlib.util.spec_from_file_location("harness.generation_git", root / "harness" / "generation_git.py")
git = importlib.util.module_from_spec(git_spec)
sys.modules[git_spec.name] = git
git_spec.loader.exec_module(git)
archive = types.SimpleNamespace(generation=4, parent_generation=0, transition="rollback", handoff="Handoff λ\nnext")
snapshot = b"snapshot\x00\xff"
print(base64.b64encode(git._commit_message(archive, snapshot).encode()).decode())
print(base64.b64encode(git._tag_message(archive, "experiment-002").encode()).decode())
`
	command := exec.Command("python3", "-c", pythonScript, root)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("Python generation Git reference failed: %v", err)
	}
	lines := strings.Fields(string(output))
	if len(lines) != 2 {
		t.Fatalf("Python generation Git reference output = %q", output)
	}
	pythonCommit, err := base64.StdEncoding.DecodeString(lines[0])
	if err != nil {
		t.Fatal(err)
	}
	pythonTag, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil {
		t.Fatal(err)
	}
	parent := uint64(0)
	archive := generationArchive{generation: 4, parent: &parent, transition: "rollback", handoff: "Handoff λ\nnext"}
	if got := []byte(generationCommitMessage(archive, []byte("snapshot\x00\xff"))); !bytes.Equal(got, pythonCommit) {
		t.Fatalf("commit message differs:\nGo: %q\nPython: %q", got, pythonCommit)
	}
	if got := []byte(generationTagMessage(archive, "experiment-002")); !bytes.Equal(got, pythonTag) {
		t.Fatalf("tag message differs:\nGo: %q\nPython: %q", got, pythonTag)
	}
}

func TestGenerationGitObjectsMatchPythonReference(t *testing.T) {
	t.Setenv("GIT_AUTHOR_DATE", "2026-01-02T03:04:05+0000")
	t.Setenv("GIT_COMMITTER_DATE", "2026-01-02T03:04:05+0000")
	root := t.TempDir()
	pythonRepository := filepath.Join(root, "python-repository")
	goRepository := filepath.Join(root, "go-repository")
	createGenerationGitRepository(t, pythonRepository)
	createGenerationGitRepository(t, goRepository)
	pythonRun := filepath.Join(root, "python-run", "experiment-conformance")
	goRun := filepath.Join(root, "go-run", "experiment-conformance")
	archiveGenerationGitCompleted(t, pythonRun, 0, nil, "initial", []guest.SnapshotFile{{Path: "seed/kernel.c", Content: []byte("G0\n")}}, "Handoff λ")
	archiveGenerationGitCompleted(t, pythonRun, 1, generationGitUint64Pointer(0), "successor", []guest.SnapshotFile{{Path: "seed/kernel.c", Content: []byte("G1\n")}}, "Handoff 1")
	archiveGenerationGitCompleted(t, pythonRun, 2, generationGitUint64Pointer(0), "rollback", []guest.SnapshotFile{{Path: "seed/kernel.c", Content: []byte("G2\n")}}, "Handoff 2")
	archiveGenerationGitCompleted(t, goRun, 0, nil, "initial", []guest.SnapshotFile{{Path: "seed/kernel.c", Content: []byte("G0\n")}}, "Handoff λ")
	archiveGenerationGitCompleted(t, goRun, 1, generationGitUint64Pointer(0), "successor", []guest.SnapshotFile{{Path: "seed/kernel.c", Content: []byte("G1\n")}}, "Handoff 1")
	archiveGenerationGitCompleted(t, goRun, 2, generationGitUint64Pointer(0), "rollback", []guest.SnapshotFile{{Path: "seed/kernel.c", Content: []byte("G2\n")}}, "Handoff 2")

	pythonScript := `
import importlib.util, json, pathlib, sys, types
root = pathlib.Path(sys.argv[1])
repository = pathlib.Path(sys.argv[2])
run = pathlib.Path(sys.argv[3])
runtime = types.ModuleType("harness.generation_runtime")
class CodexOSRun:
    def __init__(self, directory):
        self.directory = pathlib.Path(directory)
    def archived_generations(self):
        result = []
        for archive in sorted(self.directory.iterdir()):
            if not archive.name.startswith("generation-"):
                continue
            generation = int(archive.name.removeprefix("generation-"))
            metadata = json.loads((archive / "metadata.json").read_text())
            handoff = None
            if metadata["outcome"] == "completed":
                handoff = (archive / "handoff.txt").read_text()
            result.append(types.SimpleNamespace(
                generation=generation,
                parent_generation=metadata["parent_generation"],
                transition=metadata["transition"],
                outcome=metadata["outcome"],
                archive_path=archive,
                handoff=handoff,
            ))
        return result
    @staticmethod
    def _validate_archived_history(archives):
        by_generation = {archive.generation: archive for archive in archives}
        if sorted(by_generation) != list(range(len(archives))):
            raise ValueError("generation archive history is not contiguous")
        for archive in archives[1:]:
            parent = by_generation.get(archive.parent_generation)
            if parent is None or parent.outcome != "completed":
                raise ValueError("generation has no completed parent")
            if archive.transition == "successor" and archive.parent_generation != archive.generation - 1:
                raise ValueError("invalid successor ancestry")
            if archive.transition == "rollback" and archive.parent_generation == archive.generation - 1:
                raise ValueError("invalid rollback ancestry")
runtime.CodexOSRun = CodexOSRun
runtime.ArchivedGeneration = object
sys.modules[runtime.__name__] = runtime
package = types.ModuleType("harness")
package.__path__ = [str(root / "harness")]
sys.modules[package.__name__] = package
source_spec = importlib.util.spec_from_file_location("harness.source_snapshot", root / "harness" / "source_snapshot.py")
source = importlib.util.module_from_spec(source_spec)
sys.modules[source_spec.name] = source
source_spec.loader.exec_module(source)
git_spec = importlib.util.spec_from_file_location("harness.generation_git", root / "harness" / "generation_git.py")
git = importlib.util.module_from_spec(git_spec)
sys.modules[git_spec.name] = git
git_spec.loader.exec_module(git)
for record in git.GenerationGitRecorder(repository, run, "test-base").reconcile():
    print(record.generation, record.tag, record.commit, record.already_recorded)
`
	pythonCommand := exec.Command("python3", "-c", pythonScript, provenanceRepositoryRoot(t), pythonRepository, pythonRun)
	pythonOutput, err := pythonCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("Python generation Git reference failed: %v\n%s", err, pythonOutput)
	}
	pythonLines := strings.Split(strings.TrimSpace(string(pythonOutput)), "\n")
	if len(pythonLines) != 3 {
		t.Fatalf("Python generation Git records = %q", pythonOutput)
	}
	goRecorder, err := NewGenerationGitRecorder(goRepository, goRun, "test-base")
	if err != nil {
		t.Fatal(err)
	}
	goRecords, err := goRecorder.Reconcile()
	if err != nil {
		t.Fatal(err)
	}
	for index, generation := range []uint64{0, 1, 2} {
		fields := strings.Fields(pythonLines[index])
		if len(fields) != 4 || fields[0] != strconv.FormatUint(generation, 10) || fields[1] != generationTag("experiment-conformance", generation) || fields[3] != "False" {
			t.Fatalf("Python record %q", pythonLines[index])
		}
		if got, want := goRecords[index].Commit, fields[2]; got != want {
			t.Fatalf("generation %d commit differs: Go %s Python %s", generation, got, want)
		}
		pythonTag := strings.TrimSpace(generationGitCommand(t, pythonRepository, "rev-parse", "refs/tags/"+fields[1]))
		goTag := strings.TrimSpace(generationGitCommand(t, goRepository, "rev-parse", "refs/tags/"+goRecords[index].Tag))
		if goTag != pythonTag {
			t.Fatalf("generation %d tag object differs: Go %s Python %s", generation, goTag, pythonTag)
		}
	}
	for _, branch := range []string{"lineage-0000", "lineage-0001"} {
		pythonBranch := strings.TrimSpace(generationGitCommand(t, pythonRepository, "rev-parse", "refs/heads/experiment-conformance/"+branch))
		goBranch := strings.TrimSpace(generationGitCommand(t, goRepository, "rev-parse", "refs/heads/experiment-conformance/"+branch))
		if pythonBranch != goBranch {
			t.Fatalf("%s differs: Go %s Python %s", branch, goBranch, pythonBranch)
		}
	}
}

func createGenerationGitRepository(t *testing.T, repository string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repository, "seed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := generationGitRun(repository, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	for _, config := range [][2]string{{"user.name", "Existing Developer"}, {"user.email", "developer@example.invalid"}} {
		if err := generationGitRun(repository, "config", config[0], config[1]); err != nil {
			t.Fatal(err)
		}
	}
	for path, contents := range map[string]string{
		"README.md":      "trusted base\n",
		"seed/kernel.c":  "base kernel\n",
		"seed/deleted.c": "base deletion\n",
	} {
		if err := os.WriteFile(filepath.Join(repository, filepath.FromSlash(path)), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := generationGitRun(repository, "add", "README.md", "seed"); err != nil {
		t.Fatal(err)
	}
	if err := generationGitRun(repository, "commit", "-m", "Trusted experiment base"); err != nil {
		t.Fatal(err)
	}
	base := strings.TrimSpace(generationGitCommand(t, repository, "rev-parse", "HEAD"))
	if err := generationGitRun(repository, "tag", "--no-sign", "--no-annotate", "test-base", base); err != nil {
		t.Fatal(err)
	}
	return base
}

func archiveGenerationGitCompleted(t *testing.T, run string, generation uint64, parent *uint64, transition string, files []guest.SnapshotFile, handoff string) {
	t.Helper()
	archive := filepath.Join(run, generationName(generation))
	for _, directory := range []string{filepath.Join(archive, "boot"), filepath.Join(archive, "source"), filepath.Join(archive, "successor")} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	parentValue := any(nil)
	if parent != nil {
		parentValue = *parent
	}
	metadata, err := json.MarshalIndent(map[string]any{
		"generation": generation, "outcome": "completed", "parent_generation": parentValue, "transition": transition,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeGenerationGitFile(t, filepath.Join(archive, "metadata.json"), append(metadata, '\n'))
	writeGenerationGitHardware(t, archive)
	snapshot, err := guest.EncodeSourceSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	writeGenerationGitFile(t, filepath.Join(archive, "source.snapshot"), snapshot)
	for _, file := range files {
		writeGenerationGitFile(t, filepath.Join(archive, filepath.FromSlash("source/"+file.Path)), file.Content)
	}
	writeGenerationGitFile(t, filepath.Join(archive, "handoff.txt"), []byte(handoff))
	writeGenerationGitFile(t, filepath.Join(archive, "boot/codexos.iso"), []byte("boot"))
	writeGenerationGitFile(t, filepath.Join(archive, "successor/kernel.elf"), []byte("kernel"))
	writeGenerationGitFile(t, filepath.Join(archive, "successor/codexos.iso"), []byte("successor"))
	writeGenerationGitFile(t, filepath.Join(archive, "qemu.stdout"), nil)
	writeGenerationGitFile(t, filepath.Join(archive, "qemu.stderr"), nil)
}

func archiveGenerationGitAborted(t *testing.T, run string, generation uint64, parent *uint64, transition string) {
	t.Helper()
	archive := filepath.Join(run, generationName(generation))
	if err := os.MkdirAll(filepath.Join(archive, "boot"), 0o755); err != nil {
		t.Fatal(err)
	}
	parentValue := any(nil)
	if parent != nil {
		parentValue = *parent
	}
	metadata, err := json.MarshalIndent(map[string]any{
		"generation": generation, "outcome": "aborted", "parent_generation": parentValue, "transition": transition,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeGenerationGitFile(t, filepath.Join(archive, "metadata.json"), append(metadata, '\n'))
	writeGenerationGitHardware(t, archive)
	writeGenerationGitFile(t, filepath.Join(archive, "boot/codexos.iso"), []byte("boot"))
	writeGenerationGitFile(t, filepath.Join(archive, "aborted.txt"), []byte(abortMarker))
	writeGenerationGitFile(t, filepath.Join(archive, "qemu.stdout"), nil)
	writeGenerationGitFile(t, filepath.Join(archive, "qemu.stderr"), nil)
}

func writeGenerationGitHardware(t *testing.T, archive string) {
	t.Helper()
	manifest, err := qemu.TestHardwareProfile.Manifest("QEMU emulator version test")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := qemu.EncodeHardwareManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeGenerationGitFile(t, filepath.Join(archive, "hardware.json"), encoded)
}

func writeGenerationGitFile(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatal(err)
	}
}

func generationGitRun(repository string, arguments ...string) error {
	command := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-C", repository}, arguments...)...)
	command.Stdin = nil
	output, err := command.CombinedOutput()
	if err != nil {
		return &GenerationGitRecorderError{Reason: strings.TrimSpace(string(output)), Err: err}
	}
	return nil
}

func generationGitCommand(t *testing.T, repository string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-C", repository}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
	return string(output)
}

func generationGitUint64Pointer(value uint64) *uint64 { return &value }

func TestGenerationGitPathValidationMatchesHostPlatform(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Git path namespace test uses POSIX paths")
	}
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	createGenerationGitRepository(t, repository)
	run := filepath.Join(root, "experiment..002")
	if err := os.Mkdir(run, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewGenerationGitRecorder(repository, run, "test-base"); err == nil || !strings.Contains(err.Error(), "cannot form a Git tag namespace or lineage branch namespace") {
		t.Fatalf("invalid run basename error = %v", err)
	}
}
