package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"codexos/internal/guest"
	"codexos/internal/observability"
)

func TestGuestTaskToolsAdvertisementAndArguments(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
		wire []string
	}{
		{"run", map[string]any{"path": "ram/./é\x00"}, []string{"ram/./é\x00"}},
		{"reap", map[string]any{"task_id": json.Number("4294967295")}, []string{"4294967295"}},
		{"import_provided_asset", map[string]any{"id": strings.Repeat("é", 128), "path": "../ram/λ"}, []string{strings.Repeat("é", 128), "../ram/λ"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := newGenerationTestRuntime(t)
			session := NewGenerationSession(runtime, GenerationSessionOptions{})
			session.runCtx = context.Background()
			if _, err := session.dispatchTool(tc.name, tc.args); err == nil {
				t.Fatal("unadvertised tool accepted")
			}
			without, _ := json.Marshal(dynamicToolNamespace(map[string]struct{}{"read": {}}))
			if strings.Contains(string(without), `"name":"`+tc.name+`"`) {
				t.Fatal("unadvertised schema exposed")
			}
			selected, order, err := advertisedGuestToolsInOrder([]string{"read", tc.name, "unrecognized-future-tool"})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(order, []string{"read", tc.name}) {
				t.Fatalf("discovery order: %v", order)
			}
			namespace := dynamicToolNamespaceInOrder(selected, order)
			encoded, _ := json.Marshal(namespace)
			if !strings.Contains(string(encoded), `"name":"`+tc.name+`"`) {
				t.Fatalf("missing schema: %s", encoded)
			}
			session.availableTools = selected
			if _, err := session.dispatchTool(tc.name, tc.args); err != nil {
				t.Fatal(err)
			}
			if len(runtime.calls) != 1 || runtime.calls[0].name != tc.name {
				t.Fatalf("calls: %+v", runtime.calls)
			}
			var wire []string
			for _, arg := range runtime.calls[0].arguments {
				wire = append(wire, string(arg))
			}
			if !reflect.DeepEqual(wire, tc.wire) {
				t.Fatalf("wire=%q want=%q", wire, tc.wire)
			}
		})
	}
}

