package store

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
	"syscall"
	"time"
	"unicode/utf8"

	bootstrapservice "codexos/internal/bootstrap"
	"codexos/internal/guest"
	"codexos/internal/provenance"
	"codexos/internal/qemu"
	"codexos/internal/sourcecapacity"
)

const (
	// CrossRunBootstrapManifest is the immutable manifest binding a
	// destination run to one source generation.
	CrossRunBootstrapManifest = "cross-run-bootstrap.json"
	// CrossRunBootstrapHandoff is the exact handoff imported from the source
	// generation.
	CrossRunBootstrapHandoff = "cross-run-handoff.txt"
	// CrossRunBootstrapFeatureLedger is the canonical snapshot of the source
	// run's feature-request ledger.
	CrossRunBootstrapFeatureLedger = "cross-run-feature-requests.json"

	crossRunBootstrapSchemaVersion = uint64(3)
	crossRunBootstrapLegacySchema  = uint64(2)
	crossRunCopyBufferSize         = 1024 * 1024
	crossRunGitTimeout             = 30 * time.Second
	crossRunGitDiagnosticsLimit    = 16 * 1024
	crossRunGitOutputLimit         = 1024 * 1024
	crossRunProvenanceFileLimit    = 16 * 1024 * 1024
)

// CrossRunBootstrapError reports invalid continuation provenance or a failure
// while atomically initializing it. Err, when present, is the underlying
// filesystem, JSON, or Git error.
type CrossRunBootstrapError struct {
	Reason string
	Err    error
}

func (e *CrossRunBootstrapError) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *CrossRunBootstrapError) Unwrap() error { return e.Err }

// CrossRunBootstrap is the immutable continuation context persisted in a
// destination run. InheritedRequestIDs is sorted in strictly increasing
// order. Callers should treat it as read-only; loading returns a private
// decoded slice and initialization does not retain caller-owned slices.
type CrossRunBootstrap struct {
	SourceRun                  string
	SourceGeneration           uint64
	Handoff                    string
	SuccessorISOSHA256         string
	SuccessorISOSize           uint64
	FeatureLedgerSHA256        string
	FeatureLedgerSize          uint64
	InheritedRequestIDs        []uint64
	GitBaseRef                 string
	GitBaseCommit              string
	SourceHarnessIdentity      *provenance.HarnessIdentity
	DestinationHarnessIdentity *provenance.HarnessIdentity
}

type crossRunFeatureLedgerJSON struct {
	Requests []crossRunFeatureRequestJSON `json:"requests"`
}

type crossRunFeatureRequestJSON struct {
	DecisionNote string `json:"decision_note,omitempty"`
	Description  string `json:"description"`
	Generation   uint64 `json:"generation"`
	ID           uint64 `json:"id"`
	Status       string `json:"status"`
	Title        string `json:"title"`
}

type crossRunManifestJSON struct {
	FeatureRequests crossRunFeatureRequestsJSON `json:"feature_requests"`
	GitBase         crossRunGitBaseJSON         `json:"git_base"`
	Handoff         crossRunFileJSON            `json:"handoff"`
	Harness         *crossRunHarnessJSON        `json:"harness,omitempty"`
	SchemaVersion   uint64                      `json:"schema_version"`
	Source          crossRunSourceJSON          `json:"source"`
	SuccessorISO    provenance.FileIdentity     `json:"successor_iso"`
}

type crossRunFeatureRequestsJSON struct {
	Count  int      `json:"count"`
	File   string   `json:"file"`
	IDs    []uint64 `json:"ids"`
	SHA256 string   `json:"sha256"`
	Size   uint64   `json:"size"`
}

type crossRunGitBaseJSON struct {
	Commit string `json:"commit"`
	Ref    string `json:"ref"`
}

type crossRunFileJSON struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Size   uint64 `json:"size"`
}

type crossRunHarnessJSON struct {
	Destination      crossRunHarnessIdentityJSON  `json:"destination"`
	SourceGeneration *crossRunHarnessIdentityJSON `json:"source_generation"`
}

type crossRunHarnessIdentityJSON struct {
	Build            provenance.HarnessBuildIdentity `json:"build"`
	DirtyTreeSHA256  *string                         `json:"dirty_tree_sha256"`
	Executable       provenance.FileIdentity         `json:"executable"`
	RepositoryCommit string                          `json:"repository_commit"`
	RepositoryDirty  bool                            `json:"repository_dirty"`
	SchemaVersion    uint64                          `json:"schema_version"`
}

type crossRunSourceJSON struct {
	Generation uint64 `json:"generation"`
	Run        string `json:"run"`
}

// VerifyInitialISO verifies that initialISO is the exact selected successor
// recorded by this bootstrap. A symlink supplied by a caller is resolved in
// the same way as pathlib.Path.resolve() in the Python harness.
func (b *CrossRunBootstrap) VerifyInitialISO(initialISO string) error {
	if b == nil {
		return &CrossRunBootstrapError{Reason: "cross-run bootstrap is nil"}
	}
	resolved, err := resolveCrossRunPath(initialISO)
	if err != nil {
		return &CrossRunBootstrapError{Reason: "initial ISO is unavailable", Err: err}
	}
	identity, err := crossRunFileIdentity(resolved, "initial ISO")
	if err != nil {
		return err
	}
	if identity.SHA256 != b.SuccessorISOSHA256 || identity.Size != b.SuccessorISOSize {
		return &CrossRunBootstrapError{Reason: "initial ISO does not match cross-run bootstrap provenance"}
	}
	return nil
}

// InitializeCrossRunBootstrap validates one completed source generation and
// atomically seeds a fresh destination run with only its handoff and feature
// ledger. The source archive itself is never copied or modified.
func InitializeCrossRunBootstrap(
	destinationRun string,
	initialISO string,
	sourceRun string,
	sourceGeneration uint64,
	gitRepository string,
	gitBaseRef string,
) (*CrossRunBootstrap, error) {
	return initializeCrossRunBootstrap(destinationRun, initialISO, sourceRun, sourceGeneration, gitRepository, gitBaseRef, nil, 0)
}

func InitializeCrossRunBootstrapWithHarnessIdentity(
	destinationRun string,
	initialISO string,
	sourceRun string,
	sourceGeneration uint64,
	gitRepository string,
	gitBaseRef string,
	harnessIdentity *provenance.HarnessIdentity,
) (*CrossRunBootstrap, error) {
	if harnessIdentity == nil {
		return nil, &CrossRunBootstrapError{Reason: "destination harness identity is required"}
	}
	if err := provenance.ValidateHarnessIdentity(*harnessIdentity); err != nil {
		return nil, &CrossRunBootstrapError{Reason: "destination harness identity is invalid", Err: err}
	}
	return initializeCrossRunBootstrap(destinationRun, initialISO, sourceRun, sourceGeneration, gitRepository, gitBaseRef, harnessIdentity, 0)
}

// InitializeCrossRunBootstrapWithCapacity explicitly provisions the destination
// as part of atomic bootstrap publication. Zero retains the legacy 64 KiB budget;
// the source run's setting is never inherited implicitly.
func InitializeCrossRunBootstrapWithCapacity(
	destinationRun, initialISO, sourceRun string,
	sourceGeneration uint64,
	gitRepository, gitBaseRef string,
	harnessIdentity *provenance.HarnessIdentity,
	destinationBudget sourcecapacity.Budget,
) (*CrossRunBootstrap, error) {
	return initializeCrossRunBootstrap(destinationRun, initialISO, sourceRun, sourceGeneration, gitRepository, gitBaseRef, harnessIdentity, destinationBudget)
}

