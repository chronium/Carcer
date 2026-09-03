package guest

import (
	"encoding/binary"
	"fmt"
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

type SourceSnapshotError struct {
	Reason string
}

func (e *SourceSnapshotError) Error() string { return e.Reason }

func EncodeSourceSnapshot(files []SnapshotFile) ([]byte, error) {
	if err := validateSnapshotFiles(files); err != nil {
		return nil, err
	}

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
	return encoded, nil
}

func DecodeSourceSnapshot(data []byte) ([]SnapshotFile, error) {
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

func validateSnapshotFiles(files []SnapshotFile) error {
	if len(files) > maxSnapshotFiles {
		return &SourceSnapshotError{Reason: "source snapshot contains more than 128 files"}
	}
	paths := make(map[string]struct{}, len(files))
	totalContent := 0
	for _, file := range files {
		if err := validateSourcePath(file.Path); err != nil {
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

func validateSourcePath(path string) error {
	if !utf8.ValidString(path) {
		return &SourceSnapshotError{Reason: "source path is not valid UTF-8"}
	}
	if len(path) < 1 || len(path) > maxSourcePathBytes {
		return &SourceSnapshotError{Reason: "source path length must be 1 through 255"}
	}
	return validateBuildPath(path)
}

func validateBuildPath(path string) error {
	if strings.IndexByte(path, 0) >= 0 || strings.HasPrefix(path, "/") {
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
