package provenance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

const (
	harnessGenerationDirectory = "harness-generations"
	harnessTransitionDirectory = "harness-transitions"
)

// HarnessIdentityStore owns immutable run, generation, and acknowledged gate
// identity records. It never repairs or fills identity into old archives.
type HarnessIdentityStore struct {
	run string
}

type HarnessGateTransition struct {
	AfterGeneration uint64
	Current         HarnessIdentity
	Previous        *HarnessIdentity
	RequiresRecord  bool
}

func NewHarnessIdentityStore(runDirectory string) *HarnessIdentityStore {
	return &HarnessIdentityStore{run: filepath.Clean(runDirectory)}
}

func (s *HarnessIdentityStore) RecordRunCreation(identity HarnessIdentity) error {
	if s == nil || s.run == "" {
		return &HarnessIdentityError{Reason: "harness identity store is unavailable"}
	}
	if err := ValidateHarnessIdentity(identity); err != nil {
		return err
	}
	if err := os.MkdirAll(s.run, 0o755); err != nil {
		return &HarnessIdentityError{Reason: "could not create harness identity run directory", Err: err}
	}
	return writeHarnessIdentityOnce(filepath.Join(s.run, RunHarnessIdentityFilename), identity, true)
}

// VerifyCurrent confirms that identity is the harness identity already accepted
// for this run. It never creates or repairs provenance.
func (s *HarnessIdentityStore) VerifyCurrent(identity HarnessIdentity) error {
	if err := ValidateHarnessIdentity(identity); err != nil {
		return err
	}
	current, err := s.currentIdentity()
	if err != nil {
		return err
	}
	if current == nil {
		return &HarnessIdentityError{Reason: "run harness identity is unavailable"}
	}
	if !current.Equal(identity) {
		return &HarnessIdentityError{Reason: "run harness identity does not match the accepted harness"}
	}
	return nil
}

// PrepareGateTransition performs the read-only admission decision. A changed
// or historically unavailable identity is rejected unless the operator has
// explicitly acknowledged replacement at a generation gate.
func (s *HarnessIdentityStore) PrepareGateTransition(current HarnessIdentity, afterGeneration uint64, acknowledged bool) (HarnessGateTransition, error) {
	if err := ValidateHarnessIdentity(current); err != nil {
		return HarnessGateTransition{}, err
	}
	previous, err := s.currentIdentity()
	if err != nil {
		return HarnessGateTransition{}, err
	}
	changed := previous == nil || !previous.Equal(current)
	if changed && !acknowledged {
		return HarnessGateTransition{}, &HarnessIdentityError{Reason: "harness identity changed or is unavailable; reopen at the generation gate with --acknowledge-harness-change"}
	}
	return HarnessGateTransition{
		AfterGeneration: afterGeneration, Current: current, Previous: CloneHarnessIdentity(previous), RequiresRecord: changed,
	}, nil
}

func (s *HarnessIdentityStore) RecordGateTransition(transition HarnessGateTransition) error {
	if !transition.RequiresRecord {
		return nil
	}
	if err := ValidateHarnessIdentity(transition.Current); err != nil {
		return err
	}
	accepted, err := s.currentIdentity()
	if err != nil {
		return err
	}
	if accepted != nil && accepted.Equal(transition.Current) {
		return nil
	}
	if (accepted == nil && transition.Previous != nil) || (accepted != nil && (transition.Previous == nil || !accepted.Equal(*transition.Previous))) {
		return &HarnessIdentityError{Reason: "harness identity changed while gate acknowledgement was pending"}
	}
	directory := filepath.Join(s.run, harnessTransitionDirectory)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return &HarnessIdentityError{Reason: "could not create harness transition directory", Err: err}
	}
	sequence, err := nextHarnessTransition(directory)
	if err != nil {
		return err
	}
	value := map[string]any{
		"acknowledged": true, "after_generation": transition.AfterGeneration,
		"current": transition.Current.AsJSON(), "previous": nil,
		"schema_version": HarnessIdentitySchemaVersion, "transition": "gate_reopen",
	}
	if transition.Previous != nil {
		value["previous"] = transition.Previous.AsJSON()
	}
	encoded, err := encodeHarnessJSON(value)
	if err != nil {
		return err
	}
	path := filepath.Join(directory, fmt.Sprintf("transition-%06d.json", sequence))
	return writeHarnessBytesOnce(path, encoded)
}

