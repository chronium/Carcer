package provenance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"codexos/internal/guest"
	"codexos/internal/qemu"
)

const (
	generationGitTimeout        = 30 * time.Second
	maxGenerationGitDiagnostics = 16 * 1024
	maxGenerationGitOutput      = 32 * 1024 * 1024
	abortMarker                 = "Generation aborted by operator."
)

// GenerationGitRecorderError reports a local Git provenance failure. Git
// provenance is derived from immutable archives, so callers should surface
// this error rather than attempting to repair a conflicting ref.
type GenerationGitRecorderError struct {
	Reason string
	Err    error
}

func (e *GenerationGitRecorderError) Error() string {
	if e.Reason == "" {
		if e.Err == nil {
			return "generation Git provenance failed"
		}
		return e.Err.Error()
	}
	if e.Err == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *GenerationGitRecorderError) Unwrap() error { return e.Err }

// GenerationGitRecord identifies one immutable annotated generation tag.
type GenerationGitRecord struct {
	Generation      uint64
	Tag             string
	Commit          string
	AlreadyRecorded bool
}

// GenerationGitRecorder derives immutable local Git objects from one run's
// completed generation archives. The configured base ref is resolved once at
// construction and its resulting commit is used for all later reconciliation.
type GenerationGitRecorder struct {
	gitExecutable string
	repository    string
	runDirectory  string
	runIdentifier string
	baseCommit    string
}

// NewGenerationGitRecorder validates the repository/run namespace and selects
// the immutable commit named by baseRef.
func NewGenerationGitRecorder(repository, runDirectory, baseRef string) (*GenerationGitRecorder, error) {
	gitExecutable, err := exec.LookPath("git")
	if err != nil {
		return nil, generationGitError("git executable is unavailable", err)
	}
	if baseRef == "" {
		return nil, &GenerationGitRecorderError{Reason: "Git base ref must not be empty"}
	}
	repositoryPath, err := resolveGenerationGitPath(repository)
	if err != nil {
		return nil, generationGitError("Git repository is unavailable", err)
	}
	runPath, err := resolveGenerationGitPath(runDirectory)
	if err != nil {
		return nil, generationGitError("run directory is unavailable", err)
	}
	runInfo, err := os.Stat(runPath)
	if err != nil || !runInfo.IsDir() {
		if err == nil {
			err = errors.New("path is not a directory")
		}
		return nil, generationGitError("run directory is unavailable: "+runPath, err)
	}

	recorder := &GenerationGitRecorder{
		gitExecutable: gitExecutable,
		repository:    repositoryPath,
		runDirectory:  runPath,
		runIdentifier: filepath.Base(runPath),
	}
	topLevel, err := recorder.gitText(recorder.repository, []string{"rev-parse", "--show-toplevel"})
	if err != nil {
		return nil, err
	}
	topLevelPath, err := resolveGenerationGitPath(strings.TrimSpace(topLevel))
	if err != nil || topLevelPath != recorder.repository {
		if err == nil {
			err = errors.New("path is not the configured repository root")
		}
		return nil, generationGitError("Git repository path must be its worktree root", err)
	}
	if recorder.runIdentifier == "experiment" {
		return nil, &GenerationGitRecorderError{
			Reason: "run identifier 'experiment' is reserved for legacy generation tags",
		}
	}
	for _, ref := range []string{
		"refs/tags/" + generationTag(recorder.runIdentifier, 0),
		"refs/heads/" + lineageBranch(recorder.runIdentifier, 0),
	} {
		result, err := recorder.git(recorder.repository, []string{"check-ref-format", ref}, false)
		if err != nil {
			return nil, err
		}
		if result.exitCode != 0 {
			return nil, &GenerationGitRecorderError{
				Reason: "run-directory basename cannot form a Git tag namespace or lineage branch namespace: " + recorder.runIdentifier,
			}
		}
	}
	recorder.baseCommit, err = recorder.resolveCommit(baseRef, recorder.repository)
	if err != nil {
		return nil, err
	}
	return recorder, nil
}

// BaseCommit returns the commit selected by the configured base ref when the
// recorder was constructed.
func (r *GenerationGitRecorder) BaseCommit() string { return r.baseCommit }

// GenerationTagCommit resolves one required immutable annotated generation
// tag and returns the commit it directly annotates.
func (r *GenerationGitRecorder) GenerationTagCommit(tag string) (string, error) {
	commit, err := r.resolveOptionalTag(tag)
	if err != nil {
		return "", err
	}
	if commit == "" {
		return "", &GenerationGitRecorderError{
			Reason: "required completed generation tag is missing: " + tag,
		}
	}
	objectType, err := r.gitText(r.repository, []string{"cat-file", "-t", "refs/tags/" + tag})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(objectType) != "tag" {
		return "", &GenerationGitRecorderError{Reason: "generation tag is not annotated: " + tag}
	}
	return commit, nil
}

// Reconcile reads all archived generations, creates missing immutable tags,
// and reconciles only the run-scoped lineage branches. Existing conflicting
// objects are never rewritten.
func (r *GenerationGitRecorder) Reconcile() ([]GenerationGitRecord, error) {
	archives, err := r.archivedGenerations()
	if err != nil {
		return nil, asGenerationGitError(err)
	}
	if err := validateArchivedHistory(archives); err != nil {
		return nil, asGenerationGitError(err)
	}
	records := make([]GenerationGitRecord, 0)
	for _, archive := range archives {
		if archive.outcome == "completed" {
			record, err := r.record(archive)
			if err != nil {
				return nil, asGenerationGitError(err)
			}
			records = append(records, record)
			continue
		}
		tag := generationTag(r.runIdentifier, archive.generation)
		commit, err := r.resolveOptionalTag(tag)
		if err != nil {
			return nil, err
		}
		if commit != "" {
			return nil, &GenerationGitRecorderError{
				Reason: "aborted generation has a conflicting Git tag: " + tag,
			}
		}
	}
	if err := r.reconcileLineages(archives, records); err != nil {
		return nil, err
	}
	return records, nil
}

