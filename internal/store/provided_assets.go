package store

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"codexos/internal/guest"
)

const (
	ProvidedAssetsManifest         = "provided-assets.json"
	MaxProvidedAssetIDBytes        = 64
	MaxProvidedAssetFilenameBytes  = 255
	MaxProvidedAssetReadBytes      = 1024 * 1024
	providedAssetsLegacySchema     = uint64(1)
	providedAssetsSchema           = uint64(2)
	maxProvidedAssetDiagnosticSize = 1024
	maxProvidedAssetFrameSize      = 16 * 1024 * 1024
)

const maxUint64 = ^uint64(0)

var providedAssetIDPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// ProvidedAssetsError reports invalid supplied assets or persisted asset
// provenance. Err, when present, is the underlying filesystem or codec error.
type ProvidedAssetsError struct {
	Reason string
	Err    error
}

func (e *ProvidedAssetsError) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *ProvidedAssetsError) Unwrap() error { return e.Err }

// ProvidedAssetsProvenance identifies the revision represented by a frozen
// snapshot. Created reports whether this configuration call published a new
// revision.
type ProvidedAssetsProvenance struct {
	Revision             uint64
	EffectiveGeneration  uint64
	IntroducedAssetCount int
	Created              bool
}

// ProvidedAsset is one immutable named payload. Data is a copy of the bytes
// captured while configuring the snapshot; callers should treat it as read
// only. Host-service reads use a private copy retained by ProvidedAssets.
type ProvidedAsset struct {
	ID       string
	Filename string
	Data     []byte
	SHA256   string
	Size     uint64
}

// ProvidedAssetMetadata is the non-sensitive identity recorded in the run
// manifest. It deliberately contains no source path or payload bytes.
type ProvidedAssetMetadata struct {
	Filename string `json:"filename"`
	ID       string `json:"id"`
	SHA256   string `json:"sha256"`
	Size     uint64 `json:"size"`
}

type providedAssetRecord struct {
	ID       string
	Filename string
	Data     []byte
	SHA256   string
	Size     uint64
}

// ProvidedAssets is one completely frozen set of opaque named assets.
type ProvidedAssets struct {
	// Assets is a caller-visible copy of the snapshot's records. The active
	// host service uses private records so mutating this diagnostic view cannot
	// alter transported bytes.
	Assets     []ProvidedAsset
	Provenance *ProvidedAssetsProvenance

	assets map[string]providedAssetRecord
	order  []string
}

// LoadProvidedAssets freezes one external directory into memory.
func LoadProvidedAssets(directory string) (*ProvidedAssets, error) {
	root := filepath.Clean(directory)
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return nil, &ProvidedAssetsError{Reason: "provided-assets directory is unavailable", Err: err}
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, &ProvidedAssetsError{Reason: "provided-assets path must be a real directory"}
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, &ProvidedAssetsError{Reason: "could not inspect provided-assets directory", Err: err}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	assets := make([]ProvidedAsset, 0, len(entries))
	for _, entry := range entries {
		assetID := entry.Name()
		if err := ValidateProvidedAssetID(assetID); err != nil {
			return nil, err
		}
		child := filepath.Join(root, assetID)
		childInfo, err := os.Lstat(child)
		if err != nil {
			return nil, &ProvidedAssetsError{Reason: fmt.Sprintf("could not inspect provided asset %q", assetID), Err: err}
		}
		if childInfo.Mode()&os.ModeSymlink != 0 || !childInfo.IsDir() {
			return nil, &ProvidedAssetsError{Reason: fmt.Sprintf("provided asset %q must be a real directory", assetID)}
		}
		filename, data, err := deriveProvidedAsset(child, assetID)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(data)
		assets = append(assets, ProvidedAsset{
			ID:       assetID,
			Filename: filename,
			Data:     data,
			SHA256:   hex.EncodeToString(digest[:]),
			Size:     uint64(len(data)),
		})
	}
	return newProvidedAssets(assets, nil)
}

