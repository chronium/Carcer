package agent

import (
	"bytes"
	"codexos/internal/guest"
	"codexos/internal/observability"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBootstrapHelpersAreForwardedOnlyWhenAdvertised(t *testing.T) {
	runtime := newGenerationTestRuntime(t)
	session := &GenerationSession{runtime: runtime, runCtx: context.Background(), availableTools: map[string]struct{}{}}
	if _, e := session.dispatchTool("bootstrap_job", map[string]any{"request": "{}"}); e == nil {
		t.Fatal("unadvertised helper exposed")
	}
	session.availableTools = map[string]struct{}{"bootstrap_job": {}, "read_bootstrap_artifact": {}}
	request := `{"version":1,"argv":["true"],"outputs":[]}`
	if _, e := session.dispatchTool("bootstrap_job", map[string]any{"request": request}); e != nil {
		t.Fatal(e)
	}
	if _, e := session.dispatchTool("read_bootstrap_artifact", map[string]any{"id": "opaque-id", "offset": json.Number("0"), "length": json.Number("8")}); e != nil {
		t.Fatal(e)
	}
	if len(runtime.calls) != 2 || runtime.calls[0].name != "bootstrap_job" || len(runtime.calls[0].arguments) != 1 || string(runtime.calls[0].arguments[0]) != request || runtime.calls[1].name != "read_bootstrap_artifact" {
		t.Fatalf("unexpected bridge calls %+v", runtime.calls)
	}
	if _, e := session.dispatchTool("bootstrap_job", map[string]any{"request": strings.Repeat("x", 16385)}); e == nil {
		t.Fatal("oversized bridge request accepted")
	}
	if len(runtime.calls) != 2 {
		t.Fatal("oversized request reached guest")
	}
}

func TestImportBootstrapAdvertisementAndArguments(t *testing.T) {
	runtime := newGenerationTestRuntime(t)
	session := NewGenerationSession(runtime, GenerationSessionOptions{})
	session.runCtx = context.Background()
	args := map[string]any{"id": "opaque-é", "length": json.Number("33554432"), "path": "ram/new"}
	if _, err := session.dispatchTool("import_bootstrap_artifact", args); err == nil {
		t.Fatal("unadvertised import accepted")
	}
	selected, order, err := advertisedGuestToolsInOrder([]string{"read", "import_bootstrap_artifact"})
	if err != nil {
		t.Fatal(err)
	}
	session.availableTools = selected
	encoded, _ := json.Marshal(dynamicToolNamespaceInOrder(selected, order))
	if !strings.Contains(string(encoded), `"name":"import_bootstrap_artifact"`) {
		t.Fatalf("missing advertised import: %s", encoded)
	}
	without, _ := json.Marshal(dynamicToolNamespace(map[string]struct{}{"read": {}}))
	if strings.Contains(string(without), "import_bootstrap_artifact") {
		t.Fatal("unadvertised schema exposed")
	}
	for _, length := range []json.Number{"0", "33554432"} {
		args["length"] = length
		if _, err := session.dispatchTool("import_bootstrap_artifact", args); err != nil {
			t.Fatal(err)
		}
		call := runtime.calls[len(runtime.calls)-1]
		if call.name != "import_bootstrap_artifact" || len(call.arguments) != 3 || string(call.arguments[0]) != "opaque-é" || string(call.arguments[1]) != string(length) || string(call.arguments[2]) != "ram/new" {
			t.Fatalf("wrong wire call: %+v", call)
		}
	}
	for _, bad := range []map[string]any{
		{"id": "x", "length": 0}, {"id": "x", "length": 0, "path": "x", "offset": 0},
		{"id": "", "length": 0, "path": "x"}, {"id": "x\x00", "length": 0, "path": "x"},
		{"id": strings.Repeat("é", 128), "length": 0, "path": "x"}, {"id": "\xff", "length": 0, "path": "x"},
		{"id": "x", "length": "0", "path": "x"}, {"id": "x", "length": -1, "path": "x"},
		{"id": "x", "length": 33554433, "path": "x"}, {"id": "x", "length": json.Number("1.0"), "path": "x"},
		{"id": "x", "length": json.Number("01"), "path": "x"}, {"id": "x", "length": 0, "path": ""},
		{"id": "x", "length": 0, "path": strings.Repeat("é", 128)}, {"id": "x", "length": 0, "path": "\xff"},
	} {
		if _, err := session.dispatchTool("import_bootstrap_artifact", bad); err == nil {
			t.Fatalf("invalid args accepted: %#v", bad)
		}
	}
	if len(runtime.calls) != 2 {
		t.Fatal("invalid import reached guest")
	}
}

func TestImportBootstrapPhaseAndDelivery(t *testing.T) {
	for _, tc := range []struct {
		name      string
		planning  bool
		status    uint32
		err       error
		delivered bool
		kind      observability.ActivityKind
	}{
		{"planning", true, 0, nil, false, observability.ActivityToolFailed},
		{"success", false, 0, nil, true, observability.ActivityToolCompleted},
		{"guest_failure", false, 1, nil, true, observability.ActivityToolFailed},
		{"bridge_failure", false, 0, errors.New("guest disconnected"), false, observability.ActivityToolFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := newGenerationTestRuntime(t)
			runtime.invoke = func(context.Context, string, [][]byte) (guest.ToolResult, error) {
				return guest.ToolResult{Status: tc.status, Output: []byte("import result")}, tc.err
			}
			activity := observability.NewActivityStream()
			session := NewGenerationSession(runtime, GenerationSessionOptions{ActivityStream: activity})
			session.runCtx = context.Background()
			session.availableTools = map[string]struct{}{"import_bootstrap_artifact": {}}
			response := session.dynamicToolResponse(map[string]any{"callId": "import", "threadId": "thread", "turnId": "turn", "namespace": "codexos", "tool": "import_bootstrap_artifact", "arguments": map[string]any{"id": "opaque", "length": 0, "path": "ram/new"}}, "thread", "turn", tc.planning, nil)
			if response["success"] != tc.delivered {
				t.Fatalf("delivery response: %#v", response)
			}
			if tc.planning && len(runtime.calls) != 0 {
				t.Fatal("planning mutated guest")
			}
			if tc.delivered {
				var result struct {
					Status uint32 `json:"status"`
					Output string `json:"output"`
				}
				if err := json.Unmarshal([]byte(response["contentItems"].([]map[string]any)[0]["text"].(string)), &result); err != nil {
					t.Fatal(err)
				}
				if result.Status != tc.status || result.Output != "import result" {
					t.Fatalf("lost guest result: %+v", result)
				}
			}
			events := activity.Drain()
			if len(events) == 0 || events[len(events)-1].Kind != tc.kind {
				t.Fatalf("activity: %+v", events)
			}
		})
	}
}

