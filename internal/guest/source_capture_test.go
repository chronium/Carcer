package guest

import (
	"bytes"
	"context"
	"testing"
)

func TestCaptureSourceSnapshotUsesShortReadEOFAndCanonicalOrder(t *testing.T) {
	source := map[string][]byte{"seed/z.bin": {0, 1, 2}, "seed/a.c": []byte("a\n")}
	invoke := func(_ context.Context, name string, arguments [][]byte) (ToolResult, error) {
		switch name {
		case "list":
			return ToolResult{Status: 0, Output: []byte("seed/z.bin\nseed/a.c\n")}, nil
		case "read":
			return ToolResult{Status: 0, Output: append([]byte(nil), source[string(arguments[0])]...)}, nil
		default:
			t.Fatalf("unexpected tool %q", name)
			return ToolResult{}, nil
		}
	}
	encoded, err := CaptureSourceSnapshot(context.Background(), invoke)
	if err != nil {
		t.Fatal(err)
	}
	files, err := DecodeSourceSnapshot(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Path != "seed/a.c" || !bytes.Equal(files[1].Content, source["seed/z.bin"]) {
		t.Fatalf("captured files = %#v", files)
	}
}

func TestCaptureSourceSnapshotRejectsContentBeyondSnapshotBoundary(t *testing.T) {
	invoke := func(_ context.Context, name string, _ [][]byte) (ToolResult, error) {
		if name == "list" {
			return ToolResult{Status: 0, Output: []byte("seed/large\n")}, nil
		}
		return ToolResult{Status: 0, Output: bytes.Repeat([]byte("x"), maxSnapshotContent+1)}, nil
	}
	if _, err := CaptureSourceSnapshot(context.Background(), invoke); err == nil {
		t.Fatal("oversized source capture unexpectedly succeeded")
	}
}
