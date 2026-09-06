package operator

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"codexos/internal/experiment"
)

func TestOperatorRequestConsoleCommandsAndPresentation(t *testing.T) {
	runtime, err := experiment.NewCodexOSRun(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	console, err := NewPlainConsole(runtime, PlainConsoleOptions{Input: strings.NewReader(""), Output: &output})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"os-request create Example λ | Desired behavior, with | literal pipe", "os-requests", "os-request 1", "os-request withdraw 1 superseded", "os-requests"} {
		quit, err := console.ExecuteLine(context.Background(), line)
		if err != nil || quit {
			t.Fatalf("%s: quit=%v err=%v", line, quit, err)
		}
	}
	text := output.String()
	for _, want := range []string{"#1 r1 [active] Example λ", "Desired behavior, with | literal pipe", "operator/", "run=", "[withdrawn]", "grant no capabilities", "next supported implementor turn boundary"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	for _, line := range []string{"os-request create | description", "os-request create title | ", "os-request create " + strings.Repeat("λ", 129) + " | description", "os-request create title | invalid\xff", "os-request verify 1 1 unsupported"} {
		if _, err := console.ExecuteLine(context.Background(), line); err == nil {
			t.Fatalf("accepted %q", line)
		}
	}
	if runtime.PresentationSnapshot().ActiveOperatorRequests != 0 {
		t.Fatal("withdrawn request counted as active")
	}
}
