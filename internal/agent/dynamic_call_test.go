package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

type dynamicCallTestEvent struct {
	name string
	data map[string]any
}

func newDynamicCallTestRouter() (*dynamicCallRouter, *[]dynamicCallTestEvent, *sync.Mutex) {
	var mu sync.Mutex
	events := make([]dynamicCallTestEvent, 0)
	router := newDynamicCallRouter(func(name string, data map[string]any) {
		mu.Lock()
		events = append(events, dynamicCallTestEvent{name: name, data: cloneDynamicCallMap(data)})
		mu.Unlock()
	})
	// The test only needs a stable event sink; the lock is captured by the
	// recorder and returned so event snapshots can be taken safely.
	return router, &events, &mu
}

func cloneDynamicCallMap(value map[string]any) map[string]any {
	clone := make(map[string]any, len(value))
	for key, item := range value {
		if nested, ok := item.([]string); ok {
			clone[key] = append([]string(nil), nested...)
			continue
		}
		clone[key] = item
	}
	return clone
}

func dynamicCallTestMessage(requestID any, callID, threadID, turnID, tool string) map[string]any {
	return map[string]any{
		"id":     requestID,
		"method": "item/tool/call",
		"params": map[string]any{
			"callId": callID, "threadId": threadID, "turnId": turnID,
			"tool": tool,
		},
	}
}

func dynamicCallTestParams(threadID, turnID string) map[string]any {
	return map[string]any{"threadId": threadID, "turnId": turnID}
}

func dynamicCallTestItem(callID, tool, status string) map[string]any {
	return map[string]any{"id": callID, "tool": tool, "status": status}
}

func dynamicCallTestEvents(mu *sync.Mutex, events *[]dynamicCallTestEvent) []dynamicCallTestEvent {
	mu.Lock()
	defer mu.Unlock()
	return append([]dynamicCallTestEvent(nil), (*events)...)
}

func TestDynamicCallRouterAdmitsCurrentIdentityAndPreservesRequestID(t *testing.T) {
	router, events, eventsMu := newDynamicCallTestRouter()
	route := router.ensure(dynamicCallTestMessage(json.Number("17"), "call-1", "thread-1", "turn-1", "read"), "thread-1", "turn-1", "planning")
	if route == nil {
		t.Fatal("current dynamic call was not admitted")
	}
	if route.requestID != json.Number("17") || route.callID != "call-1" || route.threadID != "thread-1" || route.turnID != "turn-1" || route.turnPhase != "planning" || route.tool != "read" {
		t.Fatalf("route identity = %#v", route)
	}
	if duplicate := router.ensure(dynamicCallTestMessage("different-request", "call-1", "thread-1", "turn-1", "read"), "thread-1", "turn-1", "planning"); duplicate != route {
		t.Fatal("same call identity did not return the original route")
	}
	if stale := router.ensure(dynamicCallTestMessage("server-2", "stale", "other-thread", "turn-1", "read"), "thread-1", "turn-1", "planning"); stale != nil {
		t.Fatalf("stale thread was admitted: %#v", stale)
	}
	if stale := router.ensure(dynamicCallTestMessage("server-3", "stale-turn", "thread-1", "other-turn", "read"), "thread-1", "turn-1", "planning"); stale != nil {
		t.Fatalf("stale turn was admitted: %#v", stale)
	}
	if invalid := router.ensure(dynamicCallTestMessage(true, "bad-request-id", "thread-1", "turn-1", "read"), "thread-1", "turn-1", "planning"); invalid != nil {
		t.Fatalf("boolean request ID was admitted: %#v", invalid)
	}
	identity := router.identity(route)
	want := map[string]any{
		"request_id": json.Number("17"), "call_id": "call-1", "thread_id": "thread-1",
		"turn_id": "turn-1", "turn_phase": "planning", "tool": "read",
	}
	if !reflect.DeepEqual(identity, want) {
		t.Fatalf("identity = %#v, want %#v", identity, want)
	}
	if got := dynamicCallTestEvents(eventsMu, events); len(got) != 0 {
		t.Fatalf("unexpected routing events = %#v", got)
	}
}

