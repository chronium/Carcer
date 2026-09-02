package codexapp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTokenUsageNotificationReturnsExactDelta(t *testing.T) {
	previous := CumulativeTokenUsage{InputTokens: 100, CachedInputTokens: 35, OutputTokens: 40, ReasoningOutputTokens: 12}
	params := decodeUsageJSON(t, `{
		"threadId":"thread-1",
		"turnId":"turn-1",
		"tokenUsage":{"total":{
			"inputTokens":165,
			"cachedInputTokens":70,
			"outputTokens":58,
			"reasoningOutputTokens":19,
			"totalTokens":"ignored"
		}}
	}`)
	total, delta, err := TokenUsageDeltaFromNotification(params, "thread-1", "turn-1", previous)
	if err != nil {
		t.Fatal(err)
	}
	wantTotal := CumulativeTokenUsage{InputTokens: 165, CachedInputTokens: 70, OutputTokens: 58, ReasoningOutputTokens: 19}
	wantDelta := TokenUsageDelta{InputTokens: 65, CachedInputTokens: 35, UncachedInputTokens: 30, OutputTokens: 18, ReasoningOutputTokens: 7}
	if total != wantTotal || delta != wantDelta || delta.IsZero() {
		t.Fatalf("usage = %#v, %#v; want %#v, %#v", total, delta, wantTotal, wantDelta)
	}
	_, duplicate, err := TokenUsageDeltaFromNotification(params, "thread-1", "turn-1", total)
	if err != nil || !duplicate.IsZero() {
		t.Fatalf("duplicate usage delta = %#v, %v", duplicate, err)
	}
}

func TestTokenUsageNotificationRejectsMalformedOrDecreasingCounts(t *testing.T) {
	previous := CumulativeTokenUsage{InputTokens: 100, CachedInputTokens: 30, OutputTokens: 40, ReasoningOutputTokens: 10}
	cases := map[string]string{
		"missing field":             `{"inputTokens":100,"outputTokens":40,"reasoningOutputTokens":10}`,
		"floating point":            `{"inputTokens":100.0,"cachedInputTokens":30,"outputTokens":40,"reasoningOutputTokens":10}`,
		"boolean":                   `{"inputTokens":true,"cachedInputTokens":30,"outputTokens":40,"reasoningOutputTokens":10}`,
		"negative":                  `{"inputTokens":100,"cachedInputTokens":30,"outputTokens":40,"reasoningOutputTokens":-1}`,
		"decreasing total":          `{"inputTokens":99,"cachedInputTokens":30,"outputTokens":40,"reasoningOutputTokens":10}`,
		"decreasing cached":         `{"inputTokens":100,"cachedInputTokens":29,"outputTokens":40,"reasoningOutputTokens":10}`,
		"decreasing output":         `{"inputTokens":100,"cachedInputTokens":30,"outputTokens":39,"reasoningOutputTokens":10}`,
		"decreasing reasoning":      `{"inputTokens":100,"cachedInputTokens":30,"outputTokens":40,"reasoningOutputTokens":9}`,
		"cached exceeds input":      `{"inputTokens":100,"cachedInputTokens":101,"outputTokens":40,"reasoningOutputTokens":10}`,
		"reasoning exceeds output":  `{"inputTokens":100,"cachedInputTokens":30,"outputTokens":40,"reasoningOutputTokens":41}`,
		"uncached input decreases":  `{"inputTokens":110,"cachedInputTokens":45,"outputTokens":40,"reasoningOutputTokens":10}`,
		"unsigned integer overflow": `{"inputTokens":18446744073709551616,"cachedInputTokens":30,"outputTokens":40,"reasoningOutputTokens":10}`,
	}
	for name, total := range cases {
		t.Run(name, func(t *testing.T) {
			params := decodeUsageJSON(t, `{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{"total":`+total+`}}`)
			if _, _, err := TokenUsageDeltaFromNotification(params, "thread-1", "turn-1", previous); err == nil {
				t.Fatal("malformed token usage was accepted")
			}
		})
	}
}

func TestTokenUsageNotificationValidatesEnvelope(t *testing.T) {
	validTotal := `{"inputTokens":0,"cachedInputTokens":0,"outputTokens":0,"reasoningOutputTokens":0}`
	cases := []struct {
		name   string
		params any
		thread string
		turn   string
		error  string
	}{
		{name: "notification", params: []any{}, thread: "thread-1", turn: "turn-1", error: "notification is not an object"},
		{name: "identity", params: decodeUsageJSON(t, `{"threadId":"other","turnId":"turn-1","tokenUsage":{"total":`+validTotal+`}}`), thread: "thread-1", turn: "turn-1", error: "wrong thread or turn ID"},
		{name: "token usage", params: decodeUsageJSON(t, `{"threadId":"thread-1","turnId":"turn-1","tokenUsage":[]}`), thread: "thread-1", turn: "turn-1", error: "token usage is not an object"},
		{name: "total", params: decodeUsageJSON(t, `{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{"total":null}}`), thread: "thread-1", turn: "turn-1", error: "total token usage is not an object"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := TokenUsageDeltaFromNotification(test.params, test.thread, test.turn, CumulativeTokenUsage{})
			if err == nil || !strings.Contains(err.Error(), test.error) {
				t.Fatalf("error = %v, want %q", err, test.error)
			}
		})
	}
}

func decodeUsageJSON(t *testing.T, encoded string) any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}