func initializeCrossRunBootstrap(
	destinationRun string,
	initialISO string,
	sourceRun string,
	sourceGeneration uint64,
	gitRepository string,
	gitBaseRef string,
	harnessIdentity *provenance.HarnessIdentity,
	destinationBudget sourcecapacity.Budget,
) (*CrossRunBootstrap, error) {
	if err := destinationBudget.Validate(); err != nil {
		return nil, &CrossRunBootstrapError{Reason: "invalid destination source capacity", Err: err}
	}
	if harnessIdentity != nil {
		if err := provenance.ValidateHarnessIdentity(*harnessIdentity); err != nil {
			return nil, &CrossRunBootstrapError{Reason: "destination harness identity is invalid", Err: err}
		}
	}
	destination, err := resolveCrossRunPath(destinationRun)
	if err != nil {
		return nil, &CrossRunBootstrapError{Reason: "could not resolve cross-run destination", Err: err}
	}
	source, err := resolveCrossRunPath(sourceRun)
	if err != nil {
		return nil, &CrossRunBootstrapError{Reason: "source run is unavailable", Err: err}
	}
	image, err := resolveCrossRunPath(initialISO)
	if err != nil {
		return nil, &CrossRunBootstrapError{Reason: "initial ISO is unavailable", Err: err}
	}
	if destinationExists(destination) {
		return nil, &CrossRunBootstrapError{Reason: "cross-run bootstrap requires a fresh destination run"}
	}
	sourceInfo, err := os.Lstat(source)
	if err != nil || sourceInfo.Mode()&os.ModeSymlink != 0 || !sourceInfo.IsDir() {
		if err == nil {
			err = errors.New("path is not a real directory")
		}
		return nil, &CrossRunBootstrapError{Reason: "source run is unavailable: " + source, Err: err}
	}
	if filepath.Clean(source) == filepath.Clean(destination) {
		return nil, &CrossRunBootstrapError{Reason: "source and destination runs must be different"}
	}
	if gitBaseRef == "" {
		return nil, &CrossRunBootstrapError{Reason: "Git base ref must not be empty"}
	}
	// Constructing the Python source runtime validates any bootstrap already
	// attached to that run. Preserve the same fail-closed behavior before
	// treating its latest archive as an inheritance source.
	sourceBootstrap, err := LoadCrossRunBootstrap(source)
	if err != nil {
		return nil, wrapCrossRunSourceError(err)
	}

	archives, err := readCrossRunArchives(source)
	if err != nil {
		return nil, wrapCrossRunSourceError(err)
	}
	if err := validateCrossRunArchiveHistory(archives); err != nil {
		return nil, wrapCrossRunSourceError(err)
	}
	if len(archives) == 0 || archives[len(archives)-1].Generation != sourceGeneration {
		return nil, &CrossRunBootstrapError{Reason: "inherited generation must be the latest source-run archive"}
	}
	archived := archives[len(archives)-1]
	if archived.Outcome != "completed" || archived.Handoff == nil {
		return nil, &CrossRunBootstrapError{Reason: "inherited generation did not complete cooperatively"}
	}
	// Validate against the explicit destination budget before creating any
	// destination/staging state. Absence retains the legacy budget.
	inheritedSnapshot, err := sourcecapacity.ReadFile(filepath.Join(archived.ArchivePath, "source.snapshot"), archived.SourceCapacity.SnapshotLimit())
	if err != nil {
		return nil, wrapCrossRunSourceError(err)
	}
	if _, err := guest.ParseSourceSnapshotWithBudget(inheritedSnapshot, destinationBudget); err != nil {
		return nil, &CrossRunBootstrapError{Reason: fmt.Sprintf("inherited source exceeds destination source content capacity (%d bytes)", destinationBudget.Bytes()), Err: err}
	}
	successorISO := filepath.Join(archived.ArchivePath, "successor", "codexos.iso")
	if equal, compareErr := crossRunFilesEqual(image, successorISO); compareErr != nil {
		return nil, compareErr
	} else if !equal {
		return nil, &CrossRunBootstrapError{Reason: "initial ISO is not byte-identical to the inherited successor"}
	}
	featureStore, err := NewFeatureRequestStore(source)
	if err != nil {
		return nil, wrapCrossRunSourceError(err)
	}
	requests, err := featureStore.Requests()
	if err != nil {
		return nil, wrapCrossRunSourceError(err)
	}
	inheritedRequestIDs := make(map[uint64]struct{})
	if sourceBootstrap != nil {
		inheritedRequestIDs = make(map[uint64]struct{}, len(sourceBootstrap.InheritedRequestIDs))
		for _, requestID := range sourceBootstrap.InheritedRequestIDs {
			inheritedRequestIDs[requestID] = struct{}{}
		}
	}
	for _, request := range requests {
		// Requests inherited by the source retain their original run's
		// generation number. Only requests created by this source run must fit
		// within its local archived generation range.
		_, inherited := inheritedRequestIDs[request.ID]
		if request.Generation > sourceGeneration && !inherited {
			return nil, &CrossRunBootstrapError{Reason: "source feature-request ledger contains a request from a generation newer than the inherited generation"}
		}
	}

	isoIdentity, err := crossRunFileIdentity(successorISO, "inherited successor ISO")
	if err != nil {
		return nil, err
	}
	handoffBytes := []byte(*archived.Handoff)
	ledgerBytes, err := crossRunFeatureLedgerBytes(requests)
	if err != nil {
		return nil, &CrossRunBootstrapError{Reason: "could not encode cross-run feature ledger", Err: err}
	}
	requestIDs := make([]uint64, len(requests))
	for index, request := range requests {
		requestIDs[index] = request.ID
	}
	expectedGitBaseRef := fmt.Sprintf("%s/generation-%04d", filepath.Base(source), sourceGeneration)
	if gitBaseRef != expectedGitBaseRef {
		return nil, &CrossRunBootstrapError{Reason: "Git base ref must be the inherited generation tag: " + expectedGitBaseRef}
	}

	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o777); err != nil {
		return nil, &CrossRunBootstrapError{Reason: "could not create cross-run destination parent", Err: err}
	}
	wrapper, err := os.MkdirTemp(parent, ".cross-run-bootstrap-*")
	if err != nil {
		return nil, &CrossRunBootstrapError{Reason: "could not stage cross-run bootstrap", Err: err}
	}
	candidate := filepath.Join(wrapper, filepath.Base(destination))
	if err := os.Mkdir(candidate, 0o777); err != nil {
		_ = os.RemoveAll(wrapper)
		return nil, &CrossRunBootstrapError{Reason: "could not stage cross-run bootstrap", Err: err}
	}
	published := false
	completed := false
	cleanup := func() {
		if completed {
			return
		}
		if published && destinationExists(destination) {
			_ = os.RemoveAll(destination)
		}
		if _, statErr := os.Lstat(wrapper); statErr == nil {
			_ = os.RemoveAll(wrapper)
		}
	}
	defer cleanup()

	baseCommit, err := verifyCrossRunGitBase(gitRepository, gitBaseRef, expectedGitBaseRef, filepath.Base(source))
	if err != nil {
		return nil, err
	}
	bootstrap := &CrossRunBootstrap{
		SourceRun:                  filepath.Base(source),
		SourceGeneration:           sourceGeneration,
		Handoff:                    *archived.Handoff,
		SuccessorISOSHA256:         isoIdentity.SHA256,
		SuccessorISOSize:           isoIdentity.Size,
		FeatureLedgerSHA256:        crossRunBytesIdentity(ledgerBytes).SHA256,
		FeatureLedgerSize:          uint64(len(ledgerBytes)),
		InheritedRequestIDs:        cloneCrossRunRequestIDs(requestIDs),
		GitBaseRef:                 gitBaseRef,
		GitBaseCommit:              baseCommit,
		SourceHarnessIdentity:      provenance.CloneHarnessIdentity(archived.HarnessIdentity),
		DestinationHarnessIdentity: provenance.CloneHarnessIdentity(harnessIdentity),
	}
	if harnessIdentity != nil {
		if err := provenance.NewHarnessIdentityStore(candidate).RecordRunCreation(*harnessIdentity); err != nil {
			return nil, &CrossRunBootstrapError{Reason: "could not persist destination harness identity", Err: err}
		}
	}
	if destinationBudget != 0 {
		if err := sourcecapacity.Save(candidate, destinationBudget); err != nil {
			return nil, &CrossRunBootstrapError{Reason: "could not persist destination source capacity", Err: err}
		}
	}
	if err := writeCrossRunDurable(filepath.Join(candidate, CrossRunBootstrapHandoff), handoffBytes); err != nil {
		return nil, err
	}
	if err := writeCrossRunDurable(filepath.Join(candidate, CrossRunBootstrapFeatureLedger), ledgerBytes); err != nil {
		return nil, err
	}
	destinationStore, err := NewFeatureRequestStore(candidate)
	if err != nil {
		return nil, &CrossRunBootstrapError{Reason: "could not initialize inherited feature-request store", Err: err}
	}
	if err := destinationStore.Import(requests); err != nil {
		return nil, err
	}
	imported, err := destinationStore.Requests()
	if err != nil {
		return nil, &CrossRunBootstrapError{Reason: "could not verify inherited feature-request store", Err: err}
	}
	importedLedger, err := crossRunFeatureLedgerBytes(imported)
	if err != nil {
		return nil, &CrossRunBootstrapError{Reason: "could not encode inherited feature ledger", Err: err}
	}
	if !bytes.Equal(importedLedger, ledgerBytes) {
		return nil, &CrossRunBootstrapError{Reason: "inherited feature-request ledger changed during initialization"}
	}
	if err := inheritOperatorRequests(source, candidate, sourceGeneration); err != nil {
		return nil, err
	}
	manifest, err := crossRunManifestBytes(bootstrap, handoffBytes, len(requests))
	if err != nil {
		return nil, &CrossRunBootstrapError{Reason: "could not encode cross-run bootstrap manifest", Err: err}
	}
	if err := writeCrossRunDurable(filepath.Join(candidate, CrossRunBootstrapManifest), manifest); err != nil {
		return nil, err
	}
	if err := syncCrossRunDirectory(filepath.Join(candidate, "feature-requests"), true); err != nil {
		return nil, err
	}
	finishArtifacts, err := bootstrapservice.Inherit(source, archived.ArchivePath, candidate, destination)
	if err != nil {
		return nil, err
	}
	defer func() { _ = finishArtifacts(completed) }()
	if err := syncCrossRunDirectory(candidate, false); err != nil {
		return nil, err
	}
	if destinationExists(destination) {
		return nil, &CrossRunBootstrapError{Reason: "cross-run bootstrap destination appeared during initialization"}
	}
	if err := os.Rename(candidate, destination); err != nil {
		return nil, &CrossRunBootstrapError{Reason: "could not publish cross-run bootstrap", Err: err}
	}
	published = true
	if err := syncCrossRunDirectory(parent, false); err != nil {
		return nil, err
	}
	if err := os.Remove(wrapper); err != nil {
		return nil, &CrossRunBootstrapError{Reason: "could not remove cross-run bootstrap staging", Err: err}
	}
	completed = true
	return bootstrap, nil
}

