package codexapp

import (
	"encoding/json"
	"strconv"
)

type Error struct {
	Reason string
	Err    error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

type CumulativeTokenUsage struct {
	InputTokens           uint64
	CachedInputTokens     uint64
	OutputTokens          uint64
	ReasoningOutputTokens uint64
}

func (u CumulativeTokenUsage) UncachedInputTokens() uint64 {
	if u.CachedInputTokens > u.InputTokens {
		return 0
	}
	return u.InputTokens - u.CachedInputTokens
}

func (u CumulativeTokenUsage) DeltaFrom(previous CumulativeTokenUsage) (TokenUsageDelta, error) {
	if u.InputTokens < previous.InputTokens ||
		u.CachedInputTokens < previous.CachedInputTokens ||
		u.OutputTokens < previous.OutputTokens ||
		u.ReasoningOutputTokens < previous.ReasoningOutputTokens {
		return TokenUsageDelta{}, &Error{Reason: "token usage cumulative total decreased"}
	}
	if u.CachedInputTokens > u.InputTokens || previous.CachedInputTokens > previous.InputTokens ||
		u.UncachedInputTokens() < previous.UncachedInputTokens() {
		return TokenUsageDelta{}, &Error{Reason: "token usage cumulative uncached input decreased"}
	}
	return TokenUsageDelta{
		InputTokens:           u.InputTokens - previous.InputTokens,
		CachedInputTokens:     u.CachedInputTokens - previous.CachedInputTokens,
		UncachedInputTokens:   u.UncachedInputTokens() - previous.UncachedInputTokens(),
		OutputTokens:          u.OutputTokens - previous.OutputTokens,
		ReasoningOutputTokens: u.ReasoningOutputTokens - previous.ReasoningOutputTokens,
	}, nil
}

type TokenUsageDelta struct {
	InputTokens           uint64
	CachedInputTokens     uint64
	UncachedInputTokens   uint64
	OutputTokens          uint64
	ReasoningOutputTokens uint64
}

func (d TokenUsageDelta) IsZero() bool {
	return d.InputTokens == 0 && d.CachedInputTokens == 0 &&
		d.UncachedInputTokens == 0 && d.OutputTokens == 0 &&
		d.ReasoningOutputTokens == 0
}

func TokenUsageDeltaFromNotification(params any, threadID, turnID string, previous CumulativeTokenUsage) (CumulativeTokenUsage, TokenUsageDelta, error) {
	values, ok := params.(map[string]any)
	if !ok {
		return CumulativeTokenUsage{}, TokenUsageDelta{}, &Error{Reason: "thread/tokenUsage/updated notification is not an object"}
	}
	if values["threadId"] != threadID || values["turnId"] != turnID {
		return CumulativeTokenUsage{}, TokenUsageDelta{}, &Error{Reason: "thread/tokenUsage/updated has the wrong thread or turn ID"}
	}
	tokenUsage, ok := values["tokenUsage"].(map[string]any)
	if !ok {
		return CumulativeTokenUsage{}, TokenUsageDelta{}, &Error{Reason: "token usage is not an object"}
	}
	total, ok := tokenUsage["total"].(map[string]any)
	if !ok {
		return CumulativeTokenUsage{}, TokenUsageDelta{}, &Error{Reason: "total token usage is not an object"}
	}
	input, inputOK := tokenCount(total["inputTokens"])
	cached, cachedOK := tokenCount(total["cachedInputTokens"])
	output, outputOK := tokenCount(total["outputTokens"])
	reasoning, reasoningOK := tokenCount(total["reasoningOutputTokens"])
	if !inputOK || !cachedOK || !outputOK || !reasoningOK {
		return CumulativeTokenUsage{}, TokenUsageDelta{}, &Error{Reason: "token usage has invalid counts"}
	}
	current := CumulativeTokenUsage{
		InputTokens: input, CachedInputTokens: cached,
		OutputTokens: output, ReasoningOutputTokens: reasoning,
	}
	if current.CachedInputTokens > current.InputTokens {
		return CumulativeTokenUsage{}, TokenUsageDelta{}, &Error{Reason: "token usage cached input exceeds total input"}
	}
	if current.ReasoningOutputTokens > current.OutputTokens {
		return CumulativeTokenUsage{}, TokenUsageDelta{}, &Error{Reason: "token usage reasoning output exceeds total output"}
	}
	delta, err := current.DeltaFrom(previous)
	if err != nil {
		return CumulativeTokenUsage{}, TokenUsageDelta{}, err
	}
	return current, delta, nil
}

func tokenCount(value any) (uint64, bool) {
	switch number := value.(type) {
	case json.Number:
		encoded := number.String()
		if encoded == "" || encoded[0] == '-' {
			return 0, false
		}
		parsed, err := strconv.ParseUint(encoded, 10, 64)
		return parsed, err == nil
	case uint64:
		return number, true
	case uint:
		return uint64(number), true
	case int:
		if number < 0 {
			return 0, false
		}
		return uint64(number), true
	default:
		return 0, false
	}
}
