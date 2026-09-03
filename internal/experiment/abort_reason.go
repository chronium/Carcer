package experiment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxAbortReasonBytes bounds operator-controlled text copied into archives,
	// events, and the next generation's trusted prompt.
	MaxAbortReasonBytes = 8 * 1024

	abortReasonName         = "abort-reason.txt"
	operatorFeedbackDirName = "operator-feedback"
	operatorFeedbackLimit   = MaxAbortReasonBytes + 1024
	operatorFeedbackStaging = ".operator-feedback-attachment-*"
)

// OperatorFeedback is the immutable abort feedback attached to one generation.
// SourceAbortGeneration is evidence only and is never a lineage parent.
type OperatorFeedback struct {
	TargetGeneration      uint64 `json:"target_generation"`
	SourceAbortGeneration uint64 `json:"source_abort_generation"`
	Reason                string `json:"reason"`
	SchemaVersion         uint64 `json:"schema_version"`
}

// ValidateAbortReason validates operator text at every persistence boundary.
func ValidateAbortReason(reason string) error {
	if !utf8.ValidString(reason) {
		return &GenerationRuntimeError{Reason: "abort reason is not valid UTF-8"}
	}
	if strings.TrimFunc(reason, unicode.IsSpace) == "" {
		return &GenerationRuntimeError{Reason: "abort reason must not be empty"}
	}
	if len(reason) > MaxAbortReasonBytes {
		return &GenerationRuntimeError{Reason: fmt.Sprintf("abort reason exceeds the supported maximum of %d bytes", MaxAbortReasonBytes)}
	}
	return nil
}

// OperatorFeedback returns the feedback immutably attached to the current
// generation. The returned reason is the exact operator-supplied text.
func (r *CodexOSRun) OperatorFeedback() (uint64, string, bool) {
	if r == nil {
		return 0, "", false
	}
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	if r.currentOperatorFeedback == nil {
		return 0, "", false
	}
	return r.currentOperatorFeedback.SourceAbortGeneration, r.currentOperatorFeedback.Reason, true
}

func (r *CodexOSRun) feedbackForGeneration(archives []ArchivedGeneration, generation uint64) (*OperatorFeedback, error) {
	records, err := loadOperatorFeedbackRecords(r.runDirectory)
	if err != nil {
		return nil, err
	}
	if err := validateOperatorFeedbackRecords(records, archives); err != nil {
		return nil, err
	}
	for index := range records {
		if records[index].TargetGeneration == generation {
			value := records[index]
			return &value, nil
		}
	}
	consumed := make(map[uint64]struct{}, len(records))
	for _, record := range records {
		consumed[record.SourceAbortGeneration] = struct{}{}
	}
	for index := len(archives) - 1; index >= 0; index-- {
		archive := archives[index]
		if archive.Outcome != "aborted" || archive.AbortReason == nil {
			continue
		}
		if _, ok := consumed[archive.Generation]; ok {
			continue
		}
		return &OperatorFeedback{
			TargetGeneration: generation, SourceAbortGeneration: archive.Generation,
			Reason: *archive.AbortReason, SchemaVersion: 1,
		}, nil
	}
	return nil, nil
}

func (r *CodexOSRun) attachOperatorFeedback(generation uint64) (*OperatorFeedback, error) {
	archives, err := LoadArchivedGenerations(r.runDirectory)
	if err != nil {
		return nil, err
	}
	if err := ValidateArchivedHistory(archives); err != nil {
		return nil, err
	}
	feedback, err := r.feedbackForGeneration(archives, generation)
	if err != nil || feedback == nil {
		return feedback, err
	}
	if err := persistOperatorFeedback(r.runDirectory, *feedback); err != nil {
		return nil, err
	}
	return feedback, nil
}

