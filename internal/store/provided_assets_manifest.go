package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"unicode/utf8"
)

type providedAssetRevision struct {
	Assets              []ProvidedAssetMetadata `json:"assets"`
	EffectiveGeneration *uint64                 `json:"effective_generation"`
	Revision            uint64                  `json:"revision"`
}

type providedAssetsManifest struct {
	SchemaVersion uint64
	Assets        []ProvidedAssetMetadata
	Revisions     []providedAssetRevision
}

// ConfigureProvidedAssets freezes the explicitly supplied directory and
// publishes its append-only run-level provenance. A nil externalDirectory
// means that no assets were supplied.
func ConfigureProvidedAssets(runDirectory string, externalDirectory *string, generation uint64) (*ProvidedAssets, error) {
	run, err := resolveProvidedRunDirectory(runDirectory)
	if err != nil {
		return nil, &ProvidedAssetsError{Reason: "could not resolve provided-assets run directory", Err: err}
	}
	manifestPath := filepath.Join(run, ProvidedAssetsManifest)
	manifestExists, err := providedManifestExists(manifestPath)
	if err != nil {
		return nil, err
	}
	if externalDirectory == nil {
		if manifestExists {
			return nil, &ProvidedAssetsError{Reason: "run records provided assets; --provided-assets is required"}
		}
		return nil, nil
	}

	snapshot, err := LoadProvidedAssets(*externalDirectory)
	if err != nil {
		return nil, err
	}
	if manifestExists {
		recorded, err := readProvidedAssetsManifest(manifestPath)
		if err != nil {
			return nil, err
		}
		revisions, err := manifestRevisions(recorded)
		if err != nil {
			return nil, err
		}
		previousAssets := revisions[len(revisions)-1].Assets
		introduced, err := validateProvidedAssetsAppendOnly(previousAssets, snapshot)
		if err != nil {
			return nil, err
		}
		if recorded.SchemaVersion == providedAssetsSchema && len(introduced) == 0 {
			latest := revisions[len(revisions)-1]
			if latest.EffectiveGeneration == nil {
				return nil, &ProvidedAssetsError{Reason: "provided-assets provenance has no current activation"}
			}
			return snapshotWithProvidedProvenance(snapshot, &ProvidedAssetsProvenance{
				Revision:             latest.Revision,
				EffectiveGeneration:  *latest.EffectiveGeneration,
				IntroducedAssetCount: 0,
				Created:              false,
			})
		}
		if recorded.SchemaVersion == providedAssetsLegacySchema {
			revisions = []providedAssetRevision{{
				Revision:            1,
				EffectiveGeneration: nil,
				Assets:              cloneProvidedAssetMetadata(recorded.Assets),
			}}
		}
		latestGeneration := revisions[len(revisions)-1].EffectiveGeneration
		if latestGeneration != nil && generation < *latestGeneration {
			return nil, &ProvidedAssetsError{Reason: "provided-assets revision cannot precede its current activation"}
		}
		if revisions[len(revisions)-1].Revision == maxUint64 {
			return nil, &ProvidedAssetsError{Reason: "provided-assets revision number is exhausted"}
		}
		revisionNumber := revisions[len(revisions)-1].Revision + 1
		revision := providedAssetRevision{
			Revision:            revisionNumber,
			EffectiveGeneration: uint64Pointer(generation),
			Assets:              assetMetadataFromSnapshot(snapshot),
		}
		updated := providedAssetsManifest{
			SchemaVersion: providedAssetsSchema,
			Revisions:     append(cloneProvidedAssetRevisions(revisions), revision),
		}
		if err := replaceProvidedAssetsManifestAtomically(manifestPath, updated, recorded); err != nil {
			return nil, err
		}
		return snapshotWithProvidedProvenance(snapshot, &ProvidedAssetsProvenance{
			Revision:             revisionNumber,
			EffectiveGeneration:  generation,
			IntroducedAssetCount: len(introduced),
			Created:              true,
		})
	}

	revision := providedAssetRevision{
		Revision:            1,
		EffectiveGeneration: uint64Pointer(generation),
		Assets:              assetMetadataFromSnapshot(snapshot),
	}
	initial := providedAssetsManifest{
		SchemaVersion: providedAssetsSchema,
		Revisions:     []providedAssetRevision{revision},
	}
	if err := writeProvidedAssetsManifestOnce(manifestPath, initial); err != nil {
		return nil, err
	}
	return snapshotWithProvidedProvenance(snapshot, &ProvidedAssetsProvenance{
		Revision:             1,
		EffectiveGeneration:  generation,
		IntroducedAssetCount: len(snapshot.order),
		Created:              true,
	})
}

