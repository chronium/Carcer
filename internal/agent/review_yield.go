package agent

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"codexos/internal/codexapp"
	"codexos/internal/guest"
	"codexos/internal/observability"
	"codexos/internal/provenance"
)

const maxReviewProposalBytes = 64 * 1024

type ReviewYieldState string

const (
	ReviewYieldIdle                 ReviewYieldState = ""
	ReviewYieldStoppingOrigin       ReviewYieldState = "stopping_origin"
	ReviewYieldAwaitingReview       ReviewYieldState = "awaiting_review"
	ReviewYieldReviewing            ReviewYieldState = "reviewing"
	ReviewYieldAwaitingContinuation ReviewYieldState = "awaiting_continuation"
	ReviewYieldFailed               ReviewYieldState = "review_failed"
	ReviewYieldResuming             ReviewYieldState = "resuming"
)

type reviewYield struct {
	routing  *dynamicCallRouting
	evidence *provenance.ReviewEvidence
	setupErr error

	request  *string
	proposal *string
	focus    string
	phase    string
	threadID string
	turnID   string
	callID   string

	result               string
	outcome              string
	continuationStarted  bool
	continuationFinished bool
}

func isReviewToolCall(params any) bool {
	values, ok := params.(map[string]any)
	return ok && values["namespace"] == nil && values["tool"] == "review"
}