type generationArchive struct {
	generation  uint64
	parent      *uint64
	transition  string
	outcome     string
	archivePath string
	handoff     string
}

func (r *GenerationGitRecorder) archivedGenerations() ([]generationArchive, error) {
	entries, err := os.ReadDir(r.runDirectory)
	if err != nil {
		return nil, err
	}
	archives := make([]generationArchive, 0)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "generation-") {
			continue
		}
		suffix := strings.TrimPrefix(name, "generation-")
		generation, ok := parseASCIIUnsigned(suffix)
		if !ok || name != generationName(generation) {
			return nil, fmt.Errorf("invalid generation archive: %s", name)
		}
		archive, err := r.readArchivedGeneration(generation)
		if err != nil {
			return nil, fmt.Errorf("generation %d archive is invalid: %w", generation, err)
		}
		archives = append(archives, archive)
	}
	sort.Slice(archives, func(i, j int) bool { return archives[i].generation < archives[j].generation })
	return archives, nil
}

func validateArchivedHistory(archives []generationArchive) error {
	byGeneration := make(map[uint64]generationArchive, len(archives))
	for _, archive := range archives {
		if _, exists := byGeneration[archive.generation]; exists {
			return errors.New("generation archive history is not contiguous")
		}
		byGeneration[archive.generation] = archive
	}
	for index, archive := range archives {
		if archive.generation != uint64(index) {
			return errors.New("generation archive history is not contiguous")
		}
	}
	for index := 1; index < len(archives); index++ {
		archive := archives[index]
		if archive.parent == nil {
			return fmt.Errorf("generation %d has no completed parent", archive.generation)
		}
		parent, exists := byGeneration[*archive.parent]
		if !exists || parent.outcome != "completed" {
			return fmt.Errorf("generation %d has no completed parent", archive.generation)
		}
		if archive.transition == "successor" && *archive.parent != archive.generation-1 {
			return fmt.Errorf("generation %d has invalid successor ancestry", archive.generation)
		}
		if archive.transition == "rollback" && *archive.parent == archive.generation-1 {
			return fmt.Errorf("generation %d has invalid rollback ancestry", archive.generation)
		}
	}
	return nil
}