func snapshotWithProvidedProvenance(snapshot *ProvidedAssets, provenance *ProvidedAssetsProvenance) (*ProvidedAssets, error) {
	return newProvidedAssets(snapshot.AssetsCopy(), provenance)
}

func resolveProvidedRunDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return filepath.Clean(resolved), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		// A dangling final component is handled below. Other failures should
		// not silently switch the physical run directory.
		if info, lstatErr := os.Lstat(absolute); lstatErr != nil || info.Mode()&os.ModeSymlink == 0 {
			return "", err
		}
	}

	// EvalSymlinks is strict about a missing final component. Resolve the
	// nearest existing ancestor, then append the missing suffix, matching
	// pathlib.Path.resolve(strict=False).
	current := absolute
	suffix := make([]string, 0, 4)
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, resolveErr := filepath.EvalSymlinks(current)
			if resolveErr != nil {
				return "", resolveErr
			}
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return filepath.Clean(resolved), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return absolute, nil
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func providedManifestExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, &ProvidedAssetsError{Reason: "could not inspect provided-assets provenance", Err: err}
}

func readProvidedAssetsManifest(path string) (providedAssetsManifest, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return providedAssetsManifest{}, &ProvidedAssetsError{Reason: "provided-assets provenance is not a regular file", Err: err}
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return providedAssetsManifest{}, &ProvidedAssetsError{Reason: "provided-assets provenance is not a regular file"}
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return providedAssetsManifest{}, &ProvidedAssetsError{Reason: "provided-assets provenance is malformed", Err: err}
	}
	if !utf8.Valid(encoded) {
		return providedAssetsManifest{}, &ProvidedAssetsError{Reason: "provided-assets provenance is malformed"}
	}
	value, err := parseProvidedAssetsManifest(encoded)
	if err != nil {
		var providedErr *ProvidedAssetsError
		if errors.As(err, &providedErr) {
			return providedAssetsManifest{}, err
		}
		return providedAssetsManifest{}, &ProvidedAssetsError{Reason: "provided-assets provenance is malformed", Err: err}
	}
	return value, nil
}

// ParseProvidedAssetsManifest validates a persisted manifest.
func ParseProvidedAssetsManifest(encoded []byte) error {
	_, err := parseProvidedAssetsManifest(encoded)
	return err
}

