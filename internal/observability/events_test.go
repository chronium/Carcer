package observability

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestEventLogConcurrentAppendAndReopen(t *testing.T) {
	run := t.TempDir()
	log, err := OpenEventLog(run)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for worker := range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			generation := uint64(worker)
			for value := range 20 {
				log.Record("tool_started", &generation, map[string]any{"tool": "read", "value": value})
			}
		}()
	}
	workers.Wait()
	log.Close()

	reopened, err := OpenEventLog(run)
	if err != nil {
		t.Fatal(err)
	}
	reopened.Record("run_stopped", nil, nil)
	reopened.Close()

	encoded, err := os.ReadFile(filepath.Join(run, EventLogFilename))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(encoded, []byte{'\n'}), []byte{'\n'})
	if len(lines) != 81 {
		t.Fatalf("event count = %d, want 81", len(lines))
	}
	for index, line := range lines {
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("line %d: %v", index+1, err)
		}
		if event["sequence"] != float64(index+1) {
			t.Fatalf("line %d sequence = %v", index+1, event["sequence"])
		}
	}
}

func TestEventLogEncodingMatchesPythonJSONShape(t *testing.T) {
	log, err := OpenEventLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	generation := uint64(7)
	log.Record("tool_completed", &generation, map[string]any{
		"markup": "<tag>&\u2028Ω",
		"status": 0,
	})
	log.Close()
	encoded, err := os.ReadFile(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{[]byte(`\u003c`), []byte(`\u003e`), []byte(`\u0026`), []byte(`\u2028`), []byte(`\u03a9`)} {
		if bytes.Contains(encoded, forbidden) {
			t.Fatalf("event escaped %q: %s", forbidden, encoded)
		}
	}
	if !bytes.HasPrefix(encoded, []byte(`{"data":`)) || !bytes.HasSuffix(encoded, []byte("}\n")) {
		t.Fatalf("event JSON shape = %s", encoded)
	}
}

func TestEventLogRejectsMalformedExistingStateWithoutRewrite(t *testing.T) {
	cases := map[string][]byte{
		"invalid envelope": []byte(`{"not":"an event"}` + "\n"),
		"incomplete":       []byte(`{"data":{}}`),
		"invalid UTF-8":    {'{', 0xff, '}', '\n'},
		"invalid sequence": []byte(`{"data":{},"event":"x","generation":0,"schema_version":1,"sequence":0,"timestamp":"2026-09-01T00:00:00.000000Z"}` + "\n"),
	}
	for name, original := range cases {
		t.Run(name, func(t *testing.T) {
			run := t.TempDir()
			path := filepath.Join(run, EventLogFilename)
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenEventLog(run); err == nil {
				t.Fatal("malformed event log accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, original) {
				t.Fatal("malformed event log was rewritten")
			}
		})
	}
}

func TestEventLogRejectsSymlink(t *testing.T) {
	run := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(run, EventLogFilename)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := OpenEventLog(run); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestEventRecordingFailureDegradesWithoutControllingCaller(t *testing.T) {
	log, err := OpenEventLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log.Record("bad", nil, map[string]any{"unsupported": make(chan int)})
	if log.Healthy() || !strings.Contains(log.DegradedReason(), "local event recording failed") {
		t.Fatalf("health = %v, reason = %q", log.Healthy(), log.DegradedReason())
	}
	log.Record("good", nil, nil)
	log.Close()
	encoded, err := os.ReadFile(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"sequence":1`)) || bytes.Contains(encoded, []byte(`"sequence":2`)) {
		t.Fatalf("sequence after failed event = %s", encoded)
	}
	log.Close()
	log.Record("ignored", nil, nil)
}

func TestEventLogSizeBoundAndErrorUnwrap(t *testing.T) {
	log, err := OpenEventLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log.Record("large", nil, map[string]any{"value": strings.Repeat("x", MaxEventLogLineBytes)})
	if log.Healthy() || !strings.Contains(log.DegradedReason(), "size limit") {
		t.Fatalf("large-event health = %v, reason = %q", log.Healthy(), log.DegradedReason())
	}
	log.Close()
	cause := errors.New("cause")
	if !errors.Is(&Error{Reason: "wrapped", Err: cause}, cause) {
		t.Fatal("observability error did not unwrap")
	}
}

func TestEventLogSequenceExhaustionDoesNotPublishInvalidState(t *testing.T) {
	run := t.TempDir()
	path := filepath.Join(run, EventLogFilename)
	original := []byte(`{"data":{},"event":"last","generation":null,"schema_version":1,"sequence":18446744073709551615,"timestamp":"2026-09-01T00:00:00.000000Z"}` + "\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	log, err := OpenEventLog(run)
	if err != nil {
		t.Fatal(err)
	}
	log.Record("overflow", nil, nil)
	log.Close()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) || log.Healthy() {
		t.Fatalf("exhausted log changed=%v healthy=%v", !bytes.Equal(after, original), log.Healthy())
	}
}