func (r *GenerationGitRecorder) readArchivedGeneration(generation uint64) (generationArchive, error) {
	archivePath := filepath.Join(r.runDirectory, generationName(generation))
	if !isDirectoryWithoutSymlink(archivePath) {
		return generationArchive{}, fmt.Errorf("generation archive is missing: %s", archivePath)
	}
	metadataPath := filepath.Join(archivePath, "metadata.json")
	if !isRegularWithoutSymlink(metadataPath) {
		return generationArchive{}, fmt.Errorf("generation archive artifact is missing: %s", metadataPath)
	}
	metadataBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		return generationArchive{}, err
	}
	metadata, err := parseGenerationMetadata(metadataBytes, generation)
	if err != nil {
		return generationArchive{}, err
	}

	hardwarePath := filepath.Join(archivePath, "hardware.json")
	if !isRegularWithoutSymlink(hardwarePath) {
		return generationArchive{}, fmt.Errorf("generation archive artifact is missing: %s", hardwarePath)
	}
	hardwareBytes, err := os.ReadFile(hardwarePath)
	if err != nil {
		return generationArchive{}, err
	}
	if _, err := qemu.ParseHardwareManifest(hardwareBytes); err != nil {
		return generationArchive{}, errors.New("generation hardware manifest is malformed")
	}
	hasHarnessIdentity := false
	harnessPath := filepath.Join(archivePath, GenerationHarnessFilename)
	if _, statErr := os.Lstat(harnessPath); statErr == nil {
		if !isRegularWithoutSymlink(harnessPath) {
			return generationArchive{}, errors.New("generation harness identity is malformed")
		}
		harnessBytes, readErr := os.ReadFile(harnessPath)
		if readErr != nil {
			return generationArchive{}, readErr
		}
		if _, parseErr := ParseHarnessIdentity(harnessBytes); parseErr != nil {
			return generationArchive{}, errors.New("generation harness identity is malformed")
		}
		hasHarnessIdentity = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return generationArchive{}, statErr
	}

	boot := filepath.Join(archivePath, "boot")
	if !isDirectoryWithoutSymlink(boot) {
		return generationArchive{}, fmt.Errorf("generation archive artifact is missing: %s", boot)
	}
	for _, required := range []string{
		filepath.Join(boot, "codexos.iso"),
		filepath.Join(archivePath, "qemu.stdout"),
		filepath.Join(archivePath, "qemu.stderr"),
	} {
		if !isRegularWithoutSymlink(required) {
			return generationArchive{}, fmt.Errorf("generation archive artifact is missing: %s", required)
		}
	}

	archive := generationArchive{
		generation:  generation,
		parent:      metadata.parent,
		transition:  metadata.transition,
		outcome:     metadata.outcome,
		archivePath: archivePath,
	}
	if metadata.outcome == "completed" {
		handoffPath := filepath.Join(archivePath, "handoff.txt")
		snapshotPath := filepath.Join(archivePath, "source.snapshot")
		source := filepath.Join(archivePath, "source")
		successor := filepath.Join(archivePath, "successor")
		for _, directory := range []string{source, successor} {
			if !isDirectoryWithoutSymlink(directory) {
				return generationArchive{}, fmt.Errorf("generation archive artifact is missing: %s", directory)
			}
		}
		for _, required := range []string{
			handoffPath,
			snapshotPath,
			filepath.Join(successor, "kernel.elf"),
			filepath.Join(successor, "codexos.iso"),
		} {
			if !isRegularWithoutSymlink(required) {
				return generationArchive{}, fmt.Errorf("generation archive artifact is missing: %s", required)
			}
		}
		handoffBytes, err := os.ReadFile(handoffPath)
		if err != nil {
			return generationArchive{}, err
		}
		if !utf8.Valid(handoffBytes) {
			return generationArchive{}, errors.New("generation handoff is not valid UTF-8")
		}
		snapshot, err := os.ReadFile(snapshotPath)
		if err != nil {
			return generationArchive{}, err
		}
		if _, err := guest.DecodeSourceSnapshot(snapshot); err != nil {
			return generationArchive{}, err
		}
		archive.handoff = string(handoffBytes)
		names := []string{
			"boot", "metadata.json", "hardware.json", "handoff.txt", "source.snapshot", "source", "successor", "qemu.stdout", "qemu.stderr",
		}
		if hasHarnessIdentity {
			names = append(names, GenerationHarnessFilename)
		}
		if err := validateArchiveNames(archivePath, names); err != nil {
			return generationArchive{}, err
		}
	} else {
		abortedPath := filepath.Join(archivePath, "aborted.txt")
		if !isRegularWithoutSymlink(abortedPath) {
			return generationArchive{}, fmt.Errorf("generation archive artifact is missing: %s", abortedPath)
		}
		abortedBytes, err := os.ReadFile(abortedPath)
		if err != nil {
			return generationArchive{}, err
		}
		if string(abortedBytes) != abortMarker {
			return generationArchive{}, errors.New("generation abort marker is malformed")
		}
		names := []string{"boot", "metadata.json", "hardware.json", "aborted.txt", "qemu.stdout", "qemu.stderr"}
		if hasHarnessIdentity {
			names = append(names, GenerationHarnessFilename)
		}
		manifestPath := filepath.Join(archivePath, "latest-success.json")
		snapshotPath := filepath.Join(archivePath, "latest-success.snapshot")
		manifestExists := pathExists(manifestPath)
		snapshotExists := pathExists(snapshotPath)
		if manifestExists || snapshotExists {
			if !isRegularWithoutSymlink(manifestPath) || !isRegularWithoutSymlink(snapshotPath) {
				return generationArchive{}, errors.New("aborted generation forensic evidence is incomplete")
			}
			if err := validateAbortedSuccessEvidence(manifestPath, snapshotPath, generation); err != nil {
				return generationArchive{}, err
			}
			names = append(names, "latest-success.json", "latest-success.snapshot")
		}
		if err := validateArchiveNames(archivePath, names); err != nil {
			return generationArchive{}, err
		}
	}
	return archive, nil
}

type parsedGenerationMetadata struct {
	parent     *uint64
	transition string
	outcome    string
}

func parseGenerationMetadata(encoded []byte, expected uint64) (parsedGenerationMetadata, error) {
	var fields map[string]json.RawMessage
	if err := decodeJSON(encoded, &fields); err != nil || len(fields) != 4 {
		return parsedGenerationMetadata{}, errors.New("generation archive metadata is malformed")
	}
	for _, key := range []string{"generation", "outcome", "parent_generation", "transition"} {
		if _, ok := fields[key]; !ok {
			return parsedGenerationMetadata{}, errors.New("generation archive metadata is malformed")
		}
	}
	generation, ok := parseJSONUnsigned(fields["generation"])
	if !ok || generation != expected {
		return parsedGenerationMetadata{}, errors.New("generation archive metadata has the wrong generation")
	}
	var outcome string
	if err := json.Unmarshal(fields["outcome"], &outcome); err != nil || (outcome != "completed" && outcome != "aborted") {
		return parsedGenerationMetadata{}, errors.New("generation archive metadata is malformed")
	}
	var transition string
	if err := json.Unmarshal(fields["transition"], &transition); err != nil {
		return parsedGenerationMetadata{}, errors.New("generation archive metadata is malformed")
	}
	var parent *uint64
	if bytes.Equal(bytes.TrimSpace(fields["parent_generation"]), []byte("null")) {
		parent = nil
	} else {
		value, ok := parseJSONUnsigned(fields["parent_generation"])
		if !ok {
			return parsedGenerationMetadata{}, errors.New("generation archive metadata is malformed")
		}
		parent = &value
	}
	if generation == 0 {
		if parent != nil || transition != "initial" {
			return parsedGenerationMetadata{}, errors.New("generation archive metadata is malformed")
		}
	} else {
		if parent == nil || *parent >= generation || (transition != "successor" && transition != "rollback") {
			return parsedGenerationMetadata{}, errors.New("generation archive metadata is malformed")
		}
	}
	return parsedGenerationMetadata{parent: parent, transition: transition, outcome: outcome}, nil
}

func validateArchiveNames(directory string, expected []string) error {
	want := make(map[string]struct{}, len(expected))
	for _, name := range expected {
		want[name] = struct{}{}
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	got := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		got[entry.Name()] = struct{}{}
	}
	if len(got) != len(want) {
		return fmt.Errorf("generation %s archive has invalid contents", filepath.Base(directory))
	}
	for name := range want {
		if _, ok := got[name]; !ok {
			return fmt.Errorf("generation %s archive has invalid contents", filepath.Base(directory))
		}
	}
	return nil
}