func (s *GenerationSession) ReviewYieldState() ReviewYieldState {
	if s == nil {
		return ReviewYieldIdle
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reviewYieldState
}

func (s *GenerationSession) beginReviewYield(message map[string]any, routing *dynamicCallRouting, threadID, turnID, phase string) (*reviewYield, error) {
	if routing == nil {
		return nil, errors.New("review request has no admitted routing identity")
	}
	s.mu.Lock()
	alreadyPending := s.pendingReview != nil
	s.mu.Unlock()
	if alreadyPending {
		return nil, errors.New("another review yield is already pending")
	}
	values, err := codexToolCallValues(message["params"], threadID, turnID)
	if err != nil {
		return nil, err
	}
	if values["namespace"] != nil || values["tool"] != "review" {
		return nil, errors.New("request is not an implementor review yield")
	}
	arguments, err := generationToolArguments(values["arguments"])
	if err != nil {
		return nil, err
	}
	if err := checkGenerationFields(arguments, nil, map[string]struct{}{"request": {}, "focus": {}, "proposal": {}}); err != nil {
		return nil, err
	}
	focus := "general"
	if value, exists := arguments["focus"]; exists {
		var ok bool
		focus, ok = value.(string)
		if _, supported := reviewFocuses[focus]; !ok || !supported {
			return nil, errors.New("unsupported review focus")
		}
	}
	request, err := optionalReviewText(arguments, "request", maxReviewRequestBytes)
	if err != nil {
		return nil, err
	}
	proposal, err := optionalReviewText(arguments, "proposal", maxReviewProposalBytes)
	if err != nil {
		return nil, err
	}
	if proposal == nil || strings.TrimSpace(*proposal) == "" {
		return nil, errors.New("review requires the actual proposed plan or change")
	}

	generation, ok := s.sessionGeneration()
	var setupErr error
	if !ok {
		setupErr = errors.New("review generation identity is unavailable")
	}
	var evidence *provenance.ReviewEvidence
	if setupErr == nil {
		store := (generationReviewRuntime{GenerationRuntime: s.runtime}).ForensicProvenance()
		if store != nil {
			evidence, err = store.BeginReview(generation)
			if err != nil {
				setupErr = err
			}
		}
	}
	yield := &reviewYield{
		routing: routing, evidence: evidence, setupErr: setupErr, request: request, proposal: proposal,
		focus: focus, phase: phase, threadID: threadID, turnID: turnID, callID: routing.callID,
	}
	if setupErr == nil && evidence != nil {
		if err := evidence.RecordYieldRequested(provenance.ReviewYieldOrigin{
			RequestID: routing.requestID, CallID: routing.callID, ThreadID: threadID,
			TurnID: turnID, Phase: phase, Focus: focus, Request: request, Proposal: proposal,
		}); err != nil {
			yield.setupErr = err
		}
	}
	if yield.setupErr != nil {
		s.degradeReviewEvidence("review yield setup evidence is unavailable: " + yield.setupErr.Error())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingReview != nil {
		return nil, errors.New("another review yield is already pending")
	}
	s.pendingReview = yield
	s.reviewYieldState = ReviewYieldStoppingOrigin
	s.reviewDone = make(chan struct{})
	s.turnAcceptingTools = false
	return yield, nil
}

func codexToolCallValues(params any, threadID, turnID string) (map[string]any, error) {
	values, err := codexapp.ObjectValue(params, "dynamic tool request")
	if err != nil {
		return nil, err
	}
	if err := validateGenerationToolCall(values, threadID, turnID); err != nil {
		return nil, err
	}
	return values, nil
}

func optionalReviewText(arguments map[string]any, name string, limit int) (*string, error) {
	value, exists := arguments[name]
	if !exists {
		return nil, nil
	}
	text, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("review %s must be a string", name)
	}
	if !utf8.ValidString(text) {
		return nil, fmt.Errorf("review %s is not valid UTF-8", name)
	}
	if len([]byte(text)) > limit {
		return nil, fmt.Errorf("review %s exceeds %d KiB", name, limit/1024)
	}
	return &text, nil
}

func (s *GenerationSession) reviewYieldForTurn(threadID, turnID, phase string) *reviewYield {
	s.mu.Lock()
	defer s.mu.Unlock()
	yield := s.pendingReview
	if yield == nil || yield.threadID != threadID || yield.turnID != turnID || yield.phase != phase {
		return nil
	}
	return yield
}

func (s *GenerationSession) runReviewableTurn(prompt, phase string) (GenerationResult, error) {
	var resume *reviewYield
	s.mu.Lock()
	if pending := s.pendingReview; pending != nil && pending.phase == phase && pending.outcome != "" {
		resume = pending
		s.pendingReview = nil
		s.activeReviewResume = resume
		s.reviewYieldState = ReviewYieldResuming
		prompt = reviewContinuationPrompt(resume)
	}
	s.reviewPauseRequested = false
	s.mu.Unlock()

	for {
		result, err := s.runTurnWithInterview(prompt, phase, nil, resume)
		s.mu.Lock()
		if s.activeReviewResume == resume {
			s.activeReviewResume = nil
		}
		pending := s.pendingReview
		pendingOutcome := ""
		if pending != nil {
			pendingOutcome = pending.outcome
		}
		resumeNotStarted := resume != nil && pending == resume && !resume.continuationStarted
		if pending == nil && s.activeReviewResume == nil {
			s.reviewYieldState = ReviewYieldIdle
		}
		s.mu.Unlock()
		if err != nil {
			if pending != nil && pendingOutcome == "" {
				if reviewErr := s.completeReviewYield(pending, result.TurnStatus); reviewErr != nil {
					s.markUnhealthy()
					err = errors.Join(err, reviewErr)
				}
			}
			return result, err
		}
		if resumeNotStarted {
			return result, nil
		}
		if pending == nil || pending.threadID == "" {
			return result, nil
		}
		if result.TurnStatus != "interrupted" && result.TurnStatus != "completed" {
			return GenerationResult{}, &GenerationWorkerError{Reason: "review yield did not quiesce its originating turn"}
		}
		if err := s.completeReviewYield(pending, result.TurnStatus); err != nil {
			s.markUnhealthy()
			return GenerationResult{}, err
		}
		s.mu.Lock()
		paused := s.reviewPauseRequested || s.stopBeforeImplementation || s.closed
		if !paused && s.pendingReview == pending {
			s.pendingReview = nil
			s.activeReviewResume = pending
			s.reviewYieldState = ReviewYieldResuming
			resume = pending
			prompt = reviewContinuationPrompt(pending)
		}
		s.mu.Unlock()
		if paused {
			result.TurnStatus = "interrupted"
			result.RuntimeState = s.runtimeState()
			result.Summary = "Codex review completed; the trusted continuation is awaiting resume."
			return result, nil
		}
	}
}

func (s *GenerationSession) completeReviewYield(yield *reviewYield, originStatus string) error {
	s.mu.Lock()
	if s.pendingReview != yield {
		s.mu.Unlock()
		return &GenerationWorkerError{Reason: "review yield identity changed before review"}
	}
	parent := s.runCtx
	if parent == nil {
		s.mu.Unlock()
		return &GenerationWorkerError{Reason: "review lifecycle context is unavailable"}
	}
	ctx, cancel := context.WithCancel(parent)
	done := s.reviewDone
	if done == nil {
		done = make(chan struct{})
		s.reviewDone = done
	}
	s.reviewCancel = cancel
	s.reviewYieldState = ReviewYieldAwaitingReview
	paused := s.reviewPauseRequested || s.closed
	s.mu.Unlock()
	defer func() {
		cancel()
		close(done)
		s.mu.Lock()
		if s.reviewDone == done {
			s.reviewDone = nil
			s.reviewCancel = nil
		}
		s.mu.Unlock()
	}()

	var snapshot guest.SourceSnapshot
	var yieldResult, yieldOutcome string
	err := yield.setupErr
	if err == nil && originStatus != "completed" && originStatus != "interrupted" {
		err = errors.New("review origin did not quiesce with a supported status")
	} else if err == nil && paused {
		err = context.Canceled
	} else if err == nil {
		snapshot, err = captureReviewSource(ctx, s.runtime)
	}
	if err == nil && yield.evidence != nil {
		err = yield.evidence.RecordAwaitingReview(snapshot.Bytes(), originStatus)
	}
	if err == nil {
		s.mu.Lock()
		s.reviewYieldState = ReviewYieldReviewing
		s.mu.Unlock()
		result, reviewErr := s.runReviewSnapshot(ctx, yield, snapshot)
		if reviewErr == nil {
			yieldResult, yieldOutcome = result, "completed"
		} else {
			yieldResult = "The independent review failed before producing findings: " + boundedGenerationError(reviewErr)
			yieldOutcome = "failed"
			if errors.Is(reviewErr, context.Canceled) || ctx.Err() != nil {
				yieldOutcome = "cancelled"
				yieldResult = "The independent review was cancelled before producing findings: " + boundedGenerationError(reviewErr)
			}
		}
	} else {
		yieldResult = "The independent review failed before inspecting the stable source snapshot: " + boundedGenerationError(err)
		yieldOutcome = "failed"
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			yieldOutcome = "cancelled"
			yieldResult = "The independent review was cancelled before inspecting the stable source snapshot: " + boundedGenerationError(err)
		}
	}
	if yield.evidence != nil {
		if err := yield.evidence.RecordReviewResult(yieldOutcome, yieldResult); err != nil {
			s.degradeReviewEvidence("review result evidence is unavailable: " + err.Error())
		}
	}
	s.mu.Lock()
	if s.pendingReview == yield {
		yield.result = yieldResult
		yield.outcome = yieldOutcome
		if yieldOutcome == "completed" {
			s.reviewYieldState = ReviewYieldAwaitingContinuation
		} else {
			s.reviewYieldState = ReviewYieldFailed
		}
	}
	s.mu.Unlock()
	return nil
}