func (s *HarnessIdentityStore) RecordGenerationStart(generation uint64, identity HarnessIdentity) error {
	if err := ValidateHarnessIdentity(identity); err != nil {
		return err
	}
	directory := filepath.Join(s.run, harnessGenerationDirectory)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return &HarnessIdentityError{Reason: "could not create harness generation directory", Err: err}
	}
	value := map[string]any{
		"generation": generation, "harness_identity": identity.AsJSON(), "schema_version": HarnessIdentitySchemaVersion,
	}
	encoded, err := encodeHarnessJSON(value)
	if err != nil {
		return err
	}
	path := filepath.Join(directory, fmt.Sprintf("generation-%04d.json", generation))
	if existing, readErr := readHarnessRegular(path); readErr == nil {
		if bytes.Equal(existing, encoded) {
			return nil
		}
		return &HarnessIdentityError{Reason: fmt.Sprintf("generation %d harness identity is immutable", generation)}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	return writeHarnessBytesOnce(path, encoded)
}

func (s *HarnessIdentityStore) currentIdentity() (*HarnessIdentity, error) {
	transitions, err := s.readTransitions()
	if err != nil {
		return nil, err
	}
	if len(transitions) > 0 {
		return CloneHarnessIdentity(&transitions[len(transitions)-1].Current), nil
	}
	encoded, err := readHarnessRegular(filepath.Join(s.run, RunHarnessIdentityFilename))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	identity, err := ParseHarnessIdentity(encoded)
	if err != nil {
		return nil, err
	}
	return &identity, nil
}

func (s *HarnessIdentityStore) readTransitions() ([]HarnessGateTransition, error) {
	directory := filepath.Join(s.run, harnessTransitionDirectory)
	if info, statErr := os.Lstat(directory); statErr == nil && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir()) {
		return nil, &HarnessIdentityError{Reason: "harness transition directory is unsafe"}
	}
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, &HarnessIdentityError{Reason: "could not inspect harness transitions", Err: err}
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "transition-") || !strings.HasSuffix(entry.Name(), ".json") {
			return nil, &HarnessIdentityError{Reason: "harness transition history is malformed"}
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	transitions := make([]HarnessGateTransition, len(names))
	for index, name := range names {
		expected := fmt.Sprintf("transition-%06d.json", index+1)
		if name != expected {
			return nil, &HarnessIdentityError{Reason: "harness transition history is not contiguous"}
		}
		encoded, readErr := readHarnessRegular(filepath.Join(directory, name))
		if readErr != nil {
			return nil, readErr
		}
		transition, decodeErr := decodeHarnessTransition(encoded)
		if decodeErr != nil {
			return nil, decodeErr
		}
		transitions[index] = transition
		if index > 0 {
			prior := transitions[index-1].Current
			if transition.Previous == nil || !transition.Previous.Equal(prior) {
				return nil, &HarnessIdentityError{Reason: "harness transition history has inconsistent ancestry"}
			}
			if transition.AfterGeneration < transitions[index-1].AfterGeneration {
				return nil, &HarnessIdentityError{Reason: "harness transition history has inconsistent generation order"}
			}
		}
	}
	if len(transitions) > 0 {
		encoded, readErr := readHarnessRegular(filepath.Join(s.run, RunHarnessIdentityFilename))
		if readErr == nil {
			runIdentity, parseErr := ParseHarnessIdentity(encoded)
			if parseErr != nil {
				return nil, parseErr
			}
			if transitions[0].Previous == nil || !transitions[0].Previous.Equal(runIdentity) {
				return nil, &HarnessIdentityError{Reason: "harness transition history does not descend from the run identity"}
			}
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return nil, readErr
		} else if transitions[0].Previous != nil {
			return nil, &HarnessIdentityError{Reason: "legacy harness transition unexpectedly names a previous identity"}
		}
	}
	return transitions, nil
}

func nextHarnessTransition(directory string) (uint64, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return 0, &HarnessIdentityError{Reason: "could not inspect harness transitions", Err: err}
	}
	return uint64(len(entries) + 1), nil
}

