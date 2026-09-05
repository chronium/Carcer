package agent

import (
	"codexos/internal/store"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestFeatureDecisionNotesReachImplementor(t *testing.T) {
	runtime := newGenerationTestRuntime(t)
	runtime.requests = []store.FeatureRequest{
		{ID: 5, Generation: 9, Title: "Batch", Description: "Guest request for four jobs", Status: store.FeatureApproved, DecisionNote: "Already provisioned. λ Four is scope, not quota."},
		{ID: 6, Generation: 9, Title: "Other", Description: "Guest description", Status: store.FeatureDenied, DecisionNote: "Not available."},
		{ID: 3, Generation: 4, Title: "Pipeline", Description: "Pending dependency", Status: store.FeaturePending},
	}
	session := NewGenerationSession(runtime, GenerationSessionOptions{})
	session.runCtx = context.Background()
	result, err := session.dispatchTool("list_requests", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Requests []struct {
			ID           uint64 `json:"id"`
			Description  string `json:"description"`
			DecisionNote string `json:"decision_note"`
		}
	}
	if err := json.Unmarshal(result.Output, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Requests) != 3 || decoded.Requests[0].ID != 3 || decoded.Requests[0].DecisionNote != "" || decoded.Requests[1].DecisionNote != runtime.requests[0].DecisionNote || decoded.Requests[1].Description != runtime.requests[0].Description || decoded.Requests[2].DecisionNote != "Not available." {
		t.Fatalf("list_requests: %s", result.Output)
	}
	prompt, err := planningPrompt(runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "Guest request for four jobs\nOperator decision note:\n"+runtime.requests[0].DecisionNote) || strings.Contains(prompt, "Not available.") {
		t.Fatalf("decision prompt: %s", prompt)
	}
}
