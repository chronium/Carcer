package provenance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unicode/utf16"
	"unicode/utf8"
)

const forensicSchemaVersion uint64 = 1

// ForensicProvenanceError reports a trusted evidence failure.  Build
// provenance callers use this error to fail closed; review callers use it to
// mark evidence incomplete without changing the observed review result.
type ForensicProvenanceError struct {
	Reason string
	Err    error
}

func (e *ForensicProvenanceError) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *ForensicProvenanceError) Unwrap() error { return e.Err }

// FileIdentity is the forensic identity of exact bytes.
type FileIdentity struct {
	SHA256 string `json:"sha256"`
	Size   uint64 `json:"size"`
}

// AsJSON returns the schema representation used in provenance manifests.
func (i FileIdentity) AsJSON() map[string]any {
	return map[string]any{"sha256": i.SHA256, "size": i.Size}
}

// FileIdentityFromPath hashes a file without loading the entire file into
// memory.  Path errors are returned unchanged so callers can distinguish an
// artifact I/O failure from evidence-storage failure.
func FileIdentityFromPath(path string) (FileIdentity, error) {
	input, err := os.Open(path)
	if err != nil {
		return FileIdentity{}, err
	}
	digest := sha256.New()
	size, copyErr := io.CopyBuffer(digest, input, make([]byte, 1024*1024))
	closeErr := input.Close()
	if copyErr != nil {
		return FileIdentity{}, copyErr
	}
	if closeErr != nil {
		return FileIdentity{}, closeErr
	}
	return FileIdentity{SHA256: hex.EncodeToString(digest.Sum(nil)), Size: uint64(size)}, nil
}

// ForensicEventRecorder is optional trusted observability.  Identity fields
// are provided for correlation; source and reviewer bytes are never passed.
type ForensicEventRecorder func(event string, generation uint64, data map[string]any)

// BuildReviewProvenance allocates run-local immutable build and review
// evidence.  The optional recorder receives structured, non-content events.
type BuildReviewProvenance struct {
	root     string
	recorder ForensicEventRecorder
	mutex    sync.Mutex
}

func NewBuildReviewProvenance(runDirectory string, recorder ...ForensicEventRecorder) *BuildReviewProvenance {
	var eventRecorder ForensicEventRecorder
	if len(recorder) > 0 {
		eventRecorder = recorder[0]
	}
	return &BuildReviewProvenance{
		root:     filepath.Join(runDirectory, "build-review-provenance"),
		recorder: eventRecorder,
	}
}

// BeginBuild reserves an attempt ID before any build processing occurs.  A
// nil snapshot represents the Python reference's absent snapshot field; an
// empty non-nil slice is an exact empty snapshot.
func (s *BuildReviewProvenance) BeginBuild(generation uint64, snapshot []byte) (*BuildAttemptEvidence, error) {
	directory, attemptID, err := s.allocate(generation, "build")
	if err != nil {
		return nil, err
	}
	manifest := map[string]any{
		"schema_version": forensicSchemaVersion,
		"kind":           "build_attempt",
		"generation":     generation,
		"attempt_id":     attemptID,
		"stage":          "received",
		"outcome":        "incomplete",
	}
	if snapshot != nil {
		manifest["source_snapshot"] = map[string]any{
			"sha256":  hashBytes(snapshot),
			"size":    uint64(len(snapshot)),
			"decoded": false,
		}
	}
	if err := writeForensicJSON(filepath.Join(directory, "manifest.json"), manifest); err != nil {
		return nil, err
	}
	evidence := &BuildAttemptEvidence{
		directory:  directory,
		manifest:   manifest,
		generation: generation,
		attemptID:  attemptID,
		recorder:   s.recorder,
	}
	evidence.recordEvent("build_attempt_received", nil)
	return evidence, nil
}

func (s *BuildReviewProvenance) BeginReview(generation uint64) (*ReviewEvidence, error) {
	directory, reviewID, err := s.allocate(generation, "review")
	if err != nil {
		return nil, err
	}
	manifest := map[string]any{
		"schema_version":    forensicSchemaVersion,
		"kind":              "review",
		"generation":        generation,
		"review_id":         reviewID,
		"stage":             "started",
		"review_outcome":    "incomplete",
		"capture_outcome":   "in_progress",
		"evidence_complete": false,
		"source_reads":      []map[string]any{},
	}
	if err := writeForensicJSON(filepath.Join(directory, "manifest.json"), manifest); err != nil {
		return nil, err
	}
	return &ReviewEvidence{
		directory:  directory,
		manifest:   manifest,
		generation: generation,
		reviewID:   reviewID,
		recorder:   s.recorder,
	}, nil
}