// AssetsCopy returns a deep copy of the frozen asset view.
func (p *ProvidedAssets) AssetsCopy() []ProvidedAsset {
	if p == nil {
		return nil
	}
	result := make([]ProvidedAsset, 0, len(p.order))
	for _, assetID := range p.order {
		asset := p.assets[assetID]
		result = append(result, ProvidedAsset{
			ID: asset.ID, Filename: asset.Filename, Data: append([]byte(nil), asset.Data...), SHA256: asset.SHA256, Size: asset.Size,
		})
	}
	return result
}

// Metadata returns sorted non-sensitive identities for this snapshot.
func (p *ProvidedAssets) Metadata() []ProvidedAssetMetadata {
	if p == nil {
		return nil
	}
	result := make([]ProvidedAssetMetadata, 0, len(p.order))
	for _, assetID := range p.order {
		asset := p.assets[assetID]
		result = append(result, ProvidedAssetMetadata{
			Filename: asset.Filename,
			ID:       asset.ID,
			SHA256:   asset.SHA256,
			Size:     asset.Size,
		})
	}
	return result
}

// ManifestObject returns the legacy exact-set manifest shape used by Python
// fixtures. It is intentionally metadata-only.
func (p *ProvidedAssets) ManifestObject() map[string]any {
	return map[string]any{
		"schema_version": providedAssetsLegacySchema,
		"assets":         p.Metadata(),
	}
}

// DescriptorBytes returns the deterministic guest-facing descriptor list.
func (p *ProvidedAssets) DescriptorBytes() []byte {
	if p == nil {
		return nil
	}
	var output bytes.Buffer
	for _, assetID := range p.order {
		asset := p.assets[assetID]
		fmt.Fprintf(&output, "%s\t%s\t%d\t%s\n", asset.ID, asset.Filename, asset.Size, asset.SHA256)
	}
	return output.Bytes()
}

// ReadAsset returns an exact byte range from the private frozen snapshot.
func (p *ProvidedAssets) ReadAsset(assetID string, offset, length uint64) ([]byte, error) {
	if err := ValidateProvidedAssetID(assetID); err != nil {
		return nil, err
	}
	if length > MaxProvidedAssetReadBytes {
		return nil, &ProvidedAssetsError{Reason: "provided-asset read length exceeds 1 MiB"}
	}
	asset, ok := p.assets[assetID]
	if !ok {
		return nil, &ProvidedAssetsError{Reason: "provided asset does not exist"}
	}
	if offset > maxUint64-length || offset+length > asset.Size {
		return nil, &ProvidedAssetsError{Reason: "provided-asset read is out of range"}
	}
	data := asset.Data[int(offset):int(offset+length)]
	return append([]byte(nil), data...), nil
}

// HandleRequest serves list_provided_assets and read_provided_asset through
// the existing guest host-service envelope.
func (p *ProvidedAssets) HandleRequest(request guest.HostRequest) (guest.Frame, error) {
	if request.ServiceName == "list_provided_assets" {
		if len(request.Arguments) != 0 {
			return providedAssetResponse(request.RequestID, 1, []byte("list_provided_assets expects no arguments"))
		}
		descriptor := p.DescriptorBytes()
		if len(descriptor) > maxProvidedAssetFrameSize-4 {
			return providedAssetResponse(request.RequestID, 2, []byte("provided-asset descriptor list exceeds the frame limit"))
		}
		return providedAssetResponse(request.RequestID, 0, descriptor)
	}
	if request.ServiceName == "read_provided_asset" {
		return p.handleReadRequest(request)
	}
	return providedAssetResponse(request.RequestID, 1, []byte("unknown host service: "+request.ServiceName))
}

func (p *ProvidedAssets) handleReadRequest(request guest.HostRequest) (guest.Frame, error) {
	if len(request.Arguments) != 3 {
		return providedAssetResponse(request.RequestID, 1, []byte("read_provided_asset expects asset ID, offset, and length"))
	}
	encodedID, encodedOffset, encodedLength := request.Arguments[0], request.Arguments[1], request.Arguments[2]
	if !utf8.Valid(encodedID) {
		return providedAssetResponse(request.RequestID, 1, []byte("provided asset ID is not valid UTF-8"))
	}
	assetID := string(encodedID)
	if err := ValidateProvidedAssetID(assetID); err != nil {
		return providedAssetResponse(request.RequestID, 1, diagnosticBytes(err))
	}
	offset, err := parseCanonicalUint64(encodedOffset, "offset")
	if err != nil {
		return providedAssetResponse(request.RequestID, 1, []byte(err.Error()))
	}
	length, err := parseCanonicalUint64(encodedLength, "length")
	if err != nil {
		return providedAssetResponse(request.RequestID, 1, []byte(err.Error()))
	}
	data, err := p.ReadAsset(assetID, offset, length)
	if err != nil {
		return providedAssetResponse(request.RequestID, 1, diagnosticBytes(err))
	}
	return providedAssetResponse(request.RequestID, 0, data)
}

