package provenance

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestExitInterviewReasoningIndicesMatchPythonIntegersAndJSONNumbers(t *testing.T) {
	transcript := NewExitInterviewTranscript(ExitInterviewMetadata{})
	if err := transcript.BeginTurn(1, "Question", "turn"); err != nil {
		t.Fatal(err)
	}
	for _, activity := range []ExitInterviewActivity{
		{Kind: ExitInterviewReasoningDelta, Data: map[string]any{"text": "zero", "summary_index": false}},
		{Kind: ExitInterviewReasoningDelta, Data: map[string]any{"text": "one", "summary_index": json.Number("1")}},
	} {
		if err := transcript.Observe(activity, "turn"); err != nil {
			t.Fatal(err)
		}
	}
	got := transcript.Snapshot().Turns[0].ReasoningSummaries
	if strings.Join(got, "|") != "zero|one" {
		t.Fatalf("reasoning summaries = %#v", got)
	}
}

func TestExitInterviewTranscriptCapturesOrderedVisibleSummaries(t *testing.T) {
	transcript := NewExitInterviewTranscript(ExitInterviewMetadata{
		Run:                  "experiment-042",
		Generation:           10,
		AgentContractVersion: 4,
		Model:                "gpt-5.6-sol",
		ReasoningEffort:      "high",
		ReasoningSummary:     "auto",
		ServiceTier:          "priority",
	})
	if err := transcript.BeginTurn(1, "Why this path?", "turn-1"); err != nil {
		t.Fatal(err)
	}
	if err := transcript.BeginTurn(2, "not allowed while active", "turn-2"); err == nil {
		t.Fatal("second active turn was accepted")
	}

	// Wrong-turn and non-reasoning activity must not leak into the artifact.
	_ = transcript.Observe(ExitInterviewActivity{
		Kind:   ExitInterviewReasoningDelta,
		Data:   map[string]any{"text": "wrong", "summary_index": 0},
		ItemID: "wrong-item",
	}, "turn-2")
	_ = transcript.Observe(ExitInterviewActivity{
		Kind: ExitInterviewActivityKind("agent.message"),
		Data: map[string]any{"text": "private final message"},
	}, "turn-1")

	// Item order is first-seen order; parts within an item are numeric order.
	for _, activity := range []ExitInterviewActivity{
		{Kind: ExitInterviewReasoningDelta, Data: map[string]any{"text": "second", "summary_index": 2}, ItemID: "item-b"},
		{Kind: ExitInterviewReasoningDelta, Data: map[string]any{"text": "first", "summary_index": 0}, ItemID: "item-b"},
		{Kind: ExitInterviewReasoningDelta, Data: map[string]any{"text": "discarded", "summary_index": 1}, ItemID: "item-b"},
		{Kind: ExitInterviewReasoningSummary, Data: map[string]any{"summary": []string{"authoritative", "  "}}, ItemID: "item-a"},
	} {
		_ = transcript.Observe(activity, "turn-1")
	}
	if err := transcript.FinishTurn("turn-1", nil, "completed"); err != nil {
		t.Fatal(err)
	}
	if err := transcript.BeginTurn(2, "What followed?", "turn-2"); err != nil {
		t.Fatal(err)
	}
	answer := "Second answer."
	if err := transcript.FinishTurn("turn-2", &answer, "completed"); err != nil {
		t.Fatal(err)
	}

	snapshot := transcript.Snapshot()
	if len(snapshot.Turns) != 2 {
		t.Fatalf("turn count = %d", len(snapshot.Turns))
	}
	first := snapshot.Turns[0]
	if first.Question != "Why this path?" || first.Status != "completed" || first.Response != nil {
		t.Fatalf("first turn = %#v", first)
	}
	if strings.Join(first.ReasoningSummaries, "|") != "first|discarded|second|authoritative" {
		t.Fatalf("reasoning summaries = %#v", first.ReasoningSummaries)
	}
	if snapshot.Turns[1].Response == nil || *snapshot.Turns[1].Response != answer {
		t.Fatalf("second response = %#v", snapshot.Turns[1].Response)
	}

	// The snapshot owns its slices and response pointer; changing it cannot
	// alter later snapshots of the live recorder.
	snapshot.Turns[0].ReasoningSummaries[0] = "changed"
	*snapshot.Turns[1].Response = "changed"
	later := transcript.Snapshot()
	if later.Turns[0].ReasoningSummaries[0] != "first" || *later.Turns[1].Response != answer {
		t.Fatalf("snapshot mutation changed recorder: %#v", later)
	}
}

