package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"codexos/internal/guest"
	"codexos/internal/observability"
	"codexos/internal/provenance"
	"codexos/internal/qemu"
	"codexos/internal/store"
)

const (
	generationHelperMode      = "CODEXOS_GO_GENERATION_MODE"
	generationHelperRecord    = "CODEXOS_GO_GENERATION_RECORD"
	generationHelperPlan      = "CODEXOS_GO_GENERATION_PLAN"
	generationHelperSentinel  = "CODEXOS_GO_GENERATION_SUITE_SENTINEL"
	generationHelperToolReady = "CODEXOS_GO_GENERATION_TOOL_READY"
)

type generationTestCall struct {
	name      string
	arguments [][]byte
}

type generationTestRuntime struct {
	mu             sync.Mutex
	root           string
	running        bool
	generation     uint64
	tools          []string
	calls          []generationTestCall
	invoke         func(context.Context, string, [][]byte) (guest.ToolResult, error)
	eventLog       *observability.EventLog
	metrics        *observability.Metrics
	previous       string
	previousSet    bool
	transition     string
	profile        qemu.HardwareProfile
	requests       []store.FeatureRequest
	featureErr     error
	listToolCalls  int
	callNotify     chan string
	listStarted    chan struct{}
	listRelease    chan struct{}
	finishFrozen   bool
	finishRetained bool
}

func newGenerationTestRuntime(t *testing.T) *generationTestRuntime {
	t.Helper()
	root := t.TempDir()
	eventLog, err := observability.OpenEventLog(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eventLog.Close)
	return &generationTestRuntime{
		root:       root,
		running:    true,
		generation: 12,
		tools:      []string{"list", "read", "write", "truncate", "remove", "build", "finish_generation", "request_feature", "list_provided_assets", "read_provided_asset"},
		eventLog:   eventLog,
		profile:    qemu.TestHardwareProfile,
		callNotify: make(chan string, 32),
	}
}

func (r *generationTestRuntime) GenerationRunning() bool { return r.running }

func (r *generationTestRuntime) GenerationNumber() (uint64, bool) {
	return r.generation, true
}

func (r *generationTestRuntime) ListTools(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.listToolCalls++
	tools := append([]string(nil), r.tools...)
	started, release := r.listStarted, r.listRelease
	r.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return tools, nil
}

func (r *generationTestRuntime) InvokeTool(ctx context.Context, name string, arguments [][]byte) (guest.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return guest.ToolResult{}, err
	}
	copyArguments := make([][]byte, len(arguments))
	for index, argument := range arguments {
		copyArguments[index] = append([]byte(nil), argument...)
	}
	r.mu.Lock()
	r.calls = append(r.calls, generationTestCall{name: name, arguments: copyArguments})
	invoke := r.invoke
	r.mu.Unlock()
	select {
	case r.callNotify <- name:
	default:
	}
	if invoke != nil {
		return invoke(ctx, name, copyArguments)
	}
	return guest.ToolResult{Status: 0}, nil
}

func (r *generationTestRuntime) RunDirectory() string              { return r.root }
func (r *generationTestRuntime) EventLog() *observability.EventLog { return r.eventLog }
func (r *generationTestRuntime) Metrics() *observability.Metrics   { return r.metrics }
func (r *generationTestRuntime) PreviousHandoff() (string, bool) {
	return r.previous, r.previousSet || r.previous != ""
}
func (r *generationTestRuntime) CurrentTransition() (string, bool) {
	return r.transition, r.transition != ""
}
func (r *generationTestRuntime) HardwareProfile() qemu.HardwareProfile { return r.profile }
func (r *generationTestRuntime) FeatureRequests() ([]store.FeatureRequest, error) {
	if r.featureErr != nil {
		return nil, r.featureErr
	}
	return append([]store.FeatureRequest(nil), r.requests...), nil
}
func (r *generationTestRuntime) GenerationState() string {
	if r.running {
		return "running"
	}
	return "awaiting_next_generation"
}
func (r *generationTestRuntime) RetainGenerationFinish(generation uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finishFrozen || r.finishRetained || r.running || generation != r.generation {
		return false
	}
	r.finishRetained = true
	return true
}
func (r *generationTestRuntime) GenerationFinishRetained(generation uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.finishRetained && generation == r.generation
}
func (r *generationTestRuntime) ReleaseGenerationFinish(generation uint64) {
	r.mu.Lock()
	if r.finishRetained && generation == r.generation {
		r.finishRetained = false
	}
	r.mu.Unlock()
}

func TestGenerationPlanningPromptAndToolPolicy(t *testing.T) {
	runtime := newGenerationTestRuntime(t)
	runtime.previous = "Inherited handoff."
	runtime.transition = "rollback"
	runtime.requests = []store.FeatureRequest{
		{ID: 1, Generation: 12, Title: "Approved", Description: "Provisioned.", Status: store.FeatureApproved},
		{ID: 2, Generation: 12, Title: "Pending", Description: "Not provisioned.", Status: store.FeaturePending},
	}
	objective := "Trusted objective."
	prompt, err := planningPrompt(runtime, &objective)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"genuinely general-purpose operating system",
		"supplied Doom executable and data must remain immutable",
		"preemptive execution",
		"Trusted tools available to you",
		"Trusted provided-asset host services",
		"Inherited handoff.",
		"Later lineage was abandoned.",
		"Trusted objective.",
		"#1: Approved\nProvisioned.",
	} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("planning prompt is missing %q", expected)
		}
	}
	if strings.Contains(prompt, "Not provisioned.") {
		t.Fatal("planning prompt exposed a non-approved feature request")
	}
	runtime.featureErr = errors.New("feature request state unavailable")
	promptSession := NewGenerationSession(runtime, GenerationSessionOptions{Objective: &objective})
	if _, err := promptSession.planningPrompt(); err == nil || err.Error() != "feature request state unavailable" {
		t.Fatalf("feature request prompt error = %v", err)
	}
	runtime.featureErr = nil

	session := NewGenerationSession(runtime, GenerationSessionOptions{})
	session.runCtx = context.Background()
	session.availableTools = map[string]struct{}{"list": {}}
	denied := session.dynamicToolResponse(map[string]any{
		"callId": "call-write", "threadId": "thread", "turnId": "turn",
		"namespace": "codexos", "tool": "write", "arguments": map[string]any{
			"path": "seed/kernel.c", "offset": 0, "data": "x",
		},
	}, "thread", "turn", true, nil)
	if denied["success"] != false || len(runtime.calls) != 0 {
		t.Fatalf("planning mutation response = %#v, calls = %#v", denied, runtime.calls)
	}
	text := denied["contentItems"].([]map[string]any)[0]["text"].(string)
	if !strings.Contains(text, "unavailable during the planning phase") {
		t.Fatalf("planning denial = %q", text)
	}

	allowed := session.dynamicToolResponse(map[string]any{
		"callId": "call-list", "threadId": "thread", "turnId": "turn",
		"namespace": "codexos", "tool": "list", "arguments": map[string]any{},
	}, "thread", "turn", true, nil)
	if allowed["success"] != true || len(runtime.calls) != 1 || runtime.calls[0].name != "list" {
		t.Fatalf("planning read response = %#v, calls = %#v", allowed, runtime.calls)
	}
}

func TestGenerationPromptPreservesExplicitEmptyHandoff(t *testing.T) {
	runtime := newGenerationTestRuntime(t)
	runtime.previousSet = true
	prompt, err := planningPrompt(runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "Previous generation handoff:\n") {
		t.Fatal("explicit empty handoff was rendered as absent")
	}
	if strings.Contains(prompt, "Previous generation handoff: none.") {
		t.Fatal("explicit empty handoff used the absent-handoff text")
	}

	runtime.previousSet = false
	prompt, err = planningPrompt(runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "Previous generation handoff: none.") {
		t.Fatal("absent handoff did not use the absent-handoff text")
	}
}

