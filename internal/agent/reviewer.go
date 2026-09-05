// Package agent contains the trusted boundaries around Codex sessions.
package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"codexos/internal/codexapp"
	"codexos/internal/guest"
	"codexos/internal/observability"
	"codexos/internal/provenance"
)

const (
	// DefaultReviewerModel, DefaultReviewerReasoningEffort,
	// DefaultReviewerReasoningSummary, and DefaultReviewerServiceTier are the
	// serving settings in the trusted reviewer contract.
	DefaultReviewerModel            = "gpt-6-astra"
	DefaultReviewerReasoningEffort  = "low"
	DefaultReviewerReasoningSummary = "auto"
	DefaultReviewerServiceTier      = "priority"

	reviewerPermissionProfile = "codexos-reviewer"
	maxReviewRequestBytes     = 8 * 1024
)

var reviewFocuses = map[string]struct{}{
	"general": {}, "correctness": {}, "design": {}, "security": {},
	"performance": {},
}

// ReviewRuntime is the narrow boundary a running generation exposes to a
// reviewer.  In particular, it exposes guest tools and trusted observability,
// not a filesystem or a general command runner.
//
// EventLog, Metrics, and ForensicProvenance may return nil.  Review capture
// and telemetry are deliberately optional and never control the review result.
type ReviewRuntime interface {
	ReviewRunning() bool
	GenerationNumber() (uint64, bool)
	HarnessIdentity() *provenance.HarnessIdentity
	InvokeTool(context.Context, string, [][]byte) (guest.ToolResult, error)
	EventLog() *observability.EventLog
	Metrics() *observability.Metrics
	ForensicProvenance() *provenance.BuildReviewProvenance
}

// ReviewWorkerOptions configures the process owner for one or more fresh
// reviewer consultations. A worker never reuses a Codex app-server process.
type ReviewWorkerOptions struct {
	Executable     string
	AuthFile       string
	ActivityStream *observability.ActivityStream
	StopTimeout    time.Duration
}

// ReviewOptions contains one review request. Empty serving fields use the
// contract defaults. Objective and Request are pointers so an absent value is
// distinguishable from an explicitly supplied empty string.
type ReviewOptions struct {
	Objective        *string
	Focus            string
	Request          *string
	Proposal         *string
	SourceSnapshot   provenance.FileIdentity
	Model            string
	ReasoningEffort  string
	ReasoningSummary string
	ServiceTier      string
	Origin           map[string]any
	Evidence         *provenance.ReviewEvidence
}

// ReviewWorkerError reports a failed isolated reviewer consultation.
type ReviewWorkerError struct {
	Reason string
	Err    error
}