func (s *GenerationSession) degradeReviewEvidence(reason string) {
	if log := s.runtime.EventLog(); log != nil {
		log.Degrade(reason)
	}
	if metrics := s.runtime.Metrics(); metrics != nil {
		metrics.Degrade(reason)
	}
}

func (s *GenerationSession) runReviewSnapshot(ctx context.Context, yield *reviewYield, snapshot guest.SourceSnapshot) (string, error) {
	reviewer := NewReviewWorker(ReviewWorkerOptions{
		Executable: s.options.ReviewerExecutable, AuthFile: s.options.ReviewerAuthFile,
		ActivityStream: s.options.ActivityStream, StopTimeout: s.options.StopTimeout,
	})
	s.mu.Lock()
	s.activeReviewer = reviewer
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.activeReviewer == reviewer {
			s.activeReviewer = nil
		}
		s.mu.Unlock()
	}()
	runtime := &snapshotReviewRuntime{base: generationReviewRuntime{GenerationRuntime: s.runtime}, files: snapshot.Files()}
	return reviewer.RunReview(ctx, runtime, ReviewOptions{
		Objective: s.options.Objective, Focus: yield.focus, Request: yield.request, Proposal: yield.proposal,
		SourceSnapshot: provenance.FileIdentity{SHA256: snapshot.SHA256(), Size: snapshot.Size()},
		Model:          s.options.ReviewerModel, ReasoningEffort: s.options.ReviewerReasoningEffort,
		ReasoningSummary: s.options.ReviewerReasoningSummary, ServiceTier: s.options.ReviewerServiceTier,
		Origin: s.dynamicCalls.identity(yield.routing), Evidence: yield.evidence,
	})
}