func parseProvidedAssetsManifest(encoded []byte) (providedAssetsManifest, error) {
	if !utf8.Valid(encoded) {
		return providedAssetsManifest{}, &ProvidedAssetsError{Reason: "provided-assets provenance is malformed"}
	}
	fields, err := decodeProvidedJSONObject(encoded)
	if err != nil {
		return providedAssetsManifest{}, &ProvidedAssetsError{Reason: "provided-assets provenance is malformed", Err: err}
	}
	schemaRaw, ok := fields["schema_version"]
	if !ok {
		return providedAssetsManifest{}, &ProvidedAssetsError{Reason: "provided-assets provenance has invalid fields"}
	}
	schema, err := decodeJSONUint(schemaRaw)
	if err != nil || (schema != providedAssetsLegacySchema && schema != providedAssetsSchema) {
		return providedAssetsManifest{}, &ProvidedAssetsError{Reason: "provided-assets provenance has an unsupported schema"}
	}
	value := providedAssetsManifest{SchemaVersion: schema}
	if schema == providedAssetsLegacySchema {
		if !hasExactlyProvidedFields(fields, "assets", "schema_version") {
			return providedAssetsManifest{}, &ProvidedAssetsError{Reason: "provided-assets provenance has invalid fields"}
		}
		assets, err := decodeProvidedAssetMetadataList(fields["assets"])
		if err != nil {
			return providedAssetsManifest{}, err
		}
		value.Assets = assets
		return value, nil
	}
	if !hasExactlyProvidedFields(fields, "revisions", "schema_version") {
		return providedAssetsManifest{}, &ProvidedAssetsError{Reason: "provided-assets provenance has invalid fields"}
	}
	revisionValues, err := decodeProvidedRawArray(fields["revisions"])
	if err != nil || len(revisionValues) == 0 {
		if err != nil {
			return providedAssetsManifest{}, &ProvidedAssetsError{Reason: "provided-assets provenance has invalid revisions", Err: err}
		}
		return providedAssetsManifest{}, &ProvidedAssetsError{Reason: "provided-assets provenance has invalid revisions"}
	}
	value.Revisions = make([]providedAssetRevision, 0, len(revisionValues))
	for index, raw := range revisionValues {
		revision, err := decodeProvidedAssetRevision(raw)
		if err != nil {
			return providedAssetsManifest{}, err
		}
		if revision.Revision != uint64(index+1) {
			return providedAssetsManifest{}, &ProvidedAssetsError{Reason: "provided-assets provenance revisions are not contiguous"}
		}
		if revision.EffectiveGeneration == nil {
			if index != 0 || len(revisionValues) == 1 {
				return providedAssetsManifest{}, &ProvidedAssetsError{Reason: "provided-assets provenance has invalid activation"}
			}
		} else if index > 0 {
			previous := value.Revisions[index-1].EffectiveGeneration
			if previous != nil && *revision.EffectiveGeneration < *previous {
				return providedAssetsManifest{}, &ProvidedAssetsError{Reason: "provided-assets provenance activation is not ordered"}
			}
		}
		if len(value.Revisions) > 0 {
			previous := value.Revisions[len(value.Revisions)-1]
			introduced, err := validateProvidedMetadataAppendOnly(previous.Assets, revision.Assets)
			if err != nil {
				return providedAssetsManifest{}, err
			}
			if len(introduced) == 0 && previous.EffectiveGeneration != nil {
				return providedAssetsManifest{}, &ProvidedAssetsError{Reason: "provided-assets provenance contains a duplicate revision"}
			}
		}
		value.Revisions = append(value.Revisions, revision)
	}
	return value, nil
}

func decodeProvidedAssetRevision(raw []byte) (providedAssetRevision, error) {
	fields, err := decodeProvidedJSONObject(raw)
	if err != nil {
		return providedAssetRevision{}, &ProvidedAssetsError{Reason: "provided-assets provenance has invalid revision fields", Err: err}
	}
	if !hasExactlyProvidedFields(fields, "assets", "effective_generation", "revision") {
		return providedAssetRevision{}, &ProvidedAssetsError{Reason: "provided-assets provenance has invalid revision fields"}
	}
	revision, err := decodeJSONUint(fields["revision"])
	if err != nil {
		return providedAssetRevision{}, &ProvidedAssetsError{Reason: "provided-assets provenance revisions are not contiguous"}
	}
	assets, err := decodeProvidedAssetMetadataList(fields["assets"])
	if err != nil {
		return providedAssetRevision{}, err
	}
	var generation *uint64
	if bytes.Equal(bytes.TrimSpace(fields["effective_generation"]), []byte("null")) {
		generation = nil
	} else {
		value, err := decodeJSONUint(fields["effective_generation"])
		if err != nil {
			return providedAssetRevision{}, &ProvidedAssetsError{Reason: "provided-assets provenance has invalid activation"}
		}
		generation = uint64Pointer(value)
	}
	return providedAssetRevision{Assets: assets, EffectiveGeneration: generation, Revision: revision}, nil
}