func (e *ReviewWorkerError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err == nil {
		return e.Reason
	}
	if e.Reason == "" {
		return e.Err.Error()
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *ReviewWorkerError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// ReviewWorker owns the lifetime of one currently running reviewer server.
// The worker itself can be used for sequential consultations; every call to
// RunReview creates a new process, workspace, and ephemeral thread.
type ReviewWorker struct {
	options ReviewWorkerOptions

	mu        sync.Mutex
	server    *codexapp.CodexAppServer
	cancel    context.CancelFunc
	cancelled bool
	running   bool
}

// NewReviewWorker constructs a reviewer process owner. It does not start a
// Codex process.
func NewReviewWorker(options ReviewWorkerOptions) *ReviewWorker {
	return &ReviewWorker{options: options}
}

// Cancel terminates the currently active reviewer, if any. It is safe to call
// before RunReview has installed its process or after it has completed.
func (w *ReviewWorker) Cancel() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.cancelled = true
	cancel := w.cancel
	server := w.server
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if server != nil {
		_ = server.Close()
	}
}

// RunReview performs one fresh, read-only reviewer turn and returns the final
// agent message. Reviewer failures are returned to the caller and never alter
// the generation state.
func (w *ReviewWorker) RunReview(ctx context.Context, runtime ReviewRuntime, options ReviewOptions) (string, error) {
	if w == nil {
		return "", &ReviewWorkerError{Reason: "review worker is nil"}
	}
	if ctx == nil {
		return "", &ReviewWorkerError{Reason: "review context is nil"}
	}
	if runtime == nil {
		return "", &ReviewWorkerError{Reason: "review runtime is nil"}
	}
	if !runtime.ReviewRunning() {
		return "", &ReviewWorkerError{Reason: "CodexOS generation is not running"}
	}

	options, err := normalizeReviewOptions(options)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", &ReviewWorkerError{Reason: "Codex reviewer consultation was cancelled", Err: err}
	}
	w.mu.Lock()
	alreadyRunning := w.running
	alreadyCancelled := w.cancelled
	w.mu.Unlock()
	if alreadyRunning {
		return "", &ReviewWorkerError{Reason: "review worker already has an active consultation"}
	}
	if alreadyCancelled {
		return "", &ReviewWorkerError{Reason: "Codex reviewer consultation was cancelled"}
	}

	generation, hasGeneration := runtime.GenerationNumber()
	var generationPointer *uint64
	if hasGeneration {
		generationPointer = &generation
	}
	if options.Evidence != nil && (!hasGeneration || options.Evidence.Generation() != generation) {
		return "", &ReviewWorkerError{Reason: "review evidence belongs to another generation"}
	}

	evidence := options.Evidence
	if evidence == nil && hasGeneration {
		if store := runtime.ForensicProvenance(); store != nil {
			evidence, err = store.BeginReview(generation)
			if err != nil {
				w.degrade(runtime, "review forensic provenance was incomplete: "+err.Error())
				evidence = nil
			}
		}
	}

	startedAt := time.Now()
	outcome := "failed"
	serviceTierName := ""
	runContext, stop := context.WithCancel(ctx)
	router := newDynamicCallRouter(func(event string, data map[string]any) {
		w.record(runtime, event, generationPointer, data)
	})
	turnReady := make(chan struct{})
	var turnReadyOnce sync.Once
	var routeMu sync.Mutex
	var routeThreadID, routeTurnID, terminalStatus string
	setRoute := func(threadID, turnID string) {
		routeMu.Lock()
		routeThreadID, routeTurnID = threadID, turnID
		routeMu.Unlock()
	}
	setTerminalStatus := func(status string) {
		routeMu.Lock()
		terminalStatus = status
		routeMu.Unlock()
	}
	currentRoute := func() (string, string, string) {
		routeMu.Lock()
		defer routeMu.Unlock()
		return routeThreadID, routeTurnID, terminalStatus
	}
	var toolsMu sync.Mutex
	activeTools := 0
	toolIdle := closedChannel()
	beginTool := func() {
		toolsMu.Lock()
		activeTools++
		if activeTools == 1 {
			toolIdle = make(chan struct{})
		}
		toolsMu.Unlock()
	}
	finishTool := func() {
		toolsMu.Lock()
		activeTools--
		if activeTools == 0 {
			closeGenerationChannel(toolIdle)
		}
		toolsMu.Unlock()
	}
	currentToolIdle := func() <-chan struct{} {
		toolsMu.Lock()
		defer toolsMu.Unlock()
		return toolIdle
	}

	// A child context gives explicit cancellation and caller cancellation the
	// same bounded app-server shutdown path. The monitor is joined before this
	// method returns, so it cannot outlive the worker's process owner.
	server := codexapp.NewCodexAppServer(codexapp.CodexAppServerOptions{
		Executable:      w.options.Executable,
		AuthFile:        w.options.AuthFile,
		TemporaryPrefix: "codexos-reviewer-",
		ConfigText:      reviewerConfig,
		StopTimeout:     w.options.StopTimeout,
		ServerRequestObserver: func(message map[string]any) {
			if message["method"] != "item/tool/call" {
				return
			}
			select {
			case <-turnReady:
			case <-runContext.Done():
				return
			}
			threadID, turnID, _ := currentRoute()
			routing := router.ensure(message, threadID, turnID, "review")
			if router.reserveHandler(routing) {
				beginTool()
			}
			data := dynamicToolActivityData(message["params"])
			tool, ok := data["tool"].(string)
			if !ok {
				tool = "unknown"
			}
			queued := map[string]any{"tool": tool, "turn_phase": "review"}
			if routing != nil {
				queued = mergeMaps(queued, router.identity(routing))
			}
			w.record(runtime, "tool_app_server_queued", generationPointer, queued)
		},
	})
	if err := w.install(server, stop); err != nil {
		stop()
		return "", err
	}
	monitorStop := make(chan struct{})
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		select {
		case <-runContext.Done():
			_ = server.Close()
		case <-monitorStop:
		}
	}()
	defer func() {
		turnReadyOnce.Do(func() { close(turnReady) })
		stop()
		_ = server.Close()
		close(monitorStop)
		<-monitorDone
		w.uninstall(server)
	}()
	// Keep this defer after the process-cleanup defer above so the original
	// cancellation state is still observable while lifecycle events are
	// finalized. The cleanup defer then cancels the child context.
	defer func() {
		if (w.wasCancelled() || ctx.Err() != nil) && outcome != "completed" {
			outcome = "cancelled"
		}
		w.finish(runtime, generationPointer, evidence, outcome, options, serviceTierName, options.Focus, startedAt)
	}()

	if w.wasCancelled() {
		return "", &ReviewWorkerError{Reason: "Codex reviewer consultation was cancelled"}
	}
	if err := server.Start(runContext); err != nil {
		if w.isCancelled(runContext) {
			return "", &ReviewWorkerError{Reason: "Codex reviewer consultation was cancelled", Err: err}
		}
		return "", reviewError(err)
	}
	if w.wasCancelled() {
		return "", &ReviewWorkerError{Reason: "Codex reviewer consultation was cancelled"}
	}

	serviceTierName, err = server.ValidateModel(
		runContext,
		options.Model,
		options.ReasoningEffort,
		options.ServiceTier,
		options.ReasoningSummary,
	)
	if err != nil {
		if w.isCancelled(runContext) {
			return "", &ReviewWorkerError{Reason: "Codex reviewer consultation was cancelled", Err: err}
		}
		return "", reviewError(err)
	}
	startedData := map[string]any{
		"model":             options.Model,
		"reasoning_effort":  options.ReasoningEffort,
		"reasoning_summary": options.ReasoningSummary,
		"service_tier":      options.ServiceTier,
		"service_tier_name": serviceTierName,
		"focus":             options.Focus,
	}
	if id := evidenceID(evidence); id != "" {
		startedData["review_id"] = id
	}
	startedData = mergeMaps(startedData, options.Origin)

	threadID, err := server.StartThread(runContext, codexapp.StartThreadOptions{
		Model:             options.Model,
		Effort:            options.ReasoningEffort,
		ServiceTier:       options.ServiceTier,
		PermissionProfile: reviewerPermissionProfile,
		DynamicTools:      []map[string]any{reviewerToolNamespace()},
		RequireReadOnly:   true,
	})
	if err != nil {
		if w.isCancelled(runContext) {
			return "", &ReviewWorkerError{Reason: "Codex reviewer consultation was cancelled", Err: err}
		}
		return "", reviewError(err)
	}
	// Publish serving provenance only after the thread confirms the selection.
	w.record(runtime, "review_started", generationPointer, startedData)
	w.publish(generationPointer, observability.ActivityReviewer, observability.ActivityReviewStarted, map[string]any{
		"model":            options.Model,
		"reasoning_effort": options.ReasoningEffort,
		"service_tier":     options.ServiceTier,
		"focus":            options.Focus,
	}, "", "", "")

	var turnID string
	server.SetServerRequestHandler(func(message map[string]any) {
		select {
		case <-turnReady:
		case <-runContext.Done():
			_ = server.RejectServerRequest(message)
			return
		}
		if turnID == "" {
			_ = server.RejectServerRequest(message)
			return
		}
		routing := router.ensure(message, threadID, turnID, "review")
		if routing == nil {
			_ = server.RejectServerRequest(message)
			return
		}
		if router.reserveHandler(routing) {
			beginTool()
		}
		if !router.startHandler(routing) {
			_ = server.RejectServerRequest(message)
			return
		}
		defer finishTool()
		defer router.finishHandler(routing)
		w.handleServerRequest(runContext, server, runtime, message, threadID, turnID, evidence, router, routing, currentRoute)
	})
	turnStarted := false
	defer func() {
		if !turnStarted {
			turnReadyOnce.Do(func() { close(turnReady) })
		}
	}()
	turnID, err = server.StartTurn(runContext, codexapp.StartTurnOptions{
		ThreadID:          threadID,
		Prompt:            reviewerPrompt(options.Objective, options.Focus, options.Request, options.Proposal, options.SourceSnapshot),
		Model:             options.Model,
		Effort:            options.ReasoningEffort,
		ReasoningSummary:  options.ReasoningSummary,
		ServiceTier:       options.ServiceTier,
		PermissionProfile: reviewerPermissionProfile,
	})
	setRoute(threadID, turnID)
	if err != nil {
		if w.isCancelled(runContext) {
			return "", &ReviewWorkerError{Reason: "Codex reviewer consultation was cancelled", Err: err}
		}
		return "", reviewError(err)
	}
	if evidence != nil {
		if err := evidence.RecordReviewerTurn(threadID, turnID); err != nil {
			w.degrade(runtime, "review forensic provenance was incomplete: "+err.Error())
		}
	}
	turnReadyOnce.Do(func() { close(turnReady) })
	turnStarted = true
	w.publish(generationPointer, observability.ActivityReviewer, observability.ActivityTurnStarted, map[string]any{
		"focus": options.Focus,
	}, threadID, turnID, "")

	result, err := w.waitForTurn(runContext, runtime, server, generationPointer, threadID, turnID, options.Model, router, setTerminalStatus)
	if err != nil {
		stop()
	}
	for released := router.releaseUnstarted(threadID, turnID); released > 0; released-- {
		finishTool()
	}
	joinTimer := time.NewTimer(w.stopTimeout())
	select {
	case <-currentToolIdle():
	case <-joinTimer.C:
		err = errors.Join(err, &ReviewWorkerError{Reason: "reviewer dynamic tool call did not quiesce after the turn ended"})
	}
	joinTimer.Stop()
	if err != nil {
		if w.wasCancelled() || ctx.Err() != nil {
			return "", &ReviewWorkerError{Reason: "Codex reviewer consultation was cancelled", Err: err}
		}
		return "", err
	}
	outcome = "completed"
	return result, nil
}