func (s *BuildReviewProvenance) allocate(generation uint64, kind string) (string, string, error) {
	generationDirectory := filepath.Join(s.root, fmt.Sprintf("generation-%04d", generation))
	if err := os.MkdirAll(generationDirectory, 0o777); err != nil {
		return "", "", forensicAllocateError(kind, err)
	}
	info, err := os.Lstat(generationDirectory)
	if err != nil {
		return "", "", forensicAllocateError(kind, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", "", &ForensicProvenanceError{Reason: "generation provenance directory is unsafe"}
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()
	sequence, err := nextForensicSequence(generationDirectory, generation, kind)
	if err != nil {
		var forensicErr *ForensicProvenanceError
		if errors.As(err, &forensicErr) {
			return "", "", err
		}
		return "", "", forensicAllocateError(kind, err)
	}
	for {
		identifier := fmt.Sprintf("%s-%06d", kind, sequence)
		directory := filepath.Join(generationDirectory, identifier)
		err := os.Mkdir(directory, 0o777)
		if err == nil {
			return directory, identifier, nil
		}
		if errors.Is(err, os.ErrExist) {
			sequence++
			continue
		}
		return "", "", forensicAllocateError(kind, err)
	}
}

func forensicAllocateError(kind string, err error) error {
	return &ForensicProvenanceError{
		Reason: fmt.Sprintf("cannot allocate %s provenance", kind),
		Err:    err,
	}
}

func nextForensicSequence(generationDirectory string, generation uint64, kind string) (uint64, error) {
	entries, err := os.ReadDir(generationDirectory)
	if err != nil {
		return 0, err
	}
	highest := uint64(0)
	prefix := kind + "-"
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		path := filepath.Join(generationDirectory, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return 0, err
		}
		suffix := strings.TrimPrefix(entry.Name(), prefix)
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || !sixASCIIDigits(suffix) {
			return 0, &ForensicProvenanceError{Reason: fmt.Sprintf("malformed %s provenance entry: %s", kind, entry.Name())}
		}
		sequence, err := strconv.ParseUint(suffix, 10, 64)
		if err != nil || sequence < 1 {
			return 0, &ForensicProvenanceError{Reason: fmt.Sprintf("malformed %s provenance entry: %s", kind, entry.Name()), Err: err}
		}
		if sequence > highest {
			highest = sequence
		}

		manifestPath := filepath.Join(path, "manifest.json")
		if _, err := os.Stat(manifestPath); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return 0, malformedForensicManifest(kind, entry.Name(), err)
		}
		contents, err := os.ReadFile(manifestPath)
		if err != nil || !utf8.Valid(contents) {
			if err == nil {
				err = errors.New("manifest is not valid UTF-8")
			}
			return 0, malformedForensicManifest(kind, entry.Name(), err)
		}
		if err := validateForensicManifest(contents, generation, kind, entry.Name()); err != nil {
			var consistencyErr *forensicManifestConsistencyError
			if errors.As(err, &consistencyErr) {
				return 0, &ForensicProvenanceError{
					Reason: fmt.Sprintf("unsupported or inconsistent %s provenance: %s", kind, entry.Name()),
					Err:    consistencyErr.Err,
				}
			}
			return 0, malformedForensicManifest(kind, entry.Name(), err)
		}
	}
	return highest + 1, nil
}

