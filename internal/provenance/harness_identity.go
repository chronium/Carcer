package provenance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	HarnessIdentitySchemaVersion = uint64(1)
	RunHarnessIdentityFilename   = "run-harness-identity.json"
	GenerationHarnessFilename    = "harness-identity.json"

	harnessIdentityLimit = 256 * 1024
	harnessGitTimeout    = 30 * time.Second
)

// HarnessIdentity is the immutable identity of the exact Go harness process
// and repository state admitted to a run or generation.
type HarnessIdentity struct {
	Build            HarnessBuildIdentity `json:"build"`
	Executable       FileIdentity         `json:"executable"`
	RepositoryCommit string               `json:"repository_commit"`
	RepositoryDirty  bool                 `json:"repository_dirty"`
	DirtyTreeSHA256  *string              `json:"dirty_tree_sha256"`
	SchemaVersion    uint64               `json:"schema_version"`
}

// HarnessBuildIdentity records the Go module/version information embedded in
// the executing binary. SettingsSHA256 binds all build settings without
// exposing host-local build paths in every provenance record.
type HarnessBuildIdentity struct {
	GoVersion      string `json:"go_version"`
	ModulePath     string `json:"module_path"`
	ModuleSum      string `json:"module_sum"`
	ModuleVersion  string `json:"module_version"`
	SettingsSHA256 string `json:"settings_sha256"`
	VCS            string `json:"vcs"`
	VCSModified    bool   `json:"vcs_modified"`
	VCSRevision    string `json:"vcs_revision"`
	VCSTime        string `json:"vcs_time"`
}

type HarnessIdentityError struct {
	Reason string
	Err    error
}

func (e *HarnessIdentityError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *HarnessIdentityError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// CurrentHarnessBuildIdentity reads the build/version identity embedded by
// the Go toolchain in this process.
func CurrentHarnessBuildIdentity() (HarnessBuildIdentity, error) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return HarnessBuildIdentity{}, &HarnessIdentityError{Reason: "executing harness has no Go build identity"}
	}
	settings := append([]debug.BuildSetting(nil), info.Settings...)
	sort.Slice(settings, func(i, j int) bool {
		if settings[i].Key == settings[j].Key {
			return settings[i].Value < settings[j].Value
		}
		return settings[i].Key < settings[j].Key
	})
	digest := sha256.New()
	values := make(map[string]string, len(settings))
	for _, setting := range settings {
		values[setting.Key] = setting.Value
		writeHarnessField(digest, []byte(setting.Key))
		writeHarnessField(digest, []byte(setting.Value))
	}
	return HarnessBuildIdentity{
		GoVersion: info.GoVersion, ModulePath: info.Main.Path, ModuleVersion: info.Main.Version,
		ModuleSum: info.Main.Sum, SettingsSHA256: hex.EncodeToString(digest.Sum(nil)),
		VCS: values["vcs"], VCSRevision: values["vcs.revision"], VCSTime: values["vcs.time"],
		VCSModified: values["vcs.modified"] == "true",
	}, nil
}

// CaptureCurrentHarnessIdentity captures the current worktree, executing
// binary, and embedded build information exactly once at process startup.
func CaptureCurrentHarnessIdentity() (HarnessIdentity, error) {
	executable, err := os.Executable()
	if err != nil {
		return HarnessIdentity{}, &HarnessIdentityError{Reason: "could not locate executing harness binary", Err: err}
	}
	executablePath := executable
	if resolved, resolveErr := filepath.EvalSymlinks(executablePath); resolveErr == nil {
		executablePath = resolved
	}
	repository := filepath.Dir(executablePath)
	if _, repositoryErr := harnessGitText(repository, "rev-parse", "--show-toplevel"); repositoryErr != nil {
		repository, err = os.Getwd()
		if err != nil {
			return HarnessIdentity{}, &HarnessIdentityError{Reason: "could not locate harness repository", Err: err}
		}
	}
	if runtime.GOOS == "linux" {
		// /proc/self/exe remains bound to the executing inode even if the path
		// used to launch the process is atomically replaced.
		executablePath = "/proc/self/exe"
	}
	build, err := CurrentHarnessBuildIdentity()
	if err != nil {
		return HarnessIdentity{}, err
	}
	return CaptureHarnessIdentity(repository, executablePath, build)
}