func validateAbortedSuccessEvidence(manifestPath, snapshotPath string, generation uint64) error {
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return errors.New("aborted generation forensic manifest is malformed")
	}
	var manifest map[string]any
	if err := decodeJSON(manifestBytes, &manifest); err != nil {
		return errors.New("aborted generation forensic manifest is malformed")
	}
	if value, ok := manifest["schema_version"].(float64); !ok || value != 1 {
		return errors.New("aborted generation forensic manifest is malformed")
	}
	if value, ok := manifest["generation"].(float64); !ok || value != float64(generation) {
		return errors.New("aborted generation forensic generation is incorrect")
	}
	if manifest["ready"] != true || manifest["protocol_validated"] != true {
		return errors.New("aborted generation forensic success is invalid")
	}
	attemptID, ok := manifest["build_attempt_id"].(string)
	if !ok || !strings.HasPrefix(attemptID, "build-") {
		return errors.New("aborted generation build attempt ID is invalid")
	}
	snapshot, err := os.ReadFile(snapshotPath)
	if err != nil {
		return errors.New("aborted generation source identity is invalid")
	}
	source, ok := manifest["source_snapshot"].(map[string]any)
	if !ok || len(source) != 2 || source["sha256"] != generationGitHashBytes(snapshot) || source["size"] != float64(len(snapshot)) {
		return errors.New("aborted generation source identity is invalid")
	}
	if _, err := guest.DecodeSourceSnapshot(snapshot); err != nil {
		return errors.New("aborted generation source identity is invalid")
	}
	for _, name := range []string{"kernel", "iso"} {
		identity, ok := manifest[name].(map[string]any)
		if !ok || !validSHA256(identity["sha256"]) {
			return fmt.Errorf("aborted generation %s identity is invalid", name)
		}
		if value, ok := identity["size"].(float64); !ok || value < 0 || value != float64(uint64(value)) {
			return fmt.Errorf("aborted generation %s identity is invalid", name)
		}
	}
	return nil
}

func (r *GenerationGitRecorder) record(archive generationArchive) (record GenerationGitRecord, err error) {
	snapshotPath := filepath.Join(archive.archivePath, "source.snapshot")
	if !isRegularWithoutSymlink(snapshotPath) {
		return GenerationGitRecord{}, &GenerationGitRecorderError{
			Reason: fmt.Sprintf("generation %d source snapshot is unavailable", archive.generation),
		}
	}
	snapshot, err := os.ReadFile(snapshotPath)
	if err != nil {
		return GenerationGitRecord{}, generationGitError(fmt.Sprintf("generation %d source snapshot is unavailable", archive.generation), err)
	}
	entries, err := guest.DecodeSourceSnapshot(snapshot)
	if err != nil {
		return GenerationGitRecord{}, asGenerationGitError(err)
	}
	parent, err := r.parentCommit(archive)
	if err != nil {
		return GenerationGitRecord{}, err
	}
	tag := generationTag(r.runIdentifier, archive.generation)
	message := commitMessage(archive, snapshot)
	tagMessage := generationTagMessage(archive, r.runIdentifier)
	existing, err := r.resolveOptionalTag(tag)
	if err != nil {
		return GenerationGitRecord{}, err
	}
	if existing != "" {
		if err := r.verifyExisting(tag, existing, parent, message, tagMessage); err != nil {
			return GenerationGitRecord{}, err
		}
		return GenerationGitRecord{Generation: archive.generation, Tag: tag, Commit: existing, AlreadyRecorded: true}, nil
	}

	temporary, err := os.MkdirTemp("", fmt.Sprintf("codexos-generation-%04d-git-", archive.generation))
	if err != nil {
		return GenerationGitRecord{}, generationGitError("could not allocate temporary Git worktree", err)
	}
	defer os.RemoveAll(temporary)
	tagMessagePath := filepath.Join(temporary, "tag-message")
	if err := os.WriteFile(tagMessagePath, []byte(tagMessage), 0o600); err != nil {
		return GenerationGitRecord{}, generationGitError("could not write Git tag message", err)
	}
	worktree := filepath.Join(temporary, "worktree")
	if _, err := r.git(r.repository, []string{"worktree", "add", "--detach", worktree, parent}, true); err != nil {
		return GenerationGitRecord{}, err
	}
	worktreeAdded := true
	defer func() {
		if !worktreeAdded {
			return
		}
		cleanup, cleanupErr := r.git(r.repository, []string{"worktree", "remove", "--force", worktree}, false)
		if cleanupErr == nil && cleanup.exitCode != 0 && pathExists(worktree) {
			_ = os.RemoveAll(worktree)
		}
		_, _ = r.git(r.repository, []string{"worktree", "prune"}, false)
		if err == nil && cleanupErr == nil && cleanup.exitCode != 0 {
			err = &GenerationGitRecorderError{Reason: "failed to remove temporary Git worktree: " + gitDiagnostics(cleanup.stdout)}
		}
	}()
	if err := r.replaceSeedTree(worktree, entries); err != nil {
		return GenerationGitRecord{}, err
	}
	if _, err := r.git(worktree, []string{"add", "-f", "-A", "--", "seed"}, true); err != nil {
		return GenerationGitRecord{}, err
	}
	if err := r.verifyWorktreeScope(worktree); err != nil {
		return GenerationGitRecord{}, err
	}
	if err := r.verifyIndex(worktree, entries); err != nil {
		return GenerationGitRecord{}, err
	}
	if _, err := r.git(worktree, []string{"commit", "--allow-empty", "-m", strings.TrimRight(message, "\n")}, true); err != nil {
		return GenerationGitRecord{}, err
	}
	commit, err := r.resolveCommit("HEAD", worktree)
	if err != nil {
		return GenerationGitRecord{}, err
	}
	if _, err := r.git(r.repository, []string{
		"tag", "--annotate", "--no-sign", "--cleanup=verbatim", "--file", tagMessagePath, "--", tag, commit,
	}, true); err != nil {
		return GenerationGitRecord{}, err
	}
	return GenerationGitRecord{Generation: archive.generation, Tag: tag, Commit: commit}, nil
}