func TestGenerationToolSchemasPreserveAdvertisementOrderAndFeatureJSON(t *testing.T) {
	selected, order, err := advertisedGuestToolsInOrder([]string{"read", "unknown", "list", "build"})
	if err != nil {
		t.Fatal(err)
	}
	namespace := dynamicToolNamespaceInOrder(selected, order)
	tools, ok := namespace["tools"].([]map[string]any)
	if !ok || len(tools) != 4 {
		t.Fatalf("namespace tools = %#v", namespace["tools"])
	}
	var names []string
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		names = append(names, name)
	}
	if got, want := strings.Join(names, ","), "read,list,build,list_requests"; got != want {
		t.Fatalf("tool order = %q, want %q", got, want)
	}

	requests := []store.FeatureRequest{
		{ID: 2, Generation: 9, Title: "Second", Description: "B", Status: store.FeatureDenied},
		{ID: 1, Generation: 8, Title: "First", Description: "A", Status: store.FeatureApproved},
	}
	encoded, err := featureRequestsJSON(requests)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"requests":[{"description":"A","generation":8,"id":1,"status":"approved","title":"First"},{"description":"B","generation":9,"id":2,"status":"denied","title":"Second"}]}`
	if string(encoded) != want {
		t.Fatalf("feature request JSON = %s, want %s", encoded, want)
	}
	if requests[0].ID != 2 {
		t.Fatal("featureRequestsJSON reordered caller-owned input")
	}
}

func TestGenerationToolDispatchUsesExactGuestArguments(t *testing.T) {
	runtime := newGenerationTestRuntime(t)
	session := NewGenerationSession(runtime, GenerationSessionOptions{})
	session.runCtx = context.Background()
	session.availableTools = map[string]struct{}{"read": {}, "write": {}, "build": {}}
	if _, err := session.dispatchTool("read", map[string]any{"path": "seed/kernel.c", "offset": 3, "length": 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.dispatchTool("write", map[string]any{"path": "seed/new", "offset": 0, "encoding": "base64", "data": "AP8="}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.dispatchTool("build", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if len(runtime.calls) != 3 {
		t.Fatalf("runtime calls = %#v", runtime.calls)
	}
	if got := runtime.calls[0].arguments; len(got) != 3 || string(got[0]) != "seed/kernel.c" || string(got[1]) != "3" || string(got[2]) != "5" {
		t.Fatalf("read arguments = %#v", got)
	}
	if got := runtime.calls[1].arguments; len(got) != 3 || string(got[0]) != "seed/new" || string(got[1]) != "0" || string(got[2]) != "\x00\xff" {
		t.Fatalf("write arguments = %#v", got)
	}
	if got := runtime.calls[2].arguments; got == nil || len(got) != 0 {
		t.Fatalf("build arguments = %#v, want non-nil empty", got)
	}
	if _, err := session.dispatchTool("read", map[string]any{"path": "seed/kernel.c", "length": 5}); err == nil || err.Error() != "missing argument: offset" {
		t.Fatalf("missing read argument error = %v, want missing argument: offset", err)
	}
	if got, err := generationUnsignedDecimal(json.Number("-0"), "offset"); err != nil || string(got) != "0" {
		t.Fatalf("negative zero argument = %q, %v", got, err)
	}
}

func TestGenerationHelperSubprocessCannotEnterTestSuite(t *testing.T) {
	root := t.TempDir()
	recordPath := filepath.Join(root, "helper.json")
	sentinelPath := filepath.Join(root, "suite-entered")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGenerationSuiteSentinel$", "-test.count=1")
	command.Env = reviewerTestEnvironment(map[string]string{
		reviewerHelperEnvironment: "1",
		generationHelperMode:      "probe",
		generationHelperRecord:    recordPath,
		generationHelperSentinel:  sentinelPath,
	})
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("bounded generation helper probe did not exit: %v", ctx.Err())
	}
	if err != nil {
		t.Fatalf("generation helper probe failed: %v\n%s", err, output)
	}
	value := readReviewerJSON(t, recordPath)
	if value["mode"] != "probe" {
		t.Fatalf("generation helper record = %#v", value)
	}
	if _, err := os.Stat(sentinelPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("generation helper entered the Go test suite: %v", err)
	}
}

func TestGenerationSuiteSentinel(t *testing.T) {
	path := os.Getenv(generationHelperSentinel)
	if path == "" {
		return
	}
	if err := os.WriteFile(path, []byte("entered"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationSessionRunsPlanningAndImplementationOnOneThread(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "generation.json")
	setGenerationHelper(t, "success", recordPath)
	planText := "PRIVATE PLAN Ω must stay in response evidence"
	t.Setenv(generationHelperPlan, planText)
	runtime := newGenerationTestRuntime(t)
	runtime.invoke = func(_ context.Context, name string, _ [][]byte) (guest.ToolResult, error) {
		switch name {
		case "list":
			return guest.ToolResult{Status: 0, Output: []byte("seed/kernel.c\n")}, nil
		case "write":
			return guest.ToolResult{Status: 0}, nil
		default:
			return guest.ToolResult{}, fmt.Errorf("unexpected guest tool %s", name)
		}
	}
	session := NewGenerationSession(runtime, GenerationSessionOptions{
		Executable: os.Args[0], AuthFile: fakeAuthFile(t), StopTimeout: time.Second,
	})
	result, err := session.RunInitialTurn()
	if err != nil {
		if _, statErr := os.Stat(recordPath); statErr == nil {
			t.Fatalf("generation app-server start failed: %v; helper record: %#v", err, readReviewerJSON(t, recordPath))
		}
		t.Fatal(err)
	}
	if result.TurnStatus != "completed" || result.FinalMessage != "Implementation complete." {
		t.Fatalf("initial result = %#v", result)
	}
	if runtime.listToolCalls != 1 {
		t.Fatalf("ListTools calls = %d, want one", runtime.listToolCalls)
	}
	if got := []string{runtime.calls[0].name, runtime.calls[1].name}; strings.Join(got, ",") != "list,write" {
		t.Fatalf("guest calls = %#v", runtime.calls)
	}
	waitForReviewerFile(t, recordPath)
	record := readReviewerJSON(t, recordPath)
	if record["thread_id"] == "" {
		t.Fatalf("helper record = %#v", record)
	}
	messages, ok := record["messages"].([]any)
	if !ok {
		t.Fatalf("helper messages = %#v", record["messages"])
	}
	var turns []map[string]any
	for _, value := range messages {
		message, ok := value.(map[string]any)
		if ok && message["method"] == "turn/start" {
			turns = append(turns, message)
		}
	}
	if len(turns) != 2 {
		t.Fatalf("turn/start messages = %#v", turns)
	}
	if turns[0]["params"].(map[string]any)["threadId"] != turns[1]["params"].(map[string]any)["threadId"] {
		t.Fatal("planning and implementation did not reuse the thread")
	}
	firstParams := turns[0]["params"].(map[string]any)
	secondParams := turns[1]["params"].(map[string]any)
	if firstParams["permissions"] != planningPermissionProfile || secondParams["permissions"] != implementorPermissionProfile {
		t.Fatalf("turn permissions = %#v, %#v", firstParams, secondParams)
	}
	if roots, ok := firstParams["runtimeWorkspaceRoots"].([]any); !ok || len(roots) != 0 {
		t.Fatalf("planning roots = %#v", firstParams["runtimeWorkspaceRoots"])
	}
	if _, present := secondParams["runtimeWorkspaceRoots"]; present {
		t.Fatal("implementation turn unexpectedly supplied workspace roots")
	}
	manifestPath := filepath.Join(runtime.root, "planning-evidence", "generation-0012", "manifest.json")
	manifest := readReviewerJSON(t, manifestPath)
	if manifest["outcome"] != "completed" || manifest["thread_id"] != record["thread_id"] {
		t.Fatalf("planning manifest = %#v", manifest)
	}
	responseFile, ok := manifest["response_file"].(string)
	if !ok || responseFile != "response.txt" {
		t.Fatalf("planning response file = %#v", manifest["response_file"])
	}
	response, err := os.ReadFile(filepath.Join(runtime.root, "planning-evidence", "generation-0012", responseFile))
	if err != nil || string(response) != planText {
		t.Fatalf("private planning response = %q, %v", response, err)
	}
	events, err := os.ReadFile(runtime.eventLog.Path())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(events, []byte(planText)) {
		t.Fatal("private planning response leaked into events.jsonl")
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationSessionJoinsTerminalTurnToolBeforeImplementation(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "generation-terminal-tool.json")
	toolReadyPath := filepath.Join(t.TempDir(), "tool-ready")
	setGenerationHelper(t, "terminal-before-tool-result", recordPath)
	t.Setenv(generationHelperToolReady, toolReadyPath)
	runtime := newGenerationTestRuntime(t)
	toolStarted := make(chan struct{})
	releaseTool := make(chan struct{})
	var firstTool sync.Once
	runtime.invoke = func(ctx context.Context, _ string, _ [][]byte) (guest.ToolResult, error) {
		blocked := false
		firstTool.Do(func() {
			blocked = true
			if err := os.WriteFile(toolReadyPath, []byte("ready"), 0o600); err != nil {
				t.Errorf("write tool-ready marker: %v", err)
			}
			close(toolStarted)
		})
		if blocked {
			select {
			case <-releaseTool:
			case <-ctx.Done():
				return guest.ToolResult{}, ctx.Err()
			}
		}
		return guest.ToolResult{Status: 0}, nil
	}
	session := NewGenerationSession(runtime, GenerationSessionOptions{
		Executable: os.Args[0], AuthFile: fakeAuthFile(t), StopTimeout: time.Second,
	})
	type turnOutcome struct {
		result GenerationResult
		err    error
	}
	resultChannel := make(chan turnOutcome, 1)
	go func() {
		result, err := session.RunInitialTurn()
		resultChannel <- turnOutcome{result: result, err: err}
	}()
	select {
	case <-toolStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("planning tool did not start")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		session.mu.Lock()
		joining := session.turnPhase == "planning" && !session.turnAcceptingTools && session.activeTools == 1
		session.mu.Unlock()
		if joining {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("terminal planning turn did not begin joining its active tool")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case outcome := <-resultChannel:
		t.Fatalf("initial sequence advanced before its planning tool stopped: %#v", outcome)
	default:
	}
	close(releaseTool)
	select {
	case outcome := <-resultChannel:
		if outcome.err != nil || outcome.result.TurnStatus != "failed" {
			t.Fatalf("initial sequence = %#v, %v", outcome.result, outcome.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("initial sequence did not report the orphaned planning result")
	}
	if !session.Healthy() || !session.PlanningRetryRequired() || session.PlanningCompleted() {
		t.Fatalf("retry state: healthy=%t required=%t completed=%t", session.Healthy(), session.PlanningRetryRequired(), session.PlanningCompleted())
	}
	session.mu.Lock()
	turnNumber := session.turnNumber
	session.mu.Unlock()
	if turnNumber != 1 {
		t.Fatalf("implementation started before planning retry: turn number = %d", turnNumber)
	}
	eventsFile, err := os.Open(runtime.eventLog.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer eventsFile.Close()
	scanner := bufio.NewScanner(eventsFile)
	foundOrphan, foundRetryableFailure := false, false
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		data, _ := event["data"].(map[string]any)
		switch event["event"] {
		case "tool_result_orphaned":
			foundOrphan = data["request_id"] == "generation-call-0" && data["call_id"] == "generation-call-0" && data["turn_phase"] == "planning" && data["tool"] == "list"
		case "planning_failed":
			foundRetryableFailure = data["failure_kind"] == "orphaned_dynamic_call"
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !foundOrphan || !foundRetryableFailure {
		t.Fatalf("delivery evidence: orphan=%t retryable_failure=%t", foundOrphan, foundRetryableFailure)
	}
	manifestPath := filepath.Join(runtime.root, "planning-evidence", "generation-0012", "manifest.json")
	failedManifest := readReviewerJSON(t, manifestPath)
	attempts, _ := failedManifest["attempts"].([]any)
	if failedManifest["stage"] != "awaiting_resume" || failedManifest["outcome"] != "incomplete" || len(attempts) != 1 || attempts[0].(map[string]any)["outcome"] != "failed" {
		t.Fatalf("retryable planning manifest = %#v", failedManifest)
	}
	result, err := session.RunPlanningContinuationTurn()
	if err != nil || result.TurnStatus != "completed" || !session.PlanningCompleted() {
		t.Fatalf("planning retry sequence = %#v, %v", result, err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationSessionDoesNotImplementUndeliveredReview(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "generation-orphaned-review.json")
	setGenerationHelper(t, "orphaned-review", recordPath)
	t.Setenv(reviewerHelperMode, "success")
	reviewerExecutable := filepath.Join(t.TempDir(), "reviewer-helper")
	wrapper := "#!/bin/sh\nunset " + generationHelperMode + " " + generationHelperRecord + " " + generationHelperToolReady + "\nexec \"" + os.Args[0] + "\" \"$@\"\n"
	if err := os.WriteFile(reviewerExecutable, []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	runtime := newGenerationTestRuntime(t)
	session := NewGenerationSession(runtime, GenerationSessionOptions{
		Executable: os.Args[0], AuthFile: fakeAuthFile(t), ReviewerExecutable: reviewerExecutable, StopTimeout: time.Second,
	})
	result, err := session.RunInitialTurn()
	if err != nil || result.TurnStatus != "failed" {
		t.Fatalf("orphaned review planning result = %#v, %v", result, err)
	}
	if !session.Healthy() || !session.PlanningRetryRequired() || session.PlanningCompleted() {
		t.Fatalf("orphaned review retry state: healthy=%t required=%t completed=%t", session.Healthy(), session.PlanningRetryRequired(), session.PlanningCompleted())
	}
	session.mu.Lock()
	turnNumber := session.turnNumber
	session.mu.Unlock()
	if turnNumber != 1 {
		t.Fatalf("implementation started after undelivered review: turn number = %d", turnNumber)
	}
	events, err := os.ReadFile(runtime.eventLog.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(events, []byte(`"event":"review_completed"`)) ||
		!bytes.Contains(events, []byte(`"event":"tool_response_write_attempted"`)) ||
		!bytes.Contains(events, []byte(`"event":"tool_result_orphaned"`)) ||
		!bytes.Contains(events, []byte(`"call_id":"generation-call-0"`)) {
		t.Fatalf("missing review delivery lifecycle evidence:\n%s", events)
	}
	result, err = session.RunPlanningContinuationTurn()
	if err != nil || result.TurnStatus != "completed" || !session.PlanningCompleted() {
		t.Fatalf("review planning retry sequence = %#v, %v", result, err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationSessionRetriesOrdinaryTerminalFailureOnSameThread(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "generation.json")
	setGenerationHelper(t, "continuation-failed", recordPath)
	runtime := newGenerationTestRuntime(t)
	session := NewGenerationSession(runtime, GenerationSessionOptions{
		Executable: os.Args[0], AuthFile: fakeAuthFile(t), StopTimeout: time.Second,
	})
	defer session.Close()
	if _, err := session.RunInitialTurn(); err != nil {
		t.Fatal(err)
	}
	pid, ok := session.ProcessPID()
	if !ok {
		t.Fatal("session process is unavailable before failed continuation")
	}
	threadID := session.ThreadID()
	if _, err := session.RunContinuationTurn(); err == nil || !strings.Contains(err.Error(), "Codex turn failed") {
		t.Fatalf("ordinary failed continuation error = %v", err)
	}
	if !session.Healthy() {
		t.Fatal("ordinary terminal failure poisoned the reusable session")
	}
	result, err := session.RunContinuationTurn()
	if err != nil {
		t.Fatal(err)
	}
	if result.TurnStatus != "completed" {
		t.Fatalf("retry result = %#v", result)
	}
	if got, ok := session.ProcessPID(); !ok || got != pid {
		t.Fatalf("retry process = %d, %v; want %d", got, ok, pid)
	}
	if got := session.ThreadID(); got != threadID {
		t.Fatalf("retry thread = %q, want %q", got, threadID)
	}
}

func TestGenerationSessionRetainsReadOnlyExitInterviewOnFrozenThread(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "generation-interview.json")
	setGenerationHelper(t, "interview", recordPath)
	runtime := newGenerationTestRuntime(t)
	activity := observability.NewActivityStream()
	session := NewGenerationSession(runtime, GenerationSessionOptions{
		Executable: os.Args[0], AuthFile: fakeAuthFile(t), ActivityStream: activity, StopTimeout: time.Second,
	})
	initial, err := session.RunInitialTurn()
	if err != nil {
		t.Fatal(err)
	}
	if initial.FinalMessage != "Implementation complete." {
		t.Fatalf("initial result = %#v", initial)
	}
	pid, ok := session.ProcessPID()
	if !ok {
		t.Fatal("generation app-server has no live process")
	}
	threadID := session.ThreadID()
	if err := session.RetainForExitInterview(); err == nil || !strings.Contains(err.Error(), "not frozen") {
		t.Fatalf("retain before gate error = %v", err)
	}
	runtime.running = false
	if err := session.RetainForExitInterview(); err == nil || !strings.Contains(err.Error(), "not frozen") {
		t.Fatalf("retain without successor error = %v", err)
	}
	runtime.finishFrozen = true
	if err := session.RetainForExitInterview(); err != nil {
		t.Fatal(err)
	}
	if !session.ExitInterviewAvailable() || session.Mode() != GenerationModeRetainedAtGate {
		t.Fatalf("retained mode = %q, available = %v", session.Mode(), session.ExitInterviewAvailable())
	}
	if _, err := session.RunContinuationTurn(); err == nil || !strings.Contains(err.Error(), "unavailable after finish") {
		t.Fatalf("ordinary turn after retain error = %v", err)
	}
	runtime.ReleaseGenerationFinish(runtime.generation)
	if err := session.BeginExitInterview(); err == nil || !strings.Contains(err.Error(), "not frozen") {
		t.Fatalf("begin after gate lease loss error = %v", err)
	}
	runtime.mu.Lock()
	runtime.finishRetained = true
	runtime.mu.Unlock()
	if err := session.BeginExitInterview(); err != nil {
		t.Fatal(err)
	}
	if err := session.BeginExitInterview(); err == nil || !strings.Contains(err.Error(), "already active") {
		t.Fatalf("duplicate begin error = %v", err)
	}
	// Admission is part of the active-turn reservation even before turn/start
	// returns an ID. End must not close a question accepted by another caller.
	session.mu.Lock()
	admission := make(chan struct{})
	session.mode = GenerationModeInterviewTurn
	session.interviewPending = true
	session.interviewAdmissionDone = admission
	session.mu.Unlock()
	if err := session.EndExitInterview(); err == nil || !strings.Contains(err.Error(), "still active") {
		t.Fatalf("end during interview admission error = %v", err)
	}
	session.mu.Lock()
	session.finishInterviewAdmissionLocked(admission)
	session.mode = GenerationModeRetainedAtGate
	session.mu.Unlock()
	marker := "EXIT-INTERVIEW-QUESTION-MUST-STAY-PRIVATE"
	first, err := session.RunExitInterviewTurn(marker)
	if err != nil {
		t.Fatal(err)
	}
	second, err := session.RunExitInterviewTurn("Why this design?")
	if err != nil {
		t.Fatal(err)
	}
	if first.FinalMessage != "Retrospective answer." || first.Summary != "Exit interview turn completed." || second.FinalMessage != "Second retrospective answer." {
		t.Fatalf("interview results = %#v, %#v", first, second)
	}
	if currentPID, live := session.ProcessPID(); !live || currentPID != pid || session.ThreadID() != threadID {
		t.Fatalf("interview changed process/thread: pid=(%d,%v), thread=%q", currentPID, live, session.ThreadID())
	}
	snapshot, ok := session.ExitInterviewTranscript()
	if !ok || len(snapshot.Turns) != 2 {
		t.Fatalf("interview snapshot = %#v, %v", snapshot, ok)
	}
	if snapshot.Metadata.Run != filepath.Base(runtime.root) || snapshot.Metadata.Generation != runtime.generation || snapshot.Metadata.AgentContractVersion != AgentContractVersion {
		t.Fatalf("interview metadata = %#v", snapshot.Metadata)
	}
	if snapshot.Turns[0].Question != marker || snapshot.Turns[1].Question != "Why this design?" ||
		strings.Join(snapshot.Turns[0].ReasoningSummaries, "|") != "First explicit summary.|Then another." ||
		snapshot.Turns[0].Response == nil || *snapshot.Turns[0].Response != "Retrospective answer." ||
		snapshot.Turns[1].Response == nil || *snapshot.Turns[1].Response != "Second retrospective answer." {
		t.Fatalf("interview turns = %#v", snapshot.Turns)
	}
	if strings.Contains(fmt.Sprintf("%#v", snapshot), "PRIVATE RAW") {
		t.Fatalf("raw reasoning entered transcript: %#v", snapshot)
	}
	if len(runtime.calls) != 2 {
		t.Fatalf("interview invoked guest tools: %#v", runtime.calls)
	}
	waitForReviewerFile(t, recordPath)
	record := readReviewerJSON(t, recordPath)
	messages := record["messages"].([]any)
	turns := make([]map[string]any, 0, 4)
	for _, value := range messages {
		message, valid := value.(map[string]any)
		if valid && message["method"] == "turn/start" {
			turns = append(turns, message["params"].(map[string]any))
		}
	}
	if len(turns) != 4 {
		t.Fatalf("turn/start messages = %#v", turns)
	}
	for _, turn := range turns[2:] {
		if turn["threadId"] != threadID || turn["permissions"] != interviewPermissionProfile {
			t.Fatalf("interview turn identity/policy = %#v", turn)
		}
		if roots, valid := turn["runtimeWorkspaceRoots"].([]any); !valid || len(roots) != 0 {
			t.Fatalf("interview workspace roots = %#v", turn["runtimeWorkspaceRoots"])
		}
		if _, present := turn["dynamicTools"]; present {
			t.Fatalf("interview turn exposed dynamic tools: %#v", turn)
		}
	}
	prompt := turns[2]["input"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(prompt, "read-only exit interview") || !strings.Contains(prompt, "Operator question:\n"+marker) {
		t.Fatalf("interview prompt = %q", prompt)
	}
	serverResponses := record["messages"].([]any)
	denials := 0
	for _, value := range serverResponses {
		message, valid := value.(map[string]any)
		if !valid || message["method"] != nil || message["result"] == nil {
			continue
		}
		result, valid := message["result"].(map[string]any)
		if valid && result["success"] == false {
			denials++
		}
	}
	if denials != 2 {
		t.Fatalf("interview tool denials = %d, record = %#v", denials, record)
	}
	events, err := os.ReadFile(runtime.eventLog.Path())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(events, []byte(marker)) || bytes.Contains(events, []byte("Retrospective answer.")) {
		t.Fatalf("interview text leaked into events: %s", events)
	}
	questionCount := 0
	for _, event := range activity.Drain() {
		if event.Kind == observability.ActivityExitInterviewQuestion {
			questionCount++
		}
	}
	if questionCount != 2 {
		t.Fatalf("exit interview question activities = %d", questionCount)
	}
	if err := session.EndExitInterview(); err != nil {
		t.Fatal(err)
	}
	if session.Mode() != GenerationModeClosed || generationProcessAlive(pid) {
		t.Fatalf("ended interview mode/process = %q/%v", session.Mode(), generationProcessAlive(pid))
	}
	if runtime.GenerationFinishRetained(runtime.generation) {
		t.Fatal("ended interview did not release the frozen gate")
	}
}

func TestGenerationWorkerRejectsConcurrentRunAndAlwaysClosesSession(t *testing.T) {
	holdRecord := filepath.Join(t.TempDir(), "generation-worker-hold.json")
	setGenerationHelper(t, "hold", holdRecord)
	runtime := newGenerationTestRuntime(t)
	worker := NewGenerationWorker(GenerationSessionOptions{
		Executable: os.Args[0], AuthFile: fakeAuthFile(t), StopTimeout: time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := worker.RunGeneration(ctx, runtime)
		result <- err
	}()
	waitForReviewerFile(t, holdRecord)
	hold := readReviewerJSON(t, holdRecord)
	pid := int(hold["pid"].(float64))
	if _, err := worker.RunGeneration(context.Background(), runtime); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("concurrent worker error = %v", err)
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("cancelled generation worker unexpectedly succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled generation worker did not return")
	}
	if generationProcessAlive(pid) {
		t.Fatalf("cancelled worker left app-server process %d alive", pid)
	}

	successRecord := filepath.Join(t.TempDir(), "generation-worker-success.json")
	setGenerationHelper(t, "worker-success", successRecord)
	runtime = newGenerationTestRuntime(t)
	completed, err := worker.RunGeneration(context.Background(), runtime)
	if err != nil {
		t.Fatal(err)
	}
	if completed.FinalMessage != "Implementation complete." {
		t.Fatalf("worker result = %#v", completed)
	}
	waitForReviewerFile(t, successRecord)
	success := readReviewerJSON(t, successRecord)
	successPID := int(success["pid"].(float64))
	if generationProcessAlive(successPID) {
		t.Fatalf("completed worker left app-server process %d alive", successPID)
	}
}

func TestGenerationSessionClosePreservesPartialInterviewWithoutResponse(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "generation-interview-hold.json")
	setGenerationHelper(t, "interview-hold", recordPath)
	runtime := newGenerationTestRuntime(t)
	session := NewGenerationSession(runtime, GenerationSessionOptions{
		Executable: os.Args[0], AuthFile: fakeAuthFile(t), StopTimeout: time.Second,
	})
	if _, err := session.RunInitialTurn(); err != nil {
		t.Fatal(err)
	}
	runtime.running = false
	runtime.finishFrozen = true
	if err := session.RetainForExitInterview(); err != nil {
		t.Fatal(err)
	}
	if err := session.BeginExitInterview(); err != nil {
		t.Fatal(err)
	}
	pid, _ := session.ProcessPID()
	turnResult := make(chan error, 1)
	go func() {
		_, err := session.RunExitInterviewTurn("Explain the interrupted work.")
		turnResult <- err
	}()
	waitForReviewerFile(t, recordPath)
	deadline := time.Now().Add(2 * time.Second)
	for {
		snapshot, ok := session.ExitInterviewTranscript()
		if ok && len(snapshot.Turns) == 1 && strings.Join(snapshot.Turns[0].ReasoningSummaries, "|") == "Partial visible summary." {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("partial reasoning did not reach transcript: %#v", snapshot)
		}
		time.Sleep(time.Millisecond)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-turnResult:
		if err == nil {
			t.Fatal("closed active interview unexpectedly completed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active interview did not return after close")
	}
	snapshot, ok := session.ExitInterviewTranscript()
	if !ok || len(snapshot.Turns) != 1 {
		t.Fatalf("partial transcript = %#v, %v", snapshot, ok)
	}
	turn := snapshot.Turns[0]
	if turn.Status != "failed" || turn.Response != nil || strings.Join(turn.ReasoningSummaries, "|") != "Partial visible summary." {
		t.Fatalf("closed partial turn = %#v", turn)
	}
	events, err := os.ReadFile(runtime.eventLog.Path())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(events, []byte(`"event":"exit_interview_ended"`)) != 1 || !bytes.Contains(events, []byte(`"result":"closed"`)) {
		t.Fatalf("interview close events = %s", events)
	}
	if session.Mode() != GenerationModeClosed || generationProcessAlive(pid) {
		t.Fatalf("closed interview mode/process = %q/%v", session.Mode(), generationProcessAlive(pid))
	}
}

func TestGenerationSessionInterruptsInterviewAndReturnsToFrozenGate(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "generation-interview-interrupt.json")
	setGenerationHelper(t, "interview-interrupt", recordPath)
	runtime := newGenerationTestRuntime(t)
	session := NewGenerationSession(runtime, GenerationSessionOptions{
		Executable: os.Args[0], AuthFile: fakeAuthFile(t), StopTimeout: time.Second,
	})
	if _, err := session.RunInitialTurn(); err != nil {
		t.Fatal(err)
	}
	runtime.running = false
	runtime.finishFrozen = true
	if err := session.RetainForExitInterview(); err != nil {
		t.Fatal(err)
	}
	if err := session.BeginExitInterview(); err != nil {
		t.Fatal(err)
	}
	turnResult := make(chan struct {
		result GenerationResult
		err    error
	}, 1)
	go func() {
		result, err := session.RunExitInterviewTurn("Why was this unfinished?")
		turnResult <- struct {
			result GenerationResult
			err    error
		}{result, err}
	}()
	deadline := time.Now().Add(2 * time.Second)
	turnStarted := false
	for !turnStarted && time.Now().Before(deadline) {
		session.mu.Lock()
		turnStarted = session.turnID != ""
		session.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	if !turnStarted {
		t.Fatal("interview turn did not become active")
	}
	if err := session.InterruptTurn(time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case outcome := <-turnResult:
		if outcome.err != nil || outcome.result.TurnStatus != "interrupted" || outcome.result.Summary != "Exit interview turn interrupted." {
			t.Fatalf("interrupted interview result = %#v, %v", outcome.result, outcome.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("interrupted interview did not return")
	}
	if session.Mode() != GenerationModeRetainedAtGate || !session.ExitInterviewAvailable() {
		t.Fatalf("post-interrupt mode = %q, available = %v", session.Mode(), session.ExitInterviewAvailable())
	}
	snapshot, ok := session.ExitInterviewTranscript()
	if !ok || len(snapshot.Turns) != 1 {
		t.Fatalf("interrupted transcript = %#v, %v", snapshot, ok)
	}
	turn := snapshot.Turns[0]
	if turn.Status != "interrupted" || turn.Response != nil || strings.Join(turn.ReasoningSummaries, "|") != "Interrupted visible summary." {
		t.Fatalf("interrupted transcript turn = %#v", turn)
	}
	if err := session.EndExitInterview(); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationSessionInterruptsAndContinuesOnSameThread(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "generation-interrupt.json")
	setGenerationHelper(t, "interrupt", recordPath)
	runtime := newGenerationTestRuntime(t)
	runtime.invoke = func(_ context.Context, name string, _ [][]byte) (guest.ToolResult, error) {
		if name != "list" && name != "write" {
			return guest.ToolResult{}, fmt.Errorf("unexpected guest tool %s", name)
		}
		return guest.ToolResult{Status: 0}, nil
	}
	session := NewGenerationSession(runtime, GenerationSessionOptions{
		Executable: os.Args[0], AuthFile: fakeAuthFile(t), StopTimeout: time.Second,
	})
	resultChannel := make(chan struct {
		result GenerationResult
		err    error
	}, 1)
	go func() {
		result, err := session.RunInitialTurn()
		resultChannel <- struct {
			result GenerationResult
			err    error
		}{result, err}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for session.ActiveTurnPhase() != "implementation" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if session.ActiveTurnPhase() != "implementation" {
		t.Fatal("implementation turn did not become active")
	}
	if !waitGenerationToolCall(runtime, "write", 2*time.Second) {
		t.Fatal("implementation tool call did not reach the runtime")
	}
	if err := session.InterruptTurn(time.Second); err != nil {
		t.Fatal(err)
	}
	first := <-resultChannel
	if first.err != nil || first.result.TurnStatus != "interrupted" {
		t.Fatalf("interrupted initial result = %#v, %v", first.result, first.err)
	}
	continued, err := session.RunContinuationTurn("Continue after the pause.")
	if err != nil || continued.TurnStatus != "completed" || continued.FinalMessage != "Continuation complete." {
		t.Fatalf("continuation = %#v, %v", continued, err)
	}
	waitForReviewerFile(t, recordPath)
	record := readReviewerJSON(t, recordPath)
	messages := record["messages"].([]any)
	turnCount := 0
	interruptCount := 0
	var thread string
	for _, value := range messages {
		message := value.(map[string]any)
		switch message["method"] {
		case "turn/start":
			turnCount++
			params := message["params"].(map[string]any)
			if thread == "" {
				thread = params["threadId"].(string)
			}
			if params["threadId"] != thread {
				t.Fatal("continuation changed the app-server thread")
			}
		case "turn/interrupt":
			interruptCount++
		}
	}
	if turnCount != 3 || interruptCount != 1 {
		t.Fatalf("turn lifecycle = %d turns, %d interrupts", turnCount, interruptCount)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationSessionInterruptFailureRetiresSession(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "generation-interrupt-failed.json")
	setGenerationHelper(t, "interrupt-failed", recordPath)
	runtime := newGenerationTestRuntime(t)
	runtime.invoke = func(_ context.Context, name string, _ [][]byte) (guest.ToolResult, error) {
		if name != "list" && name != "write" {
			return guest.ToolResult{}, fmt.Errorf("unexpected guest tool %s", name)
		}
		return guest.ToolResult{Status: 0}, nil
	}
	session := NewGenerationSession(runtime, GenerationSessionOptions{
		Executable: os.Args[0], AuthFile: fakeAuthFile(t), StopTimeout: time.Second,
	})
	resultChannel := make(chan struct {
		result GenerationResult
		err    error
	}, 1)
	go func() {
		result, err := session.RunInitialTurn()
		resultChannel <- struct {
			result GenerationResult
			err    error
		}{result, err}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for session.ActiveTurnPhase() != "implementation" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if session.ActiveTurnPhase() != "implementation" {
		t.Fatal("implementation turn did not become active")
	}
	if !waitGenerationToolCall(runtime, "write", 2*time.Second) {
		t.Fatal("implementation tool call did not reach the runtime")
	}
	pid, ok := session.ProcessPID()
	if !ok || pid <= 0 {
		t.Fatalf("failed interrupt process PID = %d, %v", pid, ok)
	}
	if err := session.InterruptTurn(500 * time.Millisecond); err == nil || !strings.Contains(err.Error(), "interrupted status") {
		if _, statErr := os.Stat(recordPath); statErr == nil {
			t.Fatalf("failed interrupt error = %v; helper record = %#v", err, readReviewerJSON(t, recordPath))
		}
		t.Fatalf("failed interrupt error = %v; phase=%q active=%v", err, session.ActiveTurnPhase(), session.ActiveTurn())
	}
	if session.Mode() != GenerationModeClosed || session.Healthy() || session.ActiveTurn() {
		t.Fatalf("failed interrupt session state = mode=%s healthy=%v active=%v", session.Mode(), session.Healthy(), session.ActiveTurn())
	}
	select {
	case <-resultChannel:
	case <-time.After(2 * time.Second):
		t.Fatal("failed interrupt left the turn goroutine active")
	}
	waitForGenerationProcessExit(t, pid)
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationSessionPausesPlanningAndResumesIntoImplementation(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "generation-planning-interrupt.json")
	setGenerationHelper(t, "planning-interrupt", recordPath)
	runtime := newGenerationTestRuntime(t)
	runtime.invoke = func(_ context.Context, name string, _ [][]byte) (guest.ToolResult, error) {
		if name != "list" && name != "write" {
			return guest.ToolResult{}, fmt.Errorf("unexpected guest tool %s", name)
		}
		return guest.ToolResult{Status: 0}, nil
	}
	session := NewGenerationSession(runtime, GenerationSessionOptions{
		Executable: os.Args[0], AuthFile: fakeAuthFile(t), StopTimeout: time.Second,
	})
	resultChannel := make(chan struct {
		result GenerationResult
		err    error
	}, 1)
	go func() {
		result, err := session.RunInitialTurn()
		resultChannel <- struct {
			result GenerationResult
			err    error
		}{result, err}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for session.ActiveTurnPhase() != "planning" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if session.ActiveTurnPhase() != "planning" {
		t.Fatal("planning turn did not become active")
	}
	if !waitGenerationToolCall(runtime, "list", 2*time.Second) {
		t.Fatal("planning tool call did not reach the runtime")
	}
	if err := session.InterruptTurn(time.Second); err != nil {
		t.Fatal(err)
	}
	first := <-resultChannel
	if first.err != nil || first.result.TurnStatus != "interrupted" {
		t.Fatalf("interrupted planning result = %#v, %v", first.result, first.err)
	}
	if session.PlanningCompleted() {
		t.Fatal("interrupted planning turn was marked completed")
	}
	manifestPath := filepath.Join(runtime.root, "planning-evidence", "generation-0012", "manifest.json")
	manifest := readReviewerJSON(t, manifestPath)
	attempts, ok := manifest["attempts"].([]any)
	if manifest["outcome"] != "incomplete" || manifest["stage"] != "awaiting_resume" || !ok || len(attempts) != 1 {
		t.Fatalf("interrupted planning manifest = %#v", manifest)
	}
	if attempts[0].(map[string]any)["outcome"] != "interrupted" {
		t.Fatalf("interrupted planning attempt = %#v", attempts[0])
	}
	resumed, err := session.RunPlanningContinuationTurn("Resume the plan.")
	if err != nil || resumed.TurnStatus != "completed" || resumed.FinalMessage != "Implementation complete." {
		t.Fatalf("planning continuation = %#v, %v", resumed, err)
	}
	if !session.PlanningCompleted() {
		t.Fatal("resumed planning did not unlock implementation")
	}
	waitForReviewerFile(t, recordPath)
	record := readReviewerJSON(t, recordPath)
	messages := record["messages"].([]any)
	turnCount := 0
	interruptCount := 0
	var thread string
	for _, value := range messages {
		message := value.(map[string]any)
		switch message["method"] {
		case "turn/start":
			turnCount++
			params := message["params"].(map[string]any)
			if thread == "" {
				thread = params["threadId"].(string)
			}
			if params["threadId"] != thread {
				t.Fatal("planning resume changed the app-server thread")
			}
		case "turn/interrupt":
			interruptCount++
		}
	}
	if turnCount != 3 || interruptCount != 1 {
		t.Fatalf("planning pause lifecycle = %d turns, %d interrupts", turnCount, interruptCount)
	}
	if got := []string{runtime.calls[0].name, runtime.calls[1].name, runtime.calls[2].name}; strings.Join(got, ",") != "list,list,write" {
		t.Fatalf("planning pause guest calls = %#v", runtime.calls)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationSessionRejectsConcurrentPlanningContinuation(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "generation-planning-concurrent.json")
	setGenerationHelper(t, "planning-interrupt", recordPath)
	runtime := newGenerationTestRuntime(t)
	runtime.invoke = func(_ context.Context, name string, _ [][]byte) (guest.ToolResult, error) {
		if name != "list" && name != "write" {
			return guest.ToolResult{}, fmt.Errorf("unexpected guest tool %s", name)
		}
		return guest.ToolResult{Status: 0}, nil
	}
	session := NewGenerationSession(runtime, GenerationSessionOptions{
		Executable: os.Args[0], AuthFile: fakeAuthFile(t), StopTimeout: time.Second,
	})
	initial := make(chan struct {
		result GenerationResult
		err    error
	}, 1)
	go func() {
		result, err := session.RunInitialTurn()
		initial <- struct {
			result GenerationResult
			err    error
		}{result, err}
	}()
	if !waitGenerationToolCall(runtime, "list", 2*time.Second) {
		t.Fatal("initial planning tool call did not reach the runtime")
	}
	if _, err := session.RunPlanningContinuationTurn("must be rejected while active"); err == nil || !strings.Contains(err.Error(), "already active") {
		t.Fatalf("concurrent planning continuation error = %v", err)
	}
	if err := session.InterruptTurn(time.Second); err != nil {
		t.Fatal(err)
	}
	result := <-initial
	if result.err != nil || result.result.TurnStatus != "interrupted" {
		t.Fatalf("interrupted initial result = %#v, %v", result.result, result.err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationSessionFailedPlanningDoesNotStartImplementation(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "generation-planning-failed.json")
	setGenerationHelper(t, "planning-failed", recordPath)
	runtime := newGenerationTestRuntime(t)
	runtime.invoke = func(_ context.Context, name string, _ [][]byte) (guest.ToolResult, error) {
		if name != "list" {
			return guest.ToolResult{}, fmt.Errorf("unexpected guest tool %s", name)
		}
		return guest.ToolResult{Status: 0}, nil
	}
	session := NewGenerationSession(runtime, GenerationSessionOptions{
		Executable: os.Args[0], AuthFile: fakeAuthFile(t), StopTimeout: time.Second,
	})
	result, err := session.RunInitialTurn()
	if err == nil || !strings.Contains(err.Error(), "fake planning failure") {
		t.Fatalf("failed planning result = %#v, %v", result, err)
	}
	if len(runtime.calls) != 1 || runtime.calls[0].name != "list" {
		t.Fatalf("failed planning guest calls = %#v", runtime.calls)
	}
	waitForReviewerFile(t, recordPath)
	record := readReviewerJSON(t, recordPath)
	messages := record["messages"].([]any)
	for _, value := range messages {
		if value.(map[string]any)["method"] == "turn/start" {
			params := value.(map[string]any)["params"].(map[string]any)
			if params["permissions"] == implementorPermissionProfile {
				t.Fatal("failed planning started implementation")
			}
		}
	}
	manifest := readReviewerJSON(t, filepath.Join(runtime.root, "planning-evidence", "generation-0012", "manifest.json"))
	attempts := manifest["attempts"].([]any)
	if manifest["outcome"] != "failed" || manifest["stage"] != "completed" || len(attempts) != 1 || attempts[0].(map[string]any)["outcome"] != "failed" {
		t.Fatalf("failed planning manifest = %#v", manifest)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationSessionPlanningEvidenceCompletionFailureFailsClosed(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "generation-planning-evidence-failed.json")
	setGenerationHelper(t, "planning-complete-failure", recordPath)
	runtime := newGenerationTestRuntime(t)
	runtime.invoke = func(_ context.Context, name string, _ [][]byte) (guest.ToolResult, error) {
		if name != "list" {
			return guest.ToolResult{}, fmt.Errorf("unexpected guest tool %s", name)
		}
		// Force PlanningEvidence.Complete to fail while leaving its active
		// attempt recoverable by the fail-closed path.
		responsePath := filepath.Join(runtime.root, "planning-evidence", "generation-0012", "response.txt")
		if err := os.Mkdir(responsePath, 0o755); err != nil {
			return guest.ToolResult{}, err
		}
		return guest.ToolResult{Status: 0}, nil
	}
	session := NewGenerationSession(runtime, GenerationSessionOptions{
		Executable: os.Args[0], AuthFile: fakeAuthFile(t), StopTimeout: time.Second,
	})
	result, err := session.RunInitialTurn()
	if err == nil || !strings.Contains(err.Error(), "cannot write planning evidence response.txt") {
		t.Fatalf("planning evidence completion result = %#v, %v", result, err)
	}
	if session.Healthy() {
		t.Fatal("planning evidence completion failure left session healthy")
	}
	manifest := readReviewerJSON(t, filepath.Join(runtime.root, "planning-evidence", "generation-0012", "manifest.json"))
	attempts := manifest["attempts"].([]any)
	if manifest["outcome"] != "failed" || manifest["stage"] != "completed" || len(attempts) != 1 || attempts[0].(map[string]any)["outcome"] != "failed" {
		t.Fatalf("planning evidence completion failure manifest = %#v", manifest)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationSessionPromptFailureFailsAllocatedEvidence(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "generation-prompt-failed.json")
	setGenerationHelper(t, "hold", recordPath)
	runtime := newGenerationTestRuntime(t)
	runtime.featureErr = errors.New("feature request state unavailable")
	session := NewGenerationSession(runtime, GenerationSessionOptions{
		Executable: os.Args[0], AuthFile: fakeAuthFile(t), StopTimeout: time.Second,
	})
	if _, err := session.RunInitialTurn(); err == nil || !strings.Contains(err.Error(), "feature request state unavailable") {
		t.Fatalf("prompt failure = %v", err)
	}
	manifest := readReviewerJSON(t, filepath.Join(runtime.root, "planning-evidence", "generation-0012", "manifest.json"))
	attempts := manifest["attempts"].([]any)
	if manifest["outcome"] != "failed" || manifest["stage"] != "completed" || len(attempts) != 1 || attempts[0].(map[string]any)["outcome"] != "failed" {
		t.Fatalf("prompt failure manifest = %#v", manifest)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationSessionCloseFailsAllocatedPlanningEvidence(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "generation-close-before-turn.json")
	setGenerationHelper(t, "hold", recordPath)
	runtime := newGenerationTestRuntime(t)
	session := NewGenerationSession(runtime, GenerationSessionOptions{
		Executable: os.Args[0], AuthFile: fakeAuthFile(t), StopTimeout: time.Second,
	})
	if err := session.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	evidence, err := provenance.NewPlanningEvidenceStore(runtime.root).Begin(runtime.generation, session.ThreadID())
	if err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	session.planningEvidence = evidence
	session.mu.Unlock()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	manifest := readReviewerJSON(t, filepath.Join(runtime.root, "planning-evidence", "generation-0012", "manifest.json"))
	attempts := manifest["attempts"].([]any)
	if manifest["outcome"] != "failed" || manifest["stage"] != "completed" || len(attempts) != 1 || attempts[0].(map[string]any)["outcome"] != "failed" {
		t.Fatalf("close-before-turn manifest = %#v", manifest)
	}
}

func TestGenerationSessionPlanningManifestFailureReturnsDurabilityError(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "generation-manifest-write-failed.json")
	setGenerationHelper(t, "planning-manifest-failure", recordPath)
	runtime := newGenerationTestRuntime(t)
	runtime.invoke = func(_ context.Context, name string, _ [][]byte) (guest.ToolResult, error) {
		if name != "list" {
			return guest.ToolResult{}, fmt.Errorf("unexpected guest tool %s", name)
		}
		manifestPath := filepath.Join(runtime.root, "planning-evidence", "generation-0012", "manifest.json")
		backupPath := manifestPath + ".active"
		if err := os.Rename(manifestPath, backupPath); err != nil {
			return guest.ToolResult{}, err
		}
		if err := os.Mkdir(manifestPath, 0o755); err != nil {
			return guest.ToolResult{}, err
		}
		return guest.ToolResult{Status: 0}, nil
	}
	session := NewGenerationSession(runtime, GenerationSessionOptions{
		Executable: os.Args[0], AuthFile: fakeAuthFile(t), StopTimeout: time.Second,
	})
	if _, err := session.RunInitialTurn(); err == nil || !strings.Contains(err.Error(), "planning evidence fail-closed also failed") {
		t.Fatalf("manifest write failure = %v", err)
	}
	manifestPath := filepath.Join(runtime.root, "planning-evidence", "generation-0012", "manifest.json")
	if info, err := os.Stat(manifestPath); err != nil || !info.IsDir() {
		t.Fatalf("manifest failure marker = %v, info=%v; want an undurable path for combined error", err, info)
	}
	if _, err := os.Stat(manifestPath + ".active"); err != nil {
		t.Fatalf("original active manifest backup missing: %v", err)
	}
	// Restore the manifest path so Close can retry the fail-closed update and
	// reap the helper without turning the expected injected write failure into
	// an unrelated Close error.
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(manifestPath+".active", manifestPath); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	manifest := readReviewerJSON(t, manifestPath)
	if manifest["outcome"] != "failed" || manifest["stage"] != "completed" {
		t.Fatalf("restored manifest = %#v", manifest)
	}
}

func TestGenerationSessionFailedPlanningResumeRecordsAttemptsAndPoisonsSession(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "generation-resume-failed.json")
	setGenerationHelper(t, "resume-failed", recordPath)
	runtime := newGenerationTestRuntime(t)
	runtime.invoke = func(_ context.Context, name string, _ [][]byte) (guest.ToolResult, error) {
		if name != "list" {
			return guest.ToolResult{}, fmt.Errorf("unexpected guest tool %s", name)
		}
		return guest.ToolResult{Status: 0}, nil
	}
	session := NewGenerationSession(runtime, GenerationSessionOptions{
		Executable: os.Args[0], AuthFile: fakeAuthFile(t), StopTimeout: time.Second,
	})
	resultChannel := make(chan struct {
		result GenerationResult
		err    error
	}, 1)
	go func() {
		result, err := session.RunInitialTurn()
		resultChannel <- struct {
			result GenerationResult
			err    error
		}{result, err}
	}()
	if !waitGenerationToolCall(runtime, "list", 2*time.Second) {
		t.Fatal("initial planning tool call did not reach the runtime")
	}
	if err := session.InterruptTurn(time.Second); err != nil {
		t.Fatal(err)
	}
	first := <-resultChannel
	if first.err != nil || first.result.TurnStatus != "interrupted" {
		t.Fatalf("interrupted planning result = %#v, %v", first.result, first.err)
	}
	resumed, err := session.RunPlanningContinuationTurn("Retry the plan.")
	if err == nil || !strings.Contains(err.Error(), "fake planning failure") {
		if _, statErr := os.Stat(recordPath); statErr == nil {
			t.Fatalf("failed resumed planning result = %#v, %v; helper record: %#v", resumed, err, readReviewerJSON(t, recordPath))
		}
		t.Fatalf("failed resumed planning result = %#v, %v", resumed, err)
	}
	if session.Healthy() {
		t.Fatal("failed resumed planning left session healthy")
	}
	if len(runtime.calls) != 2 {
		t.Fatalf("failed resumed planning guest calls = %#v", runtime.calls)
	}
	waitForReviewerFile(t, recordPath)
	manifest := readReviewerJSON(t, filepath.Join(runtime.root, "planning-evidence", "generation-0012", "manifest.json"))
	attempts := manifest["attempts"].([]any)
	if manifest["outcome"] != "failed" || manifest["stage"] != "completed" || len(attempts) != 2 || attempts[0].(map[string]any)["outcome"] != "interrupted" || attempts[1].(map[string]any)["outcome"] != "failed" {
		t.Fatalf("failed resumed planning manifest = %#v", manifest)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationSessionStartFailureAndCancelReapsProcess(t *testing.T) {
	failedRuntime := newGenerationTestRuntime(t)
	failedRuntime.tools = []string{"list", "list"}
	failedSession := NewGenerationSession(failedRuntime, GenerationSessionOptions{})
	if err := failedSession.Start(context.Background()); err == nil {
		t.Fatal("duplicate guest tools unexpectedly started a session")
	}
	if failedSession.Healthy() {
		t.Fatal("failed session remained healthy")
	}

	recordPath := filepath.Join(t.TempDir(), "generation-hold.json")
	setGenerationHelper(t, "hold", recordPath)
	runtime := newGenerationTestRuntime(t)
	session := NewGenerationSession(runtime, GenerationSessionOptions{
		Executable: os.Args[0], AuthFile: fakeAuthFile(t), StopTimeout: time.Second,
	})
	if err := session.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForReviewerFile(t, recordPath)
	pid, ok := session.ProcessPID()
	if !ok || pid <= 0 {
		t.Fatalf("session process PID = %d, %v", pid, ok)
	}
	session.Cancel()
	if session.Mode() != GenerationModeClosed || session.Healthy() {
		t.Fatalf("cancelled session state = %s, healthy=%v", session.Mode(), session.Healthy())
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for generationProcessAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if generationProcessAlive(pid) {
		t.Fatalf("cancelled app-server process %d is still alive", pid)
	}
}

func TestGenerationSessionStartIsSingleFlight(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "generation-start-single-flight.json")
	setGenerationHelper(t, "hold", recordPath)
	runtime := newGenerationTestRuntime(t)
	runtime.listStarted = make(chan struct{}, 2)
	runtime.listRelease = make(chan struct{})
	session := NewGenerationSession(runtime, GenerationSessionOptions{
		Executable: os.Args[0], AuthFile: fakeAuthFile(t), StopTimeout: time.Second,
	})
	first := make(chan error, 1)
	go func() { first <- session.Start(context.Background()) }()
	select {
	case <-runtime.listStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first Start did not reach ListTools")
	}
	second := make(chan error, 1)
	go func() { second <- session.Start(context.Background()) }()
	select {
	case <-runtime.listStarted:
		// An implementation that does not join the in-progress start reaches
		// ListTools a second time; the assertion below catches that launch.
	case <-time.After(100 * time.Millisecond):
	}
	close(runtime.listRelease)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	listCalls := runtime.listToolCalls
	runtime.mu.Unlock()
	if listCalls != 1 {
		t.Fatalf("single-flight ListTools calls = %d, want one", listCalls)
	}
	pid, ok := session.ProcessPID()
	if !ok || pid <= 0 {
		t.Fatalf("single-flight process PID = %d, %v", pid, ok)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	waitForGenerationProcessExit(t, pid)

	failedRuntime := newGenerationTestRuntime(t)
	failedRuntime.tools = []string{"list", "list"}
	failedRuntime.listStarted = make(chan struct{}, 2)
	failedRuntime.listRelease = make(chan struct{})
	failedSession := NewGenerationSession(failedRuntime, GenerationSessionOptions{})
	failedFirst := make(chan error, 1)
	go func() { failedFirst <- failedSession.Start(context.Background()) }()
	select {
	case <-failedRuntime.listStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("failed first Start did not reach ListTools")
	}
	failedSecond := make(chan error, 1)
	go func() { failedSecond <- failedSession.Start(context.Background()) }()
	select {
	case <-failedRuntime.listStarted:
	case <-time.After(100 * time.Millisecond):
	}
	close(failedRuntime.listRelease)
	firstErr := <-failedFirst
	secondErr := <-failedSecond
	if firstErr == nil || secondErr == nil || firstErr.Error() != secondErr.Error() {
		t.Fatalf("single-flight start errors = %v, %v", firstErr, secondErr)
	}
	failedRuntime.mu.Lock()
	failedListCalls := failedRuntime.listToolCalls
	failedRuntime.mu.Unlock()
	if failedListCalls != 1 {
		t.Fatalf("failed single-flight ListTools calls = %d, want one", failedListCalls)
	}
	if err := failedSession.Close(); err != nil {
		t.Fatal(err)
	}
}

func waitForGenerationProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for generationProcessAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if generationProcessAlive(pid) {
		t.Fatalf("app-server process %d is still alive", pid)
	}
}

func TestGenerationSessionCloseCancelsStalledStartTurn(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "generation-stalled-start.json")
	setGenerationHelper(t, "stalled-start", recordPath)
	runtime := newGenerationTestRuntime(t)
	session := NewGenerationSession(runtime, GenerationSessionOptions{
		Executable: os.Args[0], AuthFile: fakeAuthFile(t), StopTimeout: 500 * time.Millisecond,
	})
	resultChannel := make(chan struct {
		result GenerationResult
		err    error
	}, 1)
	go func() {
		result, err := session.RunInitialTurn()
		resultChannel <- struct {
			result GenerationResult
			err    error
		}{result, err}
	}()
	waitForReviewerFile(t, recordPath)
	pid, ok := session.ProcessPID()
	if !ok || pid <= 0 {
		t.Fatalf("stalled app-server PID = %d, %v", pid, ok)
	}
	started := time.Now()
	if err := session.Close(); err != nil {
		t.Fatalf("closing stalled StartTurn: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("closing stalled StartTurn took %s", elapsed)
	}
	select {
	case result := <-resultChannel:
		if result.err == nil {
			t.Fatalf("stalled StartTurn unexpectedly succeeded: %#v", result.result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stalled StartTurn goroutine did not exit after Close")
	}
	session.mu.Lock()
	turnID, turnPhase, turnStarting, initialActive := session.turnID, session.turnPhase, session.turnStarting, session.initialActive
	initialDone, turnDone, toolIdle := session.initialDone, session.turnDone, session.toolIdle
	session.mu.Unlock()
	if turnID != "" || turnPhase != "" || turnStarting || initialActive {
		t.Fatalf("stale closed-session turn state = id=%q phase=%q starting=%v initialActive=%v", turnID, turnPhase, turnStarting, initialActive)
	}
	for name, done := range map[string]chan struct{}{"initial": initialDone, "turn": turnDone, "tool": toolIdle} {
		select {
		case <-done:
		default:
			t.Fatalf("closed session %s completion channel is not closed", name)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for generationProcessAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if generationProcessAlive(pid) {
		t.Fatalf("stalled app-server process %d is still alive", pid)
	}
}

func TestGenerationSessionCloseJoinsActiveToolCallback(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "generation-active-tool.json")
	setGenerationHelper(t, "success", recordPath)
	runtime := newGenerationTestRuntime(t)
	entered := make(chan struct{})
	exited := make(chan struct{})
	runtime.invoke = func(ctx context.Context, name string, _ [][]byte) (guest.ToolResult, error) {
		if name != "list" {
			return guest.ToolResult{}, fmt.Errorf("unexpected guest tool %s", name)
		}
		close(entered)
		<-ctx.Done()
		close(exited)
		return guest.ToolResult{}, ctx.Err()
	}
	session := NewGenerationSession(runtime, GenerationSessionOptions{
		Executable: os.Args[0], AuthFile: fakeAuthFile(t), StopTimeout: time.Second,
	})
	resultChannel := make(chan error, 1)
	go func() {
		_, err := session.RunInitialTurn()
		resultChannel <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("dynamic tool callback did not start")
	}
	if err := session.Close(); err != nil {
		t.Fatalf("closing session with active tool callback: %v", err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("Close returned before the active tool callback exited")
	}
	select {
	case err := <-resultChannel:
		if err == nil {
			t.Fatal("turn with cancelled tool callback unexpectedly succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not return after session close")
	}
}

func setGenerationHelper(t *testing.T, mode, recordPath string) {
	t.Helper()
	t.Setenv(reviewerHelperEnvironment, "1")
	t.Setenv(generationHelperMode, mode)
	t.Setenv(generationHelperRecord, recordPath)
}

func waitGenerationToolCall(runtime *generationTestRuntime, want string, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case name := <-runtime.callNotify:
			if name == want {
				return true
			}
		case <-timer.C:
			return false
		}
	}
}

func runGenerationFakeAppServer() {
	mode := os.Getenv(generationHelperMode)
	if mode == "probe" {
		writeGenerationRecord(map[string]any{"mode": "probe", "pid": os.Getpid()})
		return
	}
	if mode != "success" && mode != "worker-success" && mode != "terminal-before-tool-result" && mode != "orphaned-review" && mode != "interview" && mode != "interview-hold" && mode != "interview-interrupt" && mode != "interrupt" && mode != "interrupt-failed" && mode != "planning-interrupt" && mode != "planning-failed" && mode != "planning-complete-failure" && mode != "planning-manifest-failure" && mode != "resume-failed" && mode != "continuation-failed" && mode != "stalled-start" && mode != "hold" {
		os.Exit(20)
	}
	decoder := json.NewDecoder(bufio.NewReader(os.Stdin))
	decoder.UseNumber()
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	messages := make([]map[string]any, 0, 8)
	send := func(message map[string]any) {
		if err := encoder.Encode(message); err != nil {
			os.Exit(21)
		}
	}
	read := func() map[string]any {
		var message map[string]any
		if err := decoder.Decode(&message); err != nil {
			os.Exit(22)
		}
		return message
	}
	expect := func(method string) map[string]any {
		message := read()
		messages = append(messages, message)
		if message["method"] != method || message["id"] == nil {
			os.Exit(23)
		}
		return message
	}
	respond := func(request map[string]any, result any) {
		send(map[string]any{"id": request["id"], "result": result})
	}
	initialize := expect("initialize")
	respond(initialize, map[string]any{"userAgent": "fake-generation"})
	initialized := read()
	messages = append(messages, initialized)
	if initialized["method"] != "initialized" || initialized["id"] != nil {
		os.Exit(24)
	}
	account := expect("account/read")
	respond(account, map[string]any{"account": map[string]any{"type": "chatgpt"}})
	model := expect("model/list")
	respond(model, map[string]any{"data": []any{map[string]any{
		"model": "gpt-5.6-sol", "supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "high"}},
		"serviceTiers": []any{map[string]any{"id": "priority", "name": "Priority"}},
	}}, "nextCursor": nil})
	thread := expect("thread/start")
	threadID := fmt.Sprintf("generation-thread-%d", os.Getpid())
	respond(thread, map[string]any{
		"thread": map[string]any{"id": threadID, "ephemeral": true},
		"model":  "gpt-5.6-sol", "serviceTier": "priority",
		"activePermissionProfile": map[string]any{"id": implementorPermissionProfile},
		"sandbox":                 map[string]any{"type": "workspace-write", "networkAccess": false},
	})
	if mode == "stalled-start" {
		expect("turn/start")
		writeGenerationRecord(map[string]any{"mode": mode, "pid": os.Getpid(), "thread_id": threadID, "messages": messages})
		select {}
	}
	if mode == "hold" {
		writeGenerationRecord(map[string]any{"mode": "hold", "pid": os.Getpid(), "thread_id": threadID})
		select {}
	}
	turnCount := 2
	if mode == "terminal-before-tool-result" {
		turnCount = 3
	}
	if mode == "orphaned-review" {
		turnCount = 3
	}
	if mode == "interview" {
		turnCount = 4
	}
	if mode == "interview-hold" {
		turnCount = 3
	}
	if mode == "interview-interrupt" {
		turnCount = 3
	}
	if mode == "interrupt" || mode == "planning-interrupt" || mode == "resume-failed" {
		turnCount = 3
	}
	if mode == "planning-failed" || mode == "planning-complete-failure" || mode == "planning-manifest-failure" {
		turnCount = 1
	}
	if mode == "resume-failed" {
		turnCount = 2
	}
	if mode == "continuation-failed" {
		turnCount = 4
	}
	for index := 0; index < turnCount; index++ {
		turn := expect("turn/start")
		turnID := fmt.Sprintf("generation-turn-%d-%d", os.Getpid(), index)
		respond(turn, map[string]any{"turn": map[string]any{"id": turnID}})
		if mode == "interview-hold" && index == 2 {
			send(map[string]any{"method": "item/completed", "params": map[string]any{
				"threadId": threadID, "turnId": turnID, "item": map[string]any{
					"id": "reasoning-held", "type": "reasoning", "summary": []any{"Partial visible summary."},
				},
			}})
			writeGenerationRecord(map[string]any{"pid": os.Getpid(), "thread_id": threadID, "messages": messages})
			select {}
		}
		if mode == "interview-interrupt" && index == 2 {
			send(map[string]any{"method": "item/completed", "params": map[string]any{
				"threadId": threadID, "turnId": turnID, "item": map[string]any{
					"id": "reasoning-interrupted", "type": "reasoning", "summary": []any{"Interrupted visible summary."},
				},
			}})
		}
		tool := "list"
		namespace := any("codexos")
		arguments := map[string]any{}
		if mode == "orphaned-review" && index == 0 {
			tool = "review"
			namespace = nil
			arguments = map[string]any{"focus": "general"}
		}
		if ((mode == "success" || mode == "worker-success" || mode == "interview" || mode == "interview-hold" || mode == "interview-interrupt") && index == 1) || (mode == "interrupt" && index == 1) || (mode == "interrupt-failed" && index == 1) || (mode == "planning-interrupt" && index == 2) {
			tool = "write"
			arguments = map[string]any{"path": "seed/kernel.c", "offset": 0, "data": "x"}
		}
		callID := fmt.Sprintf("generation-call-%d", index)
		send(map[string]any{"id": callID, "method": "item/tool/call", "params": map[string]any{
			"callId": callID, "threadId": threadID, "turnId": turnID,
			"namespace": namespace, "tool": tool, "arguments": arguments,
		}})
		terminalBeforeToolResult := mode == "terminal-before-tool-result" && index == 0
		if terminalBeforeToolResult {
			deadline := time.Now().Add(2 * time.Second)
			for {
				if _, err := os.Stat(os.Getenv(generationHelperToolReady)); err == nil {
					break
				}
				if time.Now().After(deadline) {
					os.Exit(26)
				}
				time.Sleep(time.Millisecond)
			}
			item := map[string]any{"id": "message-0", "type": "agentMessage", "text": "Planning complete."}
			send(map[string]any{"method": "item/completed", "params": map[string]any{
				"threadId": threadID, "turnId": turnID, "item": item,
			}})
			send(map[string]any{"method": "turn/completed", "params": map[string]any{
				"threadId": threadID, "turn": map[string]any{"id": turnID, "items": []any{item}, "status": "completed"},
			}})
			continue
		}
		interrupted := false
		for {
			response := read()
			messages = append(messages, response)
			if response["id"] == callID {
				break
			}
			interruptAt := -1
			if mode == "interrupt" || mode == "interrupt-failed" {
				interruptAt = 1
			}
			if mode == "planning-interrupt" {
				interruptAt = 0
			}
			if mode == "resume-failed" {
				interruptAt = 0
			}
			if mode == "interview-interrupt" {
				interruptAt = 2
			}
			if index == interruptAt && response["method"] == "turn/interrupt" {
				respond(response, map[string]any{})
				if mode == "interrupt-failed" {
					send(map[string]any{"method": "turn/completed", "params": map[string]any{
						"threadId": threadID, "turn": map[string]any{"id": turnID, "items": []any{}, "status": "completed"},
					}})
					interrupted = true
					continue
				}
				send(map[string]any{"method": "turn/completed", "params": map[string]any{
					"threadId": threadID, "turn": map[string]any{"id": turnID, "items": []any{}, "status": "interrupted"},
				}})
				interrupted = true
				continue
			}
			os.Exit(25)
		}
		if !interrupted && !(mode == "orphaned-review" && index == 0) {
			send(map[string]any{"method": "item/completed", "params": map[string]any{
				"threadId": threadID, "turnId": turnID, "item": map[string]any{
					"id": callID, "type": "dynamicToolCall", "tool": tool, "status": "completed",
				},
			}})
		}
		interruptAt := -1
		if mode == "interrupt" || mode == "interrupt-failed" {
			interruptAt = 1
		}
		if mode == "planning-interrupt" {
			interruptAt = 0
		}
		if mode == "resume-failed" {
			interruptAt = 0
		}
		if mode == "interview-interrupt" {
			interruptAt = 2
		}
		if index == interruptAt {
			if !interrupted {
				interrupt := expect("turn/interrupt")
				respond(interrupt, map[string]any{})
				if mode == "interrupt-failed" {
					send(map[string]any{"method": "turn/completed", "params": map[string]any{
						"threadId": threadID, "turn": map[string]any{"id": turnID, "items": []any{}, "status": "completed"},
					}})
					continue
				}
				send(map[string]any{"method": "turn/completed", "params": map[string]any{
					"threadId": threadID, "turn": map[string]any{"id": turnID, "items": []any{}, "status": "interrupted"},
				}})
			}
			continue
		}
		if (mode == "planning-failed" && index == 0) || (mode == "resume-failed" && index == 1) || (mode == "continuation-failed" && index == 2) {
			send(map[string]any{"method": "turn/completed", "params": map[string]any{
				"threadId": threadID, "turn": map[string]any{"id": turnID, "items": []any{}, "status": "failed", "error": map[string]any{"message": "fake planning failure"}},
			}})
			continue
		}
		text := "Planning complete."
		if plan := os.Getenv(generationHelperPlan); plan != "" && ((index == 0 && mode != "resume-failed") || (mode == "planning-interrupt" && index == 1)) {
			text = plan
		}
		if ((mode == "success" || mode == "worker-success" || mode == "interview" || mode == "interview-hold" || mode == "interview-interrupt") && index == 1) || (mode == "interrupt" && index == 1) || (mode == "interrupt-failed" && index == 1) || (mode == "planning-interrupt" && index == 2) {
			text = "Implementation complete."
		}
		if mode == "interview" && index == 2 {
			text = "Retrospective answer."
			send(map[string]any{"method": "item/reasoning/summaryTextDelta", "params": map[string]any{
				"threadId": threadID, "turnId": turnID, "itemId": "reasoning-interview", "summaryIndex": 0, "delta": "First explicit ",
			}})
			send(map[string]any{"method": "item/completed", "params": map[string]any{
				"threadId": threadID, "turnId": turnID, "item": map[string]any{
					"id": "reasoning-interview", "type": "reasoning", "summary": []any{"First explicit summary.", "Then another."}, "content": []any{"PRIVATE RAW REASONING"},
				},
			}})
		}
		if mode == "interview" && index == 3 {
			text = "Second retrospective answer."
		}
		if mode == "interrupt" && index == 2 {
			text = "Continuation complete."
		}
		if mode == "planning-interrupt" && index == 1 {
			text = "Planning resumed."
		}
		item := map[string]any{"id": fmt.Sprintf("message-%d", index), "type": "agentMessage", "text": text}
		send(map[string]any{"method": "item/completed", "params": map[string]any{
			"threadId": threadID, "turnId": turnID, "item": item,
		}})
		if mode == "worker-success" && index == 1 {
			writeGenerationRecord(map[string]any{"pid": os.Getpid(), "thread_id": threadID, "messages": messages})
		}
		send(map[string]any{"method": "turn/completed", "params": map[string]any{
			"threadId": threadID, "turn": map[string]any{"id": turnID, "items": []any{item}, "status": "completed"},
		}})
	}
	writeGenerationRecord(map[string]any{"pid": os.Getpid(), "thread_id": threadID, "messages": messages})
	select {}
}

func generationProcessAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

func writeGenerationRecord(value map[string]any) {
	path := os.Getenv(generationHelperRecord)
	if path == "" {
		return
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		os.Exit(26)
	}
	temporaryPath := path + ".tmp"
	if err := os.WriteFile(temporaryPath, encoded, 0o600); err != nil {
		os.Exit(27)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		os.Exit(28)
	}
}