func decodeProvidedAssetMetadataList(raw []byte) ([]ProvidedAssetMetadata, error) {
	values, err := decodeProvidedRawArray(raw)
	if err != nil {
		return nil, &ProvidedAssetsError{Reason: "provided-assets provenance has invalid assets", Err: err}
	}
	assets := make([]ProvidedAssetMetadata, 0, len(values))
	for _, value := range values {
		asset, err := decodeProvidedAssetMetadata(value)
		if err != nil {
			return nil, err
		}
		if len(assets) > 0 && asset.ID <= assets[len(assets)-1].ID {
			return nil, &ProvidedAssetsError{Reason: "provided-assets provenance is not ID ordered"}
		}
		assets = append(assets, asset)
	}
	return assets, nil
}

func decodeProvidedAssetMetadata(raw []byte) (ProvidedAssetMetadata, error) {
	fields, err := decodeProvidedJSONObject(raw)
	if err != nil {
		return ProvidedAssetMetadata{}, &ProvidedAssetsError{Reason: "provided-assets provenance has invalid asset fields", Err: err}
	}
	if !hasExactlyProvidedFields(fields, "filename", "id", "sha256", "size") {
		return ProvidedAssetMetadata{}, &ProvidedAssetsError{Reason: "provided-assets provenance has invalid asset fields"}
	}
	id, err := decodeJSONString(fields["id"])
	if err != nil {
		return ProvidedAssetMetadata{}, &ProvidedAssetsError{Reason: "provided-assets provenance has invalid text"}
	}
	filename, err := decodeJSONString(fields["filename"])
	if err != nil {
		return ProvidedAssetMetadata{}, &ProvidedAssetsError{Reason: "provided-assets provenance has invalid text"}
	}
	if err := ValidateProvidedAssetID(id); err != nil {
		return ProvidedAssetMetadata{}, err
	}
	if err := ValidateProvidedAssetFilename(filename); err != nil {
		return ProvidedAssetMetadata{}, err
	}
	size, err := decodeJSONUint(fields["size"])
	if err != nil {
		return ProvidedAssetMetadata{}, &ProvidedAssetsError{Reason: "provided-assets provenance has invalid size"}
	}
	digest, err := decodeJSONString(fields["sha256"])
	if err != nil || len(digest) != 64 {
		return ProvidedAssetMetadata{}, &ProvidedAssetsError{Reason: "provided-assets provenance has invalid SHA-256"}
	}
	for _, character := range digest {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return ProvidedAssetMetadata{}, &ProvidedAssetsError{Reason: "provided-assets provenance has invalid SHA-256"}
		}
	}
	return ProvidedAssetMetadata{Filename: filename, ID: id, SHA256: digest, Size: size}, nil
}

func decodeProvidedJSONObject(raw []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var fields map[string]json.RawMessage
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		if err == nil {
			err = errors.New("JSON value is not an object")
		}
		return nil, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	return fields, nil
}

