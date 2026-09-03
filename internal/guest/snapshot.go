package guest

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	maxSnapshotFiles   = 128
	maxSourcePathBytes = 255
	maxSnapshotContent = 64 * 1024
)

type SnapshotFile struct {
	Path    string
	Content []byte
}

// SourceSnapshot is one validated, immutable source snapshot. It retains the
// exact framing supplied by the guest while exposing independently copied
// files and the identity used by review and build evidence.
type SourceSnapshot struct {
	encoded []byte
	files   []SnapshotFile
	sha256  string
}

type SourceSnapshotError struct {
	Reason string
}

func (e *SourceSnapshotError) Error() string { return e.Reason }

// NewSourceSnapshot validates and frames files in their supplied order.
func NewSourceSnapshot(files []SnapshotFile) (SourceSnapshot, error) {
	if err := validateSnapshotFiles(files); err != nil {
		return SourceSnapshot{}, err
	}
	files = cloneSnapshotFiles(files)
	encoded := encodeSnapshotFiles(files)
	return sourceSnapshot(encoded, files), nil
}

// NewCanonicalSourceSnapshot orders selected source paths before framing.
func NewCanonicalSourceSnapshot(files []SnapshotFile) (SourceSnapshot, error) {
	files = cloneSnapshotFiles(files)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return NewSourceSnapshot(files)
}

// ParseSourceSnapshot validates framing while retaining its exact bytes and
// file order. Incoming build snapshots are not silently rewritten.
func ParseSourceSnapshot(data []byte) (SourceSnapshot, error) {
	files, err := decodeSnapshotFiles(data)
	if err != nil {
		return SourceSnapshot{}, err
	}
	return sourceSnapshot(append([]byte(nil), data...), files), nil
}

func (s SourceSnapshot) Bytes() []byte { return append([]byte(nil), s.encoded...) }

func (s SourceSnapshot) Files() []SnapshotFile { return cloneSnapshotFiles(s.files) }

func (s SourceSnapshot) SHA256() string { return s.sha256 }

func (s SourceSnapshot) Size() uint64 { return uint64(len(s.encoded)) }

func EncodeSourceSnapshot(files []SnapshotFile) ([]byte, error) {
	snapshot, err := NewSourceSnapshot(files)
	if err != nil {
		return nil, err
	}
	return snapshot.Bytes(), nil
}

func encodeSnapshotFiles(files []SnapshotFile) []byte {
	size := 2
	for _, file := range files {
		size += 2 + len(file.Path) + 4 + len(file.Content)
	}
	encoded := make([]byte, size)
	binary.LittleEndian.PutUint16(encoded[:2], uint16(len(files)))
	offset := 2
	for _, file := range files {
		binary.LittleEndian.PutUint16(encoded[offset:offset+2], uint16(len(file.Path)))
		offset += 2
		copy(encoded[offset:], file.Path)
		offset += len(file.Path)
		binary.LittleEndian.PutUint32(encoded[offset:offset+4], uint32(len(file.Content)))
		offset += 4
		copy(encoded[offset:], file.Content)
		offset += len(file.Content)
	}
	return encoded
}

func DecodeSourceSnapshot(data []byte) ([]SnapshotFile, error) {
	snapshot, err := ParseSourceSnapshot(data)
	if err != nil {
		return nil, err
	}
	return snapshot.Files(), nil
}

