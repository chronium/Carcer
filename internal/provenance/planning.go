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
	"unicode/utf16"
	"unicode/utf8"
)

const planningSchemaVersion = 2

type PlanningEvidenceError struct {
	Reason string
	Err    error
}

func (e *PlanningEvidenceError) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *PlanningEvidenceError) Unwrap() error { return e.Err }

type PlanningResponseIdentity struct {
	SHA256 string
	Size   uint64
}

type PlanningEvidenceStore struct {
	root string
}

func NewPlanningEvidenceStore(runDirectory string) *PlanningEvidenceStore {
	return &PlanningEvidenceStore{root: filepath.Join(runDirectory, "planning-evidence")}
}

func (s *PlanningEvidenceStore) Begin(generation uint64, threadID string) (*PlanningEvidence, error) {
	if threadID == "" {
		return nil, fmt.Errorf("planning thread ID must not be empty")
	}
	if !utf8.ValidString(threadID) {
		return nil, fmt.Errorf("planning thread ID must be valid UTF-8")
	}
	directory := filepath.Join(s.root, fmt.Sprintf("generation-%04d", generation))
	if rootInfo, statErr := os.Lstat(s.root); statErr == nil && !rootInfo.IsDir() {
		if rootInfo.Mode()&os.ModeSymlink == 0 {
			return nil, &PlanningEvidenceError{Reason: fmt.Sprintf("planning evidence already exists for generation %d", generation), Err: os.ErrExist}
		}
		targetInfo, targetErr := os.Stat(s.root)
		if targetErr != nil || !targetInfo.IsDir() {
			return nil, &PlanningEvidenceError{Reason: fmt.Sprintf("planning evidence already exists for generation %d", generation), Err: os.ErrExist}
		}
	}
	if err := os.MkdirAll(s.root, 0o777); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, &PlanningEvidenceError{Reason: fmt.Sprintf("planning evidence already exists for generation %d", generation), Err: err}
		}
		return nil, &PlanningEvidenceError{Reason: "cannot allocate planning evidence", Err: err}
	}
	rootInfo, err := os.Lstat(s.root)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, &PlanningEvidenceError{Reason: "planning evidence root is unsafe", Err: err}
	}
	if err := os.Mkdir(directory, 0o777); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, &PlanningEvidenceError{Reason: fmt.Sprintf("planning evidence already exists for generation %d", generation), Err: err}
		}
		return nil, &PlanningEvidenceError{Reason: "cannot allocate planning evidence", Err: err}
	}
	manifest := planningManifest{
		Attempts:      []planningAttempt{},
		Generation:    generation,
		Kind:          "generation_plan",
		Outcome:       "incomplete",
		SchemaVersion: planningSchemaVersion,
		Stage:         "allocated",
		ThreadID:      threadID,
	}
	if err := writePlanningJSON(filepath.Join(directory, "manifest.json"), manifest); err != nil {
		return nil, err
	}
	return &PlanningEvidence{directory: directory, manifest: manifest}, nil
}

type PlanningEvidence struct {
	directory string
	manifest  planningManifest
}

func (e *PlanningEvidence) Generation() uint64 { return e.manifest.Generation }

func (e *PlanningEvidence) RecordStarted(turnID string) error {
	if turnID == "" {
		return fmt.Errorf("planning turn ID must not be empty")
	}
	if !utf8.ValidString(turnID) {
		return fmt.Errorf("planning turn ID must be valid UTF-8")
	}
	if e.manifest.Stage != "allocated" && e.manifest.Stage != "awaiting_resume" {
		return &PlanningEvidenceError{Reason: "planning evidence cannot start another attempt"}
	}
	attemptNumber := uint64(len(e.manifest.Attempts) + 1)
	candidate := e.manifest
	candidate.Attempts = append([]planningAttempt(nil), e.manifest.Attempts...)
	candidate.Attempts = append(candidate.Attempts, planningAttempt{
		Attempt: attemptNumber,
		Outcome: "active",
		TurnID:  stringPointer(turnID),
	})
	candidate.TurnID = stringPointer(turnID)
	candidate.Stage = "started"
	if err := writePlanningJSON(filepath.Join(e.directory, "manifest.json"), candidate); err != nil {
		return err
	}
	e.manifest = candidate
	return nil
}

