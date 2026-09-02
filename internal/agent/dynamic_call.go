package agent

import (
	"encoding/json"
	"sync"
)

// dynamicCallRouting identifies one app-server initiated dynamic tool call.
//
// The request ID identifies the JSON-RPC request whose response is written by
// the harness.  callId identifies the app-server item.  The remaining fields
// pin that item to the session thread, turn, phase, and tool that admitted it.
// A response write is only an attempt; the matching item/completed notification
// is what makes the result delivered.
type dynamicCallRouting struct {
	requestID any
	callID    string
	threadID  string
	turnID    string
	turnPhase string
	tool      string

	resultReady            bool
	responseWriteAttempted bool
	terminalOutcome        string
	turnTerminalRecorded   bool
	handlerFinished        bool
	handlerReserved        bool
	handlerStarted         bool
}

type dynamicCallKey struct {
	threadID string
	turnID   string
	callID   string
}

// dynamicCallRouter owns turn-scoped dynamic-call evidence. It deliberately
// has no dependency on a particular Codex session or activity/event sink so
// implementor and reviewer sessions can use the same routing state machine.
// The recorder is called outside the router lock.
type dynamicCallRouter struct {
	mu     sync.Mutex
	calls  map[dynamicCallKey]*dynamicCallRouting
	order  []*dynamicCallRouting
	record func(string, map[string]any)
}

func newDynamicCallRouter(record func(string, map[string]any)) *dynamicCallRouter {
	return &dynamicCallRouter{
		calls:  make(map[dynamicCallKey]*dynamicCallRouting),
		record: record,
	}
}

// ensure admits a valid item/tool request only when it belongs to the current
// thread, turn, and phase. Once admitted, a route remains addressable by its
// complete thread/turn/call identity even if the caller's current session
// state has since advanced or ended.
func (r *dynamicCallRouter) ensure(message map[string]any, threadID, turnID, turnPhase string) *dynamicCallRouting {
	if r == nil {
		return nil
	}
	params, ok := message["params"].(map[string]any)
	if !ok {
		return nil
	}
	requestID, ok := dynamicCallRequestID(message["id"])
	if !ok {
		return nil
	}
	callID, callOK := params["callId"].(string)
	requestThreadID, threadOK := params["threadId"].(string)
	requestTurnID, turnOK := params["turnId"].(string)
	tool, toolOK := params["tool"].(string)
	if !callOK || callID == "" || !threadOK || requestThreadID == "" ||
		!turnOK || requestTurnID == "" || !toolOK || tool == "" {
		return nil
	}
	key := dynamicCallKey{threadID: requestThreadID, turnID: requestTurnID, callID: callID}

	r.mu.Lock()
	defer r.mu.Unlock()
	if existing := r.calls[key]; existing != nil {
		return existing
	}
	if requestThreadID != threadID || requestTurnID != turnID || turnPhase == "" {
		return nil
	}
	routing := &dynamicCallRouting{
		requestID: requestID,
		callID:    callID,
		threadID:  requestThreadID,
		turnID:    requestTurnID,
		turnPhase: turnPhase,
		tool:      tool,
	}
	r.calls[key] = routing
	r.order = append(r.order, routing)
	return routing
}

// identity returns the stable, privacy-safe identity fields for event data.
func (r *dynamicCallRouter) identity(routing *dynamicCallRouting) map[string]any {
	if routing == nil {
		return nil
	}
	return map[string]any{
		"request_id": routing.requestID,
		"call_id":    routing.callID,
		"thread_id":  routing.threadID,
		"turn_id":    routing.turnID,
		"turn_phase": routing.turnPhase,
		"tool":       routing.tool,
	}
}

func (r *dynamicCallRouter) recordEvent(event string, data map[string]any) {
	if r == nil || r.record == nil {
		return
	}
	r.record(event, data)
}

// markResultReady records that the tool handler has computed a response for
// the call. It is idempotent because a handler may retry its bookkeeping.
func (r *dynamicCallRouter) markResultReady(routing *dynamicCallRouting) {
	if r == nil || routing == nil {
		return
	}
	r.mu.Lock()
	if routing.resultReady {
		r.mu.Unlock()
		return
	}
	routing.resultReady = true
	identity := r.identity(routing)
	r.mu.Unlock()
	r.recordEvent("tool_result_ready", identity)
}

