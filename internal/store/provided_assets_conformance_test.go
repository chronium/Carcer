package store

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPythonProvidedAssetsDerivationAndManifestConformance(t *testing.T) {
	root := repositoryRoot(t)
	temporary := t.TempDir()
	external := filepath.Join(temporary, "external")
	if err := os.MkdirAll(filepath.Join(external, "tree", "src", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeTestAsset(external, "alpha", "payload.bin", []byte{0, 255, 'x'}); err != nil {
		t.Fatal(err)
	}
	if err := writeTestAsset(external, "unicode-id", "line .bin", []byte("unicode name\n")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(external, "tree", "README"), []byte("read me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(external, "tree", "src.txt"), []byte("peer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(external, "tree", "src", "run.bin"), []byte("program\x00"), 0o700); err != nil {
		t.Fatal(err)
	}
	longDirectory := filepath.Join(external, "tree", "src", "nested", "path", "with", "many", "components", "that", "push", "the", "archive", "name", "past", "the", "ustar", "name", "field")
	if err := os.MkdirAll(longDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(longDirectory, "é.bin"), []byte("pax path\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pythonRun := filepath.Join(temporary, "python-run")
	expected := filepath.Join(temporary, "expected")
	if err := os.MkdirAll(expected, 0o755); err != nil {
		t.Fatal(err)
	}
	const script = `
import importlib.util, json, pathlib, sys, types
root = pathlib.Path(sys.argv[1])
external = pathlib.Path(sys.argv[2])
run = pathlib.Path(sys.argv[3])
output = pathlib.Path(sys.argv[4])
package = types.ModuleType("harness")
package.__path__ = [str(root / "harness")]
sys.modules["harness"] = package
for name in ("framing", "host_service_protocol", "provided_assets"):
    spec = importlib.util.spec_from_file_location("harness." + name, root / "harness" / (name + ".py"))
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
module = sys.modules["harness.provided_assets"]
snapshot = module.ProvidedAssets.from_directory(external)
for asset in snapshot.assets:
    (output / (asset.id + ".bin")).write_bytes(asset.data)
(output / "identities.json").write_text(json.dumps([
    {"id": asset.id, "filename": asset.filename, "size": asset.size, "sha256": asset.sha256}
    for asset in snapshot.assets
], ensure_ascii=False, sort_keys=True), encoding="utf-8")
module.configure_provided_assets(run, external, effective_generation=7)
`
	command := exec.Command("python3", "-c", script, root, external, pythonRun, expected)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python reference failed: %v\n%s", err, output)
	}

	goRun := filepath.Join(temporary, "go-run")
	goSnapshot, err := ConfigureProvidedAssets(goRun, &external, uint64(7))
	if err != nil {
		t.Fatal(err)
	}
	var identities []ProvidedAssetMetadata
	encoded, err := os.ReadFile(filepath.Join(expected, "identities.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &identities); err != nil {
		t.Fatal(err)
	}
	if len(identities) != len(goSnapshot.Metadata()) {
		t.Fatalf("identity count = %d, want %d", len(goSnapshot.Metadata()), len(identities))
	}
	for index, identity := range identities {
		asset := goSnapshot.Assets[index]
		if asset.ID != identity.ID || asset.Filename != identity.Filename || asset.Size != identity.Size {
			t.Fatalf("identity %d = (%q, %q, %d), want (%q, %q, %d)", index, asset.ID, asset.Filename, asset.Size, identity.ID, identity.Filename, identity.Size)
		}
		expectedBytes, err := os.ReadFile(filepath.Join(expected, asset.ID+".bin"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(asset.Data, expectedBytes) {
			first := -1
			for index := 0; index < len(asset.Data) && index < len(expectedBytes); index++ {
				if asset.Data[index] != expectedBytes[index] {
					first = index
					break
				}
			}
			t.Fatalf("asset %q bytes differ from Python (first diff %d, Go=%x Python=%x)", asset.ID, first, asset.Data[max(0, first-8):min(len(asset.Data), first+8)], expectedBytes[max(0, first-8):min(len(expectedBytes), first+8)])
		}
		if asset.SHA256 != identity.SHA256 {
			t.Fatalf("identity %d digest = %q, want %q", index, asset.SHA256, identity.SHA256)
		}
	}
	pythonManifest, err := os.ReadFile(filepath.Join(pythonRun, ProvidedAssetsManifest))
	if err != nil {
		t.Fatal(err)
	}
	goManifest, err := os.ReadFile(filepath.Join(goRun, ProvidedAssetsManifest))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(goManifest, pythonManifest) {
		t.Fatalf("manifest encoding differs:\nGo: %s\nPython: %s", goManifest, pythonManifest)
	}
}

func TestProvidedAssetsRunPathResolvesExistingSymlink(t *testing.T) {
	target := t.TempDir()
	parent := t.TempDir()
	link := filepath.Join(parent, "run-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := ConfigureProvidedAssets(link, nil, uint64(0)); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := ConfigureProvidedAssets(link, &missing, uint64(0)); err == nil {
		t.Fatal("missing external directory unexpectedly accepted")
	}
}