func (w *ReviewWorker) install(server *codexapp.CodexAppServer, cancel context.CancelFunc) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.running {
		return &ReviewWorkerError{Reason: "review worker already has an active consultation"}
	}
	if w.cancelled {
		return &ReviewWorkerError{Reason: "Codex reviewer consultation was cancelled"}
	}
	w.running = true
	w.server = server
	w.cancel = cancel
	return nil
}

func (w *ReviewWorker) uninstall(server *codexapp.CodexAppServer) {
	w.mu.Lock()
	if w.server == server {
		w.server = nil
		w.cancel = nil
	}
	w.running = false
	w.mu.Unlock()
}

func (w *ReviewWorker) wasCancelled() bool {
	w.mu.Lock()
	cancelled := w.cancelled
	w.mu.Unlock()
	return cancelled
}

func (w *ReviewWorker) isCancelled(ctx context.Context) bool {
	return w.wasCancelled() || ctx.Err() != nil
}

func (w *ReviewWorker) stopTimeout() time.Duration {
	if w.options.StopTimeout > 0 {
		return w.options.StopTimeout
	}
	return 2 * time.Second
}

func (w *ReviewWorker) handleServerRequest(ctx context.Context, server *codexapp.CodexAppServer, runtime ReviewRuntime, message map[string]any, threadID, turnID string, evidence *provenance.ReviewEvidence, router *dynamicCallRouter, routing *dynamicCallRouting, currentRoute func() (string, string, string)) {
	if message["method"] != "item/tool/call" {
		_ = server.RejectServerRequest(message)
		return
	}
	response := w.dynamicToolResponse(ctx, runtime, message["params"], threadID, turnID, evidence)
	if routing != nil {
		router.markResultReady(routing)
		currentThread, currentTurn, terminalStatus := currentRoute()
		interrupted := terminalStatus == "interrupted"
		if !router.mayWrite(routing, currentThread, currentTurn, "review", interrupted) {
			router.finish(routing, "orphaned", "")
			return
		}
		router.markResponseWriteAttempted(routing)
	}
	if err := server.WriteResult(message["id"], response); err != nil {
		router.finish(routing, "rejected", "")
		return
	}
	if routing != nil {
		_, _, terminalStatus := currentRoute()
		if terminalStatus == "interrupted" {
			router.finish(routing, "rejected", "")
		}
	}
}