func TestImportBootstrapReadOnlyPhasesRejectMutation(t *testing.T) {
	runtime := newGenerationTestRuntime(t)
	args := map[string]any{"id": "opaque", "length": 0, "path": "ram/new"}
	if _, err := dispatchReadOnlyTool(context.Background(), generationReviewRuntime{runtime}, "import_bootstrap_artifact", args, nil, nil); err == nil {
		t.Fatal("review import accepted")
	}
	session := NewGenerationSession(runtime, GenerationSessionOptions{})
	response := session.interviewToolDenial(map[string]any{"tool": "import_bootstrap_artifact", "arguments": args}, "thread", "turn")
	if response["success"] != false || len(runtime.calls) != 0 {
		t.Fatalf("read-only mutation: %+v", runtime.calls)
	}
}

func TestImportBootstrapAppServerConfirmsDelivery(t *testing.T) {
	for _, status := range []uint32{0, 1} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			assertGuestMutationAppServerDelivery(t, "import_bootstrap_artifact", status, "guest import result")
		})
	}
}

func assertGuestMutationAppServerDelivery(t *testing.T, tool string, status uint32, output string) {
	t.Helper()
	recordPath := filepath.Join(t.TempDir(), "generation.json")
	mode := "guest-mutation"
	if tool == "import_bootstrap_artifact" {
		mode = "bootstrap-import"
	}
	setGenerationHelper(t, mode, recordPath)
	t.Setenv("CODEXOS_TEST_GUEST_MUTATION_TOOL", tool)
	runtime := newGenerationTestRuntime(t)
	runtime.tools = append(runtime.tools, tool)
	runtime.invoke = func(_ context.Context, name string, args [][]byte) (guest.ToolResult, error) {
		if name == tool {
			return guest.ToolResult{Status: status, Output: []byte(output)}, nil
		}
		return guest.ToolResult{Status: 0}, nil
	}
	session := NewGenerationSession(runtime, GenerationSessionOptions{Executable: os.Args[0], AuthFile: fakeAuthFile(t), StopTimeout: time.Second})
	t.Cleanup(func() { _ = session.Close() })
	if result, err := session.RunInitialTurn(); err != nil || result.TurnStatus != "completed" {
		t.Fatalf("initial turn: %+v, %v", result, err)
	}
	waitForReviewerFile(t, recordPath)
	record := readReviewerJSON(t, recordPath)
	pid, ok := session.ProcessPID()
	if !ok || pid <= 0 {
		t.Fatal("missing app-server PID")
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	waitForGenerationProcessExit(t, pid)
	events, err := os.ReadFile(filepath.Join(runtime.root, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	confirmed := false
	for _, line := range bytes.Split(events, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var event struct {
			Event string         `json:"event"`
			Data  map[string]any `json:"data"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		if event.Event == "tool_result_delivered" && event.Data["tool"] == tool {
			confirmed = true
		}
	}
	if !confirmed {
		t.Fatalf("missing ordinary delivery confirmation: %s", events)
	}
	found := false
	for _, value := range record["messages"].([]any) {
		message := value.(map[string]any)
		if message["id"] != "generation-call-1" {
			continue
		}
		result := message["result"].(map[string]any)
		if result["success"] != true {
			t.Fatalf("guest failure confused with delivery: %+v", result)
		}
		var guestResult struct {
			Status uint32 `json:"status"`
			Output string `json:"output"`
		}
		text := result["contentItems"].([]any)[0].(map[string]any)["text"].(string)
		if err := json.Unmarshal([]byte(text), &guestResult); err != nil {
			t.Fatal(err)
		}
		if guestResult.Status != status || guestResult.Output != output {
			t.Fatalf("lost guest result: %+v", guestResult)
		}
		found = true
	}
	if !found {
		t.Fatal("missing app-server result")
	}
}