func sixASCIIDigits(value string) bool {
	if len(value) != 6 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func malformedForensicManifest(kind, identifier string, err error) error {
	return &ForensicProvenanceError{
		Reason: fmt.Sprintf("malformed %s provenance manifest: %s", kind, identifier),
		Err:    err,
	}
}

type forensicManifestConsistencyError struct{ Err error }

func (e *forensicManifestConsistencyError) Error() string {
	if e.Err == nil {
		return "manifest metadata is inconsistent"
	}
	return e.Err.Error()
}

func (e *forensicManifestConsistencyError) Unwrap() error { return e.Err }

func validateForensicManifest(contents []byte, generation uint64, kind, identifier string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(contents, &fields); err != nil || fields == nil {
		if err == nil {
			return &forensicManifestConsistencyError{Err: errors.New("manifest is not a JSON object")}
		}
		if json.Valid(contents) {
			return &forensicManifestConsistencyError{Err: err}
		}
		return err
	}
	var schemaVersion uint64
	if err := decodeForensicUint(fields["schema_version"], &schemaVersion); err != nil || schemaVersion != forensicSchemaVersion {
		if err == nil {
			err = fmt.Errorf("unsupported schema version %d", schemaVersion)
		}
		return &forensicManifestConsistencyError{Err: err}
	}
	expectedKind := "build_attempt"
	identifierKey := "attempt_id"
	if kind == "review" {
		expectedKind = "review"
		identifierKey = "review_id"
	}
	var actualKind, actualIdentifier string
	if err := json.Unmarshal(fields["kind"], &actualKind); err != nil {
		return &forensicManifestConsistencyError{Err: err}
	}
	if err := json.Unmarshal(fields[identifierKey], &actualIdentifier); err != nil {
		return &forensicManifestConsistencyError{Err: err}
	}
	var actualGeneration uint64
	if err := decodeForensicUint(fields["generation"], &actualGeneration); err != nil {
		return &forensicManifestConsistencyError{Err: err}
	}
	if actualKind != expectedKind || actualGeneration != generation || actualIdentifier != identifier {
		return &forensicManifestConsistencyError{Err: errors.New("manifest metadata does not match allocation")}
	}
	return nil
}

func decodeForensicUint(raw json.RawMessage, destination *uint64) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("expected an unsigned integer")
	}
	return json.Unmarshal(raw, destination)
}

// BuildAttemptEvidence is the mutable in-memory view of one allocated build
// attempt. Every successful mutation is published through an atomic manifest
// replacement.
type BuildAttemptEvidence struct {
	directory  string
	manifest   map[string]any
	generation uint64
	attemptID  string
	recorder   ForensicEventRecorder
}

func (e *BuildAttemptEvidence) AttemptID() string { return e.attemptID }

func (e *BuildAttemptEvidence) Generation() uint64 { return e.generation }

func (e *BuildAttemptEvidence) SourceIdentity() (FileIdentity, error) {
	value, ok := e.manifest["source_snapshot"]
	if !ok {
		return FileIdentity{}, &ForensicProvenanceError{Reason: "build attempt has no source identity"}
	}
	identity, ok := forensicIdentity(value)
	if !ok {
		return FileIdentity{}, &ForensicProvenanceError{Reason: "build attempt has no source identity"}
	}
	return identity, nil
}

func (e *BuildAttemptEvidence) KernelIdentity() (FileIdentity, error) {
	return e.artifactIdentity("kernel")
}

func (e *BuildAttemptEvidence) ISOIdentity() (FileIdentity, error) {
	return e.artifactIdentity("iso")
}

func (e *BuildAttemptEvidence) RecordDecoded(fileCount, contentSize uint64) error {
	value, ok := e.manifest["source_snapshot"]
	if !ok {
		return &ForensicProvenanceError{Reason: "decoded build has no source snapshot"}
	}
	source, ok := value.(map[string]any)
	if !ok {
		return &ForensicProvenanceError{Reason: "decoded build has no source snapshot"}
	}
	source["decoded"] = true
	source["file_count"] = fileCount
	source["content_size"] = contentSize
	if err := e.update("decoded"); err != nil {
		return err
	}
	e.recordEvent("build_attempt_decoded", nil)
	return nil
}

func (e *BuildAttemptEvidence) RecordCompileFailure(outcome string) error {
	e.manifest["compile"] = map[string]any{"outcome": outcome}
	if err := e.update("compilation_completed"); err != nil {
		return err
	}
	e.recordEvent("build_compilation_completed", map[string]any{"outcome": outcome})
	return e.RecordFinal(outcome)
}