func TestDynamicCallRouterRequiresMatchingDeliveryEvidence(t *testing.T) {
	router, events, eventsMu := newDynamicCallTestRouter()
	route := router.ensure(dynamicCallTestMessage("server-1", "call-1", "thread-1", "turn-1", "review"), "thread-1", "turn-1", "planning")
	if route == nil {
		t.Fatal("call was not admitted")
	}
	router.recordDelivery(dynamicCallTestParams("thread-1", "turn-1"), dynamicCallTestItem("call-1", "list", "inProgress"), "thread-1", "turn-1")
	eventsSnapshot := dynamicCallTestEvents(eventsMu, events)
	if len(eventsSnapshot) != 1 || eventsSnapshot[0].name != "tool_delivery_notification_rejected" {
		t.Fatalf("invalid delivery events = %#v", eventsSnapshot)
	}
	if got, want := eventsSnapshot[0].data["validation_failures"], []string{"result_not_ready", "response_not_attempted", "invalid_status", "tool_mismatch"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("validation failures = %#v, want %#v", got, want)
	}
	if route.terminalOutcome != "" {
		t.Fatalf("invalid notification became terminal: %#v", route)
	}

	router.markResultReady(route)
	router.markResultReady(route)
	router.markResponseWriteAttempted(route)
	router.markResponseWriteAttempted(route)
	router.recordDelivery(dynamicCallTestParams("thread-1", "turn-1"), dynamicCallTestItem("call-1", "review", "failed"), "thread-1", "turn-1")
	if route.terminalOutcome != "delivered" {
		t.Fatalf("matching terminal notification did not deliver route: %#v", route)
	}
	eventsSnapshot = dynamicCallTestEvents(eventsMu, events)
	wantNames := []string{"tool_delivery_notification_rejected", "tool_result_ready", "tool_response_write_attempted", "tool_result_delivered"}
	gotNames := make([]string, 0, len(eventsSnapshot))
	for _, event := range eventsSnapshot {
		gotNames = append(gotNames, event.name)
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("event names = %#v, want %#v", gotNames, wantNames)
	}
	if got := eventsSnapshot[len(eventsSnapshot)-1].data["item_status"]; got != "failed" {
		t.Fatalf("delivery item status = %#v", got)
	}
	if router.finish(route, "orphaned", "") {
		t.Fatal("terminal delivery was overwritten by orphaning")
	}
}

func TestDynamicCallRouterWriteAdmissionTracksTerminalAndTurnIdentity(t *testing.T) {
	router, _, _ := newDynamicCallTestRouter()
	route := router.ensure(dynamicCallTestMessage("server-1", "call-1", "thread-1", "turn-1", "review"), "thread-1", "turn-1", "planning")
	if route == nil {
		t.Fatal("call was not admitted")
	}
	if !router.mayWrite(route, "thread-1", "turn-1", "planning", false) {
		t.Fatal("current, unresolved call was not write-admissible")
	}
	if router.mayWrite(route, "thread-2", "turn-1", "planning", false) {
		t.Fatal("call was write-admissible for a stale thread")
	}
	if router.mayWrite(route, "thread-1", "turn-2", "planning", false) {
		t.Fatal("call was write-admissible for a stale turn")
	}
	if router.mayWrite(route, "thread-1", "turn-1", "implementation", false) {
		t.Fatal("call was write-admissible for a stale phase")
	}
	if !router.mayWrite(route, "thread-2", "turn-2", "implementation", true) {
		t.Fatal("unresolved interrupted call was not write-admissible")
	}
	if !router.finish(route, "rejected", "") {
		t.Fatal("call did not become rejected")
	}
	if router.mayWrite(route, "thread-1", "turn-1", "planning", true) {
		t.Fatal("terminal call remained write-admissible")
	}
}