// LoadCrossRunBootstrap loads and validates persisted bootstrap provenance
// without requiring the source run to be present. A run with none of the
// three bootstrap files returns (nil, nil), matching an ordinary initial run.
func LoadCrossRunBootstrap(runDirectory string) (*CrossRunBootstrap, error) {
	run, err := resolveCrossRunPath(runDirectory)
	if err != nil {
		return nil, &CrossRunBootstrapError{Reason: "could not resolve cross-run run directory", Err: err}
	}
	manifestPath := filepath.Join(run, CrossRunBootstrapManifest)
	handoffPath := filepath.Join(run, CrossRunBootstrapHandoff)
	ledgerPath := filepath.Join(run, CrossRunBootstrapFeatureLedger)
	if !crossRunPathExists(manifestPath) && !crossRunPathExists(handoffPath) && !crossRunPathExists(ledgerPath) {
		return nil, nil
	}
	for _, path := range []string{manifestPath, handoffPath, ledgerPath} {
		info, statErr := os.Lstat(path)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			if statErr == nil {
				statErr = errors.New("path is not a regular file")
			}
			return nil, &CrossRunBootstrapError{Reason: "cross-run bootstrap provenance is incomplete", Err: statErr}
		}
	}
	manifestBytes, err := readCrossRunLimited(manifestPath, crossRunProvenanceFileLimit)
	if err != nil {
		return nil, &CrossRunBootstrapError{Reason: "cross-run bootstrap provenance is malformed", Err: err}
	}
	handoffBytes, err := readCrossRunLimited(handoffPath, crossRunProvenanceFileLimit)
	if err != nil || !utf8.Valid(handoffBytes) {
		if err == nil {
			err = errors.New("handoff is not valid UTF-8")
		}
		return nil, &CrossRunBootstrapError{Reason: "cross-run bootstrap provenance is malformed", Err: err}
	}
	ledgerBytes, err := readCrossRunLimited(ledgerPath, crossRunProvenanceFileLimit)
	if err != nil {
		return nil, &CrossRunBootstrapError{Reason: "cross-run bootstrap provenance is malformed", Err: err}
	}
	value, err := decodeCrossRunJSON(manifestBytes)
	if err != nil {
		return nil, &CrossRunBootstrapError{Reason: "cross-run bootstrap provenance is malformed", Err: err}
	}
	bootstrap, expectedHandoff, err := decodeCrossRunManifest(value)
	if err != nil {
		return nil, err
	}
	if bootstrap.DestinationHarnessIdentity != nil {
		runIdentityPath := filepath.Join(run, provenance.RunHarnessIdentityFilename)
		runIdentityBytes, readErr := readCrossRunLimited(runIdentityPath, crossRunProvenanceFileLimit)
		if readErr != nil {
			return nil, &CrossRunBootstrapError{Reason: "cross-run bootstrap destination harness identity is unavailable", Err: readErr}
		}
		runIdentity, parseErr := provenance.ParseHarnessIdentity(runIdentityBytes)
		if parseErr != nil || !runIdentity.Equal(*bootstrap.DestinationHarnessIdentity) {
			return nil, &CrossRunBootstrapError{Reason: "cross-run bootstrap destination harness identity does not match", Err: parseErr}
		}
	}
	if identity := crossRunBytesIdentity(handoffBytes); identity.SHA256 != expectedHandoff.SHA256 || identity.Size != expectedHandoff.Size {
		return nil, &CrossRunBootstrapError{Reason: "cross-run bootstrap handoff identity does not match"}
	}
	inherited, err := decodeCrossRunFeatureLedger(ledgerBytes)
	if err != nil {
		return nil, err
	}
	ledgerIdentity := crossRunBytesIdentity(ledgerBytes)
	if ledgerIdentity.SHA256 != bootstrap.FeatureLedgerSHA256 || ledgerIdentity.Size != bootstrap.FeatureLedgerSize || !equalCrossRunRequestIDs(inherited, bootstrap.InheritedRequestIDs) {
		return nil, &CrossRunBootstrapError{Reason: "cross-run bootstrap feature ledger identity does not match"}
	}
	if err := validateCrossRunInheritedRequests(run, inherited); err != nil {
		return nil, err
	}
	if _, err := readOperatorRequests(run); err != nil {
		return nil, err
	}
	bootstrap.Handoff = string(handoffBytes)
	bootstrap.InheritedRequestIDs = cloneCrossRunRequestIDs(bootstrap.InheritedRequestIDs)
	return bootstrap, nil
}

type crossRunIdentity struct {
	SHA256 string
	Size   uint64
}

type crossRunArchivedGeneration struct {
	SourceCapacity  sourcecapacity.Budget
	Generation      uint64
	Parent          *uint64
	Transition      string
	Outcome         string
	ArchivePath     string
	Handoff         *string
	AbortReason     *string
	HarnessIdentity *provenance.HarnessIdentity
}

func wrapCrossRunSourceError(err error) error {
	var bootstrapErr *CrossRunBootstrapError
	if errors.As(err, &bootstrapErr) {
		return err
	}
	return &CrossRunBootstrapError{Reason: "source run cannot be inherited", Err: err}
}

func resolveCrossRunPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	if resolved, resolveErr := filepath.EvalSymlinks(absolute); resolveErr == nil {
		return filepath.Clean(resolved), nil
	} else if !errors.Is(resolveErr, os.ErrNotExist) {
		if info, lstatErr := os.Lstat(absolute); lstatErr != nil || info.Mode()&os.ModeSymlink == 0 {
			return "", resolveErr
		}
	}
	// Match pathlib.Path.resolve(strict=False) when only the final component
	// (or a suffix below an existing directory) does not exist.
	current := absolute
	suffix := make([]string, 0, 4)
	for {
		if _, statErr := os.Lstat(current); statErr == nil {
			resolved, resolveErr := filepath.EvalSymlinks(current)
			if resolveErr != nil {
				return "", resolveErr
			}
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return filepath.Clean(resolved), nil
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return absolute, nil
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func destinationExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func crossRunPathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readCrossRunArchives(run string) ([]crossRunArchivedGeneration, error) {
	entries, err := os.ReadDir(run)
	if err != nil {
		return nil, &CrossRunBootstrapError{Reason: "could not inspect source run archives", Err: err}
	}
	archives := make([]crossRunArchivedGeneration, 0)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "generation-") {
			continue
		}
		generation, parseErr := parseCrossRunGenerationName(entry.Name())
		if parseErr != nil {
			return nil, parseErr
		}
		archive, readErr := readCrossRunArchive(filepath.Join(run, entry.Name()), generation)
		if readErr != nil {
			return nil, readErr
		}
		archives = append(archives, archive)
	}
	sort.Slice(archives, func(left, right int) bool { return archives[left].Generation < archives[right].Generation })
	return archives, nil
}

func parseCrossRunGenerationName(name string) (uint64, error) {
	encoded := strings.TrimPrefix(name, "generation-")
	if encoded == "" || !isASCIIDigits(encoded) {
		return 0, &CrossRunBootstrapError{Reason: "invalid generation archive: " + name}
	}
	generation, err := strconv.ParseUint(encoded, 10, 64)
	if err != nil || fmt.Sprintf("generation-%04d", generation) != name {
		return 0, &CrossRunBootstrapError{Reason: "invalid generation archive: " + name, Err: err}
	}
	return generation, nil
}

func isASCIIDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func readCrossRunArchive(path string, expectedGeneration uint64) (crossRunArchivedGeneration, error) {
	archive := crossRunArchivedGeneration{Generation: expectedGeneration, ArchivePath: path}
	if err := bootstrapservice.ValidateArchive(filepath.Dir(path), path); err != nil {
		return archive, err
	}
	bootstrapRefs, err := bootstrapservice.ReadReferences(path)
	if err != nil {
		return archive, err
	}
	budget, err := sourcecapacity.Load(path)
	if err != nil {
		return archive, crossRunArchiveError(expectedGeneration, err)
	}
	archive.SourceCapacity = budget
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		if err == nil {
			err = errors.New("archive is not a real directory")
		}
		return archive, &CrossRunBootstrapError{Reason: fmt.Sprintf("generation %d archive is invalid", expectedGeneration), Err: err}
	}
	metadataBytes, err := readCrossRunRegular(path, "metadata.json")
	if err != nil {
		return archive, crossRunArchiveError(expectedGeneration, err)
	}
	metadataValue, err := decodeCrossRunJSON(metadataBytes)
	if err != nil {
		return archive, crossRunArchiveError(expectedGeneration, errors.New("generation archive metadata is malformed"))
	}
	metadata, err := decodeCrossRunMetadata(metadataValue, expectedGeneration)
	if err != nil {
		return archive, crossRunArchiveError(expectedGeneration, err)
	}
	archive.Outcome = metadata.outcome
	archive.Transition = metadata.transition
	archive.Parent = metadata.parent
	hardwareBytes, err := readCrossRunRegular(path, "hardware.json")
	if err != nil {
		return archive, crossRunArchiveError(expectedGeneration, err)
	}
	if _, err := qemu.ParseHardwareManifest(hardwareBytes); err != nil {
		return archive, crossRunArchiveError(expectedGeneration, errors.New("generation hardware manifest is malformed"))
	}
	harnessPath := filepath.Join(path, provenance.GenerationHarnessFilename)
	if _, statErr := os.Lstat(harnessPath); statErr == nil {
		harnessBytes, readErr := readCrossRunRegular(path, provenance.GenerationHarnessFilename)
		if readErr != nil {
			return archive, crossRunArchiveError(expectedGeneration, readErr)
		}
		identity, parseErr := provenance.ParseHarnessIdentity(harnessBytes)
		if parseErr != nil {
			return archive, crossRunArchiveError(expectedGeneration, errors.New("generation harness identity is malformed"))
		}
		archive.HarnessIdentity = &identity
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return archive, crossRunArchiveError(expectedGeneration, statErr)
	}
	if err := requireCrossRunDirectory(path, "boot"); err != nil {
		return archive, crossRunArchiveError(expectedGeneration, err)
	}
	if err := requireCrossRunRegular(path, "boot/codexos.iso"); err != nil {
		return archive, crossRunArchiveError(expectedGeneration, err)
	}
	for _, name := range []string{"qemu.stdout", "qemu.stderr"} {
		if err := requireCrossRunRegular(path, name); err != nil {
			return archive, crossRunArchiveError(expectedGeneration, err)
		}
	}
	if archive.Outcome == "completed" {
		handoffBytes, readErr := readCrossRunRegular(path, "handoff.txt")
		if readErr != nil {
			return archive, crossRunArchiveError(expectedGeneration, readErr)
		}
		if !utf8.Valid(handoffBytes) {
			return archive, crossRunArchiveError(expectedGeneration, errors.New("generation handoff is not valid UTF-8"))
		}
		handoff := string(handoffBytes)
		archive.Handoff = &handoff
		snapshot, readErr := sourcecapacity.ReadFile(filepath.Join(path, "source.snapshot"), budget.SnapshotLimit())
		if readErr != nil {
			return archive, crossRunArchiveError(expectedGeneration, readErr)
		}
		if _, decodeErr := guest.DecodeSourceSnapshotWithBudget(snapshot, budget); decodeErr != nil {
			return archive, crossRunArchiveError(expectedGeneration, decodeErr)
		}
		for _, name := range []string{"source", "successor"} {
			if err := requireCrossRunDirectory(path, name); err != nil {
				return archive, crossRunArchiveError(expectedGeneration, err)
			}
		}
		for _, name := range []string{"successor/kernel.elf", "successor/codexos.iso"} {
			if err := requireCrossRunRegular(path, name); err != nil {
				return archive, crossRunArchiveError(expectedGeneration, err)
			}
		}
		expected := map[string]struct{}{
			"boot": {}, "metadata.json": {}, "hardware.json": {}, "handoff.txt": {},
			"source.snapshot": {}, "source": {}, "successor": {}, "qemu.stdout": {}, "qemu.stderr": {},
		}
		if budget != 0 {
			expected[sourcecapacity.Filename] = struct{}{}
		}
		if archive.HarnessIdentity != nil {
			expected[provenance.GenerationHarnessFilename] = struct{}{}
		}
		if bootstrapRefs != nil {
			expected[bootstrapservice.ReferencesFilename] = struct{}{}
		}
		if err := validateCrossRunArchiveContents(path, expected); err != nil {
			return archive, crossRunArchiveError(expectedGeneration, err)
		}
	} else {
		aborted, readErr := readCrossRunRegular(path, "aborted.txt")
		if readErr != nil {
			return archive, crossRunArchiveError(expectedGeneration, readErr)
		}
		if !bytes.Equal(aborted, []byte("Generation aborted by operator.")) {
			return archive, crossRunArchiveError(expectedGeneration, errors.New("generation abort marker is malformed"))
		}
		expected := map[string]struct{}{
			"boot": {}, "metadata.json": {}, "hardware.json": {}, "aborted.txt": {}, "qemu.stdout": {}, "qemu.stderr": {},
		}
		reasonPath := filepath.Join(path, "abort-reason.txt")
		if crossRunPathExists(reasonPath) {
			reason, readErr := readCrossRunRegular(path, "abort-reason.txt")
			if readErr != nil || len(reason) > 8*1024 || !utf8.Valid(reason) || strings.TrimSpace(string(reason)) == "" {
				return archive, crossRunArchiveError(expectedGeneration, errors.New("generation abort reason is malformed"))
			}
			value := string(reason)
			archive.AbortReason = &value
			expected["abort-reason.txt"] = struct{}{}
		}
		if budget != 0 {
			expected[sourcecapacity.Filename] = struct{}{}
		}
		if archive.HarnessIdentity != nil {
			expected[provenance.GenerationHarnessFilename] = struct{}{}
		}
		latestManifest := filepath.Join(path, "latest-success.json")
		latestSnapshot := filepath.Join(path, "latest-success.snapshot")
		manifestExists := crossRunPathExists(latestManifest)
		snapshotExists := crossRunPathExists(latestSnapshot)
		if manifestExists || snapshotExists {
			if !manifestExists || !snapshotExists {
				return archive, crossRunArchiveError(expectedGeneration, errors.New("aborted generation forensic evidence is incomplete"))
			}
			if err := validateCrossRunAbortedSuccessEvidence(path, expectedGeneration, budget); err != nil {
				return archive, crossRunArchiveError(expectedGeneration, err)
			}
			expected["latest-success.json"] = struct{}{}
			expected["latest-success.snapshot"] = struct{}{}
		}
		if bootstrapRefs != nil {
			expected[bootstrapservice.ReferencesFilename] = struct{}{}
		}
		if err := validateCrossRunArchiveContents(path, expected); err != nil {
			return archive, crossRunArchiveError(expectedGeneration, err)
		}
	}
	return archive, nil
}

type crossRunMetadata struct {
	parent     *uint64
	transition string
	outcome    string
}

func decodeCrossRunMetadata(value any, expectedGeneration uint64) (crossRunMetadata, error) {
	fields, err := crossRunObject(value, map[string]struct{}{"generation": {}, "outcome": {}, "parent_generation": {}, "transition": {}}, "generation archive metadata")
	if err != nil {
		return crossRunMetadata{}, err
	}
	generation, err := crossRunUint(fields["generation"])
	if err != nil || generation != expectedGeneration {
		return crossRunMetadata{}, errors.New("generation archive metadata has the wrong generation")
	}
	outcome, ok := fields["outcome"].(string)
	if !ok || (outcome != "completed" && outcome != "aborted") {
		return crossRunMetadata{}, errors.New("generation archive metadata is malformed")
	}
	transition, ok := fields["transition"].(string)
	if !ok {
		return crossRunMetadata{}, errors.New("generation archive metadata is malformed")
	}
	var parent *uint64
	if fields["parent_generation"] != nil {
		parentValue, parseErr := crossRunUint(fields["parent_generation"])
		if parseErr != nil {
			return crossRunMetadata{}, errors.New("generation archive metadata is malformed")
		}
		parent = &parentValue
	}
	if expectedGeneration == 0 {
		if parent != nil || transition != "initial" {
			return crossRunMetadata{}, errors.New("generation archive metadata is malformed")
		}
	} else if parent == nil || *parent >= expectedGeneration || (transition != "successor" && transition != "rollback") {
		return crossRunMetadata{}, errors.New("generation archive metadata is malformed")
	}
	return crossRunMetadata{parent: parent, transition: transition, outcome: outcome}, nil
}