func (e *BuildAttemptEvidence) RecordArtifacts(kernel, iso FileIdentity) error {
	e.manifest["compile"] = map[string]any{"outcome": "success"}
	if err := e.update("compilation_completed"); err != nil {
		return err
	}
	e.recordEvent("build_compilation_completed", map[string]any{"outcome": "success"})
	e.manifest["artifacts"] = map[string]any{
		"kernel": kernel.AsJSON(),
		"iso":    iso.AsJSON(),
	}
	if err := e.update("artifacts_produced"); err != nil {
		return err
	}
	e.recordEvent("build_artifacts_produced", nil)
	return nil
}

// RecordCandidateStage records candidate boot/protocol stages. The optional
// map is merged into candidate_validation after stage is set.
func (e *BuildAttemptEvidence) RecordCandidateStage(event, stage string, data ...map[string]any) error {
	value, exists := e.manifest["candidate_validation"]
	var candidate map[string]any
	if !exists {
		candidate = map[string]any{}
		e.manifest["candidate_validation"] = candidate
	} else {
		var ok bool
		candidate, ok = value.(map[string]any)
		if !ok {
			return &ForensicProvenanceError{Reason: "candidate provenance is malformed"}
		}
	}
	candidate["stage"] = stage
	if len(data) > 0 {
		for key, value := range data[0] {
			candidate[key] = value
		}
	}
	if err := e.update(stage); err != nil {
		return err
	}
	e.recordEvent(event, dataOrNil(data))
	return nil
}

func dataOrNil(data []map[string]any) map[string]any {
	if len(data) == 0 {
		return nil
	}
	return data[0]
}

func (e *BuildAttemptEvidence) RecordFinal(outcome string) error {
	if err := e.complete(outcome); err != nil {
		return err
	}
	e.recordEvent("build_attempt_completed", map[string]any{"outcome": outcome})
	return nil
}

func (e *BuildAttemptEvidence) RecordLatestSuccess(snapshot []byte) error {
	if err := writeForensicBytes(filepath.Join(e.directory, "source.snapshot"), snapshot); err != nil {
		return err
	}
	source, err := e.SourceIdentity()
	if err != nil {
		return err
	}
	kernel, err := e.KernelIdentity()
	if err != nil {
		return err
	}
	iso, err := e.ISOIdentity()
	if err != nil {
		return err
	}
	e.manifest["outcome"] = "success"
	e.manifest["source_snapshot_file"] = "source.snapshot"
	e.manifest["latest_success"] = map[string]any{
		"ready":              true,
		"protocol_validated": true,
		"source_snapshot":    source.AsJSON(),
		"kernel":             kernel.AsJSON(),
		"iso":                iso.AsJSON(),
	}
	if err := e.update("latest_success"); err != nil {
		return err
	}
	e.recordEvent("build_attempt_completed", map[string]any{"outcome": "success"})
	return nil
}

func (e *BuildAttemptEvidence) RecordLatestSuccessUpdate() error {
	e.recordEvent("build_latest_success_updated", nil)
	return nil
}

// AbortedArchiveManifest returns the compact identity manifest used when an
// aborted generation archives its latest successful build. It never includes
// kernel or ISO bytes.
func (e *BuildAttemptEvidence) AbortedArchiveManifest() (map[string]any, error) {
	latest, ok := e.manifest["latest_success"].(map[string]any)
	if !ok {
		return nil, &ForensicProvenanceError{Reason: "build is not a latest success"}
	}
	return map[string]any{
		"schema_version":     forensicSchemaVersion,
		"generation":         e.generation,
		"build_attempt_id":   e.attemptID,
		"source_snapshot":    latest["source_snapshot"],
		"kernel":             latest["kernel"],
		"iso":                latest["iso"],
		"ready":              true,
		"protocol_validated": true,
	}, nil
}

func (e *BuildAttemptEvidence) recordEvent(event string, extra map[string]any) {
	if e.recorder == nil {
		return
	}
	data := map[string]any{
		"build_attempt_id": e.attemptID,
		"stage":            e.manifest["stage"],
	}
	if source, ok := e.manifest["source_snapshot"].(map[string]any); ok {
		if sha, exists := source["sha256"]; exists {
			data["source_snapshot_sha256"] = sha
		}
		if size, exists := source["size"]; exists {
			data["source_snapshot_bytes"] = size
		}
	}
	if artifacts, ok := e.manifest["artifacts"].(map[string]any); ok {
		for _, name := range []string{"kernel", "iso"} {
			if identity, ok := artifacts[name].(map[string]any); ok {
				if sha, exists := identity["sha256"]; exists {
					data[name+"_sha256"] = sha
				}
				if size, exists := identity["size"]; exists {
					data[name+"_bytes"] = size
				}
			}
		}
	}
	for key, value := range extra {
		data[key] = value
	}
	e.recorder(event, e.generation, data)
}