// markResponseWriteAttempted records that the harness attempted to write the
// app-server response. A successful write still needs matching item/completed
// evidence before this call can be considered delivered.
func (r *dynamicCallRouter) markResponseWriteAttempted(routing *dynamicCallRouting) {
	if r == nil || routing == nil {
		return
	}
	r.mu.Lock()
	if routing.responseWriteAttempted {
		r.mu.Unlock()
		return
	}
	routing.responseWriteAttempted = true
	identity := r.identity(routing)
	r.mu.Unlock()
	r.recordEvent("tool_response_write_attempted", identity)
}

// finish transitions a call to one of its single terminal outcomes. It returns
// false when the route is nil, already terminal, or the outcome is invalid.
func (r *dynamicCallRouter) finish(routing *dynamicCallRouting, outcome, itemStatus string) bool {
	if r == nil || routing == nil {
		return false
	}
	switch outcome {
	case "delivered", "rejected", "orphaned":
	default:
		return false
	}
	r.mu.Lock()
	if routing.terminalOutcome != "" {
		r.mu.Unlock()
		return false
	}
	routing.terminalOutcome = outcome
	data := r.identity(routing)
	if itemStatus != "" {
		data["item_status"] = itemStatus
	}
	r.mu.Unlock()
	r.recordEvent("tool_result_"+outcome, data)
	return true
}

func (r *dynamicCallRouter) mayWrite(routing *dynamicCallRouting, threadID, turnID, turnPhase string, interrupted bool) bool {
	if r == nil || routing == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if routing.terminalOutcome != "" {
		return false
	}
	return interrupted || (routing.threadID == threadID && routing.turnID == turnID && routing.turnPhase == turnPhase)
}

// reserveHandler returns true exactly once for an admitted call. Reviewer
// sessions use it at queue observation time so shutdown also joins a request
// whose callback has not started running yet.
func (r *dynamicCallRouter) reserveHandler(routing *dynamicCallRouting) bool {
	if r == nil || routing == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if routing.handlerReserved {
		return false
	}
	routing.handlerReserved = true
	return true
}

func (r *dynamicCallRouter) startHandler(routing *dynamicCallRouting) bool {
	if r == nil || routing == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !routing.handlerReserved || routing.handlerStarted || routing.handlerFinished {
		return false
	}
	routing.handlerStarted = true
	return true
}

// releaseUnstarted rejects and accounts for calls that were observed and
// reserved but whose callback did not begin before session cancellation.
func (r *dynamicCallRouter) releaseUnstarted(threadID, turnID string) int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	var rejected []*dynamicCallRouting
	var released []*dynamicCallRouting
	for _, routing := range r.order {
		if routing.threadID != threadID || routing.turnID != turnID || !routing.handlerReserved || routing.handlerStarted || routing.handlerFinished {
			continue
		}
		routing.handlerFinished = true
		released = append(released, routing)
		if routing.terminalOutcome == "" {
			routing.terminalOutcome = "rejected"
			rejected = append(rejected, routing)
		}
	}
	r.mu.Unlock()
	for _, routing := range rejected {
		r.recordEvent("tool_result_rejected", r.identity(routing))
	}
	r.prune()
	return len(released)
}

// recordDelivery validates the matching item/completed evidence and marks the
// call delivered only when the result was ready, a response write was
// attempted, the item status is terminal, and the item tool identity matches.
// Invalid evidence is recorded and leaves the route unresolved for turn
// terminal handling.
func (r *dynamicCallRouter) recordDelivery(params, item map[string]any, threadID, turnID string) {
	if r == nil || params == nil || item == nil {
		return
	}
	if params["threadId"] != threadID || params["turnId"] != turnID {
		return
	}
	callID, ok := item["id"].(string)
	if !ok || callID == "" {
		return
	}
	r.mu.Lock()
	routing := r.calls[dynamicCallKey{threadID: threadID, turnID: turnID, callID: callID}]
	if routing == nil {
		r.mu.Unlock()
		return
	}
	status, statusOK := item["status"].(string)
	tool := item["tool"]
	var failures []string
	if !routing.resultReady {
		failures = append(failures, "result_not_ready")
	}
	if !routing.responseWriteAttempted {
		failures = append(failures, "response_not_attempted")
	}
	if !statusOK || (status != "completed" && status != "failed") {
		failures = append(failures, "invalid_status")
	}
	if tool != routing.tool {
		failures = append(failures, "tool_mismatch")
	}
	identity := r.identity(routing)
	r.mu.Unlock()
	if len(failures) != 0 {
		identity["validation_failures"] = failures
		r.recordEvent("tool_delivery_notification_rejected", identity)
		return
	}
	r.finish(routing, "delivered", status)
}