func decodeProvidedRawArray(raw []byte) ([][]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var values []json.RawMessage
	if err := decoder.Decode(&values); err != nil || values == nil {
		if err == nil {
			err = errors.New("JSON value is not an array")
		}
		return nil, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	result := make([][]byte, 0, len(values))
	for _, value := range values {
		result = append(result, append([]byte(nil), value...))
	}
	return result, nil
}

func hasExactlyProvidedFields(fields map[string]json.RawMessage, names ...string) bool {
	if len(fields) != len(names) {
		return false
	}
	for _, name := range names {
		if _, ok := fields[name]; !ok {
			return false
		}
	}
	return true
}

func manifestRevisions(value providedAssetsManifest) ([]providedAssetRevision, error) {
	if value.SchemaVersion == providedAssetsLegacySchema {
		return []providedAssetRevision{{Revision: 1, Assets: cloneProvidedAssetMetadata(value.Assets)}}, nil
	}
	return cloneProvidedAssetRevisions(value.Revisions), nil
}

func validateProvidedAssetsAppendOnly(previous []ProvidedAssetMetadata, snapshot *ProvidedAssets) ([]ProvidedAssetMetadata, error) {
	return validateProvidedMetadataAppendOnly(previous, assetMetadataFromSnapshot(snapshot))
}

func validateProvidedMetadataAppendOnly(previous, current []ProvidedAssetMetadata) ([]ProvidedAssetMetadata, error) {
	currentByID := make(map[string]ProvidedAssetMetadata, len(current))
	for _, item := range current {
		currentByID[item.ID] = item
	}
	previousIDs := make(map[string]struct{}, len(previous))
	for _, recorded := range previous {
		previousIDs[recorded.ID] = struct{}{}
		supplied, exists := currentByID[recorded.ID]
		if !exists {
			return nil, &ProvidedAssetsError{Reason: "supplied provided-assets set omits previously introduced asset " + strconv.Quote(recorded.ID)}
		}
		if !reflect.DeepEqual(supplied, recorded) {
			return nil, &ProvidedAssetsError{Reason: "supplied provided asset changed after introduction: " + strconv.Quote(recorded.ID)}
		}
	}
	introduced := make([]ProvidedAssetMetadata, 0)
	for _, item := range current {
		if _, exists := previousIDs[item.ID]; !exists {
			introduced = append(introduced, item)
		}
	}
	return introduced, nil
}

func encodeProvidedAssetsManifest(value providedAssetsManifest) ([]byte, error) {
	var serializable any
	switch value.SchemaVersion {
	case providedAssetsLegacySchema:
		serializable = struct {
			Assets        []ProvidedAssetMetadata `json:"assets"`
			SchemaVersion uint64                  `json:"schema_version"`
		}{Assets: value.Assets, SchemaVersion: value.SchemaVersion}
	case providedAssetsSchema:
		serializable = struct {
			Revisions     []providedAssetRevision `json:"revisions"`
			SchemaVersion uint64                  `json:"schema_version"`
		}{Revisions: value.Revisions, SchemaVersion: value.SchemaVersion}
	default:
		return nil, &ProvidedAssetsError{Reason: "provided-assets provenance has an unsupported schema"}
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(serializable); err != nil {
		return nil, &ProvidedAssetsError{Reason: "could not encode provided-assets provenance", Err: err}
	}
	return unescapeJSONLineSeparators(output.Bytes()), nil
}

func writeProvidedAssetsManifestOnce(path string, value providedAssetsManifest) error {
	encoded, err := encodeProvidedAssetsManifest(value)
	if err != nil {
		return err
	}
	if err := validateProvidedManifestValue(value); err != nil {
		return err
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o777); err != nil {
		return providedAssetsPersistError(err)
	}
	if info, err := os.Lstat(parent); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		if err != nil {
			return providedAssetsPersistError(err)
		}
		return &ProvidedAssetsError{Reason: "provided-assets provenance parent is not a directory"}
	}
	temporary, err := os.CreateTemp(parent, "."+filepath.Base(path)+".")
	if err != nil {
		return providedAssetsPersistError(err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := writeAndSyncProvidedManifestFile(temporary, encoded); err != nil {
		return providedAssetsPersistError(err)
	}
	if err := os.Link(temporaryName, path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return providedAssetsPersistError(err)
		}
		recorded, readErr := readProvidedAssetsManifest(path)
		if readErr != nil {
			return readErr
		}
		if !reflect.DeepEqual(recorded, value) {
			return &ProvidedAssetsError{Reason: "provided-assets provenance was initialized concurrently with different contents"}
		}
	}
	return nil
}

func replaceProvidedAssetsManifestAtomically(path string, value, expected providedAssetsManifest) error {
	if err := validateProvidedManifestValue(value); err != nil {
		return err
	}
	encoded, err := encodeProvidedAssetsManifest(value)
	if err != nil {
		return err
	}
	parent := filepath.Dir(path)
	temporary, err := os.CreateTemp(parent, "."+filepath.Base(path)+".")
	if err != nil {
		return providedAssetsPersistError(err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := writeAndSyncProvidedManifestFile(temporary, encoded); err != nil {
		return providedAssetsPersistError(err)
	}
	recorded, err := readProvidedAssetsManifest(path)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(recorded, expected) {
		return &ProvidedAssetsError{Reason: "provided-assets provenance changed during configuration"}
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return providedAssetsPersistError(err)
	}
	return nil
}

func writeAndSyncProvidedManifestFile(file *os.File, encoded []byte) error {
	if written, err := file.Write(encoded); err != nil || written != len(encoded) {
		if err == nil {
			err = io.ErrShortWrite
		}
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func providedAssetsPersistError(err error) error {
	var providedErr *ProvidedAssetsError
	if errors.As(err, &providedErr) {
		return err
	}
	return &ProvidedAssetsError{Reason: "could not persist provided-assets provenance", Err: err}
}

func validateProvidedManifestValue(value providedAssetsManifest) error {
	if value.SchemaVersion == providedAssetsLegacySchema {
		if err := validateProvidedAssetMetadataList(value.Assets); err != nil {
			return err
		}
		return nil
	}
	if value.SchemaVersion != providedAssetsSchema || len(value.Revisions) == 0 {
		return &ProvidedAssetsError{Reason: "provided-assets provenance has invalid revisions"}
	}
	previous := []ProvidedAssetMetadata(nil)
	var previousGeneration *uint64
	for index, revision := range value.Revisions {
		if revision.Revision != uint64(index+1) {
			return &ProvidedAssetsError{Reason: "provided-assets provenance revisions are not contiguous"}
		}
		if revision.EffectiveGeneration == nil {
			if index != 0 || len(value.Revisions) == 1 {
				return &ProvidedAssetsError{Reason: "provided-assets provenance has invalid activation"}
			}
		} else if previousGeneration != nil && *revision.EffectiveGeneration < *previousGeneration {
			return &ProvidedAssetsError{Reason: "provided-assets provenance activation is not ordered"}
		}
		if revision.EffectiveGeneration != nil {
			generation := *revision.EffectiveGeneration
			previousGeneration = &generation
		}
		if err := validateProvidedAssetMetadataList(revision.Assets); err != nil {
			return err
		}
		if previous != nil {
			introduced, err := validateProvidedMetadataAppendOnly(previous, revision.Assets)
			if err != nil {
				return err
			}
			if len(introduced) == 0 && previousGeneration != nil {
				// A duplicate check during publication is only relevant when the
				// previous revision had a known activation.
				previousRevision := value.Revisions[index-1]
				if previousRevision.EffectiveGeneration != nil {
					return &ProvidedAssetsError{Reason: "provided-assets provenance contains a duplicate revision"}
				}
			}
		}
		previous = revision.Assets
	}
	return nil
}

func validateProvidedAssetMetadataList(assets []ProvidedAssetMetadata) error {
	previous := ""
	for index, asset := range assets {
		if index > 0 && asset.ID <= previous {
			return &ProvidedAssetsError{Reason: "provided-assets provenance is not ID ordered"}
		}
		if err := ValidateProvidedAssetID(asset.ID); err != nil {
			return err
		}
		if err := ValidateProvidedAssetFilename(asset.Filename); err != nil {
			return err
		}
		if len(asset.SHA256) != 64 {
			return &ProvidedAssetsError{Reason: "provided-assets provenance has invalid SHA-256"}
		}
		for _, character := range asset.SHA256 {
			if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
				return &ProvidedAssetsError{Reason: "provided-assets provenance has invalid SHA-256"}
			}
		}
		previous = asset.ID
	}
	return nil
}

func cloneProvidedAssetMetadata(value []ProvidedAssetMetadata) []ProvidedAssetMetadata {
	if value == nil {
		return nil
	}
	result := make([]ProvidedAssetMetadata, len(value))
	copy(result, value)
	return result
}

func cloneProvidedAssetRevisions(value []providedAssetRevision) []providedAssetRevision {
	result := make([]providedAssetRevision, 0, len(value))
	for _, revision := range value {
		copy := revision
		copy.Assets = cloneProvidedAssetMetadata(revision.Assets)
		if revision.EffectiveGeneration != nil {
			generation := *revision.EffectiveGeneration
			copy.EffectiveGeneration = &generation
		}
		result = append(result, copy)
	}
	return result
}

func uint64Pointer(value uint64) *uint64 { return &value }
