package provenance

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
)

func TestPythonPlanningEvidenceConformance(t *testing.T) {
	root := provenanceRepositoryRoot(t)
	pythonRun := t.TempDir()
	goRun := t.TempDir()
	const script = `
import importlib.util, pathlib, sys
root = pathlib.Path(sys.argv[1])
run = pathlib.Path(sys.argv[2])
spec = importlib.util.spec_from_file_location("planning_reference", root / "harness" / "planning_evidence.py")
module = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = module
spec.loader.exec_module(module)
evidence = module.PlanningEvidenceStore(run).begin(4, "thread-λ-😀")
evidence.record_started("turn-λ-1")
evidence.complete("interrupted", "Partial Ω.")
evidence.record_started("turn-2")
evidence.complete("completed", "Final plan.\n次")
failed = module.PlanningEvidenceStore(run).begin(5, "thread-failed")
failed.fail()
`
	command := exec.Command("python3", "-c", script, root, pythonRun)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python planning reference failed: %v\n%s", err, output)
	}

	evidence, err := NewPlanningEvidenceStore(goRun).Begin(4, "thread-λ-😀")
	if err != nil {
		t.Fatal(err)
	}
	if err := evidence.RecordStarted("turn-λ-1"); err != nil {
		t.Fatal(err)
	}
	partial := "Partial Ω."
	if _, err := evidence.Complete("interrupted", &partial); err != nil {
		t.Fatal(err)
	}
	if err := evidence.RecordStarted("turn-2"); err != nil {
		t.Fatal(err)
	}
	final := "Final plan.\n次"
	if _, err := evidence.Complete("completed", &final); err != nil {
		t.Fatal(err)
	}
	failed, err := NewPlanningEvidenceStore(goRun).Begin(5, "thread-failed")
	if err != nil {
		t.Fatal(err)
	}
	if err := failed.Fail(); err != nil {
		t.Fatal(err)
	}

	pythonFiles := evidenceFiles(t, pythonRun)
	goFiles := evidenceFiles(t, goRun)
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

func evidenceFiles(t *testing.T, run string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(filepath.Join(run, "planning-evidence"), func(path string, entry os.DirEntry, err error) error {
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

func provenanceRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate conformance test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFile), "../.."))
}
