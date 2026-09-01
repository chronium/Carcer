package observability

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

func TestActivityStreamConcurrentPublicationIsOrdered(t *testing.T) {
	stream := NewActivityStream()
	start := make(chan struct{})
	var workers sync.WaitGroup
	for worker := range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			generation := uint64(7)
			for value := range 40 {
				if _, err := stream.Publish(&generation, ActivityImplementor, ActivityToolStarted, map[string]any{
					"worker": worker,
					"value":  value,
					"bytes":  []byte{0, 's', 'r', 'c'},
				}, "thread", "turn", ""); err != nil {
					t.Errorf("publish: %v", err)
					return
				}
			}
		}()
	}
	close(start)
	workers.Wait()
	events := stream.Drain()
	for index, event := range events {
		if event.Sequence != uint64(index+1) || event.Generation == nil || *event.Generation != 7 {
			t.Fatalf("event %d = %#v", index, event)
		}
		if got := event.Data["bytes"].([]byte); len(got) != 4 || got[0] != 0 {
			t.Fatalf("event bytes = %v", got)
		}
	}
	if len(events) != 160 {
		t.Fatalf("event count = %d", len(events))
	}
}

func TestActivityStreamCopiesMutablePayloads(t *testing.T) {
	stream := NewActivityStream()
	data := []byte("before")
	returned, err := stream.Publish(nil, ActivityHarness, ActivityBuildStarted, map[string]any{"bytes": data}, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	data[0] = 'X'
	returned.Data["bytes"].([]byte)[1] = 'X'
	event := stream.Drain()[0]
	if string(event.Data["bytes"].([]byte)) != "before" {
		t.Fatalf("queued bytes = %q", event.Data["bytes"])
	}
}

func TestActivityStreamIsBoundedAndContextAware(t *testing.T) {
	stream := NewActivityStream()
	for range ActivityQueueCapacity {
		if _, err := stream.Publish(nil, ActivityHarness, ActivityBuildStarted, nil, "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := stream.Publish(nil, ActivityHarness, ActivityBuildStarted, nil, "", "", ""); !errors.Is(err, ErrActivityQueueFull) {
		t.Fatalf("full queue error = %v", err)
	}
	stream.Drain()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := stream.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled next error = %v", err)
	}
}

func TestRenderableCodexNotificationPrivacyBoundary(t *testing.T) {
	stream := NewActivityStream()
	generation := uint64(3)
	common := map[string]any{"threadId": "thread-1", "turnId": "turn-1", "itemId": "reasoning-1"}
	visible := map[string]any{
		"method": "item/reasoning/summaryTextDelta",
		"params": mergeActivityMaps(common, map[string]any{"summaryIndex": json.Number("0"), "delta": "Checking ABI."}),
	}
	activities := PublishRenderableCodexNotification(stream, &generation, ActivityReviewer, visible, "thread-1", "turn-1", "review")
	if len(activities) != 1 || activities[0].Kind != ActivityAgentReasoningDelta {
		t.Fatalf("visible activities = %#v", activities)
	}
	raw := map[string]any{
		"method": "item/reasoning/textDelta",
		"params": mergeActivityMaps(common, map[string]any{"contentIndex": json.Number("0"), "delta": "private detail"}),
	}
	if got := PublishRenderableCodexNotification(stream, &generation, ActivityReviewer, raw, "thread-1", "turn-1", "review"); len(got) != 0 {
		t.Fatalf("raw reasoning was renderable: %#v", got)
	}
	events := stream.Drain()
	if len(events) != 1 || events[0].Data["text"] != "Checking ABI." || events[0].Data["turn_phase"] != "review" {
		t.Fatalf("published events = %#v", events)
	}
}

func TestRenderableCompletedItems(t *testing.T) {
	base := map[string]any{"threadId": "t", "turnId": "v", "itemId": "outer"}
	tests := []struct {
		item map[string]any
		kind ActivityKind
	}{
		{map[string]any{"id": "message", "type": "agentMessage", "text": "done", "phase": "final"}, ActivityAgentMessage},
		{map[string]any{"id": "reasoning", "type": "reasoning", "summary": []any{"one", "two"}}, ActivityAgentReasoningSummary},
	}
	for _, test := range tests {
		message := map[string]any{"method": "item/completed", "params": mergeActivityMaps(base, map[string]any{"item": test.item})}
		got := RenderableCodexNotification(message, "t", "v")
		if len(got) != 1 || got[0].Kind != test.kind || got[0].ItemID != test.item["id"] {
			t.Fatalf("completed activity = %#v", got)
		}
	}
	wrongTurn := map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "other", "turnId": "v", "delta": "secret"}}
	if got := RenderableCodexNotification(wrongTurn, "t", "v"); len(got) != 0 {
		t.Fatalf("wrong-thread activity = %#v", got)
	}
}

func mergeActivityMaps(first, second map[string]any) map[string]any {
	result := make(map[string]any, len(first)+len(second))
	for key, value := range first {
		result[key] = value
	}
	for key, value := range second {
		result[key] = value
	}
	return result
}
