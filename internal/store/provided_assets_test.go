package store

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codexos/internal/guest"
)

func TestProvidedAssetsFreezeDerivationAndHostReads(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tree", "src", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeTestAsset(root, "binary", "payload.bin", []byte{0, 255, 'x'}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tree", "README"), []byte("read me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tree", "src.txt"), []byte("peer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runFile := filepath.Join(root, "tree", "src", "run.bin")
	if err := os.WriteFile(runFile, []byte("program\x00"), 0o700); err != nil {
		t.Fatal(err)
	}

	snapshot, err := LoadProvidedAssets(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{snapshot.Assets[0].ID, snapshot.Assets[1].ID}; !equalStringsForTest(got, []string{"binary", "tree"}) {
		t.Fatalf("asset IDs = %v", got)
	}
	if snapshot.Assets[0].Filename != "payload.bin" || !bytes.Equal(snapshot.Assets[0].Data, []byte{0, 255, 'x'}) {
		t.Fatalf("single-file asset = %#v", snapshot.Assets[0])
	}
	if snapshot.Assets[1].Filename != "tree.tar" || len(snapshot.Assets[1].Data)%10240 != 0 {
		t.Fatalf("tree asset = filename %q, size %d", snapshot.Assets[1].Filename, len(snapshot.Assets[1].Data))
	}
	archive := tar.NewReader(bytes.NewReader(snapshot.Assets[1].Data))
	wantNames := []string{"README", "src/", "src.txt", "src/empty/", "src/run.bin"}
	for index, wantName := range wantNames {
		header, err := archive.Next()
		if err != nil {
			t.Fatalf("tar member %d: %v", index, err)
		}
		if header.Name != wantName || header.ModTime.Unix() != 0 || header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" {
			t.Fatalf("tar member %d = %#v", index, header)
		}
		if wantName == "src/run.bin" && header.Mode != 0o755 {
			t.Fatalf("executable mode = %#o", header.Mode)
		}
	}
	if _, err := archive.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("tar trailing member error = %v", err)
	}

	original := append([]byte(nil), snapshot.Assets[0].Data...)
	if err := os.WriteFile(filepath.Join(root, "binary", "payload.bin"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	response, err := snapshot.HandleRequest(guest.HostRequest{
		RequestID:   4,
		ServiceName: "read_provided_asset",
		Arguments:   [][]byte{[]byte("binary"), []byte("0"), []byte("3")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.MessageType != guest.HostServiceResponse || response.RequestID != 4 || binary.LittleEndian.Uint32(response.Payload[:4]) != 0 || !bytes.Equal(response.Payload[4:], original) {
		t.Fatalf("frozen response = %#v", response)
	}
	list, err := snapshot.HandleRequest(guest.HostRequest{RequestID: 5, ServiceName: "list_provided_assets"})
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint32(list.Payload[:4]) != 0 || !bytes.Contains(list.Payload[4:], []byte("binary\tpayload.bin\t3\t")) {
		t.Fatalf("descriptor response = %x", list.Payload)
	}
	invalid, err := snapshot.HandleRequest(guest.HostRequest{RequestID: 6, ServiceName: "read_provided_asset", Arguments: [][]byte{[]byte("binary"), []byte("00"), []byte("1")}})
	if err != nil || binary.LittleEndian.Uint32(invalid.Payload[:4]) != 1 || len(invalid.Payload[4:]) > 1024 {
		t.Fatalf("invalid read response = %#v, %v", invalid, err)
	}
}

func TestProvidedAssetsManifestRevisionsAndLegacyAdoption(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	if err := writeTestAsset(first, "alpha", "alpha.bin", []byte("alpha")); err != nil {
		t.Fatal(err)
	}
	if err := writeTestAsset(second, "alpha", "alpha.bin", []byte("alpha")); err != nil {
		t.Fatal(err)
	}
	if err := writeTestAsset(second, "beta", "beta.bin", []byte("beta")); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(root, "run")
	configured, err := ConfigureProvidedAssets(run, &first, uint64(0))
	if err != nil {
		t.Fatal(err)
	}
	if configured.Provenance == nil || configured.Provenance.Revision != 1 || configured.Provenance.IntroducedAssetCount != 1 || !configured.Provenance.Created {
		t.Fatalf("initial provenance = %#v", configured.Provenance)
	}
	before, err := os.ReadFile(filepath.Join(run, ProvidedAssetsManifest))
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := ConfigureProvidedAssets(run, &first, uint64(1))
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Provenance == nil || reopened.Provenance.Created || reopened.Provenance.Revision != 1 {
		t.Fatalf("idempotent provenance = %#v", reopened.Provenance)
	}
	after, err := os.ReadFile(filepath.Join(run, ProvidedAssetsManifest))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("idempotent configuration rewrote the manifest")
	}
	expanded, err := ConfigureProvidedAssets(run, &second, uint64(2))
	if err != nil {
		t.Fatal(err)
	}
	if expanded.Provenance == nil || expanded.Provenance.Revision != 2 || expanded.Provenance.IntroducedAssetCount != 1 {
		t.Fatalf("expanded provenance = %#v", expanded.Provenance)
	}
	value := decodeTestJSON(t, filepath.Join(run, ProvidedAssetsManifest))
	if value["schema_version"] != float64(2) || len(value["revisions"].([]any)) != 2 {
		t.Fatalf("manifest = %#v", value)
	}

	removed := filepath.Join(root, "removed")
	if err := writeTestAsset(removed, "alpha", "alpha.bin", []byte("alpha")); err != nil {
		t.Fatal(err)
	}
	if _, err := ConfigureProvidedAssets(run, &removed, uint64(3)); err == nil || !strings.Contains(err.Error(), "omits") {
		t.Fatalf("removed asset error = %v", err)
	}

	legacyRun := filepath.Join(root, "legacy-run")
	if err := os.MkdirAll(legacyRun, 0o755); err != nil {
		t.Fatal(err)
	}
	legacySnapshot, err := LoadProvidedAssets(first)
	if err != nil {
		t.Fatal(err)
	}
	legacyManifest := struct {
		Assets        []ProvidedAssetMetadata `json:"assets"`
		SchemaVersion uint64                  `json:"schema_version"`
	}{Assets: legacySnapshot.Metadata(), SchemaVersion: providedAssetsLegacySchema}
	legacyBytes, err := json.MarshalIndent(legacyManifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	legacyBytes = append(legacyBytes, '\n')
	if err := os.WriteFile(filepath.Join(legacyRun, ProvidedAssetsManifest), legacyBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	legacyConfigured, err := ConfigureProvidedAssets(legacyRun, &first, uint64(12))
	if err != nil {
		t.Fatal(err)
	}
	if legacyConfigured.Provenance == nil || legacyConfigured.Provenance.Revision != 2 || legacyConfigured.Provenance.IntroducedAssetCount != 0 {
		t.Fatalf("legacy provenance = %#v", legacyConfigured.Provenance)
	}
	legacyValue := decodeTestJSON(t, filepath.Join(legacyRun, ProvidedAssetsManifest))
	revisions := legacyValue["revisions"].([]any)
	if revisions[0].(map[string]any)["effective_generation"] != nil || revisions[1].(map[string]any)["effective_generation"] != float64(12) {
		t.Fatalf("legacy revisions = %#v", revisions)
	}
}

func TestProvidedAssetsNilConfigurationAndManifestValidation(t *testing.T) {
	run := filepath.Join(t.TempDir(), "run")
	assets, err := ConfigureProvidedAssets(run, nil, 0)
	if err != nil || assets != nil {
		t.Fatalf("nil configuration = %#v, %v", assets, err)
	}
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, ProvidedAssetsManifest), []byte(`{"schema_version":2,"revisions":[{"assets":[],"effective_generation":null,"revision":1}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ParseProvidedAssetsManifest([]byte(`{"schema_version":2,"revisions":[{"assets":[],"effective_generation":null,"revision":1}]}`)); err == nil || !strings.Contains(err.Error(), "invalid activation") {
		t.Fatalf("single unknown activation error = %v", err)
	}
}

func writeTestAsset(root, assetID, filename string, data []byte) error {
	directory := filepath.Join(root, assetID)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, filename), data, 0o644)
}

func decodeTestJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func equalStringsForTest(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func TestProvidedAssetDigestMatchesBytes(t *testing.T) {
	root := t.TempDir()
	data := []byte("digest me")
	if err := writeTestAsset(root, "alpha", "payload", data); err != nil {
		t.Fatal(err)
	}
	assets, err := LoadProvidedAssets(root)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	if assets.Assets[0].SHA256 != hex.EncodeToString(digest[:]) || assets.Assets[0].Size != uint64(len(data)) {
		t.Fatalf("asset identity = %#v", assets.Assets[0])
	}
}

func TestProvidedAssetFileDoesNotFollowSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "link")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := readProvidedAssetFile(link); err == nil {
		t.Fatal("readProvidedAssetFile followed a symlink")
	}
}