func (r *GenerationGitRecorder) parentCommit(archive generationArchive) (string, error) {
	if archive.generation == 0 {
		return r.baseCommit, nil
	}
	if archive.parent == nil {
		return "", fmt.Errorf("generation %d has no parent generation", archive.generation)
	}
	tag := generationTag(r.runIdentifier, *archive.parent)
	commit, err := r.resolveOptionalTag(tag)
	if err != nil {
		return "", err
	}
	if commit == "" {
		return "", &GenerationGitRecorderError{Reason: "required completed parent tag is missing: " + tag}
	}
	return commit, nil
}

func (r *GenerationGitRecorder) verifyExisting(tag, commit, parent, message, tagMessage string) error {
	objectType, err := r.gitText(r.repository, []string{"cat-file", "-t", "refs/tags/" + tag})
	if err != nil {
		return err
	}
	if strings.TrimSpace(objectType) != "tag" {
		return &GenerationGitRecorderError{Reason: "generation tag is not annotated: " + tag}
	}
	ancestry, err := r.gitText(r.repository, []string{"rev-list", "--parents", "-n", "1", commit})
	if err != nil {
		return err
	}
	if !sameStrings(strings.Fields(ancestry), []string{commit, parent}) {
		return &GenerationGitRecorderError{Reason: "generation tag has conflicting ancestry: " + tag}
	}
	rawCommit, err := r.git(r.repository, []string{"cat-file", "commit", commit}, true)
	if err != nil {
		return err
	}
	separator := bytes.Index(rawCommit.stdout, []byte("\n\n"))
	if separator < 0 {
		return &GenerationGitRecorderError{Reason: "generation tag points to a malformed commit: " + tag}
	}
	if !utf8.Valid(rawCommit.stdout[separator+2:]) {
		return &GenerationGitRecorderError{Reason: "generation tag commit message is not UTF-8: " + tag}
	}
	if string(rawCommit.stdout[separator+2:]) != message {
		return &GenerationGitRecorderError{Reason: "generation tag has conflicting provenance: " + tag}
	}
	rawTag, err := r.git(r.repository, []string{"cat-file", "tag", "refs/tags/" + tag}, true)
	if err != nil {
		return err
	}
	tagSeparator := bytes.Index(rawTag.stdout, []byte("\n\n"))
	if tagSeparator < 0 {
		return &GenerationGitRecorderError{Reason: "generation tag object is malformed: " + tag}
	}
	headers := bytes.Split(rawTag.stdout[:tagSeparator], []byte("\n"))
	objectHeader := []byte("object " + commit)
	if !containsBytes(headers, objectHeader) || !containsBytes(headers, []byte("type commit")) {
		return &GenerationGitRecorderError{Reason: "generation tag does not directly target its commit: " + tag}
	}
	if !utf8.Valid(rawTag.stdout[tagSeparator+2:]) {
		return &GenerationGitRecorderError{Reason: "generation tag message is not UTF-8: " + tag}
	}
	if string(rawTag.stdout[tagSeparator+2:]) != tagMessage {
		return &GenerationGitRecorderError{Reason: "generation tag has conflicting annotation: " + tag}
	}
	return nil
}

func (r *GenerationGitRecorder) reconcileLineages(archives []generationArchive, records []GenerationGitRecord) error {
	commits := make(map[uint64]string, len(records))
	for _, record := range records {
		commits[record.Generation] = record.Commit
	}
	generationLineage := make(map[uint64]uint64)
	lineageCommits := make(map[uint64][]generationCommit)
	lineageOrder := make([]uint64, 0)
	nextLineage := uint64(0)
	for _, archive := range archives {
		if archive.outcome != "completed" {
			continue
		}
		var lineage uint64
		if archive.generation == 0 || archive.transition == "rollback" {
			lineage = nextLineage
			nextLineage++
		} else {
			if archive.parent == nil {
				return fmt.Errorf("generation %d has no completed lineage parent", archive.generation)
			}
			var ok bool
			lineage, ok = generationLineage[*archive.parent]
			if !ok {
				return fmt.Errorf("generation %d has no completed lineage parent", archive.generation)
			}
		}
		generationLineage[archive.generation] = lineage
		commit, ok := commits[archive.generation]
		if !ok {
			return errors.New("completed generation has no recorded commit")
		}
		if _, exists := lineageCommits[lineage]; !exists {
			lineageOrder = append(lineageOrder, lineage)
		}
		lineageCommits[lineage] = append(lineageCommits[lineage], generationCommit{generation: archive.generation, commit: commit})
	}
	expected := make(map[string]string, len(lineageCommits))
	for _, lineage := range lineageOrder {
		entries := lineageCommits[lineage]
		expected[lineageBranch(r.runIdentifier, lineage)] = entries[len(entries)-1].commit
	}
	if err := r.rejectUnexpectedLineageRefs(expected); err != nil {
		return err
	}
	activeLineage, hasActive := maxLineage(lineageCommits)
	updates := make([]lineageUpdate, 0)
	for _, lineage := range lineageOrder {
		entries := lineageCommits[lineage]
		branch := lineageBranch(r.runIdentifier, lineage)
		expectedCommit := entries[len(entries)-1].commit
		existing, err := r.resolveOptionalBranch(branch)
		if err != nil {
			return err
		}
		if existing == "" {
			updates = append(updates, lineageUpdate{branch: branch, newCommit: expectedCommit})
			continue
		}
		if existing == expectedCommit {
			continue
		}
		position := -1
		for index, entry := range entries {
			if entry.commit == existing {
				position = index
				break
			}
		}
		reached, err := r.branchHasReached(branch, expectedCommit)
		if err != nil {
			return err
		}
		if !hasActive || lineage != activeLineage || position < 0 || reached {
			return &GenerationGitRecorderError{Reason: "lineage branch has conflicting provenance: " + branch}
		}
		if position >= len(entries)-1 {
			return &GenerationGitRecorderError{Reason: "lineage branch cannot be fast-forwarded safely: " + branch}
		}
		ancestor, err := r.isAncestor(existing, expectedCommit)
		if err != nil {
			return err
		}
		if !ancestor {
			return &GenerationGitRecorderError{Reason: "lineage branch cannot be fast-forwarded safely: " + branch}
		}
		updates = append(updates, lineageUpdate{branch: branch, newCommit: expectedCommit, oldCommit: existing})
	}
	for _, update := range updates {
		if _, err := r.git(r.repository, []string{
			"update-ref", "--create-reflog", "-m", "CodexOS lineage reconciliation",
			"refs/heads/" + update.branch, update.newCommit, update.oldCommit,
		}, true); err != nil {
			return err
		}
	}
	return nil
}