// CaptureHarnessIdentity is the deterministic capture boundary used by the
// process runner and focused provenance tests.
func CaptureHarnessIdentity(repository, executable string, build HarnessBuildIdentity) (HarnessIdentity, error) {
	root, err := harnessGitText(repository, "rev-parse", "--show-toplevel")
	if err != nil {
		return HarnessIdentity{}, &HarnessIdentityError{Reason: "could not locate harness repository", Err: err}
	}
	root = strings.TrimSpace(root)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return HarnessIdentity{}, &HarnessIdentityError{Reason: "could not resolve harness repository", Err: err}
	}
	commit, err := harnessGitText(resolvedRoot, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return HarnessIdentity{}, &HarnessIdentityError{Reason: "could not identify harness repository commit", Err: err}
	}
	commit = strings.TrimSpace(commit)
	status, err := harnessGitBytes(resolvedRoot, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return HarnessIdentity{}, &HarnessIdentityError{Reason: "could not inspect harness repository state", Err: err}
	}
	identity := HarnessIdentity{
		Build: build, RepositoryCommit: commit, RepositoryDirty: len(status) != 0,
		SchemaVersion: HarnessIdentitySchemaVersion,
	}
	identity.Executable, err = FileIdentityFromPath(executable)
	if err != nil {
		return HarnessIdentity{}, &HarnessIdentityError{Reason: "could not identify executing harness binary", Err: err}
	}
	if identity.RepositoryDirty {
		digest, digestErr := harnessDirtyTreeIdentity(resolvedRoot)
		if digestErr != nil {
			return HarnessIdentity{}, &HarnessIdentityError{Reason: "could not identify dirty harness repository state", Err: digestErr}
		}
		identity.DirtyTreeSHA256 = &digest
	}
	if err := ValidateHarnessIdentity(identity); err != nil {
		return HarnessIdentity{}, err
	}
	return identity, nil
}

func ValidateHarnessIdentity(identity HarnessIdentity) error {
	if identity.SchemaVersion != HarnessIdentitySchemaVersion || !validHarnessDigest(identity.RepositoryCommit, true) ||
		!validHarnessFileIdentity(identity.Executable) || !validHarnessBuildIdentity(identity.Build) {
		return &HarnessIdentityError{Reason: "harness identity is malformed"}
	}
	if identity.RepositoryDirty {
		if identity.DirtyTreeSHA256 == nil || !validHarnessDigest(*identity.DirtyTreeSHA256, false) {
			return &HarnessIdentityError{Reason: "dirty harness identity is incomplete"}
		}
	} else if identity.DirtyTreeSHA256 != nil {
		return &HarnessIdentityError{Reason: "clean harness identity contains a dirty-tree identity"}
	}
	return nil
}

func (identity HarnessIdentity) Equal(other HarnessIdentity) bool {
	left, leftErr := EncodeHarnessIdentity(identity)
	right, rightErr := EncodeHarnessIdentity(other)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}

func (identity HarnessIdentity) AsJSON() map[string]any {
	value := map[string]any{
		"schema_version":    identity.SchemaVersion,
		"repository_commit": identity.RepositoryCommit,
		"repository_dirty":  identity.RepositoryDirty,
		"dirty_tree_sha256": nil,
		"executable":        identity.Executable.AsJSON(),
		"build": map[string]any{
			"go_version": identity.Build.GoVersion, "module_path": identity.Build.ModulePath,
			"module_version": identity.Build.ModuleVersion, "module_sum": identity.Build.ModuleSum,
			"settings_sha256": identity.Build.SettingsSHA256,
			"vcs":             identity.Build.VCS,
			"vcs_modified":    identity.Build.VCSModified,
			"vcs_revision":    identity.Build.VCSRevision,
			"vcs_time":        identity.Build.VCSTime,
		},
	}
	if identity.DirtyTreeSHA256 != nil {
		value["dirty_tree_sha256"] = *identity.DirtyTreeSHA256
	}
	return value
}