func decodeHarnessTransition(encoded []byte) (HarnessGateTransition, error) {
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	if err := decoder.Decode(&fields); err != nil || len(fields) != 6 {
		return HarnessGateTransition{}, &HarnessIdentityError{Reason: "harness transition is malformed", Err: err}
	}
	for _, key := range []string{"acknowledged", "after_generation", "current", "previous", "schema_version", "transition"} {
		if _, ok := fields[key]; !ok {
			return HarnessGateTransition{}, &HarnessIdentityError{Reason: "harness transition is malformed"}
		}
	}
	var schema, generation uint64
	var acknowledged bool
	var kind string
	if json.Unmarshal(fields["schema_version"], &schema) != nil || schema != HarnessIdentitySchemaVersion ||
		json.Unmarshal(fields["after_generation"], &generation) != nil ||
		json.Unmarshal(fields["acknowledged"], &acknowledged) != nil || !acknowledged ||
		json.Unmarshal(fields["transition"], &kind) != nil || kind != "gate_reopen" {
		return HarnessGateTransition{}, &HarnessIdentityError{Reason: "harness transition is malformed"}
	}
	current, err := ParseHarnessIdentity(fields["current"])
	if err != nil {
		return HarnessGateTransition{}, err
	}
	var previous *HarnessIdentity
	if !bytes.Equal(bytes.TrimSpace(fields["previous"]), []byte("null")) {
		value, parseErr := ParseHarnessIdentity(fields["previous"])
		if parseErr != nil {
			return HarnessGateTransition{}, parseErr
		}
		previous = &value
	}
	return HarnessGateTransition{AfterGeneration: generation, Current: current, Previous: previous, RequiresRecord: true}, nil
}

func writeHarnessIdentityOnce(path string, identity HarnessIdentity, acceptIdentical bool) error {
	encoded, err := EncodeHarnessIdentity(identity)
	if err != nil {
		return err
	}
	if acceptIdentical {
		if existing, readErr := readHarnessRegular(path); readErr == nil {
			if bytes.Equal(existing, encoded) {
				return nil
			}
			return &HarnessIdentityError{Reason: "run harness identity is immutable"}
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
	}
	return writeHarnessBytesOnce(path, encoded)
}

func encodeHarnessJSON(value any) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return nil, &HarnessIdentityError{Reason: "could not encode harness identity provenance", Err: err}
	}
	return output.Bytes(), nil
}

func writeHarnessBytesOnce(path string, encoded []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".harness-identity-")
	if err != nil {
		return &HarnessIdentityError{Reason: "could not persist immutable harness identity", Err: err}
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return &HarnessIdentityError{Reason: "could not persist immutable harness identity", Err: err}
	}
	written, writeErr := file.Write(encoded)
	if writeErr == nil && written != len(encoded) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return &HarnessIdentityError{Reason: "could not persist immutable harness identity", Err: writeErr}
	}
	if closeErr != nil {
		return &HarnessIdentityError{Reason: "could not persist immutable harness identity", Err: closeErr}
	}
	if err := os.Link(temporary, path); err != nil {
		return &HarnessIdentityError{Reason: "could not persist immutable harness identity", Err: err}
	}
	if err := syncHarnessDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	return nil
}

func syncHarnessDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return &HarnessIdentityError{Reason: "could not sync harness identity directory", Err: err}
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return &HarnessIdentityError{Reason: "could not sync harness identity directory", Err: syncErr}
	}
	if closeErr != nil {
		return &HarnessIdentityError{Reason: "could not sync harness identity directory", Err: closeErr}
	}
	return nil
}

func readHarnessRegular(path string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("could not open harness identity")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, &HarnessIdentityError{Reason: "harness identity path is unsafe", Err: err}
	}
	contents, err := io.ReadAll(io.LimitReader(file, harnessIdentityLimit+1))
	if err != nil {
		return nil, err
	}
	if len(contents) > harnessIdentityLimit {
		return nil, &HarnessIdentityError{Reason: "harness identity exceeds size limit"}
	}
	return contents, nil
}

func HarnessGenerationRecordPath(run string, generation uint64) string {
	return filepath.Join(run, harnessGenerationDirectory, "generation-"+fmt.Sprintf("%04d", generation)+".json")
}

func ParseHarnessGenerationRecord(encoded []byte, expected uint64) (HarnessIdentity, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil || len(fields) != 3 {
		return HarnessIdentity{}, &HarnessIdentityError{Reason: "generation harness identity is malformed", Err: err}
	}
	var schema, generation uint64
	if json.Unmarshal(fields["schema_version"], &schema) != nil || schema != HarnessIdentitySchemaVersion ||
		json.Unmarshal(fields["generation"], &generation) != nil || generation != expected {
		return HarnessIdentity{}, &HarnessIdentityError{Reason: "generation harness identity is malformed"}
	}
	return ParseHarnessIdentity(fields["harness_identity"])
}
