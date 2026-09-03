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
	"syscall"
	"testing"
	"time"

	"codexos/internal/guest"
	"codexos/internal/observability"
	"codexos/internal/provenance"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const (
	reviewerHelperEnvironment = "CODEXOS_GO_REVIEWER_HELPER"
	reviewerHelperMode        = "CODEXOS_GO_REVIEWER_MODE"
	reviewerHelperRecord      = "CODEXOS_GO_REVIEWER_RECORD"
	reviewerHelperReady       = "CODEXOS_GO_REVIEWER_READY"
	reviewerHelperSentinel    = "CODEXOS_GO_REVIEWER_SUITE_SENTINEL"
	reviewerHelperToolReady   = "CODEXOS_GO_REVIEWER_TOOL_READY"
)

func TestMain(tests *testing.M) {
	if os.Getenv(reviewerHelperEnvironment) == "1" {
		if os.Getenv(generationHelperMode) != "" {
			runGenerationFakeAppServer()
			os.Exit(0)
		}
		runReviewerFakeAppServer()
		os.Exit(0)
	}
	os.Exit(tests.Run())
}

func TestReviewerHelperSubprocessCannotEnterTestSuite(t *testing.T) {
	root := t.TempDir()
	recordPath := filepath.Join(root, "helper.json")
	sentinelPath := filepath.Join(root, "suite-entered")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	command := exec.CommandContext(
		ctx,
		os.Args[0],
		"-test.run=^TestReviewerSuiteSentinel$",
		"-test.count=1",
	)
	command.Env = reviewerTestEnvironment(map[string]string{
		reviewerHelperEnvironment: "1",
		reviewerHelperMode:        "probe",
		reviewerHelperRecord:      recordPath,
		reviewerHelperReady:       "",
		reviewerHelperSentinel:    sentinelPath,
	})
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("bounded reviewer helper probe did not exit: %v", ctx.Err())
	}
	if err != nil {
		t.Fatalf("reviewer helper probe failed: %v\n%s", err, output)
	}
	record := readReviewerJSON(t, recordPath)
	if record["mode"] != "probe" {
		t.Fatalf("reviewer helper record = %#v", record)
	}
	if _, err := os.Stat(sentinelPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reviewer helper entered the Go test suite: %v", err)
	}
}