func TestExitInterviewPartialTurnAndMarkdownBytes(t *testing.T) {
	transcript := NewExitInterviewTranscript(ExitInterviewMetadata{
		Run:                  "experiment-044",
		Generation:           3,
		AgentContractVersion: 4,
		Model:                "gpt-5.6-sol",
		ReasoningEffort:      "high",
		ReasoningSummary:     "auto",
		ServiceTier:          "priority",
	})
	if err := transcript.BeginTurn(1, "Interrupted\r\nquestion", "turn"); err != nil {
		t.Fatal(err)
	}
	_ = transcript.Observe(ExitInterviewActivity{
		Kind:   ExitInterviewReasoningDelta,
		Data:   map[string]any{"text": "Visible partial\rsummary", "summary_index": 0},
		ItemID: "reasoning",
	}, "turn")
	snapshot := transcript.Snapshot()
	if snapshot.Turns[0].Status != "running" || snapshot.Turns[0].Response != nil {
		t.Fatalf("partial turn = %#v", snapshot.Turns[0])
	}

	got := RenderExitInterviewMarkdown(snapshot, "interrupted")
	want := "# CodexOS Exit Interview\n\n" +
		"Run: experiment-044\n" +
		"Generation: 3\n" +
		"Agent Contract: 4\n" +
		"Model: gpt-5.6-sol\n" +
		"Reasoning effort: high\n" +
		"Reasoning summary: auto\n" +
		"Service tier: priority\n" +
		"Interview status: interrupted\n\n" +
		"## Question 1\n\n" +
		"### Operator\n\n" +
		"Interrupted\nquestion\n\n" +
		"### Sol — reasoning summary\n\n" +
		"Visible partial\nsummary\n\n" +
		"Turn status: running\n"
	if got != want {
		t.Fatalf("markdown bytes differ:\n got %q\nwant %q", got, want)
	}

	controls := "a\r\nb\rc\x00\x1b\x7f\u0080\u0085\u009f\u0100\U0001f600\t\n"
	if got, want := NormalizeExitInterviewText(controls), "a\nb\nc\\x00\\x1b\\x7f\\x80\\x85\\x9fĀ😀\t\n"; got != want {
		t.Fatalf("normalized text = %q, want %q", got, want)
	}
}

