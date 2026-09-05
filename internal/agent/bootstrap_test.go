package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
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
