package guest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strings"
	"testing"
)

func TestCaptureSourceSnapshotMatchesTrustedBuildSourceSelection(t *testing.T) {
	source := map[string][]byte{
		"seed/z.bin":     {0, 1, 2},
		"seed/a.c":       []byte("a\n"),
		"test/immutable": []byte("not source"),
	}
	var readPaths []string
	invoke := func(_ context.Context, name string, arguments [][]byte) (ToolResult, error) {
		switch name {
		case "list":
			if len(arguments) != 1 || string(arguments[0]) != "seed/" {
				t.Fatalf("source list arguments = %q", arguments)
			}
			return ToolResult{Status: 0, Output: []byte("seed/z.bin\nseed/a.c\n")}, nil
		case "read":
			path := string(arguments[0])
			readPaths = append(readPaths, path)
			return ToolResult{Status: 0, Output: append([]byte(nil), source[path]...)}, nil
		default:
			t.Fatalf("unexpected tool %q", name)
			return ToolResult{}, nil
		}
	}
	encoded, err := CaptureSourceSnapshot(context.Background(), invoke)
	if err != nil {
		t.Fatal(err)
	}
	buildSnapshot, err := EncodeSourceSnapshot([]SnapshotFile{
		{Path: "seed/a.c", Content: source["seed/a.c"]},
		{Path: "seed/z.bin", Content: source["seed/z.bin"]},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, buildSnapshot) || sha256.Sum256(encoded) != sha256.Sum256(buildSnapshot) {
		t.Fatalf("review snapshot differs from trusted build snapshot: review=%x build=%x", sha256.Sum256(encoded), sha256.Sum256(buildSnapshot))
	}
	files, err := DecodeSourceSnapshot(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Path != "seed/a.c" || len(files[0].Content) != len(source["seed/a.c"]) || files[1].Path != "seed/z.bin" || len(files[1].Content) != len(source["seed/z.bin"]) || !bytes.Equal(files[1].Content, source["seed/z.bin"]) {
		t.Fatalf("captured files = %#v", files)
	}
	if strings.Join(readPaths, ",") != "seed/a.c,seed/z.bin" {
		t.Fatalf("review source reads = %q", readPaths)
	}
}

func TestCaptureSourceSnapshotRejectsInvalidSelectedSourcePath(t *testing.T) {
	invoke := func(_ context.Context, name string, _ [][]byte) (ToolResult, error) {
		if name == "list" {
			return ToolResult{Status: 0, Output: []byte("seed/../escape.c\n")}, nil
		}
		t.Fatalf("invalid source path reached %q", name)
		return ToolResult{}, nil
	}
	if _, err := CaptureSourceSnapshot(context.Background(), invoke); err == nil || !strings.Contains(err.Error(), "unsafe source path") {
		t.Fatalf("invalid selected source path error = %v", err)
	}
}

func TestCaptureSourceSnapshotRejectsOutOfPrefixListResult(t *testing.T) {
	invoke := func(_ context.Context, name string, arguments [][]byte) (ToolResult, error) {
		if name == "list" {
			if len(arguments) != 1 || string(arguments[0]) != "seed/" {
				t.Fatalf("source list arguments = %q", arguments)
			}
			return ToolResult{Status: 0, Output: []byte("test/immutable\n")}, nil
		}
		t.Fatalf("out-of-prefix source path reached %q", name)
		return ToolResult{}, nil
	}
	if _, err := CaptureSourceSnapshot(context.Background(), invoke); err == nil || !strings.Contains(err.Error(), "outside seed/ prefix") {
		t.Fatalf("out-of-prefix source list error = %v", err)
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
