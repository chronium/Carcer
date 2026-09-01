package store

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPythonFeatureRequestStoreConformance(t *testing.T) {
	root := repositoryRoot(t)
	run := t.TempDir()
	const createScript = `
import importlib.util, pathlib, sys
root = pathlib.Path(sys.argv[1])
run = pathlib.Path(sys.argv[2])
spec = importlib.util.spec_from_file_location("feature_reference", root / "harness" / "feature_requests.py")
module = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = module
spec.loader.exec_module(module)
store = module.FeatureRequestStore(run)
store.import_requests((
    module.FeatureRequest(2, 8, "Pending λ", "Exact <&> text literal \\u2028", "pending"),
    module.FeatureRequest(7, 10, "Denied", "Exact denial", "denied"),
))
created = store.create(0, "Python new", "No collision")
assert created.id == 8
`
	command := exec.Command("python3", "-c", createScript, root, run)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python reference creation failed: %v\n%s", err, output)
	}

	store, err := NewFeatureRequestStore(run)
	if err != nil {
		t.Fatal(err)
	}
	requests, err := store.Requests()
	if err != nil {
		t.Fatal(err)
	}
	want := []FeatureRequest{
		{ID: 2, Generation: 8, Title: "Pending λ", Description: "Exact <&> text literal \\u2028", Status: FeaturePending},
		{ID: 7, Generation: 10, Title: "Denied", Description: "Exact denial", Status: FeatureDenied},
		{ID: 8, Generation: 0, Title: "Python new", Description: "No collision", Status: FeaturePending},
	}
	assertFeatureRequests(t, requests, want)
	pythonRecord, err := os.ReadFile(filepath.Join(run, "feature-requests", "request-000002.json"))
	if err != nil {
		t.Fatal(err)
	}
	goRecord, err := encodeFeatureRequest(want[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(goRecord, pythonRecord) {
		t.Fatalf("record encoding differs:\nGo: %s\nPython: %s", goRecord, pythonRecord)
	}
	if _, err := store.Approve(2); err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(11, "Go new 次", "Read by Python")
	if err != nil || created.ID != 9 {
		t.Fatalf("Go create = %#v, %v", created, err)
	}

	const verifyScript = `
import importlib.util, pathlib, sys
root = pathlib.Path(sys.argv[1])
run = pathlib.Path(sys.argv[2])
spec = importlib.util.spec_from_file_location("feature_reference_verify", root / "harness" / "feature_requests.py")
module = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = module
spec.loader.exec_module(module)
requests = module.FeatureRequestStore(run).requests()
assert [(r.id, r.generation, r.status) for r in requests] == [
    (2, 8, "approved"), (7, 10, "denied"), (8, 0, "pending"), (9, 11, "pending")
]
assert requests[-1].title == "Go new 次"
assert requests[-1].description == "Read by Python"
`
	command = exec.Command("python3", "-c", verifyScript, root, run)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python reference verification failed: %v\n%s", err, output)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate conformance test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFile), "../.."))
}