func TestGuestTaskToolsRejectMalformedArguments(t *testing.T) {
	runtime := newGenerationTestRuntime(t)
	session := NewGenerationSession(runtime, GenerationSessionOptions{})
	session.runCtx = context.Background()
	session.availableTools = map[string]struct{}{"run": {}, "reap": {}, "import_provided_asset": {}}
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"run", map[string]any{}}, {"run", map[string]any{"path": "x", "argv": []string{}}},
		{"run", map[string]any{"path": 0}}, {"run", map[string]any{"path": ""}},
		{"run", map[string]any{"path": "\xff"}}, {"run", map[string]any{"path": strings.Repeat("é", 128)}},
		{"reap", map[string]any{}}, {"reap", map[string]any{"id": 1}},
		{"reap", map[string]any{"task_id": 1, "wait": true}}, {"reap", map[string]any{"task_id": true}},
		{"reap", map[string]any{"task_id": "1"}}, {"reap", map[string]any{"task_id": -1}},
		{"reap", map[string]any{"task_id": json.Number("1.5")}}, {"reap", map[string]any{"task_id": json.Number("1e2")}},
		{"reap", map[string]any{"task_id": json.Number("4294967296")}},
		{"import_provided_asset", map[string]any{"path": "x"}}, {"import_provided_asset", map[string]any{"id": "x"}},
		{"import_provided_asset", map[string]any{"id": "x", "path": "x", "length": 0}},
		{"import_provided_asset", map[string]any{"id": "", "path": "x"}}, {"import_provided_asset", map[string]any{"id": 5, "path": "x"}},
		{"import_provided_asset", map[string]any{"id": "\xff", "path": "x"}}, {"import_provided_asset", map[string]any{"id": "x", "path": ""}},
		{"import_provided_asset", map[string]any{"id": "x", "path": "\xff"}}, {"import_provided_asset", map[string]any{"id": "x", "path": strings.Repeat("x", 256)}},
	} {
		if _, err := session.dispatchTool(tc.tool, tc.args); err == nil {
			t.Fatalf("accepted %s %#v", tc.tool, tc.args)
		}
	}
	if len(runtime.calls) != 0 {
		t.Fatal("malformed arguments reached guest")
	}
	// The guest, not the bridge, decides whether an in-range slot is usable.
	if _, err := session.dispatchTool("reap", map[string]any{"task_id": 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.dispatchTool("run", map[string]any{"path": strings.Repeat("x", 255)}); err != nil {
		t.Fatal(err)
	}
}

func TestGuestTaskToolsAreImplementationOnly(t *testing.T) {
	for _, tool := range []string{"run", "reap", "import_provided_asset"} {
		t.Run(tool, func(t *testing.T) {
			runtime := newGenerationTestRuntime(t)
			session := NewGenerationSession(runtime, GenerationSessionOptions{})
			session.runCtx = context.Background()
			session.availableTools = map[string]struct{}{tool: {}}
			args := map[string]any{"path": "ram/test"}
			if tool == "reap" {
				args = map[string]any{"task_id": 1}
			}
			if tool == "import_provided_asset" {
				args["id"] = "fixture"
			}
			params := map[string]any{"callId": "call", "namespace": "codexos", "tool": tool, "arguments": args, "threadId": "thread", "turnId": "turn"}
			response := session.dynamicToolResponse(params, "thread", "turn", true, nil)
			if response["success"] != false || !strings.Contains(response["contentItems"].([]map[string]any)[0]["text"].(string), "planning phase") {
				t.Fatalf("planning response: %+v", response)
			}
			if _, err := dispatchReadOnlyTool(context.Background(), generationReviewRuntime{runtime}, tool, args, nil, nil); err == nil {
				t.Fatal("review mutation accepted")
			}
			if result := session.interviewToolDenial(params, "thread", "turn"); result["success"] != false {
				t.Fatal("interview mutation accepted")
			}
			if len(runtime.calls) != 0 {
				t.Fatal("read-only phase reached guest")
			}
		})
	}
}

func TestGuestTaskToolsPreserveResults(t *testing.T) {
	for _, tc := range []struct {
		name, tool string
		status     uint32
		output     string
		err        error
	}{
		{"task_id", "run", 0, "31", nil}, {"launch_failure", "run", 1, "", nil},
		{"running", "reap", 0, "running", nil}, {"zero_exit", "reap", 0, "0", nil},
		{"wide_exit", "reap", 0, "9223372036854775809", nil}, {"fault_exit", "reap", 0, "18446744073709551615", nil},
		{"reap_failure", "reap", 1, "", nil}, {"import_success", "import_provided_asset", 0, "", nil},
		{"import_failure", "import_provided_asset", 2, "asset read failed", nil},
		{"transport_failure", "run", 0, "", errors.New("serial disconnected")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := newGenerationTestRuntime(t)
			runtime.invoke = func(context.Context, string, [][]byte) (guest.ToolResult, error) {
				return guest.ToolResult{Status: tc.status, Output: []byte(tc.output)}, tc.err
			}
			activity := observability.NewActivityStream()
			session := NewGenerationSession(runtime, GenerationSessionOptions{ActivityStream: activity})
			session.runCtx = context.Background()
			session.availableTools = map[string]struct{}{tc.tool: {}}
			args := map[string]any{"path": "ram/test"}
			if tc.tool == "reap" {
				args = map[string]any{"task_id": 1}
			}
			if tc.tool == "import_provided_asset" {
				args["id"] = "fixture"
			}
			response := session.dynamicToolResponse(map[string]any{"callId": "call", "namespace": "codexos", "tool": tc.tool, "arguments": args, "threadId": "thread", "turnId": "turn"}, "thread", "turn", false, nil)
			if response["success"] != (tc.err == nil) {
				t.Fatalf("bridge response success: %+v", response)
			}
			if tc.err == nil {
				var got struct {
					Status uint32 `json:"status"`
					Output string `json:"output"`
				}
				if err := json.Unmarshal([]byte(response["contentItems"].([]map[string]any)[0]["text"].(string)), &got); err != nil {
					t.Fatal(err)
				}
				if got.Status != tc.status || got.Output != tc.output {
					t.Fatalf("result changed: %+v", got)
				}
			}
			events := activity.Drain()
			want := observability.ActivityToolCompleted
			if tc.status != 0 || tc.err != nil {
				want = observability.ActivityToolFailed
			}
			if len(events) == 0 || events[len(events)-1].Kind != want {
				t.Fatalf("activity: %+v", events)
			}
		})
	}
}
