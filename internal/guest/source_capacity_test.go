package guest

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"codexos/internal/sourcecapacity"
)

func TestSourceCapacityBoundariesAndFraming(t *testing.T) {
	for _, budget := range []sourcecapacity.Budget{0, sourcecapacity.Default, sourcecapacity.Expanded} {
		t.Run(strconv.Itoa(budget.Bytes()), func(t *testing.T) {
			files := make([]SnapshotFile, maxSnapshotFiles)
			for i := range files {
				files[i].Path = "seed/" + strconv.Itoa(i) + strings.Repeat("x", maxSourcePathBytes-5-len(strconv.Itoa(i)))
			}
			files[0].Content = make([]byte, budget.Bytes())
			encoded, err := EncodeSourceSnapshotWithBudget(files, budget)
			if err != nil {
				t.Fatal(err)
			}
			if int64(len(encoded)) != budget.SnapshotLimit() {
				t.Fatalf("framing limit: %d != %d", len(encoded), budget.SnapshotLimit())
			}
			if _, err := ParseSourceSnapshotWithBudget(encoded, budget); err != nil {
				t.Fatal(err)
			}
			files[1].Content = []byte{1}
			if _, err := ParseSourceSnapshotWithBudget(encodeSnapshotFiles(files), budget); err == nil {
				t.Fatal("decoder accepted aggregate overflow")
			}
			if _, err := EncodeSourceSnapshotWithBudget(files, budget); err == nil {
				t.Fatal("accepted aggregate overflow")
			}
			if budget.Bytes() == sourcecapacity.Expanded {
				if _, err := ParseSourceSnapshot(encoded); err == nil {
					t.Fatal("default accepted expanded source")
				}
			}
		})
	}
}

func TestCaptureSourceCapacity(t *testing.T) {
	for _, size := range []int{sourcecapacity.Expanded, sourcecapacity.Expanded + 1} {
		invoke := func(_ context.Context, name string, args [][]byte) (ToolResult, error) {
			if name == "list" {
				return ToolResult{Output: []byte("seed/main.c\n")}, nil
			}
			requested, err := strconv.Atoi(string(args[2]))
			if err != nil {
				t.Fatal(err)
			}
			return ToolResult{Output: make([]byte, min(size, requested))}, nil
		}
		snapshot, err := CaptureCanonicalSourceSnapshotWithBudget(context.Background(), invoke, sourcecapacity.Expanded)
		if size > sourcecapacity.Expanded {
			if err == nil {
				t.Fatal("capture accepted overflow")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseSourceSnapshotWithBudget(snapshot.Bytes(), sourcecapacity.Expanded); err != nil {
			t.Fatal(err)
		}
		if _, err := CaptureCanonicalSourceSnapshot(context.Background(), invoke); err == nil {
			t.Fatal("default capture accepted expanded source")
		}
	}
}
