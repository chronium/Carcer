package observability

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	EventLogFilename     = "events.jsonl"
	MaxEventLogLineBytes = 16 * 1024 * 1024
	eventSchemaVersion   = uint64(1)
)

var durableEvents = map[string]struct{}{
	"generation_completed":                 {},
	"generation_aborted":                   {},
	"feature_approved":                     {},
	"feature_denied":                       {},
	"harness_identity_transition_recorded": {},
	"operator_abort_feedback_attached":     {},
}

// Error reports malformed durable observability state or an event-log setup
// failure. Event recording failures degrade an open log instead of escaping to
// experiment control.
type Error struct {
	Reason string
	Err    error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// EventLog owns one run's append-only local JSONL event record.
type EventLog struct {
	path     string
	output   *os.File
	sequence uint64
	closed   bool

	mu             sync.Mutex
	healthMu       sync.Mutex
	degradedReason string
}

// OpenEventLog validates the complete existing log before opening it for
// append. Malformed state is never truncated or rewritten.
func OpenEventLog(runDirectory string) (*EventLog, error) {
	run, err := filepath.Abs(runDirectory)
	if err != nil {
		return nil, &Error{Reason: "could not resolve event-log run directory", Err: err}
	}
	if err := os.MkdirAll(run, 0o755); err != nil {
		return nil, &Error{Reason: "could not create event-log run directory", Err: err}
	}
	run, err = filepath.EvalSymlinks(run)
	if err != nil {
		return nil, &Error{Reason: "could not resolve event-log run directory", Err: err}
	}
	path := filepath.Join(run, EventLogFilename)
	sequence, err := readLastSequence(path)
	if err != nil {
		return nil, err
	}
	output, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, &Error{Reason: fmt.Sprintf("could not open event log %s", path), Err: err}
	}
	return &EventLog{path: path, output: output, sequence: sequence}, nil
}

func (l *EventLog) Path() string { return l.path }

func (l *EventLog) Healthy() bool {
	l.healthMu.Lock()
	defer l.healthMu.Unlock()
	return l.degradedReason == ""
}

func (l *EventLog) DegradedReason() string {
	l.healthMu.Lock()
	defer l.healthMu.Unlock()
	return l.degradedReason
}

// Degrade records the first telemetry problem without changing experiment
// control flow.
func (l *EventLog) Degrade(reason string) {
	l.healthMu.Lock()
	defer l.healthMu.Unlock()
	if l.degradedReason == "" {
		l.degradedReason = reason
	}
}

