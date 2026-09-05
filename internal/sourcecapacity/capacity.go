// Package sourcecapacity stores the Go harness's per-run source-content budget.
// It does not grant guest capabilities or change the source-snapshot wire format.
package sourcecapacity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const (
	Default  = 64 * 1024
	Expanded = 1024 * 1024
	Filename = "source-capacity.json"
	// v1: count plus at most 128 (path length, 255-byte path, content length) frames.
	FramingOverhead = 2 + 128*(2+255+4)
)

type Budget int

// Zero means the legacy/default budget, never unlimited content.
func (b Budget) Bytes() int {
	if b == 0 {
		return Default
	}
	return int(b)
}
func (b Budget) SnapshotLimit() int64 { return int64(b.Bytes()) + FramingOverhead }
func (b Budget) Validate() error {
	if b.Bytes() != Default && b.Bytes() != Expanded {
		return fmt.Errorf("source content capacity must be %d or %d bytes", Default, Expanded)
	}
	return nil
}
func (b Budget) Exceeded() error {
	return fmt.Errorf("source snapshot content exceeds %d KiB (%d bytes)", b.Bytes()/1024, b.Bytes())
}

type record struct {
	SchemaVersion int `json:"schema_version"`
	ContentBytes  int `json:"content_bytes"`
}

func Load(directory string) (Budget, error) {
	path := filepath.Join(directory, Filename)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	data, err := ReadFile(path, 1024)
	if err != nil {
		return 0, err
	}
	var value record
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return 0, fmt.Errorf("invalid source capacity: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return 0, errors.New("invalid source capacity trailing data")
	}
	budget := Budget(value.ContentBytes)
	if value.SchemaVersion != 1 || value.ContentBytes == 0 {
		return 0, errors.New("invalid source capacity record")
	}
	return budget, budget.Validate()
}

// Save replaces a setting atomically. The caller must own the inactive gate (or
// an unpublished archive staging directory) and validate the proposed budget.
func Save(directory string, budget Budget) error {
	if err := budget.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record{1, budget.Bytes()}, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".source-capacity-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(append(data, '\n')); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(file.Name(), filepath.Join(directory, Filename)); err != nil {
		return err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// ReadFile bounds allocation, including serialized framing, and refuses symlinks.
func ReadFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("source artifact is not a regular file: %s", path)
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("source artifact exceeds %d serialized bytes: %s", limit, path)
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, fmt.Errorf("source artifact changed while opening: %s", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("source artifact exceeds %d serialized bytes: %s", limit, path)
	}
	return data, nil
}