func decodeSnapshotFiles(data []byte) ([]SnapshotFile, error) {
	offset := 0
	take := func(length int) ([]byte, error) {
		if length < 0 || length > len(data)-offset {
			return nil, &SourceSnapshotError{Reason: "truncated source snapshot"}
		}
		value := data[offset : offset+length]
		offset += length
		return value, nil
	}

	encodedCount, err := take(2)
	if err != nil {
		return nil, err
	}
	fileCount := int(binary.LittleEndian.Uint16(encodedCount))
	if fileCount > maxSnapshotFiles {
		return nil, &SourceSnapshotError{Reason: "source snapshot contains more than 128 files"}
	}

	files := make([]SnapshotFile, 0, fileCount)
	totalContent := 0
	for range fileCount {
		encodedPathLength, err := take(2)
		if err != nil {
			return nil, err
		}
		pathLength := int(binary.LittleEndian.Uint16(encodedPathLength))
		if pathLength == 0 || pathLength > maxSourcePathBytes {
			return nil, &SourceSnapshotError{Reason: "source path length must be 1 through 255"}
		}
		encodedPath, err := take(pathLength)
		if err != nil {
			return nil, err
		}
		if !utf8.Valid(encodedPath) {
			return nil, &SourceSnapshotError{Reason: "source path is not valid UTF-8"}
		}

		encodedContentLength, err := take(4)
		if err != nil {
			return nil, err
		}
		contentLength := int(binary.LittleEndian.Uint32(encodedContentLength))
		if contentLength > maxSnapshotContent-totalContent {
			return nil, &SourceSnapshotError{Reason: "source snapshot content exceeds 64 KiB"}
		}
		totalContent += contentLength
		content, err := take(contentLength)
		if err != nil {
			return nil, err
		}
		files = append(files, SnapshotFile{Path: string(encodedPath), Content: append([]byte(nil), content...)})
	}
	if offset != len(data) {
		return nil, &SourceSnapshotError{Reason: "unexpected trailing source snapshot data"}
	}
	if err := validateSnapshotFiles(files); err != nil {
		return nil, err
	}
	return files, nil
}

func sourceSnapshot(encoded []byte, files []SnapshotFile) SourceSnapshot {
	digest := sha256.Sum256(encoded)
	return SourceSnapshot{encoded: encoded, files: files, sha256: hex.EncodeToString(digest[:])}
}

func cloneSnapshotFiles(files []SnapshotFile) []SnapshotFile {
	cloned := make([]SnapshotFile, len(files))
	for index, file := range files {
		cloned[index] = SnapshotFile{Path: file.Path, Content: append([]byte(nil), file.Content...)}
	}
	return cloned
}

func validateSnapshotFiles(files []SnapshotFile) error {
	if len(files) > maxSnapshotFiles {
		return &SourceSnapshotError{Reason: "source snapshot contains more than 128 files"}
	}
	paths := make(map[string]struct{}, len(files))
	totalContent := 0
	for _, file := range files {
		if err := ValidateSourcePath(file.Path); err != nil {
			return err
		}
		if _, exists := paths[file.Path]; exists {
			return &SourceSnapshotError{Reason: fmt.Sprintf("duplicate source path: %s", file.Path)}
		}
		paths[file.Path] = struct{}{}
		if len(file.Content) > maxSnapshotContent-totalContent {
			return &SourceSnapshotError{Reason: "source snapshot content exceeds 64 KiB"}
		}
		totalContent += len(file.Content)
	}
	return nil
}

func ValidateSourcePath(path string) error {
	if !utf8.ValidString(path) {
		return &SourceSnapshotError{Reason: "source path is not valid UTF-8"}
	}
	if len(path) < 1 || len(path) > maxSourcePathBytes {
		return &SourceSnapshotError{Reason: "source path length must be 1 through 255"}
	}
	return validateBuildPath(path)
}

func validateBuildPath(path string) error {
	if strings.IndexByte(path, 0) >= 0 || strings.ContainsAny(path, "\r\n") || strings.HasPrefix(path, "/") {
		return &SourceSnapshotError{Reason: fmt.Sprintf("unsafe source path: %q", path)}
	}
	components := strings.Split(path, "/")
	if len(components) < 2 || components[0] != "seed" {
		return &SourceSnapshotError{Reason: fmt.Sprintf("unsafe source path: %q", path)}
	}
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return &SourceSnapshotError{Reason: fmt.Sprintf("unsafe source path: %q", path)}
		}
	}
	return nil
}
