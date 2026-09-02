package qemu

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPythonHardwareConformance(t *testing.T) {
	root := hardwareRepositoryRoot(t)
	const script = `
import base64, importlib.util, json, pathlib, sys
root = pathlib.Path(sys.argv[1])
spec = importlib.util.spec_from_file_location("hardware_reference", root / "harness" / "hardware.py")
module = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = module
spec.loader.exec_module(module)
profile = module.TEST_HARDWARE_PROFILE
arguments = profile.qemu_arguments(
    pathlib.Path("/trusted/boot-次<&>.iso"),
    pathlib.Path("/trusted/qmp.sock"),
    pathlib.Path("/trusted/serial.sock"),
)
manifest = profile.manifest("QEMU emulator version λ<&>")
encoded = (json.dumps(manifest.as_json_object(), indent=2, sort_keys=True) + "\n").encode("utf-8")
print(json.dumps({"arguments": arguments, "manifest": base64.b64encode(encoded).decode("ascii")}, separators=(",", ":")))
`
	command := exec.Command("python3", "-c", script, root)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("Python hardware reference failed: %v", err)
	}
	var reference struct {
		Arguments []string `json:"arguments"`
		Manifest  string   `json:"manifest"`
	}
	if err := json.Unmarshal(output, &reference); err != nil {
		t.Fatal(err)
	}
	arguments, err := TestHardwareProfile.QEMUCommandArguments(
		"/trusted/boot-次<&>.iso", "/trusted/qmp.sock", "/trusted/serial.sock",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(arguments, reference.Arguments) {
		t.Fatalf("QEMU arguments differ:\nGo: %#v\nPython: %#v", arguments, reference.Arguments)
	}
	manifest, err := TestHardwareProfile.Manifest("QEMU emulator version λ<&>")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeHardwareManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	pythonEncoded, err := base64.StdEncoding.DecodeString(reference.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, pythonEncoded) {
		t.Fatalf("manifest bytes differ:\nGo: %s\nPython: %s", encoded, pythonEncoded)
	}
}

func hardwareRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate hardware conformance test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFile), "../.."))
}
