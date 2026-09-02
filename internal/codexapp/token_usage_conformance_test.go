package codexapp

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPythonTokenUsageConformance(t *testing.T) {
	const script = `
import importlib.util, pathlib, sys
root = pathlib.Path(sys.argv[1])
spec = importlib.util.spec_from_file_location("codex_app_server_reference", root / "harness" / "codex_app_server.py")
module = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = module
spec.loader.exec_module(module)
previous = module.CumulativeTokenUsage(100, 35, 40, 12)
params = {
    "threadId": "thread-1",
    "turnId": "turn-1",
    "tokenUsage": {"total": {
        "inputTokens": 165,
        "cachedInputTokens": 70,
        "outputTokens": 58,
        "reasoningOutputTokens": 19,
        "totalTokens": "ignored",
    }},
}
total, delta = module.token_usage_delta_from_notification(params, "thread-1", "turn-1", previous)
print(total.input_tokens, total.cached_input_tokens, total.output_tokens, total.reasoning_output_tokens)
print(delta.input_tokens, delta.cached_input_tokens, delta.uncached_input_tokens, delta.output_tokens, delta.reasoning_output_tokens)
for changed in (
    {**params, "threadId": "other"},
    {**params, "tokenUsage": {"total": {**params["tokenUsage"]["total"], "cachedInputTokens": 166}}},
    {**params, "tokenUsage": {"total": {**params["tokenUsage"]["total"], "inputTokens": 110, "cachedInputTokens": 46}}},
):
    try:
        module.token_usage_delta_from_notification(changed, "thread-1", "turn-1", previous)
    except module.CodexAppServerError as error:
        print(str(error))
`
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate conformance test")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "../.."))
	output, err := exec.Command("python3", "-c", script, root).CombinedOutput()
	if err != nil {
		t.Fatalf("Python token-usage reference failed: %v\n%s", err, output)
	}
	pythonLines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(pythonLines) != 5 {
		t.Fatalf("Python reference returned %q", output)
	}

	params := decodeUsageJSON(t, `{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{"total":{"inputTokens":165,"cachedInputTokens":70,"outputTokens":58,"reasoningOutputTokens":19,"totalTokens":"ignored"}}}`)
	previous := CumulativeTokenUsage{InputTokens: 100, CachedInputTokens: 35, OutputTokens: 40, ReasoningOutputTokens: 12}
	total, delta, err := TokenUsageDeltaFromNotification(params, "thread-1", "turn-1", previous)
	if err != nil {
		t.Fatal(err)
	}
	goLines := []string{
		fmt.Sprintf("%d %d %d %d", total.InputTokens, total.CachedInputTokens, total.OutputTokens, total.ReasoningOutputTokens),
		fmt.Sprintf("%d %d %d %d %d", delta.InputTokens, delta.CachedInputTokens, delta.UncachedInputTokens, delta.OutputTokens, delta.ReasoningOutputTokens),
	}
	wrongIdentity := params.(map[string]any)
	wrongIdentity["threadId"] = "other"
	_, _, identityErr := TokenUsageDeltaFromNotification(wrongIdentity, "thread-1", "turn-1", previous)
	goLines = append(goLines, identityErr.Error())
	wrongIdentity["threadId"] = "thread-1"
	totalObject := wrongIdentity["tokenUsage"].(map[string]any)["total"].(map[string]any)
	totalObject["cachedInputTokens"] = 166
	_, _, cachedErr := TokenUsageDeltaFromNotification(wrongIdentity, "thread-1", "turn-1", previous)
	goLines = append(goLines, cachedErr.Error())
	totalObject["inputTokens"] = 110
	totalObject["cachedInputTokens"] = 46
	_, _, uncachedErr := TokenUsageDeltaFromNotification(wrongIdentity, "thread-1", "turn-1", previous)
	goLines = append(goLines, uncachedErr.Error())
	for index := range pythonLines {
		if goLines[index] != pythonLines[index] {
			t.Fatalf("line %d differs: Go %q Python %q", index+1, goLines[index], pythonLines[index])
		}
	}
}