func TestDynamicCallRouterOrphansAtTurnTerminalAndPrunesAfterHandler(t *testing.T) {
	router, events, eventsMu := newDynamicCallTestRouter()
	first := router.ensure(dynamicCallTestMessage("server-a", "call-a", "thread-1", "turn-1", "review"), "thread-1", "turn-1", "planning")
	second := router.ensure(dynamicCallTestMessage("server-b", "call-b", "thread-1", "turn-1", "list"), "thread-1", "turn-1", "planning")
	if first == nil || second == nil {
		t.Fatal("calls were not admitted")
	}
	router.markResultReady(first)
	pending := router.orphanUnresolved("thread-1", "turn-1", "planning", "completed")
	if len(pending) != 2 || pending[0] != first || pending[1] != second {
		t.Fatalf("pending calls = %#v", pending)
	}
	if first.terminalOutcome != "orphaned" || second.terminalOutcome != "orphaned" {
		t.Fatalf("orphaned outcomes = %q, %q", first.terminalOutcome, second.terminalOutcome)
	}
	if first.turnTerminalRecorded != true || second.turnTerminalRecorded != true {
		t.Fatalf("turn terminal flags = %v, %v", first.turnTerminalRecorded, second.turnTerminalRecorded)
	}
	if len(router.calls) != 2 {
		t.Fatalf("routes pruned before callbacks finished: %#v", router.calls)
	}
	router.finishHandler(first)
	if len(router.calls) != 1 {
		t.Fatalf("first route was not pruned after callback finish: %#v", router.calls)
	}
	router.finishHandler(second)
	if len(router.calls) != 0 {
		t.Fatalf("routes remain after terminal and handler completion: %#v", router.calls)
	}

	eventsSnapshot := dynamicCallTestEvents(eventsMu, events)
	terminal := eventByName(eventsSnapshot, "turn_dynamic_calls_terminal")
	if terminal == nil {
		t.Fatalf("missing turn terminal event: %#v", eventsSnapshot)
	}
	if got := terminal.data["pending_dynamic_call_ids"]; !reflect.DeepEqual(got, []string{"call-a", "call-b"}) {
		t.Fatalf("pending IDs = %#v", got)
	}
	if got := terminal.data["dynamic_call_count"]; got != 2 {
		t.Fatalf("dynamic call count = %#v", got)
	}
	if count := countDynamicCallEvents(eventsSnapshot, "tool_result_orphaned"); count != 2 {
		t.Fatalf("orphan event count = %d", count)
	}
}

func TestDynamicCallRouterInterruptedTurnKeepsPendingUntilQuiescence(t *testing.T) {
	router, events, eventsMu := newDynamicCallTestRouter()
	route := router.ensure(dynamicCallTestMessage("server-1", "call-1", "thread-1", "turn-1", "read"), "thread-1", "turn-1", "implementation")
	if route == nil {
		t.Fatal("call was not admitted")
	}
	pending := router.orphanUnresolved("thread-1", "turn-1", "implementation", "interrupted")
	if len(pending) != 1 || pending[0] != route || route.terminalOutcome != "" {
		t.Fatalf("interrupted pending route = %#v", pending)
	}
	if !route.turnTerminalRecorded {
		t.Fatal("interrupted route did not record turn terminal")
	}
	router.finishHandler(route)
	if len(router.calls) != 1 {
		t.Fatalf("unresolved interrupted route was pruned: %#v", router.calls)
	}
	if !router.finish(route, "rejected", "") {
		t.Fatal("quiesced interrupted route did not become rejected")
	}
	router.finishHandler(route)
	if len(router.calls) != 0 {
		t.Fatalf("rejected interrupted route was not pruned: %#v", router.calls)
	}
	if countDynamicCallEvents(dynamicCallTestEvents(eventsMu, events), "tool_result_orphaned") != 0 {
		t.Fatal("interrupted pending route was incorrectly orphaned")
	}
}