func (e *BuildAttemptEvidence) artifactIdentity(name string) (FileIdentity, error) {
	artifacts, ok := e.manifest["artifacts"].(map[string]any)
	if !ok {
		return FileIdentity{}, &ForensicProvenanceError{Reason: fmt.Sprintf("build attempt has no %s identity", name)}
	}
	identity, ok := forensicIdentity(artifacts[name])
	if !ok {
		return FileIdentity{}, &ForensicProvenanceError{Reason: fmt.Sprintf("build attempt has no %s identity", name)}
	}
	return identity, nil
}

func (e *BuildAttemptEvidence) complete(outcome string) error {
	e.manifest["outcome"] = outcome
	return e.update("completed")
}

func (e *BuildAttemptEvidence) update(stage string) error {
	e.manifest["stage"] = stage
	return writeForensicJSON(filepath.Join(e.directory, "manifest.json"), e.manifest)
}

// ReviewEvidence records exact source-read results while keeping review
// outcome independent from capture completeness.
type ReviewEvidence struct {
	directory               string
	manifest                map[string]any
	generation              uint64
	reviewID                string
	recorder                ForensicEventRecorder
	irrecoverablyIncomplete bool
}

func (e *ReviewEvidence) ReviewID() string { return e.reviewID }

func (e *ReviewEvidence) Generation() uint64 { return e.generation }

// RecordSourceRead stores the exact returned bytes before publishing the
// corresponding manifest entry. Offset, length, and status intentionally use
// signed integers to preserve the Python reference's integer metadata.
func (e *ReviewEvidence) RecordSourceRead(path string, offset, length, status int64, output []byte) error {
	reads, ok := e.manifest["source_reads"].([]map[string]any)
	if !ok {
		return &ForensicProvenanceError{Reason: "review source-read provenance is malformed"}
	}
	sequence := uint64(len(reads) + 1)
	filename := fmt.Sprintf("read-%06d.bin", sequence)
	if err := writeForensicBytes(filepath.Join(e.directory, filename), output); err != nil {
		e.irrecoverablyIncomplete = true
		e.markCaptureIncomplete()
		return err
	}
	entry := map[string]any{
		"sequence":       sequence,
		"path":           path,
		"offset":         offset,
		"length":         length,
		"status":         status,
		"returned_bytes": uint64(len(output)),
		"sha256":         hashBytes(output),
		"content_file":   filename,
	}
	reads = append(reads, entry)
	e.manifest["source_reads"] = reads
	e.manifest["stage"] = "source_read"
	if err := e.update(); err != nil {
		// The content file and in-memory entry remain available for conservative
		// recovery during finalization, just as in the reference implementation.
		return err
	}
	if e.recorder != nil {
		e.recorder("review_source_read", e.generation, map[string]any{
			"review_id":       e.reviewID,
			"source_path":     path,
			"offset":          offset,
			"length":          length,
			"status":          status,
			"returned_bytes":  uint64(len(output)),
			"returned_sha256": hashBytes(output),
		})
	}
	return nil
}

func (e *ReviewEvidence) Complete(outcome string) error {
	e.manifest["stage"] = "completed"
	e.manifest["review_outcome"] = outcome
	complete := !e.irrecoverablyIncomplete && e.allSourceReadsAreVerifiable()
	e.manifest["evidence_complete"] = complete
	if complete {
		e.manifest["capture_outcome"] = "complete"
	} else {
		e.manifest["capture_outcome"] = "incomplete"
	}
	return e.update()
}

func (e *ReviewEvidence) markCaptureIncomplete() {
	e.manifest["capture_outcome"] = "incomplete"
	e.manifest["evidence_complete"] = false
	if err := e.update(); err != nil {
		// The initialized manifest already claims incomplete capture.  Do not
		// replace it with a potentially misleading final state on this path.
		return
	}
}

func (e *ReviewEvidence) update() error {
	return writeForensicJSON(filepath.Join(e.directory, "manifest.json"), e.manifest)
}