type generationCommit struct {
	generation uint64
	commit     string
}

type lineageUpdate struct {
	branch    string
	newCommit string
	oldCommit string
}

func maxLineage(lineages map[uint64][]generationCommit) (uint64, bool) {
	var maximum uint64
	found := false
	for lineage := range lineages {
		if !found || lineage > maximum {
			maximum, found = lineage, true
		}
	}
	return maximum, found
}

func (r *GenerationGitRecorder) rejectUnexpectedLineageRefs(expected map[string]string) error {
	prefix := "refs/heads/" + r.runIdentifier + "/lineage-"
	refs, err := r.gitText(r.repository, []string{"for-each-ref", "--format=%(refname)", "refs/heads/" + r.runIdentifier})
	if err != nil {
		return err
	}
	for _, ref := range strings.Split(strings.TrimSuffix(refs, "\n"), "\n") {
		if ref == "" || !strings.HasPrefix(ref, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(ref, prefix)
		lineage, ok := parseASCIIUnsigned(suffix)
		if !ok || ref != "refs/heads/"+lineageBranch(r.runIdentifier, lineage) {
			continue
		}
		if _, ok := expected[strings.TrimPrefix(ref, "refs/heads/")]; !ok {
			return &GenerationGitRecorderError{Reason: "unexpected lineage branch conflicts with archives: " + ref}
		}
	}
	return nil
}

func (r *GenerationGitRecorder) resolveOptionalBranch(branch string) (string, error) {
	ref := "refs/heads/" + branch
	exists, err := r.git(r.repository, []string{"show-ref", "--verify", "--quiet", ref}, false)
	if err != nil {
		return "", err
	}
	if exists.exitCode == 1 {
		return "", nil
	}
	if exists.exitCode != 0 {
		return "", &GenerationGitRecorderError{Reason: "failed to inspect lineage branch " + branch + ": " + gitDiagnostics(exists.stdout)}
	}
	symbolic, err := r.git(r.repository, []string{"symbolic-ref", "--quiet", ref}, false)
	if err != nil {
		return "", err
	}
	if symbolic.exitCode == 0 {
		return "", &GenerationGitRecorderError{Reason: "lineage branch must be a direct branch ref: " + branch}
	}
	if symbolic.exitCode != 1 {
		return "", &GenerationGitRecorderError{Reason: "failed to inspect lineage branch " + branch + ": " + gitDiagnostics(symbolic.stdout)}
	}
	target, err := r.resolveCommit(ref, r.repository)
	if err != nil {
		return "", err
	}
	objectType, err := r.gitText(r.repository, []string{"cat-file", "-t", target})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(objectType) != "commit" {
		return "", &GenerationGitRecorderError{Reason: "lineage branch does not directly target a commit: " + branch}
	}
	return target, nil
}

func (r *GenerationGitRecorder) branchHasReached(branch, commit string) (bool, error) {
	ref := "refs/heads/" + branch
	exists, err := r.git(r.repository, []string{"reflog", "exists", ref}, false)
	if err != nil {
		return false, err
	}
	if exists.exitCode == 1 {
		return false, nil
	}
	if exists.exitCode != 0 {
		return false, &GenerationGitRecorderError{Reason: "failed to inspect lineage branch history " + branch + ": " + gitDiagnostics(exists.stdout)}
	}
	history, err := r.gitText(r.repository, []string{"reflog", "show", "--format=%H", ref})
	if err != nil {
		return false, &GenerationGitRecorderError{Reason: "failed to inspect lineage branch history " + branch + ": " + err.Error(), Err: err}
	}
	for _, value := range strings.Fields(history) {
		if value == commit {
			return true, nil
		}
	}
	return false, nil
}

func (r *GenerationGitRecorder) isAncestor(ancestor, descendant string) (bool, error) {
	result, err := r.git(r.repository, []string{"merge-base", "--is-ancestor", ancestor, descendant}, false)
	if err != nil {
		return false, err
	}
	if result.exitCode == 0 {
		return true, nil
	}
	if result.exitCode == 1 {
		return false, nil
	}
	return false, &GenerationGitRecorderError{Reason: "failed to verify lineage branch ancestry: " + gitDiagnostics(result.stdout)}
}

func (r *GenerationGitRecorder) replaceSeedTree(worktree string, entries []guest.SnapshotFile) error {
	seed := filepath.Join(worktree, "seed")
	if info, err := os.Lstat(seed); err == nil {
		if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			if err := os.RemoveAll(seed); err != nil {
				return err
			}
		} else if err := os.Remove(seed); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Mkdir(seed, 0o777); err != nil {
		return err
	}
	seedRoot, err := filepath.EvalSymlinks(seed)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		output := filepath.Join(worktree, filepath.FromSlash(entry.Path))
		resolved := filepath.Clean(output)
		relative, err := filepath.Rel(seedRoot, resolved)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return &GenerationGitRecorderError{Reason: fmt.Sprintf("source path escapes temporary seed tree: %q", entry.Path)}
		}
		if err := os.MkdirAll(filepath.Dir(output), 0o777); err != nil {
			return err
		}
		if err := os.WriteFile(output, entry.Content, 0o666); err != nil {
			return err
		}
	}
	return nil
}