// finishHandler marks the server-request callback as returned. A route is
// retained until both its turn-terminal evidence and handler completion have
// been observed, allowing late callbacks to be classified before pruning.
func (r *dynamicCallRouter) finishHandler(routing *dynamicCallRouting) {
	if r == nil || routing == nil {
		return
	}
	r.mu.Lock()
	routing.handlerFinished = true
	r.mu.Unlock()
	r.prune()
}

// orphanUnresolved marks unresolved calls for a terminal turn as orphaned,
// except interrupted turns, whose pending calls remain eligible for the
// interrupt/quiescence path. It records one turn summary and returns all
// pending routes (including interrupted ones) to the caller.
func (r *dynamicCallRouter) orphanUnresolved(threadID, turnID, turnPhase, turnStatus string) []*dynamicCallRouting {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	var calls []*dynamicCallRouting
	for _, routing := range r.order {
		if routing.threadID == threadID && routing.turnID == turnID {
			calls = append(calls, routing)
		}
	}
	pending := make([]*dynamicCallRouting, 0, len(calls))
	orphaned := make([]*dynamicCallRouting, 0, len(calls))
	for _, routing := range calls {
		if routing.terminalOutcome != "" {
			continue
		}
		pending = append(pending, routing)
		if turnStatus != "interrupted" {
			routing.terminalOutcome = "orphaned"
			orphaned = append(orphaned, routing)
		}
	}
	for _, routing := range calls {
		routing.turnTerminalRecorded = true
	}
	data := map[string]any{
		"thread_id":                  threadID,
		"turn_id":                    turnID,
		"turn_phase":                 turnPhase,
		"turn_status":                turnStatus,
		"dynamic_call_count":         len(calls),
		"pending_dynamic_call_count": len(pending),
		"pending_dynamic_call_ids":   dynamicCallIDs(pending),
	}
	r.mu.Unlock()
	r.recordEvent("turn_dynamic_calls_terminal", data)
	for _, routing := range orphaned {
		r.recordEvent("tool_result_orphaned", r.identity(routing))
	}
	r.prune()
	return pending
}

func (r *dynamicCallRouter) prune() {
	if r == nil {
		return
	}
	r.mu.Lock()
	remaining := r.order[:0]
	for key, routing := range r.calls {
		if routing.terminalOutcome != "" && routing.turnTerminalRecorded && routing.handlerFinished {
			delete(r.calls, key)
		}
	}
	for _, routing := range r.order {
		if routing.terminalOutcome != "" && routing.turnTerminalRecorded && routing.handlerFinished {
			continue
		}
		remaining = append(remaining, routing)
	}
	r.order = remaining
	r.mu.Unlock()
}

func dynamicCallIDs(routes []*dynamicCallRouting) []string {
	ids := make([]string, 0, len(routes))
	for _, routing := range routes {
		ids = append(ids, routing.callID)
	}
	return ids
}

func dynamicCallRequestID(value any) (any, bool) {
	switch value := value.(type) {
	case string:
		return value, true
	case json.Number:
		if dynamicCallInteger(value.String()) {
			return value, true
		}
	case int:
		return value, true
	case int8:
		return value, true
	case int16:
		return value, true
	case int32:
		return value, true
	case int64:
		return value, true
	case uint:
		return value, true
	case uint8:
		return value, true
	case uint16:
		return value, true
	case uint32:
		return value, true
	case uint64:
		return value, true
	}
	return nil, false
}

func dynamicCallInteger(value string) bool {
	if value == "" {
		return false
	}
	if value[0] == '-' {
		value = value[1:]
	}
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
