package observability

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
)

const ActivityQueueCapacity = 4096

var ErrActivityQueueFull = errors.New("Codex activity queue is full")

type ActivityRole string

const (
	ActivityImplementor ActivityRole = "implementor"
	ActivityReviewer    ActivityRole = "reviewer"
	ActivityHarness     ActivityRole = "harness"
)

type ActivityKind string

const (
	ActivitySessionStarted         ActivityKind = "session.started"
	ActivitySessionStopped         ActivityKind = "session.stopped"
	ActivityTurnStarted            ActivityKind = "turn.started"
	ActivityTurnCompleted          ActivityKind = "turn.completed"
	ActivityTurnInterrupted        ActivityKind = "turn.interrupted"
	ActivityTurnFailed             ActivityKind = "turn.failed"
	ActivityAgentMessage           ActivityKind = "agent.message"
	ActivityAgentTextDelta         ActivityKind = "agent.text_delta"
	ActivityAgentReasoningSummary  ActivityKind = "agent.reasoning_summary"
	ActivityAgentReasoningDelta    ActivityKind = "agent.reasoning_delta"
	ActivityToolStarted            ActivityKind = "tool.started"
	ActivityToolCompleted          ActivityKind = "tool.completed"
	ActivityToolFailed             ActivityKind = "tool.failed"
	ActivityReviewStarted          ActivityKind = "review.started"
	ActivityReviewCompleted        ActivityKind = "review.completed"
	ActivityReviewCancelled        ActivityKind = "review.cancelled"
	ActivityReviewFailed           ActivityKind = "review.failed"
	ActivityExitInterviewStarted   ActivityKind = "exit_interview.started"
	ActivityExitInterviewQuestion  ActivityKind = "exit_interview.question"
	ActivityExitInterviewEnded     ActivityKind = "exit_interview.ended"
	ActivityBuildStarted           ActivityKind = "build.started"
	ActivityBuildCompileCompleted  ActivityKind = "build.compile_completed"
	ActivityBuildCandidateStarted  ActivityKind = "build.candidate_started"
	ActivityBuildCandidateReady    ActivityKind = "build.candidate_ready"
	ActivityBuildProtocolValidated ActivityKind = "build.protocol_validated"
	ActivityBuildCandidateFailed   ActivityKind = "build.candidate_failed"
	ActivityBuildCompleted         ActivityKind = "build.completed"
)

type ActivityEvent struct {
	Sequence   uint64
	Generation *uint64
	Role       ActivityRole
	Kind       ActivityKind
	Data       map[string]any
	ThreadID   string
	TurnID     string
	ItemID     string
}

type RenderableActivity struct {
	Kind   ActivityKind
	Data   map[string]any
	ItemID string
}

// ActivityStream is a bounded ordered queue. Producers never invoke consumers.
type ActivityStream struct {
	mu       sync.Mutex
	sequence uint64
	events   chan ActivityEvent
}

func NewActivityStream() *ActivityStream {
	return &ActivityStream{events: make(chan ActivityEvent, ActivityQueueCapacity)}
}

func (s *ActivityStream) Publish(generation *uint64, role ActivityRole, kind ActivityKind, data map[string]any, threadID, turnID, itemID string) (ActivityEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) == cap(s.events) {
		return ActivityEvent{}, ErrActivityQueueFull
	}
	s.sequence++
	event := ActivityEvent{
		Sequence:   s.sequence,
		Generation: cloneGeneration(generation),
		Role:       role,
		Kind:       kind,
		Data:       cloneActivityData(data),
		ThreadID:   threadID,
		TurnID:     turnID,
		ItemID:     itemID,
	}
	s.events <- event
	return cloneActivityEvent(event), nil
}

func (s *ActivityStream) Next(ctx context.Context) (ActivityEvent, error) {
	select {
	case event := <-s.events:
		return cloneActivityEvent(event), nil
	case <-ctx.Done():
		return ActivityEvent{}, ctx.Err()
	}
}

func (s *ActivityStream) Drain() []ActivityEvent {
	events := make([]ActivityEvent, 0, len(s.events))
	for {
		select {
		case event := <-s.events:
			events = append(events, cloneActivityEvent(event))
		default:
			return events
		}
	}
}

// PublishActivity deliberately ignores observer failure.
func PublishActivity(stream *ActivityStream, generation *uint64, role ActivityRole, kind ActivityKind, data map[string]any, threadID, turnID, itemID string) {
	if stream == nil {
		return
	}
	_, _ = stream.Publish(generation, role, kind, data, threadID, turnID, itemID)
}