func newProvidedAssets(input []ProvidedAsset, provenance *ProvidedAssetsProvenance) (*ProvidedAssets, error) {
	ordered := append([]ProvidedAsset(nil), input...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	result := &ProvidedAssets{
		Assets:     make([]ProvidedAsset, 0, len(ordered)),
		Provenance: cloneProvidedAssetsProvenance(provenance),
		assets:     make(map[string]providedAssetRecord, len(ordered)),
		order:      make([]string, 0, len(ordered)),
	}
	for _, asset := range ordered {
		if err := ValidateProvidedAssetID(asset.ID); err != nil {
			return nil, err
		}
		if err := ValidateProvidedAssetFilename(asset.Filename); err != nil {
			return nil, err
		}
		if _, exists := result.assets[asset.ID]; exists {
			return nil, &ProvidedAssetsError{Reason: "provided asset IDs are not unique"}
		}
		data := append([]byte(nil), asset.Data...)
		digest := sha256.Sum256(data)
		computed := hex.EncodeToString(digest[:])
		if asset.SHA256 != "" && asset.SHA256 != computed {
			return nil, &ProvidedAssetsError{Reason: "provided asset SHA-256 does not match its bytes"}
		}
		asset.SHA256 = computed
		asset.Size = uint64(len(data))
		asset.Data = append([]byte(nil), data...)
		result.Assets = append(result.Assets, ProvidedAsset{
			ID: asset.ID, Filename: asset.Filename, Data: append([]byte(nil), data...), SHA256: asset.SHA256, Size: asset.Size,
		})
		result.assets[asset.ID] = providedAssetRecord{
			ID: asset.ID, Filename: asset.Filename, Data: data, SHA256: asset.SHA256, Size: asset.Size,
		}
		result.order = append(result.order, asset.ID)
	}
	return result, nil
}

func cloneProvidedAssetsProvenance(value *ProvidedAssetsProvenance) *ProvidedAssetsProvenance {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func deriveProvidedAsset(directory, assetID string) (string, []byte, error) {
	entries, err := walkProvidedAsset(directory, "")
	if err != nil {
		return "", nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	if len(entries) == 1 && entries[0].kind == tar.TypeReg && !strings.Contains(entries[0].path, "/") {
		filename := entries[0].path
		if err := ValidateProvidedAssetFilename(filename); err != nil {
			return "", nil, err
		}
		return filename, entries[0].data, nil
	}
	data, err := encodeProvidedAssetTar(entries)
	if err != nil {
		return "", nil, &ProvidedAssetsError{Reason: fmt.Sprintf("could not derive provided asset %q", assetID), Err: err}
	}
	return assetID + ".tar", data, nil
}

type providedAssetEntry struct {
	path string
	kind byte
	mode os.FileMode
	data []byte
}

func walkProvidedAsset(directory, relativeDirectory string) ([]providedAssetEntry, error) {
	children, err := os.ReadDir(directory)
	if err != nil {
		return nil, &ProvidedAssetsError{Reason: "could not inspect provided asset", Err: err}
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
	result := make([]providedAssetEntry, 0, len(children))
	for _, child := range children {
		name := child.Name()
		if err := ValidateProvidedAssetFilename(name); err != nil {
			return nil, err
		}
		relative := name
		if relativeDirectory != "" {
			relative = relativeDirectory + "/" + name
		}
		path := filepath.Join(directory, name)
		metadata, err := os.Lstat(path)
		if err != nil {
			return nil, &ProvidedAssetsError{Reason: fmt.Sprintf("could not inspect provided asset entry %q", relative), Err: err}
		}
		if metadata.Mode()&os.ModeSymlink != 0 {
			return nil, &ProvidedAssetsError{Reason: fmt.Sprintf("provided asset contains a symlink: %q", relative)}
		}
		switch {
		case metadata.IsDir():
			result = append(result, providedAssetEntry{path: relative, kind: tar.TypeDir, mode: metadata.Mode()})
			nested, err := walkProvidedAsset(path, relative)
			if err != nil {
				return nil, err
			}
			result = append(result, nested...)
		case metadata.Mode().IsRegular():
			data, err := readProvidedAssetFile(path)
			if err != nil {
				return nil, err
			}
			result = append(result, providedAssetEntry{path: relative, kind: tar.TypeReg, mode: metadata.Mode(), data: data})
		default:
			return nil, &ProvidedAssetsError{Reason: "provided asset contains a special filesystem entry: " + strconv.Quote(relative)}
		}
	}
	return result, nil
}

func readProvidedAssetFile(path string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, &ProvidedAssetsError{Reason: "could not read provided asset entry", Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, &ProvidedAssetsError{Reason: "could not read provided asset entry", Err: errors.New("could not create file handle")}
	}
	defer file.Close()
	metadata, err := file.Stat()
	if err != nil {
		return nil, &ProvidedAssetsError{Reason: "could not read provided asset entry", Err: err}
	}
	if !metadata.Mode().IsRegular() {
		return nil, &ProvidedAssetsError{Reason: "provided asset entry is not a regular file"}
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, &ProvidedAssetsError{Reason: "could not read provided asset entry", Err: err}
	}
	return data, nil
}

// encodeProvidedAssetTar mirrors Python tarfile.TarFile(format=PAX_FORMAT,
// dereference=False): deterministic USTAR headers, PAX path records when a
// path cannot fit, and the 10,240-byte record padding used by tarfile.
func encodeProvidedAssetTar(entries []providedAssetEntry) ([]byte, error) {
	var output bytes.Buffer
	for _, entry := range entries {
		name := entry.path
		typeflag := byte(tar.TypeReg)
		mode := int64(0o644)
		size := int64(len(entry.data))
		if entry.kind == tar.TypeDir {
			typeflag = tar.TypeDir
			name += "/"
			size = 0
			mode = 0o755
		} else if entry.mode&0o111 != 0 {
			mode = 0o755
		}
		if !utf8.ValidString(name) {
			return nil, errors.New("tar path is not valid UTF-8")
		}
		if !isASCII(name) || len(name) > 100 {
			record := paxRecord("path", name)
			if err := writeTarHeader(&output, "././@PaxHeader", 0, int64(len(record)), 'x'); err != nil {
				return nil, err
			}
			writeTarPayload(&output, record)
		}
		if err := writeTarHeader(&output, name, mode, size, typeflag); err != nil {
			return nil, err
		}
		if entry.kind == tar.TypeReg {
			writeTarPayload(&output, entry.data)
		}
	}
	// Python writes two end blocks, then fills to a 10,240-byte record.
	output.Write(make([]byte, 1024))
	if remainder := output.Len() % 10240; remainder != 0 {
		output.Write(make([]byte, 10240-remainder))
	}
	return output.Bytes(), nil
}

func writeTarHeader(output *bytes.Buffer, name string, mode, size int64, typeflag byte) error {
	// TarInfo.create_pax_header emits the main USTAR header with ASCII
	// encoding and errors="replace". The PAX path record retains the UTF-8
	// spelling, while the otherwise-unused USTAR name field contains '?'.
	encodedName := make([]byte, 0, len(name))
	for _, character := range name {
		if character < utf8.RuneSelf {
			encodedName = append(encodedName, byte(character))
		} else {
			encodedName = append(encodedName, '?')
		}
	}
	if len(encodedName) > 100 {
		encodedName = encodedName[:100]
	}
	header := make([]byte, 512)
	copy(header[0:100], encodedName)
	putTarOctal(header[100:108], mode)
	putTarOctal(header[108:116], 0)
	putTarOctal(header[116:124], 0)
	putTarOctal(header[124:136], size)
	putTarOctal(header[136:148], 0)
	for i := 148; i < 156; i++ {
		header[i] = ' '
	}
	header[156] = typeflag
	copy(header[257:265], []byte("ustar\x0000"))
	checksum := 0
	for _, value := range header {
		checksum += int(value)
	}
	encoded := fmt.Sprintf("%06o\x00 ", checksum)
	copy(header[148:156], encoded)
	output.Write(header)
	return nil
}

func putTarOctal(destination []byte, value int64) {
	if value < 0 {
		value = 0
	}
	encoded := fmt.Sprintf("%0*o", len(destination)-1, value)
	copy(destination, encoded)
	destination[len(destination)-1] = 0
}

func writeTarPayload(output *bytes.Buffer, data []byte) {
	output.Write(data)
	if remainder := len(data) % 512; remainder != 0 {
		output.Write(make([]byte, 512-remainder))
	}
}

func paxRecord(key, value string) []byte {
	lengthWithoutDigits := len(key) + len(value) + 3
	digits := 0
	length := 0
	for {
		length = lengthWithoutDigits + digits
		next := lengthWithoutDigits + len(strconv.Itoa(length))
		if next == length {
			break
		}
		digits = len(strconv.Itoa(next))
	}
	return []byte(strconv.Itoa(length) + " " + key + "=" + value + "\n")
}

func isASCII(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// ValidateProvidedAssetID applies the manifest and host-service ID rule.
func ValidateProvidedAssetID(assetID string) error {
	if !utf8.ValidString(assetID) {
		return &ProvidedAssetsError{Reason: "provided asset ID is not valid UTF-8"}
	}
	if len(assetID) > MaxProvidedAssetIDBytes {
		return &ProvidedAssetsError{Reason: "provided asset ID exceeds 64 bytes"}
	}
	if !providedAssetIDPattern.MatchString(assetID) {
		return &ProvidedAssetsError{Reason: fmt.Sprintf("invalid provided asset ID: %q", assetID)}
	}
	return nil
}

// ValidateProvidedAssetFilename applies the safe basename rule.
func ValidateProvidedAssetFilename(filename string) error {
	if !utf8.ValidString(filename) {
		return &ProvidedAssetsError{Reason: "provided asset filename is not valid UTF-8"}
	}
	if len(filename) == 0 || len(filename) > MaxProvidedAssetFilenameBytes {
		return &ProvidedAssetsError{Reason: "provided asset filename has an invalid length"}
	}
	if filename == "." || filename == ".." || strings.ContainsAny(filename, `/\\`) {
		return &ProvidedAssetsError{Reason: fmt.Sprintf("unsafe provided asset filename: %q", filename)}
	}
	for _, character := range filename {
		if character < 0x20 || (character >= 0x7f && character <= 0x9f) {
			return &ProvidedAssetsError{Reason: fmt.Sprintf("unsafe provided asset filename: %q", filename)}
		}
	}
	return nil
}

func parseCanonicalUint64(encoded []byte, name string) (uint64, error) {
	if len(encoded) == 0 {
		return 0, fmt.Errorf("%s is not canonical unsigned ASCII decimal", name)
	}
	if len(encoded) > 1 && encoded[0] == '0' {
		return 0, fmt.Errorf("%s is not canonical unsigned ASCII decimal", name)
	}
	for _, value := range encoded {
		if value < '0' || value > '9' {
			return 0, fmt.Errorf("%s is not canonical unsigned ASCII decimal", name)
		}
	}
	value, err := strconv.ParseUint(string(encoded), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s exceeds uint64", name)
	}
	return value, nil
}

func diagnosticBytes(err error) []byte {
	if err == nil {
		return nil
	}
	data := []byte(err.Error())
	if len(data) > maxProvidedAssetDiagnosticSize {
		data = data[:maxProvidedAssetDiagnosticSize]
	}
	return data
}

func providedAssetResponse(requestID, status uint32, output []byte) (guest.Frame, error) {
	if len(output) > maxProvidedAssetDiagnosticSize && status != 0 {
		output = output[:maxProvidedAssetDiagnosticSize]
	}
	return guest.CreateHostServiceResponse(requestID, status, output)
}

// assetMetadataFromSnapshot returns a fresh sorted metadata list.
func assetMetadataFromSnapshot(snapshot *ProvidedAssets) []ProvidedAssetMetadata {
	if snapshot == nil {
		return []ProvidedAssetMetadata{}
	}
	return snapshot.Metadata()
}