func (e *PlanningEvidence) Complete(outcome string, response *string) (PlanningResponseIdentity, error) {
	if outcome != "completed" && outcome != "interrupted" {
		return PlanningResponseIdentity{}, fmt.Errorf("planning completion outcome is invalid")
	}
	activeAttempt, err := e.activeAttempt()
	if err != nil {
		return PlanningResponseIdentity{}, err
	}
	exactResponse := ""
	if response != nil {
		exactResponse = *response
	}
	if !utf8.ValidString(exactResponse) {
		return PlanningResponseIdentity{}, fmt.Errorf("planning response must be valid UTF-8")
	}
	encoded := []byte(exactResponse)
	digest := sha256.Sum256(encoded)
	identity := PlanningResponseIdentity{SHA256: hex.EncodeToString(digest[:]), Size: uint64(len(encoded))}
	responseFile := "response.txt"
	if outcome == "interrupted" {
		responseFile = fmt.Sprintf("attempt-%04d-response.txt", activeAttempt.Attempt)
	}
	if err := writePlanningBytes(filepath.Join(e.directory, responseFile), encoded); err != nil {
		return PlanningResponseIdentity{}, err
	}
	present := response != nil
	candidate := e.manifest
	candidate.Attempts = append([]planningAttempt(nil), e.manifest.Attempts...)
	attempt := &candidate.Attempts[len(candidate.Attempts)-1]
	attempt.Outcome = outcome
	attempt.ResponseBytes = uint64Pointer(identity.Size)
	attempt.ResponseFile = responseFile
	attempt.ResponsePresent = boolPointer(present)
	attempt.ResponseSHA256 = identity.SHA256
	if outcome == "completed" {
		candidate.Stage = "completed"
		candidate.Outcome = "completed"
		candidate.ResponseBytes = uint64Pointer(identity.Size)
		candidate.ResponseFile = responseFile
		candidate.ResponsePresent = boolPointer(present)
		candidate.ResponseSHA256 = identity.SHA256
	} else {
		candidate.Stage = "awaiting_resume"
		candidate.Outcome = "incomplete"
	}
	if err := writePlanningJSON(filepath.Join(e.directory, "manifest.json"), candidate); err != nil {
		return PlanningResponseIdentity{}, err
	}
	e.manifest = candidate
	return identity, nil
}

func (e *PlanningEvidence) Fail() error {
	if e.manifest.Outcome == "completed" || e.manifest.Outcome == "failed" {
		return nil
	}
	candidate := e.manifest
	candidate.Attempts = append([]planningAttempt(nil), e.manifest.Attempts...)
	if candidate.Stage == "started" {
		attempt, err := candidateActiveAttempt(candidate)
		if err != nil {
			return err
		}
		attempt.Outcome = "failed"
	} else {
		candidate.Attempts = append(candidate.Attempts, planningAttempt{
			Attempt: uint64(len(candidate.Attempts) + 1),
			Outcome: "failed",
		})
	}
	candidate.Stage = "completed"
	candidate.Outcome = "failed"
	if err := writePlanningJSON(filepath.Join(e.directory, "manifest.json"), candidate); err != nil {
		return err
	}
	e.manifest = candidate
	return nil
}