func persistOperatorFeedback(runDirectory string, feedback OperatorFeedback) error {
	if err := validateOperatorFeedback(feedback); err != nil {
		return err
	}
	encoded, err := encodeOperatorFeedback(feedback)
	if err != nil {
		return err
	}
	directory := filepath.Join(runDirectory, operatorFeedbackDirName)
	createdDirectory := false
	if err := os.Mkdir(directory, 0o700); err == nil {
		createdDirectory = true
	} else if !errors.Is(err, os.ErrExist) {
		return &GenerationRuntimeError{Reason: "could not create operator feedback directory", Err: err}
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return &GenerationRuntimeError{Reason: "operator feedback directory is malformed", Err: err}
	}
	if createdDirectory {
		if err := syncDirectory(runDirectory); err != nil {
			return &GenerationRuntimeError{Reason: "could not persist operator feedback directory", Err: err}
		}
	}
	path := filepath.Join(directory, operatorFeedbackFilename(feedback.TargetGeneration))
	if existing, readErr := readRegularLimited(path, operatorFeedbackLimit); readErr == nil {
		if bytes.Equal(existing, encoded) {
			return nil
		}
		return &GenerationRuntimeError{Reason: "operator feedback attachment conflicts with immutable state"}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return &GenerationRuntimeError{Reason: "could not inspect operator feedback attachment", Err: readErr}
	}
	// Stage in the run root, outside the strictly validated canonical
	// directory. A process death may leave this inode behind, but it cannot be
	// mistaken for a canonical attachment or prevent gate recovery.
	stagingPath, err := stageOperatorFeedback(runDirectory, encoded)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(stagingPath)
		}
	}()
	if err := os.Link(stagingPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			existing, readErr := readRegularLimited(path, operatorFeedbackLimit)
			if readErr == nil && bytes.Equal(existing, encoded) {
				return nil
			}
			return &GenerationRuntimeError{Reason: "operator feedback attachment conflicts with immutable state", Err: readErr}
		}
		return &GenerationRuntimeError{Reason: "could not publish operator feedback attachment", Err: err}
	}
	// Persist the final link before removing the recoverable staging link. A
	// crash on either side of this boundary leaves at least one complete link.
	if err := syncDirectory(directory); err != nil {
		return &GenerationRuntimeError{Reason: "could not persist operator feedback attachment", Err: err}
	}
	if err := os.Remove(stagingPath); err != nil {
		return &GenerationRuntimeError{Reason: "could not finalize operator feedback attachment", Err: err}
	}
	remove = false
	if err := syncDirectory(runDirectory); err != nil {
		return &GenerationRuntimeError{Reason: "could not persist operator feedback attachment", Err: err}
	}
	return nil
}

func stageOperatorFeedback(runDirectory string, encoded []byte) (string, error) {
	staging, err := os.CreateTemp(runDirectory, operatorFeedbackStaging)
	if err != nil {
		return "", &GenerationRuntimeError{Reason: "could not stage operator feedback attachment", Err: err}
	}
	path := staging.Name()
	complete := false
	defer func() {
		_ = staging.Close()
		if !complete {
			_ = os.Remove(path)
		}
	}()
	if err := staging.Chmod(0o600); err != nil {
		return "", &GenerationRuntimeError{Reason: "could not stage operator feedback attachment", Err: err}
	}
	if err := writeAllFile(staging, encoded); err != nil {
		return "", &GenerationRuntimeError{Reason: "could not persist operator feedback attachment", Err: err}
	}
	if err := staging.Sync(); err != nil {
		return "", &GenerationRuntimeError{Reason: "could not persist operator feedback attachment", Err: err}
	}
	if err := staging.Close(); err != nil {
		return "", &GenerationRuntimeError{Reason: "could not persist operator feedback attachment", Err: err}
	}
	complete = true
	return path, nil
}

func loadOperatorFeedbackRecords(runDirectory string) ([]OperatorFeedback, error) {
	directory := filepath.Join(runDirectory, operatorFeedbackDirName)
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, &GenerationRuntimeError{Reason: "could not inspect operator feedback", Err: err}
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, &GenerationRuntimeError{Reason: "operator feedback directory is malformed", Err: err}
	}
	records := make([]OperatorFeedback, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		generation, ok := parseOperatorFeedbackFilename(name)
		if !ok || entry.Type()&os.ModeSymlink != 0 {
			return nil, &GenerationRuntimeError{Reason: "operator feedback directory contains an unexpected entry: " + name}
		}
		encoded, err := readRegularLimited(filepath.Join(directory, name), operatorFeedbackLimit)
		if err != nil {
			return nil, &GenerationRuntimeError{Reason: "could not read operator feedback attachment", Err: err}
		}
		record, err := decodeOperatorFeedback(encoded)
		if err != nil || record.TargetGeneration != generation {
			return nil, &GenerationRuntimeError{Reason: "operator feedback attachment is malformed", Err: err}
		}
		records = append(records, record)
	}
	return records, nil
}

