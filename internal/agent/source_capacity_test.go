package agent

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"codexos/internal/guest"
	"codexos/internal/sourcecapacity"
)

type capacityReviewRuntime struct {
	*generationTestRuntime
	budget sourcecapacity.Budget
}

func (r *capacityReviewRuntime) SourceCapacity() sourcecapacity.Budget { return r.budget }

func TestSourceCapacityReviewUsesRunBudget(t *testing.T) {
	encoded, err := guest.EncodeSourceSnapshotWithBudget([]guest.SnapshotFile{{Path: "seed/kernel.c", Content: make([]byte, sourcecapacity.Expanded)}}, sourcecapacity.Expanded)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &capacityReviewRuntime{generationTestRuntime: newGenerationTestRuntime(t), budget: sourcecapacity.Expanded}
	runtime.capture = func(context.Context) ([]byte, error) { return encoded, nil }
	snapshot, err := captureReviewSource(context.Background(), runtime)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(snapshot.Bytes(), encoded) {
		t.Fatal("review changed captured bytes")
	}
	prompt, err := planningPrompt(runtime, nil)
	if err != nil || !strings.Contains(prompt, "1048576 aggregate file-content bytes") {
		t.Fatalf("provisioned planning context: %v", err)
	}
	runtime.budget = 0
	if _, err := captureReviewSource(context.Background(), runtime); err == nil {
		t.Fatal("review accepted source exceeding run budget")
	}
	if _, err := captureReviewSource(context.Background(), runtime.generationTestRuntime); err == nil {
		t.Fatal("legacy runtime lost default budget")
	}
}