// PublishRenderableCodexNotification filters app-server notifications at the
// privacy boundary before placing them on the activity stream.
func PublishRenderableCodexNotification(stream *ActivityStream, generation *uint64, role ActivityRole, message map[string]any, threadID, turnID, turnPhase string) []RenderableActivity {
	activities := RenderableCodexNotification(message, threadID, turnID)
	for _, activity := range activities {
		data := cloneActivityData(activity.Data)
		if turnPhase != "" {
			data["turn_phase"] = turnPhase
		}
		PublishActivity(stream, generation, role, activity.Kind, data, threadID, turnID, activity.ItemID)
	}
	return activities
}

// RenderableCodexNotification returns only text explicitly exposed by the
// app-server protocol. Raw reasoning notifications are ignored.
func RenderableCodexNotification(message map[string]any, threadID, turnID string) []RenderableActivity {
	params, ok := message["params"].(map[string]any)
	if !ok || params["threadId"] != threadID || params["turnId"] != turnID {
		return nil
	}
	itemID := ""
	if value, exists := params["itemId"]; exists {
		var ok bool
		itemID, ok = value.(string)
		if !ok {
			return nil
		}
	}
	method, _ := message["method"].(string)
	switch method {
	case "item/agentMessage/delta":
		delta, ok := params["delta"].(string)
		if !ok {
			return nil
		}
		return []RenderableActivity{{Kind: ActivityAgentTextDelta, Data: map[string]any{"text": delta}, ItemID: itemID}}
	case "item/reasoning/summaryTextDelta":
		delta, textOK := params["delta"].(string)
		index, indexOK := activityInteger(params["summaryIndex"])
		if !textOK || !indexOK {
			return nil
		}
		return []RenderableActivity{{Kind: ActivityAgentReasoningDelta, Data: map[string]any{"text": delta, "summary_index": index}, ItemID: itemID}}
	case "item/completed":
		return completedRenderableActivity(params, itemID)
	default:
		return nil
	}
}

func completedRenderableActivity(params map[string]any, itemID string) []RenderableActivity {
	item, ok := params["item"].(map[string]any)
	if !ok {
		return nil
	}
	if completedID, ok := item["id"].(string); ok {
		itemID = completedID
	}
	switch item["type"] {
	case "agentMessage":
		text, ok := item["text"].(string)
		if !ok {
			return nil
		}
		data := map[string]any{"text": text}
		if phase, ok := item["phase"].(string); ok {
			data["phase"] = phase
		}
		return []RenderableActivity{{Kind: ActivityAgentMessage, Data: data, ItemID: itemID}}
	case "reasoning":
		summary, ok := activityStringSlice(item["summary"])
		if !ok {
			return nil
		}
		return []RenderableActivity{{Kind: ActivityAgentReasoningSummary, Data: map[string]any{"summary": summary}, ItemID: itemID}}
	default:
		return nil
	}
}

func activityInteger(value any) (int, bool) {
	switch value := value.(type) {
	case bool:
		if value {
			return 1, true
		}
		return 0, true
	case int:
		return value, true
	case int64:
		if int64(int(value)) == value {
			return int(value), true
		}
	case json.Number:
		parsed, err := value.Int64()
		if err == nil && int64(int(parsed)) == parsed {
			return int(parsed), true
		}
	}
	return 0, false
}

func activityStringSlice(value any) ([]string, bool) {
	switch value := value.(type) {
	case []string:
		return append([]string(nil), value...), true
	case []any:
		result := make([]string, 0, len(value))
		for _, item := range value {
			text, ok := item.(string)
			if !ok {
				return nil, false
			}
			result = append(result, text)
		}
		return result, true
	default:
		return nil, false
	}
}

func cloneActivityEvent(event ActivityEvent) ActivityEvent {
	event.Generation = cloneGeneration(event.Generation)
	event.Data = cloneActivityData(event.Data)
	return event
}

func cloneGeneration(generation *uint64) *uint64 {
	if generation == nil {
		return nil
	}
	value := *generation
	return &value
}

func cloneActivityData(data map[string]any) map[string]any {
	result := make(map[string]any, len(data))
	for key, value := range data {
		result[key] = cloneActivityValue(value)
	}
	return result
}

func cloneActivityValue(value any) any {
	switch value := value.(type) {
	case []byte:
		return append([]byte(nil), value...)
	case []string:
		return append([]string(nil), value...)
	case []any:
		result := make([]any, len(value))
		for index, item := range value {
			result[index] = cloneActivityValue(item)
		}
		return result
	case map[string]any:
		return cloneActivityData(value)
	default:
		return value
	}
}
