// Package bootstrap owns the concrete, optional Linux bootstrap capability.
// Guest commands are data until executed by the isolated Podman workload.
package bootstrap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	"codexos/internal/guest"
	"codexos/internal/sourcecapacity"
)

const (
	Image              = "docker.io/library/gcc@sha256:a689e29bc3adf4663ef9a141d23081252764d1319c63f591a027bd6fd676f4c1"
	ImageID            = "f3d916a4884034b89cb6148781f07d8c92d94b6c6dc1b74dcbec3475d16400da"
	TCCCommit          = "0fb54300b56512754221d80adda85ddb9815bceb"
	TCCSHA256          = "e696d12b9429faf08a08aeeaffe96769370e5ea50cf98218f45c74956b3b3f18"
	Account            = "codexos-bootstrap"
	WorkerExecutable   = "/usr/local/libexec/codexos-bootstrap"
	StorageDirectory   = "/var/lib/codexos-bootstrap-artifacts"
	ConfigFilename     = "bootstrap-service.json"
	ReferencesFilename = "bootstrap-artifacts.json"
	MaxRequest         = 16 << 10
	MaxDiagnostics     = 64 << 10
	MaxInputs          = 64 << 20
	MaxOutput          = 16 << 20
	MaxOutputs         = 32 << 20
	MaxRead            = 1 << 20
	MaxRunBytes        = 128 << 20
	MaxGlobalBytes     = 512 << 20
	MaxManifest        = 512 << 10
)

// Limits are recorded verbatim in provenance. Version 1 is deliberately fixed.
type Limits struct {
	CPU         int `json:"cpu"`
	Memory      int `json:"memory_bytes"`
	PIDs        int `json:"pids"`
	Seconds     int `json:"seconds"`
	Scratch     int `json:"scratch_bytes"`
	Tmp         int `json:"tmp_bytes"`
	Diagnostics int `json:"diagnostic_bytes"`
	OutputFile  int `json:"output_file_bytes"`
	OutputTotal int `json:"output_total_bytes"`
	RunBytes    int `json:"run_bytes"`
	GlobalBytes int `json:"global_bytes"`
}

func Baseline() Limits {
	return Limits{1, 512 << 20, 64, 180, 256 << 20, 16 << 20, MaxDiagnostics, MaxOutput, MaxOutputs, MaxRunBytes, MaxGlobalBytes}
}

type AssetRef struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
}
type Request struct {
	Version   int        `json:"version"`
	Argv      []string   `json:"argv"`
	Assets    []AssetRef `json:"assets"`
	Artifacts []string   `json:"artifacts"`
	Outputs   []string   `json:"outputs"`
}
type Input struct {
	Path string `json:"path"`
	Data []byte `json:"data"`
}
type Artifact struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}
type Result struct {
	Status      uint32     `json:"status"`
	Reason      string     `json:"reason"`
	Diagnostics string     `json:"diagnostics"`
	ExitCode    int        `json:"exit_code"`
	OOM         bool       `json:"oom"`
	Cleaned     bool       `json:"cleaned"`
	Artifacts   []Artifact `json:"artifacts"`
}

func Digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func validID(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}
func safePath(s string) bool {
	if s == "" || len(s) > 255 || !utf8.ValidString(s) || strings.ContainsAny(s, "\x00\r\n\\") || path.IsAbs(s) {
		return false
	}
	for _, c := range strings.Split(s, "/") {
		if c == "" || c == "." || c == ".." {
			return false
		}
	}
	return true
}
func safeAssetID(s string) bool { return safePath(s) && !strings.Contains(s, "/") }

// strictJSON checks duplicate keys at every depth before schema decoding. The
// token pass also rejects trailing values, invalid UTF-8 and excessive nesting.
func strictJSON(data []byte, dst any, limit int) error {
	if len(data) > limit || !utf8.Valid(data) {
		return errors.New("JSON exceeds byte bound or is not UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	var value func(int) error
	value = func(depth int) error {
		if depth > 16 {
			return errors.New("JSON nesting limit")
		}
		t, e := d.Token()
		if e != nil {
			return e
		}
		if c, ok := t.(json.Delim); ok {
			switch c {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					k, e := d.Token()
					if e != nil {
						return e
					}
					s, ok := k.(string)
					if !ok || seen[s] {
						return errors.New("duplicate JSON field")
					}
					seen[s] = true
					if e = value(depth + 1); e != nil {
						return e
					}
				}
			case '[':
				for d.More() {
					if e = value(depth + 1); e != nil {
						return e
					}
				}
			default:
				return errors.New("unexpected JSON delimiter")
			}
			_, e = d.Token()
			return e
		}
		return nil
	}
	if e := value(0); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return errors.New("trailing JSON data")
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(dst)
}
func ParseRequest(data, snapshot []byte, budget sourcecapacity.Budget) (Request, guest.SourceSnapshot, error) {
	var r Request
	if e := strictJSON(data, &r, MaxRequest); e != nil {
		return r, guest.SourceSnapshot{}, e
	}
	s, e := guest.ParseSourceSnapshotWithBudget(snapshot, budget)
	if e != nil {
		return r, s, e
	}
	if e = r.Validate(); e != nil {
		return r, s, e
	}
	return r, s, nil
}
func (r Request) Validate() error {
	if r.Version != 1 || len(r.Argv) == 0 || len(r.Argv) > 32 || r.Argv[0] == "" {
		return errors.New("invalid job version or argv")
	}
	total := 0
	for _, a := range r.Argv {
		total += len(a)
		if len(a) > 1024 || !utf8.ValidString(a) || strings.ContainsRune(a, 0) {
			return errors.New("invalid job argument")
		}
	}
	if total > 8192 {
		return errors.New("job arguments exceed 8 KiB")
	}
	if len(r.Assets)+len(r.Artifacts) > 8 || len(r.Outputs) > 32 {
		return errors.New("job input/output count limit")
	}
	seen := map[string]bool{}
	for _, a := range r.Assets {
		if !safeAssetID(a.ID) || !validID(a.SHA256) || seen["a"+a.ID] {
			return errors.New("invalid/duplicate asset reference")
		}
		seen["a"+a.ID] = true
	}
	for _, a := range r.Artifacts {
		if !validID(a) || seen["b"+a] {
			return errors.New("invalid/duplicate artifact reference")
		}
		seen["b"+a] = true
	}
	for _, p := range r.Outputs {
		if !safePath(p) || seen["o"+p] {
			return fmt.Errorf("unsafe/duplicate output path %q", p)
		}
		seen["o"+p] = true
	}
	return nil
}

func mustJSON(v any) []byte {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return b
}
