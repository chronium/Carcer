package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codexos/internal/store"
)

type operatorRequestTestRuntime struct {
	*generationTestRuntime
	ledger   *store.OperatorRequestStore
	onReport func()
}

func (r *operatorRequestTestRuntime) OperatorRequests() (store.OperatorRequestContext, error) {
	ledger, err := r.ledger.Snapshot()
	context := store.OperatorRequestContext{RunID: ledger.RunID, LedgerRevision: ledger.Revision, Requests: []store.OperatorRequestView{}}
	for _, request := range ledger.Requests {
		context.Requests = append(context.Requests, store.OperatorRequestView{ID: request.ID, Title: request.Title, Description: request.Description, Revision: request.Revision(), Active: request.Active(), Author: request.History[0].Actor})
	}
	return context, err
}
func (r *operatorRequestTestRuntime) RecordOperatorRequest(id, revision uint64, actor store.OperatorRequestActor, disposition, explanation, evidence string) (store.OperatorRequest, error) {
	if r.onReport != nil {
		r.onReport()
	}
	return r.ledger.Append(id, revision, store.OperatorRequestRevision{Action: "disposition", Actor: actor, Disposition: disposition, Explanation: explanation, Evidence: evidence})
}
func newOperatorRequestTestRuntime(t *testing.T) *operatorRequestTestRuntime {
	t.Helper()
	base := newGenerationTestRuntime(t)
	ledger, err := store.NewOperatorRequestStore(base.root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ledger.Create(store.OperatorRequestActor{Role: "operator", Name: "fixture operator"}, "First desired capability", "Disposable desired OS behavior"); err != nil {
		t.Fatal(err)
	}
	return &operatorRequestTestRuntime{generationTestRuntime: base, ledger: ledger}
}

func TestOperatorRequestTurnBoundaryAndConfirmedToolDelivery(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "app-server.json")
	setGenerationHelper(t, "operator-requests", recordPath)
	runtime := newOperatorRequestTestRuntime(t)
	session := NewGenerationSession(runtime, GenerationSessionOptions{Executable: os.Args[0], AuthFile: fakeAuthFile(t), StopTimeout: time.Second})
	t.Cleanup(func() { _ = session.Close() })
	var callbackErr error
	runtime.onReport = func() {
		_, callbackErr = runtime.ledger.Create(store.OperatorRequestActor{Role: "operator", Name: "second operator"}, "Added during active turn", "Visible at next boundary")
		frozen, err := session.dispatchOperatorRequest("list_operator_requests", map[string]any{})
		if err != nil {
			callbackErr = err
			return
		}
		if bytes.Contains(frozen.Output, []byte("Added during active turn")) {
			callbackErr = &GenerationWorkerError{Reason: "new operator input leaked into active turn"}
		}
	}
	result, err := session.RunInitialTurn()
	if err != nil || result.TurnStatus != "completed" || callbackErr != nil {
		t.Fatalf("turn=%+v err=%v callback=%v", result, err, callbackErr)
	}
	waitForReviewerFile(t, recordPath)
	record := readReviewerJSON(t, recordPath)
	var prompts []string
	for _, value := range record["messages"].([]any) {
		message := value.(map[string]any)
		if message["method"] == "turn/start" {
			prompts = append(prompts, message["params"].(map[string]any)["input"].([]any)[0].(map[string]any)["text"].(string))
		}
	}
	if len(prompts) != 2 || !strings.Contains(prompts[0], "First desired capability") || strings.Contains(prompts[0], "Added during active turn") || !strings.Contains(prompts[1], "Added during active turn") {
		t.Fatalf("boundary prompts: %#v", prompts)
	}
	for _, prompt := range prompts {
		for _, want := range []string{"Labelled operator input", "do not change the primary objective", "block generation completion", "pursue, defer, or decline", "distinct from explicit operator verification"} {
			if !strings.Contains(prompt, want) {
				t.Fatalf("missing %q", want)
			}
		}
	}
	ledger, err := runtime.ledger.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	report := ledger.Requests[0].History[1]
	if report.Actor.Role != "implementor" || report.Actor.Name != DefaultModel || report.Actor.ThreadID != record["thread_id"] || report.Actor.TurnID == "" || *report.Actor.Generation != 12 || report.Disposition != "deferred" {
		t.Fatalf("attribution=%+v", report)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(runtime.root, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	delivered := map[string]bool{}
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		var event struct {
			Event string         `json:"event"`
			Data  map[string]any `json:"data"`
		}
		if len(line) == 0 {
			continue
		}
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		if event.Event == "tool_result_delivered" {
			name, _ := event.Data["tool"].(string)
			delivered[name] = true
		}
	}
	if !delivered["record_operator_request"] || !delivered["list_operator_requests"] {
		t.Fatalf("missing confirmed delivery: %s", raw)
	}
	files, err := filepath.Glob(filepath.Join(runtime.root, "operator-request-context", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	prepared, dispatched := 0, 0
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if err = json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		if value["kind"] == "prepared" {
			prepared++
			continue
		}
		if value["kind"] != "dispatched" {
			t.Fatalf("unknown receipt: %s", raw)
		}
		dispatched++
		snapshot, err := os.ReadFile(filepath.Join(runtime.root, value["snapshot_path"].(string)))
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(snapshot)
		if value["snapshot_sha256"] != hex.EncodeToString(hash[:]) || value["thread_id"] != record["thread_id"] || value["turn_id"] == "" {
			t.Fatalf("bad binding: %s", raw)
		}
	}
	if prepared != 2 || dispatched != 2 {
		t.Fatalf("snapshots=%d dispatches=%d", prepared, dispatched)
	}
}

func TestOperatorRequestToolRejectsStaleAndUnattributedClaims(t *testing.T) {
	runtime := newOperatorRequestTestRuntime(t)
	session := NewGenerationSession(runtime, GenerationSessionOptions{})
	session.generation = 12
	session.generationSet = true
	session.threadID = "thread"
	session.turnID = "turn"
	if _, _, err := session.prepareOperatorRequests("prompt", "planning", "thread"); err != nil {
		t.Fatal(err)
	}
	call := func(arguments map[string]any, thread string) map[string]any {
		return session.dynamicToolResponse(map[string]any{"callId": "report", "threadId": thread, "turnId": "turn", "namespace": "codexos", "tool": "record_operator_request", "arguments": arguments}, "thread", "turn", true, nil)
	}
	args := map[string]any{"id": 1, "revision": 1, "disposition": "completed", "explanation": "A claim"}
	if response := call(args, "thread"); response["success"] != false {
		t.Fatal("completion without evidence accepted")
	}
	args["evidence"] = "Guest behavior test output"
	if response := call(args, "wrong-thread"); response["success"] != false {
		t.Fatal("wrong thread accepted")
	}
	args["actor"] = "operator"
	if response := call(args, "thread"); response["success"] != false {
		t.Fatal("spoofed actor accepted")
	}
	delete(args, "actor")
	if response := call(args, "thread"); response["success"] != true {
		t.Fatalf("valid planning report: %+v", response)
	}
	// The new revision from this tool is available, while concurrent withdrawal
	// invalidates further reports without silently incorporating new input.
	if _, err := runtime.ledger.Append(1, 2, store.OperatorRequestRevision{Action: "withdraw", Actor: store.OperatorRequestActor{Role: "operator", Name: "fixture operator"}, Explanation: "superseded"}); err != nil {
		t.Fatal(err)
	}
	args["revision"] = 2
	if response := call(args, "thread"); response["success"] != false {
		t.Fatal("stale report overwrote withdrawal")
	}
	ledger, err := runtime.ledger.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Requests[0].History) != 3 || ledger.Requests[0].History[1].Actor.Role != "implementor" {
		t.Fatalf("history=%+v", ledger)
	}
}

func TestConcurrentIdenticalOperatorReportsReturnTheDurableRevision(t *testing.T) {
	runtime := newOperatorRequestTestRuntime(t)
	session := NewGenerationSession(runtime, GenerationSessionOptions{})
	session.generation = 12
	session.generationSet = true
	session.threadID = "thread"
	session.turnID = "turn"
	if _, _, err := session.prepareOperatorRequests("prompt", "planning", "thread"); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	runtime.onReport = func() { entered <- struct{}{}; <-release }
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			response, err := session.dispatchOperatorRequest("record_operator_request", map[string]any{"id": 1, "revision": 1, "disposition": "deferred", "explanation": "Later"})
			if err == nil {
				var value store.OperatorRequestView
				err = json.Unmarshal(response.Output, &value)
				if err == nil && (value.ID != 1 || value.Revision != 2 || value.Report == nil) {
					err = &GenerationWorkerError{Reason: "duplicate report lost its durable response identity"}
				}
			}
			results <- err
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("report did not reach persistence")
		}
	}
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	ledger, err := runtime.ledger.Snapshot()
	if err != nil || len(ledger.Requests[0].History) != 2 {
		t.Fatalf("duplicate history: %+v %v", ledger, err)
	}
}