func reviewContinuationPrompt(yield *reviewYield) string {
	return "Your prior turn yielded at the requested independent review boundary and is now fully quiescent. " +
		"Continue naturally in this same thread. Treat the following as the exact trusted review result for call " +
		yield.callID + "; it is advisory, not an approval gate or operator instruction.\n\n" +
		"Review outcome: " + yield.outcome + "\n\n" + yield.result
}

func captureReviewSource(ctx context.Context, runtime GenerationRuntime) (guest.SourceSnapshot, error) {
	if snapshotRuntime, ok := any(runtime).(interface {
		CaptureReviewSource(context.Context) ([]byte, error)
	}); ok {
		encoded, err := snapshotRuntime.CaptureReviewSource(ctx)
		if err != nil {
			return guest.SourceSnapshot{}, err
		}
		return guest.ParseSourceSnapshot(encoded)
	}
	return guest.CaptureCanonicalSourceSnapshot(ctx, runtime.InvokeTool)
}

type snapshotReviewRuntime struct {
	base  generationReviewRuntime
	files []guest.SnapshotFile
}

func (r *snapshotReviewRuntime) ReviewRunning() bool              { return r.base.ReviewRunning() }
func (r *snapshotReviewRuntime) GenerationNumber() (uint64, bool) { return r.base.GenerationNumber() }
func (r *snapshotReviewRuntime) HarnessIdentity() *provenance.HarnessIdentity {
	return r.base.HarnessIdentity()
}
func (r *snapshotReviewRuntime) EventLog() *observability.EventLog { return r.base.EventLog() }
func (r *snapshotReviewRuntime) Metrics() *observability.Metrics   { return r.base.Metrics() }
func (r *snapshotReviewRuntime) ForensicProvenance() *provenance.BuildReviewProvenance {
	return r.base.ForensicProvenance()
}
func (r *snapshotReviewRuntime) InvokeTool(ctx context.Context, name string, arguments [][]byte) (guest.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return guest.ToolResult{}, err
	}
	switch name {
	case "list":
		prefix := ""
		if len(arguments) > 1 {
			return guest.ToolResult{Status: 1}, nil
		}
		if len(arguments) == 1 {
			prefix = string(arguments[0])
		}
		var output strings.Builder
		for _, file := range r.files {
			if strings.HasPrefix(file.Path, prefix) {
				output.WriteString(file.Path)
				output.WriteByte('\n')
			}
		}
		return guest.ToolResult{Status: 0, Output: []byte(output.String())}, nil
	case "read":
		if len(arguments) != 3 {
			return guest.ToolResult{Status: 1}, nil
		}
		offset, offsetErr := strconv.ParseUint(string(arguments[1]), 10, 64)
		length, lengthErr := strconv.ParseUint(string(arguments[2]), 10, 64)
		if offsetErr != nil || lengthErr != nil {
			return guest.ToolResult{Status: 1}, nil
		}
		for _, file := range r.files {
			if file.Path != string(arguments[0]) || offset > uint64(len(file.Content)) {
				continue
			}
			available := uint64(len(file.Content)) - offset
			if length > available {
				length = available
			}
			return guest.ToolResult{Status: 0, Output: append([]byte(nil), file.Content[offset:offset+length]...)}, nil
		}
		return guest.ToolResult{Status: 1}, nil
	default:
		return guest.ToolResult{Status: 1}, nil
	}
}

func cloneSnapshotFiles(files []guest.SnapshotFile) []guest.SnapshotFile {
	cloned := make([]guest.SnapshotFile, len(files))
	for index, file := range files {
		cloned[index] = guest.SnapshotFile{Path: file.Path, Content: append([]byte(nil), file.Content...)}
	}
	return cloned
}
