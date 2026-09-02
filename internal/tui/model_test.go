package tui

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"codexos/internal/observability"
)

func modelEvent(sequence uint64, kind observability.ActivityKind, data map[string]any, role observability.ActivityRole, item string) observability.ActivityEvent {
	generation := uint64(7)
	return observability.ActivityEvent{
		Sequence:   sequence,
		Generation: &generation,
		Role:       role,
		Kind:       kind,
		Data:       data,
		ThreadID:   "thread",
		TurnID:     "turn",
		ItemID:     item,
	}
}

func newTestModel(t *testing.T, options ...ActivityModelOptions) *OperatorActivityModel {
	t.Helper()
	model, err := NewOperatorActivityModel(options...)
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func TestModelMessagesReasoningAndInterviewRowsRemainAttributed(t *testing.T) {
	model := newTestModel(t, ActivityModelOptions{DisplayBytes: 32})
	if !model.Consume(modelEvent(1, observability.ActivityAgentTextDelta, map[string]any{"text": "Inspect "}, observability.ActivityImplementor, "message")) {
		t.Fatal("first message delta did not change model")
	}
	model.Consume(modelEvent(2, observability.ActivityAgentTextDelta, map[string]any{"text": "state."}, observability.ActivityImplementor, "message"))
	model.Consume(modelEvent(3, observability.ActivityAgentMessage, map[string]any{"text": "**Inspect state.**"}, observability.ActivityImplementor, "message"))
	model.Consume(modelEvent(4, observability.ActivityAgentMessage, map[string]any{"text": "Reviewer result."}, observability.ActivityReviewer, "review-message"))
	model.Consume(modelEvent(5, observability.ActivityAgentReasoningDelta, map[string]any{"text": "Check ", "summary_index": 0}, observability.ActivityImplementor, "reasoning"))
	model.Consume(modelEvent(6, observability.ActivityAgentReasoningSummary, map[string]any{"summary": []string{"Check ownership."}, "turn_phase": "planning"}, observability.ActivityImplementor, "plan-reasoning"))
	model.Consume(modelEvent(7, observability.ActivityExitInterviewQuestion, map[string]any{"text": "Why?\x1b[2J"}, observability.ActivityHarness, "question"))

	entries := model.Entries()
	if len(entries) != 5 {
		t.Fatalf("entry count = %d, want 5", len(entries))
	}
	message, ok := entries[0].Presentation.(AgentMessagePresentation)
	if !ok || !message.Finalized || message.Text != "**Inspect state.**" || message.Role != observability.ActivityImplementor {
		t.Fatalf("message presentation = %#v", entries[0].Presentation)
	}
	reviewer, ok := entries[1].Presentation.(AgentMessagePresentation)
	if !ok || reviewer.Role != observability.ActivityReviewer {
		t.Fatalf("reviewer presentation = %#v", entries[1].Presentation)
	}
	firstReasoning, ok := entries[2].Presentation.(ReasoningPresentation)
	if !ok || firstReasoning.Text != "Check " || firstReasoning.TurnPhase != "" {
		t.Fatalf("reasoning presentation = %#v", entries[2].Presentation)
	}
	planReasoning, ok := entries[3].Presentation.(ReasoningPresentation)
	if !ok || planReasoning.Text != "Check ownership." || planReasoning.TurnPhase != "planning" {
		t.Fatalf("planning reasoning presentation = %#v", entries[3].Presentation)
	}
	question, ok := entries[4].Presentation.(InterviewQuestionPresentation)
	if !ok || question.Text != "Why?\\x1b[2J" {
		t.Fatalf("question presentation = %#v", entries[3].Presentation)
	}
	if strings.Contains(model.RenderText(), "\x1b") {
		t.Fatal("rendered transcript contains a raw terminal escape")
	}
}

func TestModelToolsPreserveDetailFeatureStatusAndReviewerDeduplication(t *testing.T) {
	model := newTestModel(t, ActivityModelOptions{DisplayBytes: 80})
	source := strings.Repeat("int task;\n", 20)
	write := map[string]any{
		"tool":      "write",
		"arguments": map[string]any{"path": "seed/tasks.c", "offset": 12, "data": source},
	}
	model.Consume(modelEvent(1, observability.ActivityToolStarted, write, observability.ActivityImplementor, "write"))
	model.Consume(modelEvent(2, observability.ActivityToolCompleted, map[string]any{
		"tool": "write", "arguments": map[string]any{"path": "seed/tasks.c", "offset": 12, "data": source},
		"result": map[string]any{"status": 0, "output": []byte{}},
	}, observability.ActivityImplementor, "write"))
	tool := model.Entries()[0].Presentation.(ToolPresentation)
	if tool.State != ActivityDisplayStateCompleted || tool.Detail == nil || tool.Detail.ByteCount != len([]byte(source)) || !tool.Detail.Truncated {
		t.Fatalf("completed write = %#v", tool)
	}

	request := map[string]any{"tool": "request_feature", "arguments": map[string]any{"title": "External capacity", "description": "Need Δ"}}
	model.Consume(modelEvent(3, observability.ActivityToolStarted, request, observability.ActivityImplementor, "feature"))
	model.Consume(modelEvent(4, observability.ActivityToolCompleted, map[string]any{
		"tool": "request_feature", "arguments": request["arguments"], "result": map[string]any{"status": 0, "output": []byte("4")},
	}, observability.ActivityImplementor, "feature"))
	feature := model.Entries()[1].Presentation.(FeatureRequestPresentation)
	if feature.RecordingState != FeatureRequestRecordingStateRecorded || feature.InitialStatus == nil || *feature.InitialStatus != FeatureRequestInitialStatusPending || feature.RequestID != "4" {
		t.Fatalf("feature presentation = %#v", feature)
	}
	model.Consume(modelEvent(41, observability.ActivityToolCompleted, map[string]any{
		"tool": "request_feature", "arguments": request["arguments"], "result": map[string]any{"status": 0, "output": []byte("0")},
	}, observability.ActivityImplementor, "feature-zero"))
	zeroID := model.Entries()[2].Presentation.(FeatureRequestPresentation)
	if zeroID.RequestID != "" {
		t.Fatalf("zero feature request ID = %q", zeroID.RequestID)
	}

	reviewText := "One issue found."
	model.Consume(modelEvent(5, observability.ActivityAgentMessage, map[string]any{"text": reviewText}, observability.ActivityReviewer, "review-message"))
	review := map[string]any{"tool": "review", "arguments": map[string]any{"focus": "correctness"}}
	model.Consume(modelEvent(6, observability.ActivityToolStarted, review, observability.ActivityReviewer, "review"))
	model.Consume(modelEvent(7, observability.ActivityToolCompleted, map[string]any{"tool": "review", "arguments": review["arguments"], "result": reviewText}, observability.ActivityReviewer, "review"))
	completedReview := model.Entries()[4].Presentation.(ToolPresentation)
	if completedReview.Detail != nil || completedReview.ResultNote != "result returned to Sol" || strings.Count(model.RenderText(), reviewText) != 1 {
		t.Fatalf("review presentation = %#v; transcript = %q", completedReview, model.RenderText())
	}

	model.Consume(modelEvent(8, observability.ActivityToolFailed, map[string]any{"tool": "read", "arguments": map[string]any{}, "result": map[string]any{"status": 7, "output": []byte{0xff, 0x00, 0x1b}}}, observability.ActivityImplementor, "failed"))
	failed := model.Entries()[5].Presentation.(ToolPresentation)
	if failed.Detail == nil || !failed.Detail.Binary || !strings.Contains(failed.Detail.Text, "ff 00 1b") {
		t.Fatalf("failed binary tool = %#v", failed)
	}
}

func TestModelBuildLifecycleOperatorAndFollowBounds(t *testing.T) {
	model := newTestModel(t, ActivityModelOptions{MaxEntries: 4, MaxBytes: 160})
	kinds := []struct {
		kind observability.ActivityKind
		data map[string]any
	}{
		{observability.ActivityBuildStarted, nil},
		{observability.ActivityBuildCompileCompleted, map[string]any{"result": "success"}},
		{observability.ActivityBuildCandidateStarted, nil},
		{observability.ActivityBuildCandidateReady, nil},
		{observability.ActivityBuildProtocolValidated, nil},
		{observability.ActivityBuildCompleted, map[string]any{"status": 0}},
	}
	for sequence, item := range kinds {
		model.Consume(modelEvent(uint64(sequence+1), item.kind, item.data, observability.ActivityHarness, ""))
	}
	entries := model.Entries()
	if len(entries) != 1 || entries[0].Kind != ActivityDisplayKindBuild {
		t.Fatalf("build entries = %#v", entries)
	}
	build := entries[0].Presentation.(BuildPresentation)
	if build.State != ActivityDisplayStateCompleted {
		t.Fatalf("build state = %s", build.State)
	}
	for _, phase := range build.Phases {
		if phase.State != ActivityDisplayStateCompleted {
			t.Fatalf("phase = %#v", phase)
		}
	}
	firstBuildKey := entries[0].Key
	model.Consume(modelEvent(20, observability.ActivityBuildStarted, nil, observability.ActivityHarness, ""))
	entries = model.Entries()
	if len(entries) != 2 || entries[1].Key == firstBuildKey {
		t.Fatalf("later build did not receive a fresh key: %#v", entries)
	}

	for index := 0; index < 5; index++ {
		command := "inspect " + strconv.Itoa(index)
		model.BeginOperatorBlock(&command)
		model.AppendOperatorOutput("Generation " + strconv.Itoa(index))
		model.FinishOperatorBlock()
	}
	if len(model.Entries()) > 4 || model.Entries()[0].Kind != ActivityDisplayKindNotice {
		t.Fatalf("bounded entries = %#v", model.Entries())
	}
	if !strings.Contains(model.RenderText(), "older live activity discarded") {
		t.Fatalf("missing discard marker in %q", model.RenderText())
	}

	follow := NewActivityFollowState()
	follow.Scrolled(42)
	follow.Arrived(3)
	if follow.Following || follow.NewEvents != 3 || follow.ScrollY != 42 {
		t.Fatalf("follow state = %#v", follow)
	}
	follow.ReturnToLive()
	if !follow.Following || follow.NewEvents != 0 {
		t.Fatalf("returned follow state = %#v", follow)
	}
}

func TestModelFailedWritePrefersDiagnosticAndAbnormalLifecycleRemainsVisible(t *testing.T) {
	model := newTestModel(t)
	source := strings.Repeat("int attempted_write;\n", 20)
	write := map[string]any{
		"tool": "write",
		"arguments": map[string]any{
			"path": "seed/tasks.c", "offset": 0, "data": source,
		},
		"error": "guest write failed",
	}
	model.Consume(modelEvent(1, observability.ActivityToolFailed, write, observability.ActivityImplementor, "write-error"))
	failed := model.Entries()[0].Presentation.(ToolPresentation)
	if failed.Detail == nil || failed.Detail.Text != "guest write failed" || strings.Contains(failed.Detail.Text, "attempted_write") {
		t.Fatalf("failed write detail = %#v", failed.Detail)
	}

	for sequence, kind := range []observability.ActivityKind{
		observability.ActivitySessionStarted,
		observability.ActivityTurnStarted,
		observability.ActivityTurnCompleted,
		observability.ActivityReviewStarted,
		observability.ActivityReviewCompleted,
		observability.ActivitySessionStopped,
	} {
		if model.Consume(modelEvent(uint64(sequence+2), kind, map[string]any{"model": "hidden"}, observability.ActivityImplementor, "")) {
			t.Fatalf("routine lifecycle event %s changed the model", kind)
		}
	}
	model.Consume(modelEvent(9, observability.ActivityTurnFailed, map[string]any{
		"status": "failed", "model": "not repeated",
	}, observability.ActivityImplementor, ""))
	entries := model.Entries()
	lifecycle, ok := entries[len(entries)-1].Presentation.(LifecyclePresentation)
	if !ok || lifecycle.State != ActivityDisplayStateFailed || strings.Contains(lifecycle.Detail, "model") {
		t.Fatalf("abnormal lifecycle presentation = %#v", entries[len(entries)-1].Presentation)
	}
}

func TestSafeDisplayFunctionsAreByteBoundedAndDeterministic(t *testing.T) {
	if got := SafeDisplayText("a\tb\r\x1b[2J\nλ", 8); got != "a    b\\r\\x1b[2J\n… 3 bytes more in activity payload …" {
		t.Fatalf("SafeDisplayText = %q", got)
	}
	data := []byte{0xff, 0x00, 0x1b, 0x80}
	if got := SafeDisplayBytes(data, 4); got != "binary (4 bytes): ff 00 … 2 bytes more …" {
		t.Fatalf("SafeDisplayBytes = %q", got)
	}
	if !bytes.Equal(data, []byte{0xff, 0x00, 0x1b, 0x80}) {
		t.Fatal("safe display mutated input")
	}
}

func TestModelConstructorRejectsInvalidBoundsAndEntriesUseUTF8Accounting(t *testing.T) {
	for _, options := range []ActivityModelOptions{{MaxEntries: -1}, {MaxBytes: -1}, {DisplayBytes: -1}} {
		if _, err := NewOperatorActivityModel(options); err == nil {
			t.Fatalf("invalid options accepted: %#v", options)
		}
	}
	model := newTestModel(t)
	model.Consume(modelEvent(1, observability.ActivityAgentMessage, map[string]any{"text": "λ"}, observability.ActivityImplementor, "item"))
	entry := model.Entries()[0]
	want := ActivityDisplayEntry{Key: entry.Key, Kind: ActivityDisplayKindMessage, Presentation: AgentMessagePresentation{Role: observability.ActivityImplementor, Text: "λ", Finalized: true}}
	if !reflect.DeepEqual(entry, want) || entry.SizeBytes() != len([]byte("λ")) {
		t.Fatalf("entry = %#v, size = %d", entry, entry.SizeBytes())
	}
}

func TestEntriesAreImmutableSnapshotsAndBooleanReasoningIndexMatchesPython(t *testing.T) {
	model := newTestModel(t)
	model.Consume(modelEvent(1, observability.ActivityBuildStarted, nil, observability.ActivityHarness, ""))
	first := model.Entries()
	build := first[0].Presentation.(BuildPresentation)
	build.Phases[0].State = ActivityDisplayStateFailed
	first[0].Presentation = build
	current := model.Entries()[0].Presentation.(BuildPresentation)
	if current.Phases[0].State != ActivityDisplayStateRunning {
		t.Fatalf("caller mutated model build phases: %#v", current.Phases)
	}

	model.Consume(modelEvent(2, observability.ActivityAgentReasoningDelta, map[string]any{
		"text": "Boolean index", "summary_index": true,
	}, observability.ActivityImplementor, "boolean-index"))
	entries := model.Entries()
	reasoning, ok := entries[len(entries)-1].Presentation.(ReasoningPresentation)
	if !ok || reasoning.Text != "Boolean index" {
		t.Fatalf("boolean reasoning index presentation = %#v", entries[len(entries)-1].Presentation)
	}
}

func TestModelEscapesToolNamesAndMatchesPythonStatusSemantics(t *testing.T) {
	model := newTestModel(t)
	hostileTool := "read\x1b[2J"
	model.Consume(modelEvent(1, observability.ActivityToolStarted, map[string]any{
		"tool": hostileTool, "arguments": map[string]any{},
	}, observability.ActivityImplementor, "hostile-tool"))
	tool := model.Entries()[0].Presentation.(ToolPresentation)
	if tool.Tool != `read\x1b[2J` || strings.Contains(model.RenderText(), "\x1b") {
		t.Fatalf("unsafe tool presentation = %#v; transcript = %q", tool, model.RenderText())
	}

	model.Consume(modelEvent(2, observability.ActivityBuildStarted, nil, observability.ActivityHarness, ""))
	model.Consume(modelEvent(3, observability.ActivityBuildCompleted, map[string]any{"status": "0"}, observability.ActivityHarness, ""))
	build := model.Entries()[1].Presentation.(BuildPresentation)
	if build.State != ActivityDisplayStateCompleted {
		t.Fatalf("string-zero build state = %q", build.State)
	}

	started := map[string]any{"tool": "write", "arguments": map[string]any{"path": "seed/a.c", "data": "content"}}
	model.Consume(modelEvent(4, observability.ActivityToolStarted, started, observability.ActivityImplementor, "float-status"))
	model.Consume(modelEvent(5, observability.ActivityToolCompleted, map[string]any{
		"tool": "write", "arguments": started["arguments"],
		"result": map[string]any{"status": float64(0), "output": []byte{}},
	}, observability.ActivityImplementor, "float-status"))
	completed := model.Entries()[2].Presentation.(ToolPresentation)
	if completed.Detail == nil || completed.Detail.Text != "content" {
		t.Fatalf("float-zero tool completion lost detail: %#v", completed)
	}

	model.Consume(modelEvent(6, observability.ActivityToolCompleted, map[string]any{
		"tool": "request_feature", "arguments": map[string]any{"title": "Capacity"},
		"result": map[string]any{"status": "0", "output": []byte("7")},
	}, observability.ActivityImplementor, "string-status-feature"))
	feature := model.Entries()[3].Presentation.(FeatureRequestPresentation)
	if feature.RequestID != "" {
		t.Fatalf("string-zero feature status produced request ID %q", feature.RequestID)
	}
}

func TestModelMatchesPythonPayloadRenderingEdges(t *testing.T) {
	model := newTestModel(t)
	model.Consume(modelEvent(1, observability.ActivityToolStarted, map[string]any{
		"tool": "write", "arguments": map[string]any{
			"path": "seed/blob", "data": "AB==", "encoding": "base64",
		},
	}, observability.ActivityImplementor, "base64"))
	detail := model.Entries()[0].Presentation.(ToolPresentation).Detail
	if detail == nil || !detail.Binary || detail.ByteCount != 1 {
		t.Fatalf("noncanonical base64 detail = %#v", detail)
	}

	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"typed map", map[string]string{"b": "x", "a": "y"}, `{"a": "y", "b": "x"}`},
		{"typed slice", []int{1, 2}, `[1, 2]`},
		{"bytes", []byte{0xff, 0}, `"b'\\xff\\x00'"`},
		{"single quote bytes", []byte("'"), `"b\"'\""`},
		{"float", 1.0, `1.0`},
		{"negative zero", math.Copysign(0, -1), `-0.0`},
		{"nan", math.NaN(), `NaN`},
		{"positive infinity", math.Inf(1), `Infinity`},
		{"negative infinity", math.Inf(-1), `-Infinity`},
		{"json zero", json.Number("0.0"), `0.0`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := pythonJSON(test.value); got != test.want {
				t.Fatalf("pythonJSON(%#v) = %q, want %q", test.value, got, test.want)
			}
		})
	}
}