func TestDynamicCallRouterReleasesObservedCallbackThatNeverStarts(t *testing.T) {
	router, events, eventsMu := newDynamicCallTestRouter()
	route := router.ensure(dynamicCallTestMessage("server-1", "call-1", "thread-1", "turn-1", "read"), "thread-1", "turn-1", "review")
	if route == nil || !router.reserveHandler(route) {
		t.Fatal("review callback was not reserved")
	}
	router.orphanUnresolved("thread-1", "turn-1", "review", "completed")
	if released := router.releaseUnstarted("thread-1", "turn-1"); released != 1 {
		t.Fatalf("released callbacks = %d, want 1", released)
	}
	if released := router.releaseUnstarted("thread-1", "turn-1"); released != 0 {
		t.Fatalf("callback was released twice: %d", released)
	}
	if router.startHandler(route) {
		t.Fatal("released callback was allowed to start")
	}
	if len(router.calls) != 0 {
		t.Fatalf("released route was not pruned: %#v", router.calls)
	}
	if countDynamicCallEvents(dynamicCallTestEvents(eventsMu, events), "tool_result_orphaned") != 1 {
		t.Fatal("released orphan did not retain its terminal evidence")
	}
}

func TestDynamicCallRouterYieldIsTerminalAndLateDuplicateCannotReopenTurn(t *testing.T) {
	router, events, eventsMu := newDynamicCallTestRouter()
	message := dynamicCallTestMessage("server-1", "call-1", "thread-1", "turn-1", "review")
	route := router.ensure(message, "thread-1", "turn-1", "planning")
	if route == nil || !router.reserveHandler(route) || !router.startHandler(route) {
		t.Fatal("review route was not admitted")
	}
	if !router.finish(route, "yielded", "") {
		t.Fatal("review route did not become yielded")
	}
	if pending := router.orphanUnresolved("thread-1", "turn-1", "planning", "completed"); len(pending) != 0 {
		t.Fatalf("yielded route became pending: %#v", pending)
	}
	router.finishHandler(route)
	if len(router.calls) != 0 {
		t.Fatalf("yielded route was not pruned: %#v", router.calls)
	}
	if duplicate := router.ensure(message, "thread-1", "turn-1", "planning"); duplicate != nil {
		t.Fatalf("late duplicate reopened a terminal turn: %#v", duplicate)
	}
	eventsSnapshot := dynamicCallTestEvents(eventsMu, events)
	if countDynamicCallEvents(eventsSnapshot, "tool_result_yielded") != 1 || countDynamicCallEvents(eventsSnapshot, "tool_result_orphaned") != 0 {
		t.Fatalf("yield terminal events = %#v", eventsSnapshot)
	}
}

func TestDynamicCallRouterBoundsTerminalTurnTombstones(t *testing.T) {
	router, _, _ := newDynamicCallTestRouter()
	for index := 0; index < maxTerminalDynamicTurns+20; index++ {
		turnID := fmt.Sprintf("turn-%d", index)
		router.orphanUnresolved("thread-1", turnID, "continuation", "completed")
	}
	if len(router.terminalTurns) != maxTerminalDynamicTurns || len(router.terminalOrder) != maxTerminalDynamicTurns {
		t.Fatalf("terminal tombstones = %d/%d", len(router.terminalTurns), len(router.terminalOrder))
	}
}

func eventByName(events []dynamicCallTestEvent, name string) *dynamicCallTestEvent {
	for index := range events {
		if events[index].name == name {
			return &events[index]
		}
	}
	return nil
}

func countDynamicCallEvents(events []dynamicCallTestEvent, name string) int {
	count := 0
	for _, event := range events {
		if event.name == name {
			count++
		}
	}
	return count
}