// RecordRetryableFailure records a failed planning attempt while preserving
// the evidence object for a later planning turn on the same Codex thread.
func (e *PlanningEvidence) RecordRetryableFailure() error {
	activeAttempt, err := e.activeAttempt()
	if err != nil {
		return err
	}
	candidate := e.manifest
	candidate.Attempts = append([]planningAttempt(nil), e.manifest.Attempts...)
	attempt := &candidate.Attempts[len(candidate.Attempts)-1]
	if attempt.Attempt != activeAttempt.Attempt {
		return &PlanningEvidenceError{Reason: "planning evidence active attempt changed"}
	}
	attempt.Outcome = "failed"
	candidate.Stage = "awaiting_resume"
	candidate.Outcome = "incomplete"
	if err := writePlanningJSON(filepath.Join(e.directory, "manifest.json"), candidate); err != nil {
		return err
	}
	e.manifest = candidate
	return nil
}

func candidateActiveAttempt(manifest planningManifest) (*planningAttempt, error) {
	if manifest.Stage != "started" || len(manifest.Attempts) == 0 {
		return nil, &PlanningEvidenceError{Reason: "planning evidence is not active"}
	}
	attempt := &manifest.Attempts[len(manifest.Attempts)-1]
	if attempt.Outcome != "active" {
		return nil, &PlanningEvidenceError{Reason: "planning evidence has no active attempt"}
	}
	return attempt, nil
}

func (e *PlanningEvidence) activeAttempt() (*planningAttempt, error) {
	if e.manifest.Stage != "started" || len(e.manifest.Attempts) == 0 {
		return nil, &PlanningEvidenceError{Reason: "planning evidence is not active"}
	}
	attempt := &e.manifest.Attempts[len(e.manifest.Attempts)-1]
	if attempt.Outcome != "active" {
		return nil, &PlanningEvidenceError{Reason: "planning evidence has no active attempt"}
	}
	return attempt, nil
}

// Fields are alphabetized to match json.dumps(sort_keys=True).
type planningManifest struct {
	Attempts        []planningAttempt `json:"attempts"`
	Generation      uint64            `json:"generation"`
	Kind            string            `json:"kind"`
	Outcome         string            `json:"outcome"`
	ResponseBytes   *uint64           `json:"response_bytes,omitempty"`
	ResponseFile    string            `json:"response_file,omitempty"`
	ResponsePresent *bool             `json:"response_present,omitempty"`
	ResponseSHA256  string            `json:"response_sha256,omitempty"`
	SchemaVersion   uint64            `json:"schema_version"`
	Stage           string            `json:"stage"`
	ThreadID        string            `json:"thread_id"`
	TurnID          *string           `json:"turn_id"`
}

type planningAttempt struct {
	Attempt         uint64  `json:"attempt"`
	Outcome         string  `json:"outcome"`
	ResponseBytes   *uint64 `json:"response_bytes,omitempty"`
	ResponseFile    string  `json:"response_file,omitempty"`
	ResponsePresent *bool   `json:"response_present,omitempty"`
	ResponseSHA256  string  `json:"response_sha256,omitempty"`
	TurnID          *string `json:"turn_id"`
}

func writePlanningJSON(path string, value planningManifest) error {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return &PlanningEvidenceError{Reason: fmt.Sprintf("cannot write planning evidence %s", filepath.Base(path)), Err: err}
	}
	return writePlanningBytes(path, asciiJSON(output.Bytes()))
}

func asciiJSON(encoded []byte) []byte {
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

func writePlanningBytes(path string, value []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-")
	if err != nil {
		return planningWriteError(path, err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if written, err := temporary.Write(value); err != nil || written != len(value) {
		temporary.Close()
		if err == nil {
			err = io.ErrShortWrite
		}
		return planningWriteError(path, err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return planningWriteError(path, err)
	}
	if err := temporary.Close(); err != nil {
		return planningWriteError(path, err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return planningWriteError(path, err)
	}
	return nil
}

func planningWriteError(path string, err error) error {
	return &PlanningEvidenceError{Reason: fmt.Sprintf("cannot write planning evidence %s", filepath.Base(path)), Err: err}
}

func stringPointer(value string) *string { return &value }
func uint64Pointer(value uint64) *uint64 { return &value }
func boolPointer(value bool) *bool       { return &value }