func validateCrossRunArchiveHistory(archives []crossRunArchivedGeneration) error {
	byGeneration := make(map[uint64]crossRunArchivedGeneration, len(archives))
	for _, archive := range archives {
		if _, exists := byGeneration[archive.Generation]; exists {
			return errors.New("generation archive history is not contiguous")
		}
		byGeneration[archive.Generation] = archive
	}
	for generation := uint64(0); generation < uint64(len(archives)); generation++ {
		if _, exists := byGeneration[generation]; !exists {
			return errors.New("generation archive history is not contiguous")
		}
	}
	for _, archive := range archives {
		if archive.Generation == 0 {
			continue
		}
		parent := archive.Parent
		if parent == nil {
			return fmt.Errorf("generation %d has no completed parent", archive.Generation)
		}
		parentArchive, exists := byGeneration[*parent]
		if !exists || parentArchive.Outcome != "completed" {
			return fmt.Errorf("generation %d has no completed parent", archive.Generation)
		}
		if archive.Transition == "successor" && *parent != archive.Generation-1 {
			return fmt.Errorf("generation %d has invalid successor ancestry", archive.Generation)
		}
		if archive.Transition == "rollback" && *parent == archive.Generation-1 {
			return fmt.Errorf("generation %d has invalid rollback ancestry", archive.Generation)
		}
	}
	return nil
}

func validateCrossRunArchiveContents(path string, expected map[string]struct{}) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	actual := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		actual[entry.Name()] = struct{}{}
	}
	if len(actual) != len(expected) {
		return errors.New("generation archive has invalid contents")
	}
	for name := range expected {
		if _, exists := actual[name]; !exists {
			return errors.New("generation archive has invalid contents")
		}
	}
	return nil
}

func validateCrossRunAbortedSuccessEvidence(path string, generation uint64, budget sourcecapacity.Budget) error {
	manifestBytes, err := readCrossRunRegular(path, "latest-success.json")
	if err != nil {
		return err
	}
	snapshot, err := sourcecapacity.ReadFile(filepath.Join(path, "latest-success.snapshot"), budget.SnapshotLimit())
	if err != nil {
		return err
	}
	value, err := decodeCrossRunJSON(manifestBytes)
	if err != nil {
		return errors.New("aborted generation forensic manifest is malformed")
	}
	fields, err := crossRunObject(value, nil, "aborted generation forensic manifest")
	if err != nil {
		return err
	}
	if version, ok := fields["schema_version"].(uint64); !ok || version != 1 {
		return errors.New("aborted generation forensic manifest is malformed")
	}
	// decodeCrossRunJSON represents all JSON numbers as uint64, so this
	// generation comparison remains strict rather than accepting 1.0/bool.
	if recorded, ok := fields["generation"].(uint64); !ok || recorded != generation {
		return errors.New("aborted generation forensic generation is incorrect")
	}
	if fields["ready"] != true || fields["protocol_validated"] != true {
		return errors.New("aborted generation forensic success is invalid")
	}
	attempt, ok := fields["build_attempt_id"].(string)
	if !ok || !strings.HasPrefix(attempt, "build-") {
		return errors.New("aborted generation build attempt ID is invalid")
	}
	sourceValue, ok := fields["source_snapshot"].(map[string]any)
	if !ok || len(sourceValue) != 2 {
		return errors.New("aborted generation source identity is invalid")
	}
	identity := crossRunBytesIdentity(snapshot)
	if sourceValue["sha256"] != identity.SHA256 || sourceValue["size"] != identity.Size {
		return errors.New("aborted generation source identity is invalid")
	}
	if _, err := guest.DecodeSourceSnapshotWithBudget(snapshot, budget); err != nil {
		return errors.New("aborted generation source identity is invalid")
	}
	for _, name := range []string{"kernel", "iso"} {
		identityValue, ok := fields[name].(map[string]any)
		if !ok || len(identityValue) != 2 {
			return fmt.Errorf("aborted generation %s identity is invalid", name)
		}
		digest, digestOK := identityValue["sha256"].(string)
		if !digestOK || !validCrossRunSHA(digest, false) || !crossRunNonNegativeUint(identityValue["size"]) {
			return fmt.Errorf("aborted generation %s identity is invalid", name)
		}
	}
	return nil
}

func readCrossRunRegular(root, relative string) ([]byte, error) {
	path := filepath.Join(root, relative)
	if err := requireCrossRunRegular(root, relative); err != nil {
		return nil, err
	}
	contents, err := readCrossRunLimited(path, crossRunProvenanceFileLimit)
	if err != nil {
		return nil, err
	}
	return contents, nil
}

func readCrossRunLimited(path string, limit int64) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("could not create cross-run provenance file handle")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("cross-run provenance path is not a regular file")
	}
	contents, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > limit {
		return nil, errors.New("cross-run provenance file exceeds 16 MiB")
	}
	return contents, nil
}

func requireCrossRunRegular(root, relative string) error {
	path := filepath.Join(root, relative)
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("generation archive artifact is missing: %s", path)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("generation archive artifact is missing: %s", path)
	}
	return nil
}

func requireCrossRunDirectory(root, relative string) error {
	path := filepath.Join(root, relative)
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		if err == nil {
			err = errors.New("path is not a real directory")
		}
		return fmt.Errorf("generation archive artifact is missing: %s: %w", path, err)
	}
	return nil
}

func crossRunArchiveError(generation uint64, err error) error {
	return &CrossRunBootstrapError{Reason: fmt.Sprintf("generation %d archive is invalid", generation), Err: err}
}

func crossRunObject(value any, expected map[string]struct{}, label string) (map[string]any, error) {
	fields, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s is malformed", label)
	}
	if expected != nil {
		if len(fields) != len(expected) {
			return nil, fmt.Errorf("%s is malformed", label)
		}
		for key := range fields {
			if _, exists := expected[key]; !exists {
				return nil, fmt.Errorf("%s is malformed", label)
			}
		}
	}
	return fields, nil
}