func (w *ReviewWorker) dynamicToolResponse(ctx context.Context, runtime ReviewRuntime, params any, threadID, turnID string, evidence *provenance.ReviewEvidence) map[string]any {
	activityData := dynamicToolActivityData(params)
	callID := activityCallID(params)
	// The generation pointer is intentionally obtained only for activity and
	// event plumbing; malformed tool calls must still produce a bounded result.
	var generationPointer *uint64
	if generation, ok := runtime.GenerationNumber(); ok {
		generationPointer = &generation
	}
	w.publish(generationPointer, observability.ActivityReviewer, observability.ActivityToolStarted, activityData, threadID, turnID, callID)
	tryResult := func() (guest.ToolResult, error) {
		values, err := codexapp.ObjectValue(params, "reviewer dynamic tool request")
		if err != nil {
			return guest.ToolResult{}, err
		}
		if err := validateToolCall(values, threadID, turnID); err != nil {
			return guest.ToolResult{}, err
		}
		if values["namespace"] != "codexos" {
			return guest.ToolResult{}, errors.New("unsupported reviewer dynamic tool namespace")
		}
		tool, ok := values["tool"].(string)
		if !ok {
			return guest.ToolResult{}, errors.New("reviewer dynamic tool name must be a string")
		}
		arguments, err := toolArguments(values["arguments"])
		if err != nil {
			return guest.ToolResult{}, err
		}
		return dispatchReadOnlyTool(ctx, runtime, tool, arguments, evidence, w)
	}
	result, err := tryResult()
	if err != nil {
		w.publish(generationPointer, observability.ActivityReviewer, observability.ActivityToolFailed, mergeActivity(activityData, map[string]any{
			"success": false,
			"error":   boundedError(err),
		}), threadID, turnID, callID)
		return map[string]any{
			"contentItems": []map[string]any{{"type": "inputText", "text": "Bridge error: " + boundedError(err)}},
			"success":      false,
		}
	}
	activityKind := observability.ActivityToolCompleted
	if result.Status != 0 {
		activityKind = observability.ActivityToolFailed
	}
	w.publish(generationPointer, observability.ActivityReviewer, activityKind, mergeActivity(activityData, map[string]any{
		"success": result.Status == 0,
		"result": map[string]any{
			"status": result.Status,
			"output": append([]byte(nil), result.Output...),
		},
	}), threadID, turnID, callID)
	formatted := formatToolResult(result)
	return map[string]any{
		"contentItems": []map[string]any{{"type": "inputText", "text": formatted}},
		"success":      true,
	}
}

