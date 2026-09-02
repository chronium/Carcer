package provenance

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
)

func TestPythonForensicProvenanceConformance(t *testing.T) {
	root := provenanceRepositoryRoot(t)
	pythonRun := t.TempDir()
	goRun := t.TempDir()
	const script = `
import importlib.util, pathlib, sys, types
root = pathlib.Path(sys.argv[1])
run = pathlib.Path(sys.argv[2])
package = types.ModuleType("harness")
package.__path__ = [str(root / "harness")]
sys.modules["harness"] = package
observability = types.ModuleType("harness.observability")
observability.ExperimentObservability = object
sys.modules["harness.observability"] = observability
spec = importlib.util.spec_from_file_location(
    "harness.forensic_provenance", root / "harness" / "forensic_provenance.py"
)
module = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = module
spec.loader.exec_module(module)
BuildReviewProvenance = module.BuildReviewProvenance
FileIdentity = module.FileIdentity

snapshot = b"snapshot\x00bytes\xff \xce\xbb"
build = BuildReviewProvenance(run).begin_build(12, snapshot)
build.record_decoded(2, 37)
build.record_artifacts(FileIdentity("a" * 64, 123), FileIdentity("b" * 64, 456))
build.record_candidate_stage(
    "build_protocol_validation_completed",
    "protocol_validated",
    outcome="success",
    protocol_validated=True,
)
build.record_latest_success(snapshot)
build.record_latest_success_update()

review = BuildReviewProvenance(run).begin_review(12)
review.record_source_read("seed/tasks.c", 7, 14, 0, b"source\x00bytes\xff")
review.complete("completed")
`
	command := exec.Command("python3", "-c", script, root, pythonRun)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python forensic reference failed: %v\n%s", err, output)
	}

	snapshot := []byte("snapshot\x00bytes\xff λ")
	build, err := NewBuildReviewProvenance(goRun).BeginBuild(12, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := build.RecordDecoded(2, 37); err != nil {
		t.Fatal(err)
	}
	if err := build.RecordArtifacts(
		FileIdentity{SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Size: 123},
		FileIdentity{SHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Size: 456},
	); err != nil {
		t.Fatal(err)
	}
	if err := build.RecordCandidateStage(
		"build_protocol_validation_completed",
		"protocol_validated",
		map[string]any{"outcome": "success", "protocol_validated": true},
	); err != nil {
		t.Fatal(err)
	}
	if err := build.RecordLatestSuccess(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := build.RecordLatestSuccessUpdate(); err != nil {
		t.Fatal(err)
	}
	review, err := NewBuildReviewProvenance(goRun).BeginReview(12)
	if err != nil {
		t.Fatal(err)
	}
	if err := review.RecordSourceRead("seed/tasks.c", 7, 14, 0, []byte("source\x00bytes\xff")); err != nil {
		t.Fatal(err)
	}
	if err := review.Complete("completed"); err != nil {
		t.Fatal(err)
	}

	pythonFiles := forensicEvidenceFiles(t, pythonRun)
	goFiles := forensicEvidenceFiles(t, goRun)
	if len(pythonFiles) != len(goFiles) {
		t.Fatalf("file counts differ: Python %v Go %v", pythonFiles, goFiles)
	}
	for index, relative := range pythonFiles {
		if goFiles[index] != relative {
			t.Fatalf("file lists differ: Python %v Go %v", pythonFiles, goFiles)
		}
		pythonContents, err := os.ReadFile(filepath.Join(pythonRun, relative))
		if err != nil {
			t.Fatal(err)
		}
		goContents, err := os.ReadFile(filepath.Join(goRun, relative))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(goContents, pythonContents) {
			t.Fatalf("%s differs:\nGo: %s\nPython: %s", relative, goContents, pythonContents)
		}
	}
}

func forensicEvidenceFiles(t *testing.T, run string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(filepath.Join(run, "build-review-provenance"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(run, path)
		if err != nil {
			return err
		}
		files = append(files, relative)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}