func decodeCrossRunJSON(contents []byte) (any, error) {
	if !utf8.Valid(contents) {
		return nil, errors.New("invalid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	return normalizeCrossRunJSONNumbers(value)
}

func normalizeCrossRunJSONNumbers(value any) (any, error) {
	switch value := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseUint(string(value), 10, 64)
		if err != nil {
			return nil, err
		}
		return parsed, nil
	case []any:
		for index, item := range value {
			normalized, err := normalizeCrossRunJSONNumbers(item)
			if err != nil {
				return nil, err
			}
			value[index] = normalized
		}
		return value, nil
	case map[string]any:
		for key, item := range value {
			normalized, err := normalizeCrossRunJSONNumbers(item)
			if err != nil {
				return nil, err
			}
			value[key] = normalized
		}
		return value, nil
	default:
		return value, nil
	}
}

func crossRunUint(value any) (uint64, error) {
	parsed, ok := value.(uint64)
	if !ok {
		return 0, errors.New("not a non-negative integer")
	}
	return parsed, nil
}

func crossRunNonNegativeUint(value any) bool {
	_, ok := value.(uint64)
	return ok
}

func decodeCrossRunManifest(value any) (*CrossRunBootstrap, crossRunIdentity, error) {
	fields, err := crossRunObject(value, nil, "cross-run bootstrap manifest")
	if err != nil {
		return nil, crossRunIdentity{}, &CrossRunBootstrapError{Reason: "cross-run bootstrap manifest has invalid fields"}
	}
	version, ok := fields["schema_version"].(uint64)
	if !ok || version != crossRunBootstrapSchemaVersion && version != crossRunBootstrapLegacySchema {
		return nil, crossRunIdentity{}, &CrossRunBootstrapError{Reason: "cross-run bootstrap schema version is unsupported"}
	}
	expected := map[string]struct{}{"schema_version": {}, "source": {}, "successor_iso": {}, "handoff": {}, "feature_requests": {}, "git_base": {}}
	if version == crossRunBootstrapSchemaVersion {
		expected["harness"] = struct{}{}
	}
	if len(fields) != len(expected) {
		return nil, crossRunIdentity{}, &CrossRunBootstrapError{Reason: "cross-run bootstrap manifest has invalid fields"}
	}
	for key := range fields {
		if _, exists := expected[key]; !exists {
			return nil, crossRunIdentity{}, &CrossRunBootstrapError{Reason: "cross-run bootstrap manifest has invalid fields"}
		}
	}
	sourceFields, err := crossRunObject(fields["source"], map[string]struct{}{"run": {}, "generation": {}}, "cross-run bootstrap source")
	if err != nil {
		return nil, crossRunIdentity{}, &CrossRunBootstrapError{Reason: "cross-run bootstrap source has invalid fields"}
	}
	sourceRun, ok := sourceFields["run"].(string)
	generation, generationOK := sourceFields["generation"].(uint64)
	if !ok || sourceRun == "" || sourceRun == "." || sourceRun == ".." || filepath.Base(sourceRun) != sourceRun || !generationOK {
		return nil, crossRunIdentity{}, &CrossRunBootstrapError{Reason: "cross-run bootstrap source is invalid"}
	}
	isoFields, err := crossRunObject(fields["successor_iso"], map[string]struct{}{"sha256": {}, "size": {}}, "cross-run bootstrap successor ISO")
	if err != nil {
		return nil, crossRunIdentity{}, &CrossRunBootstrapError{Reason: "cross-run bootstrap successor ISO has invalid fields"}
	}
	iso, err := decodeCrossRunIdentity(isoFields, "successor ISO", false)
	if err != nil {
		return nil, crossRunIdentity{}, err
	}
	handoffFields, err := crossRunObject(fields["handoff"], map[string]struct{}{"file": {}, "sha256": {}, "size": {}}, "cross-run bootstrap handoff")
	if err != nil {
		return nil, crossRunIdentity{}, &CrossRunBootstrapError{Reason: "cross-run bootstrap handoff has invalid fields"}
	}
	if handoffFields["file"] != CrossRunBootstrapHandoff {
		return nil, crossRunIdentity{}, &CrossRunBootstrapError{Reason: "cross-run bootstrap handoff file is invalid"}
	}
	handoff, err := decodeCrossRunIdentity(handoffFields, "handoff", false)
	if err != nil {
		return nil, crossRunIdentity{}, err
	}
	requestFields, err := crossRunObject(fields["feature_requests"], map[string]struct{}{"count": {}, "file": {}, "ids": {}, "sha256": {}, "size": {}}, "cross-run bootstrap ledger")
	if err != nil {
		return nil, crossRunIdentity{}, &CrossRunBootstrapError{Reason: "cross-run bootstrap ledger has invalid fields"}
	}
	if requestFields["file"] != CrossRunBootstrapFeatureLedger {
		return nil, crossRunIdentity{}, &CrossRunBootstrapError{Reason: "cross-run bootstrap feature ledger file is invalid"}
	}
	count, countOK := requestFields["count"].(uint64)
	idsValue, idsOK := requestFields["ids"].([]any)
	if !countOK || !idsOK || count > uint64(len(idsValue)) {
		return nil, crossRunIdentity{}, &CrossRunBootstrapError{Reason: "cross-run bootstrap feature-request identity is invalid"}
	}
	ids := make([]uint64, len(idsValue))
	for index, value := range idsValue {
		id, idErr := crossRunUint(value)
		if idErr != nil || id == 0 || (index > 0 && id <= ids[index-1]) {
			return nil, crossRunIdentity{}, &CrossRunBootstrapError{Reason: "cross-run bootstrap feature-request identity is invalid"}
		}
		ids[index] = id
	}
	if count != uint64(len(ids)) {
		return nil, crossRunIdentity{}, &CrossRunBootstrapError{Reason: "cross-run bootstrap feature-request identity is invalid"}
	}
	ledgerHash, hashErr := crossRunDigest(requestFields["sha256"], "feature-request ledger", false)
	if hashErr != nil {
		return nil, crossRunIdentity{}, hashErr
	}
	ledgerSize, sizeOK := requestFields["size"].(uint64)
	if !sizeOK {
		return nil, crossRunIdentity{}, &CrossRunBootstrapError{Reason: "cross-run bootstrap feature ledger size is invalid"}
	}
	gitFields, err := crossRunObject(fields["git_base"], map[string]struct{}{"ref": {}, "commit": {}}, "cross-run bootstrap Git base")
	if err != nil {
		return nil, crossRunIdentity{}, &CrossRunBootstrapError{Reason: "cross-run bootstrap Git base has invalid fields"}
	}
	gitRef, refOK := gitFields["ref"].(string)
	gitCommit, commitOK := gitFields["commit"].(string)
	if !refOK || gitRef == "" {
		return nil, crossRunIdentity{}, &CrossRunBootstrapError{Reason: "cross-run bootstrap Git ref is invalid"}
	}
	if !commitOK || !validCrossRunSHA(gitCommit, true) {
		return nil, crossRunIdentity{}, &CrossRunBootstrapError{Reason: "cross-run bootstrap Git commit digest is invalid"}
	}
	bootstrap := &CrossRunBootstrap{
		SourceRun:           sourceRun,
		SourceGeneration:    generation,
		SuccessorISOSHA256:  iso.SHA256,
		SuccessorISOSize:    iso.Size,
		FeatureLedgerSHA256: ledgerHash,
		FeatureLedgerSize:   ledgerSize,
		InheritedRequestIDs: ids,
		GitBaseRef:          gitRef,
		GitBaseCommit:       gitCommit,
	}
	if version == crossRunBootstrapSchemaVersion {
		harnessFields, harnessErr := crossRunObject(fields["harness"], map[string]struct{}{"destination": {}, "source_generation": {}}, "cross-run bootstrap harness identity")
		if harnessErr != nil {
			return nil, crossRunIdentity{}, &CrossRunBootstrapError{Reason: "cross-run bootstrap harness identity has invalid fields"}
		}
		destination, parseErr := crossRunHarnessIdentity(harnessFields["destination"])
		if parseErr != nil || destination == nil {
			return nil, crossRunIdentity{}, &CrossRunBootstrapError{Reason: "cross-run bootstrap destination harness identity is invalid", Err: parseErr}
		}
		var sourceIdentity *provenance.HarnessIdentity
		if harnessFields["source_generation"] != nil {
			sourceIdentity, parseErr = crossRunHarnessIdentity(harnessFields["source_generation"])
			if parseErr != nil || sourceIdentity == nil {
				return nil, crossRunIdentity{}, &CrossRunBootstrapError{Reason: "cross-run bootstrap source harness identity is invalid", Err: parseErr}
			}
		}
		bootstrap.DestinationHarnessIdentity = destination
		bootstrap.SourceHarnessIdentity = sourceIdentity
	}
	return bootstrap, handoff, nil
}

func crossRunHarnessIdentity(value any) (*provenance.HarnessIdentity, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	identity, err := provenance.ParseHarnessIdentity(encoded)
	if err != nil {
		return nil, err
	}
	return &identity, nil
}

func decodeCrossRunIdentity(fields map[string]any, label string, allowGit bool) (crossRunIdentity, error) {
	digest, err := crossRunDigest(fields["sha256"], label, allowGit)
	if err != nil {
		return crossRunIdentity{}, err
	}
	size, ok := fields["size"].(uint64)
	if !ok {
		return crossRunIdentity{}, &CrossRunBootstrapError{Reason: "cross-run bootstrap " + label + " size is invalid"}
	}
	return crossRunIdentity{SHA256: digest, Size: size}, nil
}

func crossRunDigest(value any, label string, allowGit bool) (string, error) {
	digest, ok := value.(string)
	if !ok || !validCrossRunSHA(digest, allowGit) {
		return "", &CrossRunBootstrapError{Reason: "cross-run bootstrap " + label + " digest is invalid"}
	}
	return digest, nil
}

func validCrossRunSHA(value string, allowGit bool) bool {
	minimum := 64
	if allowGit {
		minimum = 40
	}
	if len(value) < minimum || (!allowGit && len(value) != 64) {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func decodeCrossRunFeatureLedger(contents []byte) ([]FeatureRequest, error) {
	fields, err := decodeCrossRunRawObject(contents)
	if err != nil {
		return nil, &CrossRunBootstrapError{Reason: "cross-run bootstrap feature ledger is malformed", Err: err}
	}
	if len(fields) != 1 {
		return nil, &CrossRunBootstrapError{Reason: "cross-run bootstrap feature ledger has invalid fields"}
	}
	recordsRaw, ok := fields["requests"]
	if !ok {
		return nil, &CrossRunBootstrapError{Reason: "cross-run bootstrap feature ledger has invalid fields"}
	}
	var records []json.RawMessage
	if bytes.Equal(bytes.TrimSpace(recordsRaw), []byte("null")) || json.Unmarshal(recordsRaw, &records) != nil {
		return nil, &CrossRunBootstrapError{Reason: "cross-run bootstrap feature ledger is invalid"}
	}
	requests := make([]FeatureRequest, len(records))
	for index, record := range records {
		recordFields, objectErr := decodeCrossRunRawObject(record)
		_, hasNote := recordFields["decision_note"]
		if objectErr != nil || (len(recordFields) != 5 && !(hasNote && len(recordFields) == 6)) {
			return nil, &CrossRunBootstrapError{Reason: "cross-run bootstrap feature ledger record is invalid"}
		}
		for _, key := range []string{"description", "generation", "id", "status", "title"} {
			if _, exists := recordFields[key]; !exists {
				return nil, &CrossRunBootstrapError{Reason: "cross-run bootstrap feature ledger record is invalid"}
			}
		}
		request, decodeErr := DecodeFeatureRequest(record)
		if decodeErr != nil {
			return nil, &CrossRunBootstrapError{Reason: "cross-run bootstrap feature ledger record is invalid", Err: decodeErr}
		}
		requests[index] = request
	}
	for index := 1; index < len(requests); index++ {
		if requests[index-1].ID >= requests[index].ID {
			return nil, &CrossRunBootstrapError{Reason: "cross-run bootstrap feature ledger is not canonical"}
		}
	}
	canonical, err := crossRunFeatureLedgerBytes(requests)
	if err != nil || !bytes.Equal(canonical, contents) {
		if err == nil {
			err = errors.New("ledger bytes differ from canonical encoding")
		}
		return nil, &CrossRunBootstrapError{Reason: "cross-run bootstrap feature ledger is not canonical", Err: err}
	}
	return requests, nil
}

func decodeCrossRunRawObject(contents []byte) (map[string]json.RawMessage, error) {
	if !utf8.Valid(contents) {
		return nil, errors.New("invalid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	var fields map[string]json.RawMessage
	if err := decoder.Decode(&fields); err != nil {
		return nil, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, errors.New("JSON value is not an object")
	}
	return fields, nil
}

func crossRunFeatureLedgerBytes(requests []FeatureRequest) ([]byte, error) {
	records := make([]crossRunFeatureRequestJSON, len(requests))
	for index, request := range requests {
		records[index] = crossRunFeatureRequestJSON{
			DecisionNote: request.DecisionNote,
			Description:  request.Description,
			Generation:   request.Generation,
			ID:           request.ID,
			Status:       request.Status,
			Title:        request.Title,
		}
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(crossRunFeatureLedgerJSON{Requests: records}); err != nil {
		return nil, err
	}
	return unescapeJSONLineSeparators(output.Bytes()), nil
}

func crossRunManifestBytes(bootstrap *CrossRunBootstrap, handoff []byte, requestCount int) ([]byte, error) {
	ids := append([]uint64(nil), bootstrap.InheritedRequestIDs...)
	if ids == nil {
		ids = []uint64{}
	}
	version := crossRunBootstrapLegacySchema
	handoffIdentity := crossRunBytesIdentity(handoff)
	value := crossRunManifestJSON{
		FeatureRequests: crossRunFeatureRequestsJSON{
			Count: requestCount, File: CrossRunBootstrapFeatureLedger, IDs: ids,
			SHA256: bootstrap.FeatureLedgerSHA256, Size: bootstrap.FeatureLedgerSize,
		},
		GitBase: crossRunGitBaseJSON{
			Commit: bootstrap.GitBaseCommit, Ref: bootstrap.GitBaseRef,
		},
		Handoff: crossRunFileJSON{
			File: CrossRunBootstrapHandoff, SHA256: handoffIdentity.SHA256, Size: uint64(len(handoff)),
		},
		SchemaVersion: version,
		Source: crossRunSourceJSON{
			Generation: bootstrap.SourceGeneration, Run: bootstrap.SourceRun,
		},
		SuccessorISO: provenance.FileIdentity{
			SHA256: bootstrap.SuccessorISOSHA256, Size: bootstrap.SuccessorISOSize,
		},
	}
	if bootstrap.DestinationHarnessIdentity != nil {
		value.SchemaVersion = crossRunBootstrapSchemaVersion
		value.Harness = &crossRunHarnessJSON{
			Destination:      crossRunHarnessJSONValue(*bootstrap.DestinationHarnessIdentity),
			SourceGeneration: crossRunOptionalHarnessIdentity(bootstrap.SourceHarnessIdentity),
		}
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return unescapeJSONLineSeparators(output.Bytes()), nil
}

func crossRunOptionalHarnessIdentity(identity *provenance.HarnessIdentity) *crossRunHarnessIdentityJSON {
	if identity == nil {
		return nil
	}
	value := crossRunHarnessJSONValue(*identity)
	return &value
}

func crossRunHarnessJSONValue(identity provenance.HarnessIdentity) crossRunHarnessIdentityJSON {
	return crossRunHarnessIdentityJSON{
		Build: identity.Build, DirtyTreeSHA256: identity.DirtyTreeSHA256,
		Executable: identity.Executable, RepositoryCommit: identity.RepositoryCommit,
		RepositoryDirty: identity.RepositoryDirty, SchemaVersion: identity.SchemaVersion,
	}
}

func crossRunBytesIdentity(contents []byte) crossRunIdentity {
	digest := sha256.Sum256(contents)
	return crossRunIdentity{SHA256: hex.EncodeToString(digest[:]), Size: uint64(len(contents))}
}

func crossRunFileIdentity(path, label string) (crossRunIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		if err == nil {
			err = errors.New("path is not a regular file")
		}
		return crossRunIdentity{}, &CrossRunBootstrapError{Reason: "file is unavailable: " + path, Err: err}
	}
	input, err := os.Open(path)
	if err != nil {
		return crossRunIdentity{}, &CrossRunBootstrapError{Reason: label + " is unavailable", Err: err}
	}
	digest := sha256.New()
	size, copyErr := io.CopyBuffer(digest, input, make([]byte, crossRunCopyBufferSize))
	closeErr := input.Close()
	if copyErr != nil {
		return crossRunIdentity{}, &CrossRunBootstrapError{Reason: label + " cannot be read", Err: copyErr}
	}
	if closeErr != nil {
		return crossRunIdentity{}, &CrossRunBootstrapError{Reason: label + " cannot be read", Err: closeErr}
	}
	return crossRunIdentity{SHA256: hex.EncodeToString(digest.Sum(nil)), Size: uint64(size)}, nil
}

func crossRunFilesEqual(left, right string) (bool, error) {
	leftInfo, err := os.Lstat(left)
	if err != nil || leftInfo.Mode()&os.ModeSymlink != 0 || !leftInfo.Mode().IsRegular() {
		if err == nil {
			err = errors.New("path is not a regular file")
		}
		return false, &CrossRunBootstrapError{Reason: "initial ISO is unavailable: " + left, Err: err}
	}
	rightInfo, err := os.Lstat(right)
	if err != nil || rightInfo.Mode()&os.ModeSymlink != 0 || !rightInfo.Mode().IsRegular() {
		if err == nil {
			err = errors.New("path is not a regular file")
		}
		return false, &CrossRunBootstrapError{Reason: "inherited successor ISO is unavailable", Err: err}
	}
	if leftInfo.Size() != rightInfo.Size() {
		return false, nil
	}
	leftFile, err := os.Open(left)
	if err != nil {
		return false, err
	}
	rightFile, err := os.Open(right)
	if err != nil {
		_ = leftFile.Close()
		return false, err
	}
	leftBuffer := make([]byte, crossRunCopyBufferSize)
	rightBuffer := make([]byte, crossRunCopyBufferSize)
	for {
		leftRead, leftErr := leftFile.Read(leftBuffer)
		rightRead, rightErr := rightFile.Read(rightBuffer)
		if leftRead != rightRead || !bytes.Equal(leftBuffer[:leftRead], rightBuffer[:rightRead]) {
			_ = leftFile.Close()
			_ = rightFile.Close()
			return false, nil
		}
		if leftErr == io.EOF || rightErr == io.EOF {
			_ = leftFile.Close()
			_ = rightFile.Close()
			if leftErr == io.EOF && rightErr == io.EOF {
				return true, nil
			}
			return false, nil
		}
		if leftErr != nil || rightErr != nil {
			_ = leftFile.Close()
			_ = rightFile.Close()
			if leftErr != nil {
				return false, leftErr
			}
			return false, rightErr
		}
	}
}

func writeCrossRunDurable(path string, contents []byte) error {
	output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return &CrossRunBootstrapError{Reason: "could not persist cross-run bootstrap record: " + filepath.Base(path), Err: err}
	}
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(path)
		}
	}()
	if written, writeErr := output.Write(contents); writeErr != nil || written != len(contents) {
		_ = output.Close()
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return &CrossRunBootstrapError{Reason: "could not persist cross-run bootstrap record: " + filepath.Base(path), Err: writeErr}
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		return &CrossRunBootstrapError{Reason: "could not persist cross-run bootstrap record: " + filepath.Base(path), Err: err}
	}
	if err := output.Close(); err != nil {
		return &CrossRunBootstrapError{Reason: "could not persist cross-run bootstrap record: " + filepath.Base(path), Err: err}
	}
	remove = false
	return nil
}

func syncCrossRunDirectory(path string, optional bool) error {
	if optional {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return nil
		}
	}
	directory, err := os.Open(path)
	if err != nil {
		return &CrossRunBootstrapError{Reason: "could not sync cross-run bootstrap directory", Err: err}
	}
	err = directory.Sync()
	closeErr := directory.Close()
	if err != nil {
		return &CrossRunBootstrapError{Reason: "could not sync cross-run bootstrap directory", Err: err}
	}
	if closeErr != nil {
		return &CrossRunBootstrapError{Reason: "could not close cross-run bootstrap directory", Err: closeErr}
	}
	return nil
}

func verifyCrossRunGitBase(repository, baseRef, expectedTag, runIdentifier string) (string, error) {
	repositoryPath, err := resolveCrossRunPath(repository)
	if err != nil {
		return "", &CrossRunBootstrapError{Reason: "Git repository is unavailable", Err: err}
	}
	info, err := os.Lstat(repositoryPath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		if err == nil {
			err = errors.New("path is not a real directory")
		}
		return "", &CrossRunBootstrapError{Reason: "Git repository is unavailable", Err: err}
	}
	if runIdentifier == "experiment" {
		return "", &CrossRunBootstrapError{Reason: "run identifier 'experiment' is reserved for legacy generation tags"}
	}
	topLevel, err := crossRunGitText(repositoryPath, []string{"rev-parse", "--show-toplevel"})
	if err != nil {
		return "", err
	}
	topResolved, err := resolveCrossRunPath(strings.TrimSpace(topLevel))
	if err != nil || filepath.Clean(topResolved) != filepath.Clean(repositoryPath) {
		if err == nil {
			err = errors.New("Git repository path must be its worktree root")
		}
		return "", &CrossRunBootstrapError{Reason: "Git repository path must be its worktree root", Err: err}
	}
	baseCommit, err := crossRunGitCommit(repositoryPath, baseRef)
	if err != nil {
		return "", err
	}
	// The generation tag must be a real annotated tag and must resolve to the
	// exact same commit as the configured base ref.
	tagType, err := crossRunGitText(repositoryPath, []string{"cat-file", "-t", "refs/tags/" + expectedTag})
	if err != nil {
		return "", &CrossRunBootstrapError{Reason: "required completed generation tag is missing: " + expectedTag, Err: err}
	}
	if strings.TrimSpace(tagType) != "tag" {
		return "", &CrossRunBootstrapError{Reason: "generation tag is not annotated: " + expectedTag}
	}
	tagCommit, err := crossRunGitCommit(repositoryPath, "refs/tags/"+expectedTag)
	if err != nil {
		return "", err
	}
	if tagCommit != baseCommit {
		return "", &CrossRunBootstrapError{Reason: "Git base commit does not match the inherited generation tag"}
	}
	return baseCommit, nil
}

func crossRunGitCommit(repository string, ref string) (string, error) {
	output, err := crossRunGitText(repository, []string{"rev-parse", "--verify", "--end-of-options", ref + "^{commit}"})
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(output)
	if !validCrossRunSHA(commit, true) {
		return "", &CrossRunBootstrapError{Reason: "Git base commit is invalid"}
	}
	return commit, nil
}

func crossRunGitText(repository string, arguments []string) (string, error) {
	contextValue, cancel := context.WithTimeout(context.Background(), crossRunGitTimeout)
	defer cancel()
	commandArguments := []string{"-c", "core.hooksPath=/dev/null", "-C", repository}
	commandArguments = append(commandArguments, arguments...)
	command := exec.CommandContext(contextValue, "git", commandArguments...)
	command.Stdin = nil
	var output crossRunBoundedOutput
	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()
	encoded, exceeded := output.result()
	if contextValue.Err() != nil {
		return "", &CrossRunBootstrapError{Reason: "Git command failed: " + arguments[0], Err: contextValue.Err()}
	}
	if exceeded {
		return "", &CrossRunBootstrapError{Reason: "Git command output exceeds 1 MiB: " + arguments[0]}
	}
	if err != nil {
		return "", &CrossRunBootstrapError{Reason: "Git command failed: " + arguments[0] + ": " + crossRunGitDiagnostics(encoded), Err: err}
	}
	if !utf8.Valid(encoded) {
		return "", &CrossRunBootstrapError{Reason: "Git returned non-UTF-8 output"}
	}
	return string(encoded), nil
}

type crossRunBoundedOutput struct {
	mu       sync.Mutex
	data     []byte
	exceeded bool
}

func (o *crossRunBoundedOutput) Write(value []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	accepted := len(value)
	remaining := crossRunGitOutputLimit - len(o.data)
	if remaining < len(value) {
		o.exceeded = true
		if remaining < 0 {
			remaining = 0
		}
		value = value[:remaining]
	}
	o.data = append(o.data, value...)
	return accepted, nil
}

func (o *crossRunBoundedOutput) result() ([]byte, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]byte(nil), o.data...), o.exceeded
}