func (r *GenerationGitRecorder) verifyWorktreeScope(worktree string) error {
	result, err := r.git(worktree, []string{"status", "--porcelain=v1", "-z", "--untracked-files=all"}, true)
	if err != nil {
		return err
	}
	records := bytes.Split(result.stdout, []byte{0})
	for index := 0; index < len(records); index++ {
		record := records[index]
		if len(record) == 0 {
			continue
		}
		if len(record) < 4 || record[2] != ' ' {
			return &GenerationGitRecorderError{Reason: "temporary Git worktree status is malformed"}
		}
		statusCode := record[:2]
		if err := verifySeedPath(record[3:]); err != nil {
			return err
		}
		if bytes.Contains(statusCode, []byte{'R'}) || bytes.Contains(statusCode, []byte{'C'}) {
			index++
			if index >= len(records) || len(records[index]) == 0 {
				return &GenerationGitRecorderError{Reason: "temporary Git worktree status is malformed"}
			}
			if err := verifySeedPath(records[index]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *GenerationGitRecorder) verifyIndex(worktree string, entries []guest.SnapshotFile) error {
	result, err := r.git(worktree, []string{"ls-files", "-s", "-z", "--", "seed"}, true)
	if err != nil {
		return err
	}
	indexed := make(map[string]string)
	for _, record := range bytes.Split(result.stdout, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		metadata, path, ok := bytes.Cut(record, []byte{'\t'})
		fields := bytes.Fields(metadata)
		if !ok || len(fields) != 3 || string(fields[2]) != "0" {
			return &GenerationGitRecorderError{Reason: "temporary Git index is malformed"}
		}
		if err := verifySeedPath(path); err != nil {
			return err
		}
		indexed[string(path)] = string(fields[1])
	}
	expected := make(map[string]guest.SnapshotFile, len(entries))
	for _, entry := range entries {
		expected[entry.Path] = entry
	}
	if len(indexed) != len(expected) {
		return &GenerationGitRecorderError{Reason: "Git cannot represent the source snapshot exactly"}
	}
	for path, entry := range expected {
		objectID, ok := indexed[path]
		if !ok {
			return &GenerationGitRecorderError{Reason: "Git cannot represent the source snapshot exactly"}
		}
		blob, err := r.git(worktree, []string{"cat-file", "blob", objectID}, true)
		if err != nil {
			return err
		}
		if !bytes.Equal(blob.stdout, entry.Content) {
			return &GenerationGitRecorderError{Reason: fmt.Sprintf("Git changed source bytes while staging %q", entry.Path)}
		}
	}
	return nil
}

func verifySeedPath(path []byte) error {
	if !bytes.Equal(path, []byte("seed")) && !bytes.HasPrefix(path, []byte("seed/")) {
		return &GenerationGitRecorderError{Reason: "temporary Git worktree changed content outside seed/"}
	}
	return nil
}

func (r *GenerationGitRecorder) resolveOptionalTag(tag string) (string, error) {
	exists, err := r.git(r.repository, []string{"show-ref", "--verify", "--quiet", "refs/tags/" + tag}, false)
	if err != nil {
		return "", err
	}
	if exists.exitCode == 1 {
		return "", nil
	}
	if exists.exitCode != 0 {
		return "", &GenerationGitRecorderError{Reason: "failed to inspect generation tag " + tag + ": " + gitDiagnostics(exists.stdout)}
	}
	return r.resolveCommit(tag, r.repository)
}

func (r *GenerationGitRecorder) resolveCommit(ref, location string) (string, error) {
	text, err := r.gitText(location, []string{"rev-parse", "--verify", "--end-of-options", ref + "^{commit}"})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(text), nil
}

type gitResult struct {
	stdout   []byte
	exitCode int
}

type synchronizedBuffer struct {
	mutex    sync.Mutex
	exceeded bool
	bytes.Buffer
}

func (b *synchronizedBuffer) Write(value []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	accepted := len(value)
	remaining := maxGenerationGitOutput - b.Buffer.Len()
	if remaining < len(value) {
		b.exceeded = true
		if remaining < 0 {
			remaining = 0
		}
		value = value[:remaining]
	}
	_, _ = b.Buffer.Write(value)
	return accepted, nil
}

func (b *synchronizedBuffer) result() ([]byte, bool) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return append([]byte(nil), b.Buffer.Bytes()...), b.exceeded
}

func (r *GenerationGitRecorder) gitText(location string, arguments []string) (string, error) {
	result, err := r.git(location, arguments, true)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(result.stdout) {
		return "", &GenerationGitRecorderError{Reason: "Git returned non-UTF-8 output"}
	}
	return string(result.stdout), nil
}

func (r *GenerationGitRecorder) git(location string, arguments []string, check bool) (gitResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), generationGitTimeout)
	defer cancel()
	commandArguments := make([]string, 0, len(arguments)+4)
	commandArguments = append(commandArguments, "-c", "core.hooksPath=/dev/null", "-C", location)
	commandArguments = append(commandArguments, arguments...)
	command := exec.CommandContext(ctx, r.gitExecutable, commandArguments...)
	var output synchronizedBuffer
	command.Stdout = &output
	command.Stderr = &output
	runErr := command.Run()
	commandOutput, outputExceeded := output.result()
	if ctx.Err() != nil {
		return gitResult{}, &GenerationGitRecorderError{Reason: fmt.Sprintf("Git command failed: %s", firstArgument(arguments)), Err: ctx.Err()}
	}
	if outputExceeded {
		return gitResult{}, &GenerationGitRecorderError{Reason: fmt.Sprintf("Git command output exceeds 32 MiB: %s", firstArgument(arguments))}
	}
	exitCode := 0
	if command.ProcessState != nil {
		exitCode = command.ProcessState.ExitCode()
	}
	result := gitResult{stdout: commandOutput, exitCode: exitCode}
	if runErr != nil && command.ProcessState == nil {
		return gitResult{}, &GenerationGitRecorderError{Reason: fmt.Sprintf("Git command failed: %s", firstArgument(arguments)), Err: runErr}
	}
	if runErr != nil && exitCode < 0 {
		return gitResult{}, &GenerationGitRecorderError{Reason: fmt.Sprintf("Git command failed: %s", firstArgument(arguments)), Err: runErr}
	}
	if check && exitCode != 0 {
		return gitResult{}, &GenerationGitRecorderError{Reason: fmt.Sprintf("Git command failed: %s: %s", firstArgument(arguments), gitDiagnostics(result.stdout))}
	}
	return result, nil
}

func resolveGenerationGitPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return resolved, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return absolute, nil
	}
	return "", err
}

func generationTag(run string, generation uint64) string {
	return fmt.Sprintf("%s/generation-%04d", run, generation)
}

func lineageBranch(run string, lineage uint64) string {
	return fmt.Sprintf("%s/lineage-%04d", run, lineage)
}

func generationName(generation uint64) string {
	return fmt.Sprintf("generation-%04d", generation)
}

func generationCommitMessage(archive generationArchive, snapshot []byte) string {
	parent := "none"
	if archive.parent != nil {
		parent = strconv.FormatUint(*archive.parent, 10)
	}
	digest := sha256.Sum256(snapshot)
	return fmt.Sprintf(
		"CodexOS generation %d\n\nGeneration: %d\nParent-Generation: %s\nTransition: %s\nOutcome: completed\nSource-Snapshot-SHA256: %s\nRecorded-By: CodexOS harness\n",
		archive.generation, archive.generation, parent, archive.transition, hex.EncodeToString(digest[:]),
	)
}

func commitMessage(archive generationArchive, snapshot []byte) string {
	return generationCommitMessage(archive, snapshot)
}

func generationTagMessage(archive generationArchive, run string) string {
	parent := "none"
	if archive.parent != nil {
		parent = strconv.FormatUint(*archive.parent, 10)
	}
	return fmt.Sprintf(
		"CodexOS generation %d\n\nRun: %s\nGeneration: %d\nParent-Generation: %s\nTransition: %s\nOutcome: completed\n\nHandoff:\n%s",
		archive.generation, run, archive.generation, parent, archive.transition, archive.handoff,
	)
}

func generationGitHashBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func validSHA256(value any) bool {
	text, ok := value.(string)
	if !ok || len(text) != sha256.Size*2 {
		return false
	}
	for _, character := range text {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func decodeJSON(encoded []byte, output any) error {
	if !utf8.Valid(encoded) {
		return errors.New("invalid UTF-8 JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("trailing JSON")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func parseJSONUnsigned(encoded []byte) (uint64, bool) {
	if bytes.Equal(bytes.TrimSpace(encoded), []byte("null")) {
		return 0, false
	}
	var value uint64
	if err := json.Unmarshal(encoded, &value); err != nil {
		return 0, false
	}
	return value, true
}

func isRegularWithoutSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0
}

func isDirectoryWithoutSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func parseASCIIUnsigned(value string) (uint64, bool) {
	if value == "" {
		return 0, false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, false
		}
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	return parsed, err == nil
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func containsBytes(values [][]byte, target []byte) bool {
	for _, value := range values {
		if bytes.Equal(value, target) {
			return true
		}
	}
	return false
}

func firstArgument(arguments []string) string {
	if len(arguments) == 0 {
		return "unknown"
	}
	return arguments[0]
}

func gitDiagnostics(output []byte) string {
	if len(output) > maxGenerationGitDiagnostics {
		output = output[:maxGenerationGitDiagnostics]
	}
	return strings.TrimSpace(string(output))
}

func generationGitError(reason string, err error) error {
	return &GenerationGitRecorderError{Reason: reason, Err: err}
}

func asGenerationGitError(err error) error {
	var recorderError *GenerationGitRecorderError
	if errors.As(err, &recorderError) {
		return err
	}
	return &GenerationGitRecorderError{Err: err}
}