// Record appends one trusted event. Failures are reflected through Healthy and
// DegradedReason; observability never controls the experiment.
func (l *EventLog) Record(event string, generation *uint64, data map[string]any) {
	data = cloneActivityData(data)
	var generationValue any
	if generation != nil {
		generationValue = *generation
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	if l.sequence == ^uint64(0) {
		l.mu.Unlock()
		l.Degrade("local event recording failed: event sequence is exhausted")
		return
	}
	sequence := l.sequence + 1
	envelope := map[string]any{
		"schema_version": eventSchemaVersion,
		"sequence":       sequence,
		"timestamp":      time.Now().UTC().Format("2006-01-02T15:04:05.000000Z"),
		"event":          event,
		"generation":     generationValue,
		"data":           data,
	}
	encoded, err := encodeEventEnvelope(envelope)
	if err == nil {
		err = writeAll(l.output, encoded)
	}
	if err == nil {
		if _, durable := durableEvents[event]; durable {
			err = l.output.Sync()
		}
	}
	if err == nil {
		l.sequence = sequence
	}
	l.mu.Unlock()
	if err != nil {
		l.Degrade("local event recording failed: " + err.Error())
	}
}

// Close is idempotent. A close failure degrades observability and is not
// returned to experiment control.
func (l *EventLog) Close() {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	err := l.output.Close()
	l.mu.Unlock()
	if err != nil {
		l.Degrade("local event close failed: " + err.Error())
	}
}

func encodeEventEnvelope(envelope map[string]any) ([]byte, error) {
	if !validEventStrings(envelope) {
		return nil, errors.New("event contains invalid UTF-8")
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(envelope); err != nil {
		return nil, err
	}
	encoded := output.Bytes()
	encoded = bytes.ReplaceAll(encoded, []byte(`\u2028`), []byte("\u2028"))
	encoded = bytes.ReplaceAll(encoded, []byte(`\u2029`), []byte("\u2029"))
	if len(encoded) > MaxEventLogLineBytes {
		return nil, errors.New("event exceeds line size limit")
	}
	return encoded, nil
}

func validEventStrings(value any) bool {
	switch value := value.(type) {
	case string:
		return utf8.ValidString(value)
	case map[string]any:
		for key, item := range value {
			if !utf8.ValidString(key) || !validEventStrings(item) {
				return false
			}
		}
		return true
	case []any:
		for _, item := range value {
			if !validEventStrings(item) {
				return false
			}
		}
		return true
	case []string:
		for _, item := range value {
			if !utf8.ValidString(item) {
				return false
			}
		}
		return true
	default:
		return true
	}
}

func writeAll(output *os.File, encoded []byte) error {
	for len(encoded) > 0 {
		written, err := output.Write(encoded)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		encoded = encoded[written:]
	}
	return nil
}

func readLastSequence(path string) (uint64, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, &Error{Reason: fmt.Sprintf("could not read event log %s", path), Err: err}
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return 0, &Error{Reason: fmt.Sprintf("event log is not a regular file: %s", path)}
	}
	input, err := os.Open(path)
	if err != nil {
		return 0, &Error{Reason: fmt.Sprintf("could not read event log %s", path), Err: err}
	}
	defer input.Close()
	reader := bufio.NewReader(input)
	var previous uint64
	for lineNumber := 1; ; lineNumber++ {
		line, readErr := readEventLine(reader)
		if len(line) == 0 && errors.Is(readErr, io.EOF) {
			return previous, nil
		}
		if len(line) == 0 || line[len(line)-1] != '\n' {
			return 0, &Error{Reason: fmt.Sprintf("event log line %d is incomplete", lineNumber)}
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return 0, &Error{Reason: fmt.Sprintf("could not read event log %s", path), Err: readErr}
		}
		if !utf8.Valid(line) {
			return 0, &Error{Reason: "event log is not valid UTF-8"}
		}
		sequence, err := validateEventEnvelope(bytes.TrimSuffix(line, []byte{'\n'}), lineNumber, previous)
		if err != nil {
			return 0, err
		}
		previous = sequence
		if errors.Is(readErr, io.EOF) {
			return previous, nil
		}
	}
}

func readEventLine(reader *bufio.Reader) ([]byte, error) {
	line := make([]byte, 0, 4096)
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > MaxEventLogLineBytes {
			return nil, errors.New("event log line exceeds size limit")
		}
		line = append(line, fragment...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}

func validateEventEnvelope(encoded []byte, lineNumber int, previous uint64) (uint64, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return 0, &Error{Reason: fmt.Sprintf("event log line %d is malformed", lineNumber), Err: err}
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return 0, &Error{Reason: fmt.Sprintf("event log line %d is malformed", lineNumber), Err: err}
	}
	envelope, ok := value.(map[string]any)
	if !ok || !exactEventFields(envelope) {
		return 0, &Error{Reason: fmt.Sprintf("event log line %d has an invalid envelope", lineNumber)}
	}
	schema, ok := jsonUint64(envelope["schema_version"])
	if !ok || schema != eventSchemaVersion {
		return 0, &Error{Reason: fmt.Sprintf("event log line %d has an unsupported schema version", lineNumber)}
	}
	sequence, ok := jsonUint64(envelope["sequence"])
	if !ok || sequence <= previous {
		return 0, &Error{Reason: fmt.Sprintf("event log line %d has an invalid sequence", lineNumber)}
	}
	if envelope["generation"] != nil {
		if _, ok := jsonUint64(envelope["generation"]); !ok {
			return 0, &Error{Reason: fmt.Sprintf("event log line %d has an invalid generation", lineNumber)}
		}
	}
	event, ok := envelope["event"].(string)
	if !ok || event == "" {
		return 0, &Error{Reason: fmt.Sprintf("event log line %d has an invalid event name", lineNumber)}
	}
	if _, ok := envelope["data"].(map[string]any); !ok {
		return 0, &Error{Reason: fmt.Sprintf("event log line %d has invalid event data", lineNumber)}
	}
	timestamp, ok := envelope["timestamp"].(string)
	if !ok || len(timestamp) == 0 || timestamp[len(timestamp)-1] != 'Z' {
		return 0, &Error{Reason: fmt.Sprintf("event log line %d has an invalid timestamp", lineNumber)}
	}
	parsed, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return 0, &Error{Reason: fmt.Sprintf("event log line %d has an invalid timestamp", lineNumber), Err: err}
	}
	_, offset := parsed.Zone()
	if offset != 0 {
		return 0, &Error{Reason: fmt.Sprintf("event log line %d timestamp is not UTC", lineNumber)}
	}
	return sequence, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func exactEventFields(value map[string]any) bool {
	if len(value) != 6 {
		return false
	}
	for _, key := range []string{"schema_version", "sequence", "timestamp", "event", "generation", "data"} {
		if _, ok := value[key]; !ok {
			return false
		}
	}
	return true
}

func jsonUint64(value any) (uint64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := number.Int64()
	if err == nil {
		if parsed < 0 {
			return 0, false
		}
		return uint64(parsed), true
	}
	var result uint64
	for _, digit := range string(number) {
		if digit < '0' || digit > '9' || result > (^uint64(0)-uint64(digit-'0'))/10 {
			return 0, false
		}
		result = result*10 + uint64(digit-'0')
	}
	return result, string(number) != ""
}