func TestExitInterviewArtifactStoreIsImmutableAndIdempotent(t *testing.T) {
	repository := t.TempDir()
	run := filepath.Join(t.TempDir(), "experiment-042")
	if err := os.Mkdir(run, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := NewExitInterviewTranscript(ExitInterviewMetadata{Run: "experiment-042", Generation: 10})
	if err := transcript.BeginTurn(1, "Question", "turn"); err != nil {
		t.Fatal(err)
	}
	answer := "Answer"
	if err := transcript.FinishTurn("turn", &answer, "completed"); err != nil {
		t.Fatal(err)
	}

	store, err := NewExitInterviewArtifactStore(repository, run)
	if err != nil {
		t.Fatal(err)
	}
	if artifact, err := store.Persist(ExitInterviewTranscriptSnapshot{Metadata: ExitInterviewMetadata{Run: "experiment-042", Generation: 10}}, "completed"); err != nil || artifact != nil {
		t.Fatalf("empty persist = %#v, %v", artifact, err)
	}
	first, err := store.Persist(transcript.Snapshot(), "completed")
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || first.AlreadyRecorded {
		t.Fatalf("first artifact = %#v", first)
	}
	if want := "artifacts/interviews/experiment-042/generation-0010.md"; first.RelativePath != want {
		t.Fatalf("relative path = %q, want %q", first.RelativePath, want)
	}
	before, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Finalize(transcript.Snapshot(), "completed")
	if err != nil {
		t.Fatal(err)
	}
	if second == nil || !second.AlreadyRecorded {
		t.Fatalf("second artifact = %#v", second)
	}

	if err := transcript.BeginTurn(2, "Late failed turn", "turn-2"); err != nil {
		t.Fatal(err)
	}
	if err := transcript.FinishTurn("turn-2", nil, "failed"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Persist(transcript.Snapshot(), "failed"); err == nil || !strings.Contains(err.Error(), "conflicting exit-interview artifact") {
		t.Fatalf("conflicting persist error = %v", err)
	}
	after, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("conflicting persist changed finalized artifact")
	}
	info, err := os.Stat(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("artifact mode = %o, want 644", info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(first.Path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temporary file leaked: %s", entry.Name())
		}
	}
}

func TestExitInterviewArtifactStoreRejectsUnsafeNamespacesAndWriteFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink and directory-mode checks are Unix-specific")
	}
	repository := t.TempDir()
	run := filepath.Join(t.TempDir(), "experiment-safe")
	if err := os.Mkdir(run, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := NewExitInterviewTranscript(ExitInterviewMetadata{Run: "experiment-safe", Generation: 1})
	if err := transcript.BeginTurn(1, "Question", "turn"); err != nil {
		t.Fatal(err)
	}
	if err := transcript.FinishTurn("turn", nil, "completed"); err != nil {
		t.Fatal(err)
	}

	store, err := NewExitInterviewArtifactStore(repository, run)
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(repository, "artifacts")
	outside := t.TempDir()
	if err := os.Symlink(outside, parent); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Persist(transcript.Snapshot(), "completed"); err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("symlinked parent error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "interviews")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink target was modified: %v", err)
	}
	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}

	artifactDirectory := filepath.Join(repository, "artifacts", "interviews", "experiment-safe")
	if err := os.MkdirAll(artifactDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(artifactDirectory, "generation-0001.md")
	protected := filepath.Join(outside, "protected.md")
	if err := os.WriteFile(protected, []byte("protected\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(protected, output); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Persist(transcript.Snapshot(), "completed"); err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("symlinked output error = %v", err)
	}
	contents, err := os.ReadFile(protected)
	if err != nil || string(contents) != "protected\n" {
		t.Fatalf("protected output changed: %q, %v", contents, err)
	}
	if err := os.Remove(output); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(artifactDirectory, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(artifactDirectory, 0o700)
	if _, err := store.Persist(transcript.Snapshot(), "completed"); err == nil || !strings.Contains(err.Error(), "cannot write exit-interview artifact") {
		t.Fatalf("write failure error = %v", err)
	}
	entries, err := os.ReadDir(artifactDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("write failure left entries: %v", entries)
	}
}

func TestExitInterviewArtifactStoreConcurrentFinalization(t *testing.T) {
	repository := t.TempDir()
	run := filepath.Join(t.TempDir(), "experiment-concurrent")
	if err := os.Mkdir(run, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := NewExitInterviewTranscript(ExitInterviewMetadata{Run: "experiment-concurrent", Generation: 2})
	if err := transcript.BeginTurn(1, "Question", "turn"); err != nil {
		t.Fatal(err)
	}
	answer := "Answer"
	if err := transcript.FinishTurn("turn", &answer, "completed"); err != nil {
		t.Fatal(err)
	}
	snapshot := transcript.Snapshot()
	store, err := NewExitInterviewArtifactStore(repository, run)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	artifacts := make([]*ExitInterviewArtifact, workers)
	errorsSeen := make([]error, workers)
	var wait sync.WaitGroup
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			artifacts[index], errorsSeen[index] = store.Persist(snapshot, "completed")
		}(index)
	}
	wait.Wait()
	firstCount := 0
	for index := range artifacts {
		if errorsSeen[index] != nil {
			t.Fatal(errorsSeen[index])
		}
		if artifacts[index] == nil {
			t.Fatal("concurrent persist returned no artifact")
		}
		if artifacts[index].AlreadyRecorded {
			continue
		}
		firstCount++
	}
	if firstCount != 1 {
		t.Fatalf("new artifact results = %d, want 1", firstCount)
	}
}