func (e *ReviewEvidence) allSourceReadsAreVerifiable() bool {
	reads, ok := e.manifest["source_reads"].([]map[string]any)
	if !ok {
		return false
	}
	expectedFiles := make(map[string]struct{}, len(reads))
	for index, entry := range reads {
		sequence, ok := forensicUintValue(entry["sequence"])
		if !ok || sequence != uint64(index+1) {
			return false
		}
		filename, ok := entry["content_file"].(string)
		if !ok || filename != fmt.Sprintf("read-%06d.bin", sequence) {
			return false
		}
		expectedFiles[filename] = struct{}{}
		contentPath := filepath.Join(e.directory, filename)
		info, err := os.Lstat(contentPath)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return false
		}
		content, err := os.ReadFile(contentPath)
		if err != nil {
			return false
		}
		returnedBytes, ok := forensicUintValue(entry["returned_bytes"])
		if !ok || returnedBytes != uint64(len(content)) {
			return false
		}
		sha, ok := entry["sha256"].(string)
		if !ok || sha != hashBytes(content) {
			return false
		}
	}
	entries, err := os.ReadDir(e.directory)
	if err != nil {
		return false
	}
	actualFiles := make(map[string]struct{})
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "read-") && strings.HasSuffix(entry.Name(), ".bin") {
			actualFiles[entry.Name()] = struct{}{}
		}
	}
	if len(actualFiles) != len(expectedFiles) {
		return false
	}
	for filename := range expectedFiles {
		if _, ok := actualFiles[filename]; !ok {
			return false
		}
	}
	return true
}

func forensicIdentity(value any) (FileIdentity, bool) {
	identity, ok := value.(map[string]any)
	if !ok {
		return FileIdentity{}, false
	}
	sha, ok := identity["sha256"].(string)
	if !ok {
		return FileIdentity{}, false
	}
	size, ok := forensicUintValue(identity["size"])
	if !ok {
		return FileIdentity{}, false
	}
	return FileIdentity{SHA256: sha, Size: size}, true
}

func forensicUintValue(value any) (uint64, bool) {
	switch number := value.(type) {
	case uint64:
		return number, true
	case uint32:
		return uint64(number), true
	case uint16:
		return uint64(number), true
	case uint8:
		return uint64(number), true
	case uint:
		return uint64(number), true
	case int64:
		if number < 0 {
			return 0, false
		}
		return uint64(number), true
	case int32:
		if number < 0 {
			return 0, false
		}
		return uint64(number), true
	case int16:
		if number < 0 {
			return 0, false
		}
		return uint64(number), true
	case int8:
		if number < 0 {
			return 0, false
		}
		return uint64(number), true
	case int:
		if number < 0 {
			return 0, false
		}
		return uint64(number), true
	case json.Number:
		parsed, err := strconv.ParseUint(string(number), 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func hashBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func writeForensicJSON(path string, value any) error {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return forensicWriteError(path, err)
	}
	return writeForensicBytes(path, forensicASCIIJSON(output.Bytes()))
}

func forensicASCIIJSON(encoded []byte) []byte {
	output := make([]byte, 0, len(encoded))
	for len(encoded) > 0 {
		r, size := utf8.DecodeRune(encoded)
		if r < utf8.RuneSelf {
			output = append(output, byte(r))
		} else if r <= 0xffff {
			output = fmt.Appendf(output, `\u%04x`, r)
		} else {
			first, second := utf16.EncodeRune(r)
			output = fmt.Appendf(output, `\u%04x\u%04x`, first, second)
		}
		encoded = encoded[size:]
	}
	return output
}

func writeForensicBytes(path string, value []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-")
	if err != nil {
		return forensicWriteError(path, err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if written, err := temporary.Write(value); err != nil || written != len(value) {
		_ = temporary.Close()
		if err == nil {
			err = io.ErrShortWrite
		}
		return forensicWriteError(path, err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return forensicWriteError(path, err)
	}
	if err := temporary.Close(); err != nil {
		return forensicWriteError(path, err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return forensicWriteError(path, err)
	}
	return nil
}

func forensicWriteError(path string, err error) error {
	return &ForensicProvenanceError{
		Reason: fmt.Sprintf("cannot write forensic provenance %s", filepath.Base(path)),
		Err:    err,
	}
}
