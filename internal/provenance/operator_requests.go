package provenance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Each file is a separate immutable attempt or dispatch receipt. A snapshot
// alone proves preparation; a receipt binds it to a successfully started turn.
func WriteOperatorRequestPresentation(run string, value any) (string, string, error) {
	directory := filepath.Join(run, "operator-request-context")
	if err := os.Mkdir(directory, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", "", err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return "", "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", "", errors.New("unsafe operator request presentation directory")
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", "", err
	}
	file, err := os.CreateTemp(directory, "presentation-*.json")
	if err != nil {
		return "", "", err
	}
	success := false
	defer func() {
		_ = file.Close()
		if !success {
			_ = os.Remove(file.Name())
		}
	}()
	if err = writeAll(file, encoded); err != nil {
		return "", "", err
	}
	if err = file.Chmod(0444); err != nil {
		return "", "", err
	}
	if err = file.Sync(); err != nil {
		return "", "", err
	}
	if err = file.Close(); err != nil {
		return "", "", err
	}
	if err = syncDirectory(directory); err != nil {
		return "", "", err
	}
	if err = syncDirectory(run); err != nil {
		return "", "", err
	}
	hash := sha256.Sum256(encoded)
	success = true
	return filepath.Join("operator-request-context", filepath.Base(file.Name())), hex.EncodeToString(hash[:]), nil
}