func (w *ReviewWorker) waitForTurn(ctx context.Context, runtime ReviewRuntime, server *codexapp.CodexAppServer, generation *uint64, threadID, turnID, model string, router *dynamicCallRouter, setTerminalStatus func(string)) (string, error) {
	var lastAgentMessage string
	var haveLastAgentMessage bool
	var total codexapp.CumulativeTokenUsage
	for {
		message, err := server.NextNotification(ctx)
		if err != nil {
			return "", reviewError(err)
		}
		observability.PublishRenderableCodexNotification(runtimeActivity(runtime, w.options.ActivityStream), generation, observability.ActivityReviewer, message, threadID, turnID, "review")
		method := message["method"]
		params := message["params"]
		if method == "thread/tokenUsage/updated" {
			metrics := runtime.Metrics()
			if metrics == nil {
				continue
			}
			updated, delta, err := codexapp.TokenUsageDeltaFromNotification(params, threadID, turnID, total)
			if err != nil {
				w.degrade(runtime, "reviewer token usage telemetry was ignored: "+err.Error())
				continue
			}
			total = updated
			if !delta.IsZero() {
				metrics.RecordModelTokens(model, "reviewer", delta.InputTokens, delta.CachedInputTokens, delta.UncachedInputTokens, delta.OutputTokens, delta.ReasoningOutputTokens)
			}
			continue
		}
		if method == "item/completed" {
			if values, ok := params.(map[string]any); ok {
				if item, ok := values["item"].(map[string]any); ok {
					switch item["type"] {
					case "agentMessage":
						if text, ok := item["text"].(string); ok {
							lastAgentMessage = text
							haveLastAgentMessage = true
						}
					case "dynamicToolCall":
						router.recordDelivery(values, item, threadID, turnID)
					}
				}
			}
			continue
		}
		if method != "turn/completed" {
			continue
		}
		values, err := codexapp.ObjectValue(params, "turn/completed notification")
		if err != nil {
			return "", reviewError(err)
		}
		if values["threadId"] != threadID {
			return "", &ReviewWorkerError{Reason: "review turn/completed has the wrong thread ID"}
		}
		turn, err := codexapp.ObjectValue(values["turn"], "completed review turn")
		if err != nil {
			return "", reviewError(err)
		}
		if turn["id"] != turnID {
			return "", &ReviewWorkerError{Reason: "review turn/completed has the wrong turn ID"}
		}
		status, ok := turn["status"].(string)
		if !ok || (status != "completed" && status != "interrupted" && status != "failed") {
			return "", &ReviewWorkerError{Reason: fmt.Sprintf("review turn has invalid status %#v", turn["status"])}
		}
		setTerminalStatus(status)
		pending := router.orphanUnresolved(threadID, turnID, "review", status)
		if status == "completed" && len(pending) != 0 {
			return "", &ReviewWorkerError{Reason: "review turn completed before its dynamic tool results were delivered"}
		}
		if status != "completed" {
			kind := observability.ActivityTurnFailed
			if status == "interrupted" {
				kind = observability.ActivityTurnInterrupted
			}
			w.publish(generation, observability.ActivityReviewer, kind, map[string]any{"status": status}, threadID, turnID, "")
			return "", &ReviewWorkerError{Reason: fmt.Sprintf("Codex reviewer turn %s: %s", status, shortJSON(turn["error"]))}
		}
		finalMessage, haveFinalMessage := finalAgentMessage(turn)
		if !haveFinalMessage && haveLastAgentMessage {
			finalMessage = lastAgentMessage
			haveFinalMessage = true
		}
		if !haveFinalMessage {
			return "", &ReviewWorkerError{Reason: "Codex reviewer completed without a final response"}
		}
		w.publish(generation, observability.ActivityReviewer, observability.ActivityTurnCompleted, map[string]any{"status": "completed"}, threadID, turnID, "")
		return finalMessage, nil
	}
}

func (w *ReviewWorker) finish(runtime ReviewRuntime, generation *uint64, evidence *provenance.ReviewEvidence, outcome string, options ReviewOptions, serviceTierName, focus string, startedAt time.Time) {
	if w.wasCancelled() && outcome != "completed" {
		outcome = "cancelled"
	}
	data := map[string]any{
		"model":             options.Model,
		"reasoning_effort":  options.ReasoningEffort,
		"reasoning_summary": options.ReasoningSummary,
		"service_tier":      options.ServiceTier,
		"focus":             focus,
		"duration_seconds":  math.Max(0, time.Since(startedAt).Seconds()),
	}
	if serviceTierName != "" {
		data["service_tier_name"] = serviceTierName
	}
	if id := evidenceID(evidence); id != "" {
		data["review_id"] = id
	}
	data = mergeMaps(data, options.Origin)
	w.record(runtime, "review_"+outcome, generation, data)
	if evidence != nil {
		if err := evidence.Complete(outcome); err != nil {
			w.degrade(runtime, "review forensic provenance was incomplete: "+err.Error())
		}
	}
	kind := observability.ActivityReviewFailed
	switch outcome {
	case "completed":
		kind = observability.ActivityReviewCompleted
	case "cancelled":
		kind = observability.ActivityReviewCancelled
	}
	w.publish(generation, observability.ActivityReviewer, kind, map[string]any{
		"model":            options.Model,
		"reasoning_effort": options.ReasoningEffort,
		"service_tier":     options.ServiceTier,
		"focus":            focus,
		"duration_seconds": math.Max(0, time.Since(startedAt).Seconds()),
	}, "", "", "")
}