func crossRunGitDiagnostics(output []byte) string {
	if len(output) > crossRunGitDiagnosticsLimit {
		output = output[:crossRunGitDiagnosticsLimit]
	}
	return strings.TrimSpace(string(output))
}

func validateCrossRunInheritedRequests(run string, inherited []FeatureRequest) error {
	store, err := NewFeatureRequestStore(run)
	if err != nil {
		return &CrossRunBootstrapError{Reason: "inherited feature-request state is invalid", Err: err}
	}
	current, err := store.Requests()
	if err != nil {
		return &CrossRunBootstrapError{Reason: "inherited feature-request state is invalid", Err: err}
	}
	currentByID := make(map[uint64]FeatureRequest, len(current))
	for _, request := range current {
		currentByID[request.ID] = request
	}
	generationArchived := false
	entries, readErr := os.ReadDir(run)
	if readErr != nil {
		return &CrossRunBootstrapError{Reason: "inherited feature-request state is invalid", Err: readErr}
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "generation-") {
			info, statErr := os.Stat(filepath.Join(run, entry.Name()))
			if statErr == nil && info.IsDir() {
				generationArchived = true
				break
			}
		}
	}
	inheritedIDs := make(map[uint64]struct{}, len(inherited))
	maximumInheritedID := uint64(0)
	for _, original := range inherited {
		inheritedIDs[original.ID] = struct{}{}
		if original.ID > maximumInheritedID {
			maximumInheritedID = original.ID
		}
		observed, exists := currentByID[original.ID]
		if !exists {
			return &CrossRunBootstrapError{Reason: fmt.Sprintf("inherited feature request #%d is missing", original.ID)}
		}
		statusMatches := observed.Status == original.Status
		noteMatches := observed.DecisionNote == original.DecisionNote
		if !statusMatches && generationArchived && original.Status == FeaturePending && (observed.Status == FeatureApproved || observed.Status == FeatureDenied) {
			statusMatches = true
			noteMatches = true // This pending request was decided at a destination gate.
		}
		if observed.Generation != original.Generation || observed.Title != original.Title || observed.Description != original.Description || !statusMatches || !noteMatches {
			return &CrossRunBootstrapError{Reason: fmt.Sprintf("inherited feature request #%d was altered", original.ID)}
		}
	}
	for _, request := range current {
		if _, inherited := inheritedIDs[request.ID]; !inherited && request.ID <= maximumInheritedID {
			return &CrossRunBootstrapError{Reason: "new feature request collides with an inherited identity"}
		}
	}
	return nil
}

func equalCrossRunRequestIDs(requests []FeatureRequest, ids []uint64) bool {
	if len(requests) != len(ids) {
		return false
	}
	for index, request := range requests {
		if request.ID != ids[index] {
			return false
		}
	}
	return true
}

func cloneCrossRunRequestIDs(ids []uint64) []uint64 {
	clone := make([]uint64, len(ids))
	copy(clone, ids)
	return clone
}