func validateOperatorFeedbackRecords(records []OperatorFeedback, archives []ArchivedGeneration) error {
	aborted := make(map[uint64]string)
	for _, archive := range archives {
		if archive.Outcome == "aborted" && archive.AbortReason != nil {
			aborted[archive.Generation] = *archive.AbortReason
		}
	}
	seenSources := make(map[uint64]struct{}, len(records))
	seenTargets := make(map[uint64]struct{}, len(records))
	var maximumTarget uint64
	if len(archives) > 0 {
		maximumTarget = archives[len(archives)-1].Generation
		if maximumTarget != ^uint64(0) {
			maximumTarget++
		}
	}
	for _, record := range records {
		if err := validateOperatorFeedback(record); err != nil {
			return err
		}
		reason, ok := aborted[record.SourceAbortGeneration]
		if !ok || reason != record.Reason {
			return &GenerationRuntimeError{Reason: "operator feedback does not match its aborted generation"}
		}
		if len(archives) == 0 || record.TargetGeneration > maximumTarget {
			return &GenerationRuntimeError{Reason: "operator feedback target generation is inconsistent"}
		}
		if _, ok := seenSources[record.SourceAbortGeneration]; ok {
			return &GenerationRuntimeError{Reason: "abort reason was attached more than once"}
		}
		if _, ok := seenTargets[record.TargetGeneration]; ok {
			return &GenerationRuntimeError{Reason: "generation has multiple operator feedback attachments"}
		}
		seenSources[record.SourceAbortGeneration] = struct{}{}
		seenTargets[record.TargetGeneration] = struct{}{}
	}
	return nil
}

func validateOperatorFeedback(feedback OperatorFeedback) error {
	if feedback.SchemaVersion != 1 || feedback.TargetGeneration <= feedback.SourceAbortGeneration {
		return &GenerationRuntimeError{Reason: "operator feedback attachment is malformed"}
	}
	return ValidateAbortReason(feedback.Reason)
}

func encodeOperatorFeedback(feedback OperatorFeedback) ([]byte, error) {
	if err := validateOperatorFeedback(feedback); err != nil {
		return nil, err
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(feedback); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func decodeOperatorFeedback(encoded []byte) (OperatorFeedback, error) {
	if len(encoded) > operatorFeedbackLimit || !utf8.Valid(encoded) {
		return OperatorFeedback{}, errors.New("invalid encoding")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var feedback OperatorFeedback
	if err := decoder.Decode(&feedback); err != nil {
		return OperatorFeedback{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return OperatorFeedback{}, errors.New("trailing data")
	}
	if err := validateOperatorFeedback(feedback); err != nil {
		return OperatorFeedback{}, err
	}
	return feedback, nil
}

func operatorFeedbackFilename(generation uint64) string {
	return fmt.Sprintf("generation-%04d.json", generation)
}

func parseOperatorFeedbackFilename(name string) (uint64, bool) {
	if !strings.HasPrefix(name, "generation-") || !strings.HasSuffix(name, ".json") {
		return 0, false
	}
	digits := strings.TrimSuffix(strings.TrimPrefix(name, "generation-"), ".json")
	if len(digits) < 4 || (len(digits) > 4 && digits[0] == '0') {
		return 0, false
	}
	value, err := strconv.ParseUint(digits, 10, 64)
	return value, err == nil && operatorFeedbackFilename(value) == name
}

func writeAllFile(output *os.File, value []byte) error {
	for len(value) > 0 {
		written, err := output.Write(value)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		value = value[written:]
	}
	return nil
}