func (w *ReviewWorker) record(runtime ReviewRuntime, event string, generation *uint64, data map[string]any) {
	if identity := runtime.HarnessIdentity(); identity != nil {
		copy := make(map[string]any, len(data)+1)
		for key, value := range data {
			copy[key] = value
		}
		copy["harness_identity"] = identity.AsJSON()
		data = copy
	}
	if log := runtime.EventLog(); log != nil {
		log.Record(event, generation, data)
	}
	if metrics := runtime.Metrics(); metrics != nil {
		metrics.Record(event, data)
	}
}

func (w *ReviewWorker) degrade(runtime ReviewRuntime, reason string) {
	if log := runtime.EventLog(); log != nil {
		log.Degrade(reason)
	}
	if metrics := runtime.Metrics(); metrics != nil {
		metrics.Degrade(reason)
	}
}

func (w *ReviewWorker) publish(generation *uint64, role observability.ActivityRole, kind observability.ActivityKind, data map[string]any, threadID, turnID, itemID string) {
	observability.PublishActivity(w.options.ActivityStream, generation, role, kind, data, threadID, turnID, itemID)
}

// runtimeActivity keeps the notification privacy filter in one place. The
// reviewer owns its configured stream; the runtime interface intentionally has
// no activity-stream requirement.
func runtimeActivity(_ ReviewRuntime, configured *observability.ActivityStream) *observability.ActivityStream {
	return configured
}

func normalizeReviewOptions(options ReviewOptions) (ReviewOptions, error) {
	if options.Focus == "" {
		options.Focus = "general"
	}
	if _, ok := reviewFocuses[options.Focus]; !ok {
		return ReviewOptions{}, &ReviewWorkerError{Reason: "unsupported review focus"}
	}
	if options.Model == "" {
		options.Model = DefaultReviewerModel
	}
	if options.ReasoningEffort == "" {
		options.ReasoningEffort = DefaultReviewerReasoningEffort
	}
	if options.ReasoningSummary == "" {
		options.ReasoningSummary = DefaultReviewerReasoningSummary
	}
	if options.ServiceTier == "" {
		options.ServiceTier = DefaultReviewerServiceTier
	}
	for name, value := range map[string]*string{
		"objective": options.Objective,
		"request":   options.Request,
		"proposal":  options.Proposal,
	} {
		if value == nil {
			continue
		}
		if !utf8.ValidString(*value) {
			return ReviewOptions{}, &ReviewWorkerError{Reason: name + " is not valid UTF-8"}
		}
		if name == "request" && len([]byte(*value)) > maxReviewRequestBytes {
			return ReviewOptions{}, &ReviewWorkerError{Reason: "review request exceeds 8 KiB"}
		}
	}
	return options, nil
}

func dispatchReadOnlyTool(ctx context.Context, runtime ReviewRuntime, tool string, arguments map[string]any, evidence *provenance.ReviewEvidence, worker *ReviewWorker) (guest.ToolResult, error) {
	switch tool {
	case "list":
		if err := checkFields(arguments, nil, map[string]struct{}{"prefix": {}}); err != nil {
			return guest.ToolResult{}, err
		}
		var guestArguments [][]byte
		if prefix, exists := arguments["prefix"]; exists {
			encoded, err := utf8Argument(prefix, "prefix")
			if err != nil {
				return guest.ToolResult{}, err
			}
			guestArguments = [][]byte{encoded}
		}
		return runtime.InvokeTool(ctx, "list", guestArguments)
	case "read":
		if err := checkFields(arguments, map[string]struct{}{"path": {}, "offset": {}, "length": {}}, nil); err != nil {
			return guest.ToolResult{}, err
		}
		path, err := utf8Argument(arguments["path"], "path")
		if err != nil {
			return guest.ToolResult{}, err
		}
		offset, err := unsignedDecimal(arguments["offset"], "offset")
		if err != nil {
			return guest.ToolResult{}, err
		}
		length, err := unsignedDecimal(arguments["length"], "length")
		if err != nil {
			return guest.ToolResult{}, err
		}
		result, err := runtime.InvokeTool(ctx, "read", [][]byte{path, offset, length})
		if err != nil {
			return guest.ToolResult{}, err
		}
		if evidence != nil {
			if err := evidence.RecordSourceRead(string(path), decimalInt64(offset), decimalInt64(length), int64(result.Status), result.Output); err != nil {
				worker.degrade(runtime, "review forensic provenance was incomplete: "+err.Error())
			}
		}
		return result, nil
	default:
		return guest.ToolResult{}, fmt.Errorf("unsupported reviewer CodexOS tool: %s", tool)
	}
}