func EncodeHarnessIdentity(identity HarnessIdentity) ([]byte, error) {
	if err := ValidateHarnessIdentity(identity); err != nil {
		return nil, err
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(identity.AsJSON()); err != nil {
		return nil, &HarnessIdentityError{Reason: "could not encode harness identity", Err: err}
	}
	return output.Bytes(), nil
}

func ParseHarnessIdentity(encoded []byte) (HarnessIdentity, error) {
	if !utf8.Valid(encoded) || len(encoded) > harnessIdentityLimit {
		return HarnessIdentity{}, &HarnessIdentityError{Reason: "harness identity is malformed"}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil || !hasExactHarnessFields(fields,
		"build", "dirty_tree_sha256", "executable", "repository_commit", "repository_dirty", "schema_version") {
		return HarnessIdentity{}, &HarnessIdentityError{Reason: "harness identity is malformed", Err: err}
	}
	var buildFields map[string]json.RawMessage
	if err := json.Unmarshal(fields["build"], &buildFields); err != nil || !hasExactHarnessFields(buildFields,
		"go_version", "module_path", "module_sum", "module_version", "settings_sha256", "vcs", "vcs_modified", "vcs_revision", "vcs_time") {
		return HarnessIdentity{}, &HarnessIdentityError{Reason: "harness build identity is malformed", Err: err}
	}
	var executableFields map[string]json.RawMessage
	if err := json.Unmarshal(fields["executable"], &executableFields); err != nil || !hasExactHarnessFields(executableFields, "sha256", "size") {
		return HarnessIdentity{}, &HarnessIdentityError{Reason: "harness executable identity is malformed", Err: err}
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var identity HarnessIdentity
	if err := decoder.Decode(&identity); err != nil {
		return HarnessIdentity{}, &HarnessIdentityError{Reason: "harness identity is malformed", Err: err}
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return HarnessIdentity{}, &HarnessIdentityError{Reason: "harness identity is malformed"}
	}
	if err := ValidateHarnessIdentity(identity); err != nil {
		return HarnessIdentity{}, err
	}
	return identity, nil
}

func hasExactHarnessFields(fields map[string]json.RawMessage, names ...string) bool {
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

func CloneHarnessIdentity(identity *HarnessIdentity) *HarnessIdentity {
	if identity == nil {
		return nil
	}
	clone := *identity
	if identity.DirtyTreeSHA256 != nil {
		value := *identity.DirtyTreeSHA256
		clone.DirtyTreeSHA256 = &value
	}
	return &clone
}

func validHarnessBuildIdentity(build HarnessBuildIdentity) bool {
	for _, value := range []string{build.GoVersion, build.ModulePath, build.ModuleVersion, build.SettingsSHA256} {
		if value == "" || !utf8.ValidString(value) {
			return false
		}
	}
	for _, value := range []string{build.ModuleSum, build.VCS, build.VCSRevision, build.VCSTime} {
		if !utf8.ValidString(value) {
			return false
		}
	}
	return validHarnessDigest(build.SettingsSHA256, false)
}

func validHarnessFileIdentity(identity FileIdentity) bool {
	return validHarnessDigest(identity.SHA256, false)
}

func validHarnessDigest(value string, git bool) bool {
	if (!git && len(value) != 64) || (git && len(value) != 40 && len(value) != 64) {
		return false
	}
	for _, character := range value {
		if character < '0' || (character > '9' && character < 'a') || character > 'f' {
			return false
		}
	}
	return true
}

func harnessDirtyTreeIdentity(repository string) (string, error) {
	diff, err := harnessGitBytes(repository, "diff", "--binary", "--full-index", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", "--ignore-submodules=none", "HEAD", "--")
	if err != nil {
		return "", err
	}
	untracked, err := harnessGitBytes(repository, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	paths := bytes.Split(bytes.TrimSuffix(untracked, []byte{0}), []byte{0})
	if len(untracked) == 0 {
		paths = nil
	}
	sort.Slice(paths, func(i, j int) bool { return bytes.Compare(paths[i], paths[j]) < 0 })
	digest := sha256.New()
	writeHarnessField(digest, []byte("codexos-dirty-tree-v1"))
	writeHarnessField(digest, diff)
	for _, pathBytes := range paths {
		path := string(pathBytes)
		info, statErr := os.Lstat(filepath.Join(repository, path))
		if statErr != nil {
			return "", statErr
		}
		writeHarnessField(digest, pathBytes)
		var kind byte
		mode := "100644"
		var contents []byte
		switch {
		case info.Mode().IsRegular():
			kind = 'f'
			if info.Mode().Perm()&0o111 != 0 {
				mode = "100755"
			}
			contents, err = os.ReadFile(filepath.Join(repository, path))
		case info.Mode()&os.ModeSymlink != 0:
			kind = 'l'
			mode = "120000"
			var target string
			target, err = os.Readlink(filepath.Join(repository, path))
			contents = []byte(target)
		default:
			return "", fmt.Errorf("untracked path has unsupported type: %s", path)
		}
		if err != nil {
			return "", err
		}
		_, _ = digest.Write([]byte{kind})
		writeHarnessField(digest, []byte(mode))
		writeHarnessField(digest, contents)
	}
	staged, err := harnessGitBytes(repository, "ls-files", "--stage", "-z")
	if err != nil {
		return "", err
	}
	for _, record := range bytes.Split(bytes.TrimSuffix(staged, []byte{0}), []byte{0}) {
		if !bytes.HasPrefix(record, []byte("160000 ")) {
			continue
		}
		tab := bytes.IndexByte(record, '\t')
		if tab < 0 || tab+1 == len(record) {
			return "", errors.New("malformed Git submodule index entry")
		}
		pathBytes := record[tab+1:]
		submodule := filepath.Join(repository, string(pathBytes))
		commit, commitErr := harnessGitText(submodule, "rev-parse", "--verify", "HEAD^{commit}")
		if commitErr != nil {
			return "", fmt.Errorf("could not identify submodule %s: %w", pathBytes, commitErr)
		}
		status, statusErr := harnessGitBytes(submodule, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
		if statusErr != nil {
			return "", statusErr
		}
		writeHarnessField(digest, []byte("submodule"))
		writeHarnessField(digest, pathBytes)
		writeHarnessField(digest, []byte(strings.TrimSpace(commit)))
		if len(status) == 0 {
			writeHarnessField(digest, nil)
			continue
		}
		nested, nestedErr := harnessDirtyTreeIdentity(submodule)
		if nestedErr != nil {
			return "", nestedErr
		}
		writeHarnessField(digest, []byte(nested))
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func writeHarnessField(output io.Writer, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = output.Write(size[:])
	_, _ = output.Write(value)
}

func harnessGitText(repository string, arguments ...string) (string, error) {
	output, err := harnessGitBytes(repository, arguments...)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(output) {
		return "", errors.New("Git output is not valid UTF-8")
	}
	return string(output), nil
}

func harnessGitBytes(repository string, arguments ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), harnessGitTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "git", append([]string{"-C", repository}, arguments...)...)
	output, err := command.Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			diagnostics := strings.TrimSpace(string(exitErr.Stderr))
			if diagnostics != "" {
				return nil, errors.New(diagnostics)
			}
		}
		return nil, err
	}
	return output, nil
}
