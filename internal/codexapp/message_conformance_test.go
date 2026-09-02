package codexapp

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"testing"
)

func TestPythonMessageJSONConformance(t *testing.T) {
	const script = `
import json
message = {"id": 17, "method": "工具<&>", "params": {"text": "λ\n次"}}
print(json.dumps(message, ensure_ascii=False, separators=(",", ":")))
print(json.dumps({"text": "λ<&>"}, ensure_ascii=False, separators=(",", ":")))
`
	output, err := exec.Command("python3", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("Python JSON reference failed: %v\n%s", err, output)
	}
	separator := bytes.IndexByte(output, '\n')
	if separator < 0 {
		t.Fatalf("Python reference returned %q", output)
	}
	encoded, err := EncodeMessage(map[string]any{
		"id": json.Number("17"), "method": "工具<&>", "params": map[string]any{"text": "λ\n次"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, output[:separator+1]) {
		t.Fatalf("message differs: Go %q Python %q", encoded, output[:separator+1])
	}
	short, err := ShortJSON(map[string]any{"text": "λ<&>"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(bytes.TrimSpace(output[separator+1:])); short != got {
		t.Fatalf("short JSON differs: Go %q Python %q", short, got)
	}
}