func checkFields(arguments map[string]any, required, optional map[string]struct{}) error {
	for name := range required {
		if _, ok := arguments[name]; !ok {
			return fmt.Errorf("missing argument: %s", firstField(required))
		}
	}
	for name := range arguments {
		if _, ok := required[name]; ok {
			continue
		}
		if _, ok := optional[name]; ok {
			continue
		}
		return fmt.Errorf("unexpected argument: %s", name)
	}
	return nil
}

func firstField(fields map[string]struct{}) string {
	first := ""
	for name := range fields {
		if first == "" || name < first {
			first = name
		}
	}
	return first
}

func utf8Argument(value any, name string) ([]byte, error) {
	text, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("%s must be a string", name)
	}
	if !utf8.ValidString(text) {
		return nil, fmt.Errorf("%s is not valid UTF-8", name)
	}
	return []byte(text), nil
}

func unsignedDecimal(value any, name string) ([]byte, error) {
	var text string
	switch value := value.(type) {
	case json.Number:
		text = value.String()
	case uint64:
		text = strconv.FormatUint(value, 10)
	case uint:
		text = strconv.FormatUint(uint64(value), 10)
	case uint32:
		text = strconv.FormatUint(uint64(value), 10)
	case int:
		if value < 0 {
			return nil, fmt.Errorf("%s must be a non-negative integer", name)
		}
		text = strconv.Itoa(value)
	case int64:
		if value < 0 {
			return nil, fmt.Errorf("%s must be a non-negative integer", name)
		}
		text = strconv.FormatInt(value, 10)
	case int32:
		if value < 0 {
			return nil, fmt.Errorf("%s must be a non-negative integer", name)
		}
		text = strconv.FormatInt(int64(value), 10)
	default:
		return nil, fmt.Errorf("%s must be a non-negative integer", name)
	}
	if text == "" || strings.HasPrefix(text, "-") {
		return nil, fmt.Errorf("%s must be a non-negative integer", name)
	}
	if _, err := strconv.ParseUint(text, 10, 64); err != nil {
		return nil, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return []byte(text), nil
}

func decimalInt64(value []byte) int64 {
	parsed, err := strconv.ParseUint(string(value), 10, 64)
	if err != nil || parsed > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(parsed)
}

func validateToolCall(values map[string]any, threadID, turnID string) error {
	callID, ok := values["callId"].(string)
	if !ok || callID == "" {
		return errors.New("dynamic tool call ID must be a non-empty string")
	}
	if values["threadId"] != threadID {
		return errors.New("dynamic tool request has the wrong thread ID")
	}
	if values["turnId"] != turnID {
		return errors.New("dynamic tool request has the wrong turn ID")
	}
	return nil
}

func toolArguments(value any) (map[string]any, error) {
	arguments, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("dynamic tool arguments are not an object")
	}
	return arguments, nil
}

func dynamicToolActivityData(params any) map[string]any {
	values, ok := params.(map[string]any)
	if !ok {
		return map[string]any{"namespace": nil, "tool": nil, "arguments": params}
	}
	return map[string]any{"namespace": values["namespace"], "tool": values["tool"], "arguments": values["arguments"]}
}

func activityCallID(params any) string {
	values, ok := params.(map[string]any)
	if !ok {
		return ""
	}
	callID, _ := values["callId"].(string)
	return callID
}

func mergeActivity(first, second map[string]any) map[string]any {
	merged := make(map[string]any, len(first)+len(second))
	for key, value := range first {
		merged[key] = value
	}
	for key, value := range second {
		merged[key] = value
	}
	return merged
}

func formatToolResult(result guest.ToolResult) string {
	output := ""
	encoding := "utf8"
	if utf8.Valid(result.Output) {
		output = string(result.Output)
	} else {
		output = base64.StdEncoding.EncodeToString(result.Output)
		encoding = "base64"
	}
	value := map[string]any{"status": result.Status, "encoding": encoding, "output": output}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return `{"encoding":"utf8","output":"","status":0}`
	}
	return strings.TrimSuffix(encoded.String(), "\n")
}

func finalAgentMessage(turn map[string]any) (string, bool) {
	items, ok := turn["items"].([]any)
	if !ok {
		if typed, typedOK := turn["items"].([]map[string]any); typedOK {
			for index := len(typed) - 1; index >= 0; index-- {
				if typed[index]["type"] == "agentMessage" {
					if text, textOK := typed[index]["text"].(string); textOK {
						return text, true
					}
				}
			}
		}
		return "", false
	}
	for index := len(items) - 1; index >= 0; index-- {
		item, ok := items[index].(map[string]any)
		if !ok || item["type"] != "agentMessage" {
			continue
		}
		if text, ok := item["text"].(string); ok {
			return text, true
		}
	}
	return "", false
}

func evidenceID(evidence *provenance.ReviewEvidence) string {
	if evidence == nil {
		return ""
	}
	return evidence.ReviewID()
}

func reviewError(err error) error {
	if err == nil {
		return nil
	}
	if workerError, ok := err.(*ReviewWorkerError); ok {
		return workerError
	}
	return &ReviewWorkerError{Reason: err.Error()}
}