func TestReviewerSuiteSentinel(t *testing.T) {
	path := os.Getenv(reviewerHelperSentinel)
	if path == "" {
		return
	}
	if err := os.WriteFile(path, []byte("entered"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReviewWorkerCapturesReadOnlySourceAndEvidence(t *testing.T) {
	runtime := newFakeReviewRuntime(t)
	identity := reviewerHarnessIdentity()
	runtime.identity = &identity
	runtime.provenance = provenance.NewBuildReviewProvenance(runtime.root)
	runtime.invoke = func(_ context.Context, name string, arguments [][]byte) (guest.ToolResult, error) {
		switch name {
		case "list":
			return guest.ToolResult{Status: 0, Output: []byte("seed/kernel.c\n")}, nil
		case "read":
			return guest.ToolResult{Status: 0, Output: []byte("a\x00b\xff")}, nil
		default:
			return guest.ToolResult{}, fmt.Errorf("unexpected tool %s", name)
		}
	}
	stream := observability.NewActivityStream()
	objective := "Improve the current bootstrap safely."
	request := "Check the source-read boundary."
	worker := NewReviewWorker(ReviewWorkerOptions{
		Executable:     os.Args[0],
		AuthFile:       fakeAuthFile(t),
		ActivityStream: stream,
		StopTimeout:    time.Second,
	})
	setReviewerHelper(t, "success", "", "")
	result, err := worker.RunReview(context.Background(), runtime, ReviewOptions{
		Objective: &objective,
		Focus:     "security",
		Request:   &request,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result != "No meaningful issues found." {
		t.Fatalf("review result = %q", result)
	}
	if len(runtime.calls) != 2 {
		t.Fatalf("runtime calls = %#v", runtime.calls)
	}
	if got := runtime.calls[1].arguments; len(got) != 3 || string(got[0]) != "seed/tasks.c" || string(got[1]) != "3" || string(got[2]) != "5" {
		t.Fatalf("read arguments = %#v", got)
	}
	manifestPath := filepath.Join(runtime.root, "build-review-provenance", "generation-0012", "review-000001", "manifest.json")
	manifest := readReviewerJSON(t, manifestPath)
	if manifest["review_outcome"] != "completed" || manifest["capture_outcome"] != "complete" || manifest["evidence_complete"] != true {
		t.Fatalf("review manifest = %#v", manifest)
	}
	reads, ok := manifest["source_reads"].([]any)
	if !ok || len(reads) != 1 {
		t.Fatalf("source reads = %#v", manifest["source_reads"])
	}
	read, ok := reads[0].(map[string]any)
	if !ok || read["path"] != "seed/tasks.c" {
		t.Fatalf("source read = %#v", reads[0])
	}
	contentPath := filepath.Join(filepath.Dir(manifestPath), read["content_file"].(string))
	if got, err := os.ReadFile(contentPath); err != nil || string(got) != "a\x00b\xff" {
		t.Fatalf("captured source bytes = %q, %v", got, err)
	}
	activities := stream.Drain()
	if !hasReviewerActivity(activities, observability.ActivityReviewStarted) || !hasReviewerActivity(activities, observability.ActivityReviewCompleted) || !hasReviewerActivity(activities, observability.ActivityToolStarted) {
		t.Fatalf("review activities = %#v", activities)
	}
	events, err := os.ReadFile(runtime.eventLog.Path())
	if err != nil || !reviewerEventHasHarnessIdentity(t, events, "review_completed", identity) {
		t.Fatalf("review completion lacks harness identity: %v", err)
	}
}

func TestReviewWorkerRejectsTerminalTurnWithUnresolvedSourceRead(t *testing.T) {
	runtime := newFakeReviewRuntime(t)
	toolReady := filepath.Join(t.TempDir(), "tool-ready")
	readStarted := make(chan struct{})
	readCancelled := make(chan struct{})
	runtime.invoke = func(ctx context.Context, _ string, _ [][]byte) (guest.ToolResult, error) {
		if err := os.WriteFile(toolReady, []byte("ready"), 0o600); err != nil {
			return guest.ToolResult{}, err
		}
		close(readStarted)
		<-ctx.Done()
		close(readCancelled)
		return guest.ToolResult{}, ctx.Err()
	}
	worker := NewReviewWorker(ReviewWorkerOptions{
		Executable: os.Args[0], AuthFile: fakeAuthFile(t), StopTimeout: time.Second,
	})
	setReviewerHelper(t, "terminal-before-tool-result", "", "")
	t.Setenv(reviewerHelperToolReady, toolReady)
	result, err := worker.RunReview(context.Background(), runtime, ReviewOptions{})
	if err == nil || !strings.Contains(err.Error(), "before its dynamic tool results were delivered") {
		t.Fatalf("review result = %q, %v", result, err)
	}
	select {
	case <-readStarted:
	default:
		t.Fatal("review source read did not start")
	}
	select {
	case <-readCancelled:
	default:
		t.Fatal("review source read was not cancelled and quiesced")
	}
}

func TestReviewWorkerDeniesMutationAndMismatchedCallsBeforeRuntime(t *testing.T) {
	runtime := newFakeReviewRuntime(t)
	runtime.invoke = func(_ context.Context, name string, _ [][]byte) (guest.ToolResult, error) {
		if name != "list" {
			return guest.ToolResult{}, fmt.Errorf("unexpected tool %s", name)
		}
		return guest.ToolResult{Status: 0, Output: nil}, nil
	}
	setReviewerHelper(t, "mutation", "", "")
	worker := NewReviewWorker(ReviewWorkerOptions{
		Executable:  os.Args[0],
		AuthFile:    fakeAuthFile(t),
		StopTimeout: time.Second,
	})
	result, err := worker.RunReview(context.Background(), runtime, ReviewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result != "No meaningful issues found." {
		t.Fatalf("review result = %q", result)
	}
	if len(runtime.calls) != 1 || runtime.calls[0].name != "list" {
		t.Fatalf("runtime calls = %#v", runtime.calls)
	}
}

func TestReviewWorkerMalformedUsageDegradesMetricsWithoutFailing(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	metrics, err := observability.NewMetrics(t.TempDir(), observability.MetricsOptions{MetricReaders: []sdkmetric.Reader{reader}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(metrics.Close)
	runtime := newFakeReviewRuntime(t)
	runtime.metrics = metrics
	setReviewerHelper(t, "malformed-usage", "", "")
	worker := NewReviewWorker(ReviewWorkerOptions{
		Executable:  os.Args[0],
		AuthFile:    fakeAuthFile(t),
		StopTimeout: time.Second,
	})
	if result, err := worker.RunReview(context.Background(), runtime, ReviewOptions{}); err != nil || result != "No meaningful issues found." {
		t.Fatalf("review = %q, %v", result, err)
	}
	if metrics.Healthy() || !strings.Contains(metrics.DegradedReason(), "reviewer token usage telemetry was ignored") {
		t.Fatalf("metrics health = %v (%s)", metrics.Healthy(), metrics.DegradedReason())
	}
}

func TestReviewWorkerUsesFreshServersThreadsAndTokenBaselines(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	metrics, err := observability.NewMetrics(t.TempDir(), observability.MetricsOptions{MetricReaders: []sdkmetric.Reader{reader}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(metrics.Close)
	runtime := newFakeReviewRuntime(t)
	runtime.metrics = metrics
	threads := make([]string, 0, 2)
	for index := 0; index < 2; index++ {
		recordPath := filepath.Join(t.TempDir(), fmt.Sprintf("review-%d.json", index))
		setReviewerHelper(t, "tokens", recordPath, "")
		worker := NewReviewWorker(ReviewWorkerOptions{
			Executable:  os.Args[0],
			AuthFile:    fakeAuthFile(t),
			StopTimeout: time.Second,
		})
		if result, err := worker.RunReview(context.Background(), runtime, ReviewOptions{}); err != nil || result != "No meaningful issues found." {
			t.Fatalf("review %d = %q, %v", index, result, err)
		}
		record := readReviewerJSON(t, recordPath)
		threads = append(threads, record["thread_id"].(string))
	}
	if threads[0] == threads[1] {
		t.Fatalf("fresh reviews reused thread %q", threads[0])
	}
	data := readerMetrics(t, reader)
	if got := data["codexos_model_input_tokens_total"]; got != 160 {
		t.Fatalf("input token metric = %d, want 160", got)
	}
	if got := data["codexos_model_cached_input_tokens_total"]; got != 50 {
		t.Fatalf("cached token metric = %d, want 50", got)
	}
	if got := data["codexos_model_output_tokens_total"]; got != 60 {
		t.Fatalf("output token metric = %d, want 60", got)
	}
}

func TestReviewWorkerCancellationCleansUpFreshServer(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "ready")
	recordPath := filepath.Join(t.TempDir(), "record.json")
	setReviewerHelper(t, "hold", recordPath, readyPath)
	runtime := newFakeReviewRuntime(t)
	worker := NewReviewWorker(ReviewWorkerOptions{
		Executable:  os.Args[0],
		AuthFile:    fakeAuthFile(t),
		StopTimeout: 100 * time.Millisecond,
	})
	resultChannel := make(chan error, 1)
	go func() {
		_, err := worker.RunReview(context.Background(), runtime, ReviewOptions{})
		resultChannel <- err
	}()
	waitForReviewerFile(t, readyPath)
	record := readReviewerJSON(t, recordPath)
	pid := int(record["pid"].(float64))
	worker.Cancel()
	select {
	case err := <-resultChannel:
		if err == nil || !strings.Contains(err.Error(), "cancelled") {
			t.Fatalf("cancellation error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled review did not return")
	}
	deadline := time.Now().Add(time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reviewer process %d remains after cancellation: %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type fakeReviewCall struct {
	name      string
	arguments [][]byte
}

type fakeReviewRuntime struct {
	root       string
	running    bool
	generation uint64
	calls      []fakeReviewCall
	invoke     func(context.Context, string, [][]byte) (guest.ToolResult, error)
	eventLog   *observability.EventLog
	metrics    *observability.Metrics
	provenance *provenance.BuildReviewProvenance
	identity   *provenance.HarnessIdentity
}

func newFakeReviewRuntime(t *testing.T) *fakeReviewRuntime {
	root := t.TempDir()
	eventLog, err := observability.OpenEventLog(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eventLog.Close)
	return &fakeReviewRuntime{root: root, running: true, generation: 12, eventLog: eventLog}
}

func (r *fakeReviewRuntime) ReviewRunning() bool { return r.running }

func (r *fakeReviewRuntime) GenerationNumber() (uint64, bool) { return r.generation, true }

func (r *fakeReviewRuntime) HarnessIdentity() *provenance.HarnessIdentity {
	return provenance.CloneHarnessIdentity(r.identity)
}

func (r *fakeReviewRuntime) InvokeTool(ctx context.Context, name string, arguments [][]byte) (guest.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return guest.ToolResult{}, err
	}
	copyArguments := make([][]byte, len(arguments))
	for index, argument := range arguments {
		copyArguments[index] = append([]byte(nil), argument...)
	}
	r.calls = append(r.calls, fakeReviewCall{name: name, arguments: copyArguments})
	if r.invoke == nil {
		return guest.ToolResult{Status: 0}, nil
	}
	return r.invoke(ctx, name, copyArguments)
}

func (r *fakeReviewRuntime) EventLog() *observability.EventLog { return r.eventLog }

func (r *fakeReviewRuntime) Metrics() *observability.Metrics { return r.metrics }

func (r *fakeReviewRuntime) ForensicProvenance() *provenance.BuildReviewProvenance {
	return r.provenance
}

func reviewerHarnessIdentity() provenance.HarnessIdentity {
	return provenance.HarnessIdentity{
		SchemaVersion:    provenance.HarnessIdentitySchemaVersion,
		RepositoryCommit: strings.Repeat("a", 40),
		Executable:       provenance.FileIdentity{SHA256: strings.Repeat("b", 64), Size: 123},
		Build: provenance.HarnessBuildIdentity{
			GoVersion: "go1.test", ModulePath: "codexos", ModuleVersion: "(devel)",
			SettingsSHA256: strings.Repeat("c", 64),
		},
	}
}

func reviewerEventHasHarnessIdentity(t *testing.T, contents []byte, event string, expected provenance.HarnessIdentity) bool {
	t.Helper()
	scanner := bufio.NewScanner(bytes.NewReader(contents))
	for scanner.Scan() {
		var envelope struct {
			Event string         `json:"event"`
			Data  map[string]any `json:"data"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Event != event {
			continue
		}
		encoded, err := json.Marshal(envelope.Data["harness_identity"])
		if err != nil {
			t.Fatal(err)
		}
		identity, err := provenance.ParseHarnessIdentity(encoded)
		return err == nil && identity.Equal(expected)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return false
}

func fakeAuthFile(t *testing.T) string {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte("fake login"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func setReviewerHelper(t *testing.T, mode, recordPath, readyPath string) {
	t.Setenv(reviewerHelperEnvironment, "1")
	t.Setenv(reviewerHelperMode, mode)
	if recordPath == "" {
		t.Setenv(reviewerHelperRecord, "")
	} else {
		t.Setenv(reviewerHelperRecord, recordPath)
	}
	if readyPath == "" {
		t.Setenv(reviewerHelperReady, "")
	} else {
		t.Setenv(reviewerHelperReady, readyPath)
	}
}

func hasReviewerActivity(events []observability.ActivityEvent, kind observability.ActivityKind) bool {
	for _, event := range events {
		if event.Kind == kind {
			return true
		}
	}
	return false
}

func readReviewerJSON(t *testing.T, path string) map[string]any {
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func waitForReviewerFile(t *testing.T, path string) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// runReviewerFakeAppServer is a real stdio app-server peer used by the
// integration tests above. TestMain invokes it before testing.M.Run, so a
// helper subprocess cannot recursively execute the test suite.
func runReviewerFakeAppServer() {
	if os.Getenv(reviewerHelperMode) == "probe" {
		writeReviewerRecord(map[string]any{"mode": "probe", "pid": os.Getpid()})
		return
	}
	decoder := json.NewDecoder(bufio.NewReader(os.Stdin))
	decoder.UseNumber()
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	send := func(message map[string]any) {
		if err := encoder.Encode(message); err != nil {
			os.Exit(2)
		}
	}
	read := func() map[string]any {
		var message map[string]any
		if err := decoder.Decode(&message); err != nil {
			os.Exit(3)
		}
		return message
	}
	expect := func(method string) map[string]any {
		message := read()
		if message["method"] != method || message["id"] == nil {
			os.Exit(4)
		}
		return message
	}
	respond := func(request map[string]any, result any) {
		send(map[string]any{"id": request["id"], "result": result})
	}
	initialize := expect("initialize")
	respond(initialize, map[string]any{"userAgent": "fake-reviewer"})
	initialized := read()
	if initialized["method"] != "initialized" || initialized["id"] != nil {
		os.Exit(5)
	}
	account := expect("account/read")
	respond(account, map[string]any{"account": map[string]any{"type": "chatgpt"}})
	modelRequest := expect("model/list")
	respond(modelRequest, map[string]any{"data": []any{map[string]any{
		"model": "gpt-5.6-luna", "supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "high"}},
		"serviceTiers": []any{map[string]any{"id": "priority", "name": "Fast"}},
	}}, "nextCursor": nil})
	threadRequest := expect("thread/start")
	threadID := fmt.Sprintf("thread-%d", os.Getpid())
	respond(threadRequest, map[string]any{
		"thread": map[string]any{"id": threadID, "ephemeral": true},
		"model":  "gpt-5.6-luna", "serviceTier": "priority",
		"activePermissionProfile": map[string]any{"id": "codexos-reviewer"},
		"sandbox":                 map[string]any{"type": "readOnly", "networkAccess": false},
	})
	turnRequest := expect("turn/start")
	turnID := fmt.Sprintf("turn-%d", os.Getpid())
	respond(turnRequest, map[string]any{"turn": map[string]any{"id": turnID}})
	writeReviewerRecord(map[string]any{"pid": os.Getpid(), "thread_id": threadID, "turn_request": turnRequest})
	if path := os.Getenv(reviewerHelperReady); path != "" {
		_ = os.WriteFile(path, []byte("ready"), 0o600)
	}

	call := func(callID, thread, turn, tool string, arguments any, includeCallID bool) {
		params := map[string]any{"threadId": thread, "turnId": turn, "namespace": "codexos", "tool": tool, "arguments": arguments}
		if includeCallID {
			params["callId"] = callID
		}
		send(map[string]any{"id": callID, "method": "item/tool/call", "params": params})
		_ = read()
		if includeCallID && thread == threadID && turn == turnID {
			send(map[string]any{"method": "item/completed", "params": map[string]any{
				"threadId": threadID, "turnId": turnID, "item": map[string]any{
					"id": callID, "type": "dynamicToolCall", "tool": tool, "status": "completed",
				},
			}})
		}
	}
	mode := os.Getenv(reviewerHelperMode)
	switch mode {
	case "success":
		call("call-list", threadID, turnID, "list", map[string]any{}, true)
		call("call-read", threadID, turnID, "read", map[string]any{"path": "seed/tasks.c", "offset": 3, "length": 5}, true)
	case "mutation":
		call("bad-thread", "wrong-thread", turnID, "list", map[string]any{}, true)
		call("bad-turn", threadID, "wrong-turn", "read", map[string]any{"path": "seed/kernel.c", "offset": 0, "length": 1}, true)
		call("mutation", threadID, turnID, "write", map[string]any{"path": "seed/kernel.c", "offset": 0, "data": "x"}, true)
		call("missing-id", threadID, turnID, "list", map[string]any{}, false)
		call("good-list", threadID, turnID, "list", map[string]any{}, true)
	case "malformed-usage":
		send(map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{"threadId": threadID, "turnId": turnID, "tokenUsage": map[string]any{"total": map[string]any{"inputTokens": 1}}}})
	case "tokens":
		send(map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{"threadId": threadID, "turnId": turnID, "tokenUsage": map[string]any{"total": map[string]any{"inputTokens": 80, "cachedInputTokens": 25, "outputTokens": 30, "reasoningOutputTokens": 12}}}})
	case "terminal-before-tool-result":
		callID := "call-unresolved"
		send(map[string]any{"id": callID, "method": "item/tool/call", "params": map[string]any{
			"callId": callID, "threadId": threadID, "turnId": turnID,
			"namespace": "codexos", "tool": "read",
			"arguments": map[string]any{"path": "seed/kernel.c", "offset": 0, "length": 1},
		}})
		deadline := time.Now().Add(2 * time.Second)
		for {
			if _, err := os.Stat(os.Getenv(reviewerHelperToolReady)); err == nil {
				break
			}
			if time.Now().After(deadline) {
				os.Exit(9)
			}
			time.Sleep(time.Millisecond)
		}
		item := map[string]any{"id": "message", "type": "agentMessage", "text": "Review finished early."}
		send(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": threadID, "turnId": turnID, "item": item}})
		send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": threadID, "turn": map[string]any{"id": turnID, "items": []any{item}, "status": "completed"}}})
		for {
			time.Sleep(time.Hour)
		}
	case "failure":
		send(map[string]any{"method": "turn/completed", "params": map[string]any{
			"threadId": threadID, "turn": map[string]any{"id": turnID, "items": []any{}, "status": "failed", "error": map[string]any{"message": "synthetic review failure"}},
		}})
		for {
			time.Sleep(time.Hour)
		}
	case "hold":
		for {
			time.Sleep(time.Hour)
		}
	default:
		os.Exit(6)
	}
	item := map[string]any{"id": "message", "type": "agentMessage", "text": "No meaningful issues found."}
	send(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": threadID, "turnId": turnID, "item": item}})
	send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": threadID, "turn": map[string]any{"id": turnID, "items": []any{item}, "status": "completed"}}})
	for {
		time.Sleep(time.Hour)
	}
}

func writeReviewerRecord(value map[string]any) {
	path := os.Getenv(reviewerHelperRecord)
	if path == "" {
		return
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		os.Exit(7)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		os.Exit(8)
	}
}

func reviewerTestEnvironment(overrides map[string]string) []string {
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, item := range os.Environ() {
		name, _, found := strings.Cut(item, "=")
		if found {
			if _, replaced := overrides[name]; replaced {
				continue
			}
		}
		environment = append(environment, item)
	}
	for name, value := range overrides {
		environment = append(environment, name+"="+value)
	}
	return environment
}

func readerMetrics(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	data := metricdata.ResourceMetrics{}
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	values := make(map[string]int64)
	for _, scope := range data.ScopeMetrics {
		for _, metric := range scope.Metrics {
			sum, ok := metric.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, point := range sum.DataPoints {
				values[metric.Name] += point.Value
			}
		}
	}
	return values
}