func shortJSON(value any) string {
	encoded, err := codexapp.ShortJSON(value)
	if err != nil {
		return "<unserializable>"
	}
	return encoded
}

func boundedError(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if len(text) <= codexapp.MaxErrorOutput {
		return text
	}
	return text[:codexapp.MaxErrorOutput]
}

const reviewerConfig = `default_permissions = "codexos-reviewer"
allow_login_shell = false
web_search = "disabled"

[agents]
enabled = false

[features]
apps = false
browser_use = false
browser_use_external = false
browser_use_full_cdp_access = false
computer_use = false
goals = false
hooks = false
image_generation = false
image_tools = false
memories = false
multi_agent = false
plugins = false
remote_plugin = false
skill_mcp_dependency_install = false
skill_search = false
web_search = false
web_search_cached = false
web_search_request = false

[feedback]
enabled = false

[history]
persistence = "none"

[shell_environment_policy]
inherit = "none"

[tools]
view_image = false
web_search = false

[permissions.codexos-reviewer.filesystem]
":root" = "deny"
":minimal" = "read"
":tmpdir" = "deny"
":slash_tmp" = "deny"

[permissions.codexos-reviewer.filesystem.":workspace_roots"]
"." = "read"

[permissions.codexos-reviewer.network]
enabled = false
`

func reviewerToolNamespace() map[string]any {
	return map[string]any{
		"type":        "namespace",
		"name":        "codexos",
		"description": "Read the current mutable CodexOS guest source.",
		"tools": []map[string]any{
			dynamicFunction("list", "List current mutable guest source paths, optionally by prefix.", map[string]any{
				"prefix": map[string]any{"type": "string"},
			}, nil),
			dynamicFunction("read", "Read exact bytes from a current mutable guest source file.", map[string]any{
				"path":   map[string]any{"type": "string"},
				"offset": map[string]any{"type": "integer", "minimum": 0},
				"length": map[string]any{"type": "integer", "minimum": 0},
			}, []string{"path", "offset", "length"}),
		},
	}
}

func dynamicFunction(name, description string, properties map[string]any, required []string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return map[string]any{
		"type":        "function",
		"name":        name,
		"description": description,
		"inputSchema": schema,
	}
}

func reviewerPrompt(objective *string, focus string, request, proposal *string, snapshot provenance.FileIdentity) string {
	objectiveText := "No trusted per-generation objective was supplied."
	if objective != nil {
		objectiveText = "Trusted current objective:\n" + *objective
	}
	requestText := "No additional review request was supplied."
	if request != nil {
		requestText = "Implementor review request:\n" + *request
	}
	proposalText := "No explicit proposal was supplied."
	if proposal != nil {
		proposalText = "Implementor's exact proposed plan or change:\n" + *proposal
	}
	snapshotText := fmt.Sprintf("Stable source snapshot: sha256=%s bytes=%d.", snapshot.SHA256, snapshot.Size)
	return "You are a read-only reviewer for the current CodexOS generation.\n\n" +
		"CodexOS is an autonomous experiment evolving toward a general-purpose " +
		"operating system. Doom is the first major interactive userland milestone, " +
		"not the final purpose of the OS.\n\n" +
		"The implementor's originating turn is fully quiescent. The read tools expose " +
		"one stable, explicitly captured source snapshot. You are here only to inspect that work and provide an independent " +
		"technical review.\n\n" +
		"Read enough of the current source through the available codexos tools to " +
		"understand the work in context.\n\n" +
		"Identify only issues that genuinely matter to the success of the current " +
		"work, including where relevant correctness bugs, logic errors, security " +
		"vulnerabilities, design flaws, incorrect assumptions, unnecessary " +
		"complexity that materially increases risk, performance problems that " +
		"materially matter, and divergence from the stated objective.\n\n" +
		"For every finding, explain the issue, its concrete impact, and a specific " +
		"suggested change. Categorize findings as Blocking, Non-blocking, or " +
		"Suggestions. Blocking findings must be addressed for the current work to " +
		"succeed. Non-blocking findings are real problems worth addressing. " +
		"Suggestions are lower-priority improvements with a real expected impact.\n\n" +
		"Do not report formatting or naming preferences, comment grammar, stylistic " +
		"taste, minor refactors, speculative abstractions, generic best practices " +
		"with no concrete impact, or alternative designs merely because you prefer " +
		"them.\n\n" +
		"Do not redesign CodexOS, prescribe an unrelated architecture, modify " +
		"anything, build anything, or try to finish the generation. If you find no " +
		"meaningful issues, say exactly that clearly. Your findings are advisory; " +
		"the implementor decides what to do with them.\n\n" +
		"Review focus: " + focus + ". Prioritize that focus, while still reporting any " +
		"blocking issue you discover.\n\n" + snapshotText + "\n\n" + objectiveText + "\n\n" + requestText + "\n\n" + proposalText + "\n\n" + doomMilestoneContract
}
