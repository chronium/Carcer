package operator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"codexos/internal/agent"
	"codexos/internal/experiment"
	"codexos/internal/provenance"
)

// generationRuntime is the concrete operator boundary implemented by
// experiment.CodexOSRun. Keeping the boundary here lets the ownership rules be
// tested without starting QEMU or touching a real run.
type generationRuntime interface {
	agent.GenerationRuntime
	agent.GenerationStateRuntime
	agent.GenerationGateRuntime
	Pause(context.Context) error
	Resume(context.Context) error
	ContinueGeneration() error
	ForkFromGeneration(uint64) error
	AbortGeneration() error
	Close() error
}

var _ generationRuntime = (*experiment.CodexOSRun)(nil)

// GenerationControllerOptions contains the trusted Codex session settings
// owned by the operator for every generation in one run.
type GenerationControllerOptions struct {
	Session          agent.GenerationSessionOptions
	InterruptTimeout time.Duration
}

// TurnOutcome is delivered exactly once for an asynchronously started Codex
// turn. Retained is true only when a successfully completed generation turn
// preserved its original thread at the frozen generation gate.
type TurnOutcome struct {
	Result   agent.GenerationResult
	Err      error
	Retained bool
}

type controlledTurn struct {
	done      chan struct{}
	outcome   chan TurnOutcome
	interview bool
}

// GenerationController owns at most one Codex app-server session and one turn
// for a concrete run. A session is never replaced within a generation, is
// preserved across pause/resume, and is retired before any generation
// transition or runtime shutdown.
type GenerationController struct {
	runtime generationRuntime
	options GenerationControllerOptions

	operationMu   sync.Mutex
	mu            sync.Mutex
	session       *agent.GenerationSession
	generation    uint64
	generationSet bool
	unavailable   *uint64
	active        *controlledTurn
	resumeTurn    bool
	interviewOpen bool
	lastInterview provenance.ExitInterviewTranscriptSnapshot
	interviewSet  bool
	retirementErr error
	runtimeClosed bool
	closed        bool
}

// NewGenerationController joins a concrete experiment run to operator-owned
// Codex session lifecycle. It starts neither QEMU nor Codex.
func NewGenerationController(runtime *experiment.CodexOSRun, options GenerationControllerOptions) (*GenerationController, error) {
	if runtime == nil {
		return nil, errors.New("CodexOS operator runtime is nil")
	}
	return newGenerationController(runtime, options)
}

func newGenerationController(runtime generationRuntime, options GenerationControllerOptions) (*GenerationController, error) {
	if runtime == nil {
		return nil, errors.New("CodexOS operator runtime is nil")
	}
	if options.InterruptTimeout < 0 {
		return nil, errors.New("Codex operator interrupt timeout must not be negative")
	}
	if options.InterruptTimeout == 0 {
		options.InterruptTimeout = agent.DefaultInterruptTimeout
	}
	return &GenerationController{runtime: runtime, options: options}, nil
}

// StartTurn starts planning and implementation for a fresh generation, resumes
// interrupted planning, or starts an ordinary continuation on the one existing
// thread. The returned channel is buffered and closes after its one outcome.
func (c *GenerationController) StartTurn(prompt string) (<-chan TurnOutcome, error) {
	if c == nil {
		return nil, errors.New("CodexOS generation controller is nil")
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	return c.startTurn(prompt)
}

func (c *GenerationController) startTurn(prompt string) (<-chan TurnOutcome, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("CodexOS generation controller is closed")
	}
	if c.retirementErr != nil {
		err := c.retirementErr
		c.mu.Unlock()
		return nil, fmt.Errorf("Codex session retirement previously failed: %w", err)
	}
	if c.active != nil {
		c.mu.Unlock()
		return nil, errors.New("Codex implementor turn is already active")
	}
	if !c.runtime.GenerationRunning() {
		c.mu.Unlock()
		return nil, errors.New("CodexOS generation is not running")
	}
	generation, ok := c.runtime.GenerationNumber()
	if !ok {
		c.mu.Unlock()
		return nil, errors.New("CodexOS generation number is unavailable")
	}
	if c.unavailable != nil && *c.unavailable == generation {
		c.mu.Unlock()
		return nil, errors.New("Codex session failed and cannot be replaced in this generation")
	}
	session := c.session
	initial := session == nil
	planning := false
	if initial {
		session = agent.NewGenerationSession(c.runtime, c.options.Session)
		c.session = session
		c.generation = generation
		c.generationSet = true
	} else {
		if !c.generationSet || c.generation != generation {
			c.mu.Unlock()
			return nil, errors.New("Codex session belongs to another generation")
		}
		if !session.Healthy() {
			c.mu.Unlock()
			return nil, errors.New("Codex generation session is unusable")
		}
		planning = !session.PlanningCompleted()
	}
	turn := &controlledTurn{done: make(chan struct{}), outcome: make(chan TurnOutcome, 1)}
	c.active = turn
	c.mu.Unlock()

	go c.runGenerationTurn(turn, session, initial, planning, prompt)
	return turn.outcome, nil
}

func (c *GenerationController) runGenerationTurn(turn *controlledTurn, session *agent.GenerationSession, initial, planning bool, prompt string) {
	var result agent.GenerationResult
	var err error
	switch {
	case initial:
		result, err = session.RunInitialTurn()
	case planning:
		result, err = session.RunPlanningContinuationTurn()
	default:
		if prompt == "" {
			result, err = session.RunContinuationTurn()
		} else {
			result, err = session.RunContinuationTurn(prompt)
		}
	}
	c.finishTurn(turn, session, TurnOutcome{Result: result, Err: err})
}

func (c *GenerationController) finishTurn(turn *controlledTurn, session *agent.GenerationSession, outcome TurnOutcome) {
	closeSession := false
	markUnavailable := false
	if !turn.interview && c.runtime.GenerationState() == string(experiment.RuntimeStateAwaitingNextGeneration) {
		if outcome.Err == nil && outcome.Result.TurnStatus == "completed" {
			if err := session.RetainForExitInterview(); err == nil {
				outcome.Retained = true
			} else {
				closeSession = true
			}
		} else {
			closeSession = true
		}
	} else if outcome.Err != nil && !session.Healthy() {
		closeSession = true
		markUnavailable = true
	}
	var closeErr error
	if closeSession {
		closeErr = session.Close()
		outcome.Err = errors.Join(outcome.Err, closeErr)
	}
	transcript, transcriptOK := session.ExitInterviewTranscript()

	c.mu.Lock()
	if c.active == turn {
		c.active = nil
	}
	if closeSession && c.session == session {
		if transcriptOK {
			c.lastInterview = transcript
			c.interviewSet = true
		}
		c.session = nil
		c.generationSet = false
		c.interviewOpen = false
		if markUnavailable {
			generation := c.generation
			c.unavailable = &generation
		}
	}
	if closeErr != nil {
		c.retirementErr = closeErr
	}
	c.mu.Unlock()
	turn.outcome <- outcome
	close(turn.outcome)
	close(turn.done)
}

// Pause interrupts and fully quiesces an active Codex turn before stopping
// QEMU. A failed interrupt leaves QEMU untouched.
func (c *GenerationController) Pause(ctx context.Context) error {
	if c == nil {
		return errors.New("CodexOS generation controller is nil")
	}
	if ctx == nil {
		return errors.New("CodexOS pause context is nil")
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("CodexOS generation controller is closed")
	}
	if c.retirementErr != nil {
		err := c.retirementErr
		c.mu.Unlock()
		return fmt.Errorf("Codex session retirement previously failed: %w", err)
	}
	session, turn := c.session, c.active
	c.mu.Unlock()
	resume := false
	if turn != nil {
		if session == nil {
			return errors.New("Codex turn has no generation session")
		}
		deadline := time.Now().Add(c.options.InterruptTimeout)
		if callerDeadline, ok := ctx.Deadline(); ok && callerDeadline.Before(deadline) {
			deadline = callerDeadline
		}
		admitted, err := waitForTurnAdmission(ctx, session, turn.done, deadline)
		if err != nil {
			return err
		}
		if admitted {
			if err := session.InterruptTurn(maxDuration(time.Until(deadline))); err != nil {
				return err
			}
		}
		if err := waitControlledTurn(ctx, turn.done, deadline); err != nil {
			return err
		}
		c.mu.Lock()
		resume = c.session == session && session.Healthy()
		c.mu.Unlock()
	}
	c.mu.Lock()
	c.resumeTurn = resume
	c.mu.Unlock()
	return c.runtime.Pause(ctx)
}

// Resume continues QEMU and, if pause interrupted a healthy active turn,
// starts the appropriate planning or implementation continuation on the same
// session and thread.
func (c *GenerationController) Resume(ctx context.Context) (<-chan TurnOutcome, error) {
	if c == nil {
		return nil, errors.New("CodexOS generation controller is nil")
	}
	if ctx == nil {
		return nil, errors.New("CodexOS resume context is nil")
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if err := c.requireOpen(); err != nil {
		return nil, err
	}
	if err := c.runtime.Resume(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	restart := c.resumeTurn
	c.resumeTurn = false
	c.mu.Unlock()
	if !restart {
		return nil, nil
	}
	return c.startTurn(agent.ResumePrompt)
}

// ContinueGeneration retires any retained previous-generation session before
// booting the selected successor. A failed boot leaves the durable gate intact,
// but the old Codex thread remains permanently retired.
func (c *GenerationController) ContinueGeneration() error {
	if c == nil {
		return errors.New("CodexOS generation controller is nil")
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if err := c.requireOpen(); err != nil {
		return err
	}
	if err := c.retireIdleGateSession(); err != nil {
		return err
	}
	if err := c.runtime.ContinueGeneration(); err != nil {
		return err
	}
	c.clearGenerationOwnership()
	return nil
}

// Rollback retires any retained session before booting an earlier completed
// generation's selected successor.
func (c *GenerationController) Rollback(generation uint64) error {
	if c == nil {
		return errors.New("CodexOS generation controller is nil")
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if err := c.requireOpen(); err != nil {
		return err
	}
	if err := c.retireIdleGateSession(); err != nil {
		return err
	}
	if err := c.runtime.ForkFromGeneration(generation); err != nil {
		return err
	}
	c.clearGenerationOwnership()
	return nil
}

// Abort retires the current Codex session before permanently archiving the
// running or paused generation as aborted.
func (c *GenerationController) Abort() error {
	if c == nil {
		return errors.New("CodexOS generation controller is nil")
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if err := c.requireOpen(); err != nil {
		return err
	}
	if err := c.retireSession(true); err != nil {
		return err
	}
	if err := c.runtime.AbortGeneration(); err != nil {
		return err
	}
	c.clearGenerationOwnership()
	return nil
}

// ExitInterviewAvailable reports whether the exact healthy generation thread
// is retained at the current frozen gate.
func (c *GenerationController) ExitInterviewAvailable() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	session, active := c.session, c.active
	c.mu.Unlock()
	return active == nil && session != nil && session.ExitInterviewAvailable()
}

func (c *GenerationController) BeginExitInterview() error {
	if c == nil {
		return errors.New("CodexOS generation controller is nil")
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if err := c.requireOpen(); err != nil {
		return err
	}
	c.mu.Lock()
	session, active := c.session, c.active
	c.mu.Unlock()
	if active != nil {
		return errors.New("Codex turn is already active")
	}
	if session == nil {
		return errors.New("no retained Codex session is available")
	}
	if err := session.BeginExitInterview(); err != nil {
		return err
	}
	c.mu.Lock()
	if c.session != session {
		c.mu.Unlock()
		_ = session.Close()
		return errors.New("retained Codex session changed unexpectedly")
	}
	c.lastInterview = provenance.ExitInterviewTranscriptSnapshot{}
	c.interviewSet = false
	c.interviewOpen = true
	c.mu.Unlock()
	return nil
}

// StartExitInterviewTurn sends a read-only retrospective question on the
// retained thread.
func (c *GenerationController) StartExitInterviewTurn(question string) (<-chan TurnOutcome, error) {
	if c == nil {
		return nil, errors.New("CodexOS generation controller is nil")
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if err := c.requireOpen(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("CodexOS generation controller is closed")
	}
	if c.active != nil {
		c.mu.Unlock()
		return nil, errors.New("exit interview turn is already active")
	}
	if !c.interviewOpen {
		c.mu.Unlock()
		return nil, errors.New("exit interview is not active")
	}
	session := c.session
	if session == nil {
		c.mu.Unlock()
		return nil, errors.New("exit interview session is unavailable")
	}
	turn := &controlledTurn{done: make(chan struct{}), outcome: make(chan TurnOutcome, 1), interview: true}
	c.active = turn
	c.mu.Unlock()
	go func() {
		result, err := session.RunExitInterviewTurn(question)
		c.finishTurn(turn, session, TurnOutcome{Result: result, Err: err})
	}()
	return turn.outcome, nil
}

// ExitInterviewTranscript returns a copy safe for durable publication by the
// concrete operator.
func (c *GenerationController) ExitInterviewTranscript() (provenance.ExitInterviewTranscriptSnapshot, bool) {
	if c == nil {
		return provenance.ExitInterviewTranscriptSnapshot{}, false
	}
	c.mu.Lock()
	session, cached, cachedOK := c.session, c.lastInterview, c.interviewSet
	c.mu.Unlock()
	if session != nil {
		if transcript, ok := session.ExitInterviewTranscript(); ok {
			return transcript, true
		}
	}
	return cached, cachedOK
}

// EndExitInterview permanently retires the retained thread. Transcript
// publication remains a separate trusted operator action.
func (c *GenerationController) EndExitInterview() error {
	if c == nil {
		return nil
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if err := c.requireOpen(); err != nil {
		return err
	}
	c.mu.Lock()
	session, active, interviewOpen := c.session, c.active, c.interviewOpen
	c.mu.Unlock()
	if session == nil || !interviewOpen {
		return errors.New("exit interview is not active")
	}
	if active != nil && !active.interview {
		return errors.New("Codex implementor turn is still active")
	}
	if active != nil {
		// Python treats explicit interview end as a hard, bounded retirement:
		// interrupt the active answer, retain its partial transcript, and close
		// the thread. Interrupt errors are recoverable if Close still succeeds.
		_ = session.InterruptTurn(c.options.InterruptTimeout)
		deadline := time.Now().Add(c.options.InterruptTimeout)
		if err := waitControlledTurn(context.Background(), active.done, deadline); err != nil {
			closeErr := session.Close()
			c.finishInterviewRetirement(session)
			if closeErr != nil {
				c.rememberRetirementError(closeErr)
			}
			return errors.Join(err, closeErr)
		}
	}
	transcript, transcriptOK := session.ExitInterviewTranscript()
	endErr := session.EndExitInterview()
	var closeErr error
	if endErr != nil {
		closeErr = session.Close()
	}
	c.mu.Lock()
	if c.session == session {
		if transcriptOK {
			c.lastInterview = transcript
			c.interviewSet = true
		}
		c.session = nil
		c.generationSet = false
		c.interviewOpen = false
	}
	if closeErr != nil {
		c.retirementErr = closeErr
	}
	c.mu.Unlock()
	return closeErr
}

func (c *GenerationController) finishInterviewRetirement(session *agent.GenerationSession) {
	transcript, ok := session.ExitInterviewTranscript()
	c.mu.Lock()
	if c.session == session {
		if ok {
			c.lastInterview = transcript
			c.interviewSet = true
		}
		c.session = nil
		c.generationSet = false
		c.interviewOpen = false
		c.active = nil
	}
	c.mu.Unlock()
}

func (c *GenerationController) rememberRetirementError(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	c.retirementErr = err
	c.mu.Unlock()
}

func (c *GenerationController) SessionPID() (int, bool) {
	if c == nil {
		return 0, false
	}
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session == nil {
		return 0, false
	}
	return session.ProcessPID()
}

// SessionOwned reports allocation independently of whether the subprocess has
// progressed far enough to expose a PID.
func (c *GenerationController) SessionOwned() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session != nil
}

func (c *GenerationController) SessionThreadID() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session == nil {
		return ""
	}
	return session.ThreadID()
}

func (c *GenerationController) ReviewYieldState() agent.ReviewYieldState {
	if c == nil {
		return agent.ReviewYieldIdle
	}
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session == nil {
		return agent.ReviewYieldIdle
	}
	return session.ReviewYieldState()
}

// ActiveTurnPhase reports the live generation phase from the session rather
// than the broader operator command that originally started it.
func (c *GenerationController) ActiveTurnPhase() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session == nil {
		return ""
	}
	return session.ActiveTurnPhase()
}

// NextTurnKind reports how StartTurn would use the current generation session.
// It is operator presentation state, not an admission reservation.
func (c *GenerationController) NextTurnKind() string {
	if c == nil {
		return "initial"
	}
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session == nil {
		return "initial"
	}
	if session.PlanningRetryRequired() {
		return "planning failed"
	}
	if !session.PlanningCompleted() {
		return "planning"
	}
	return "continuation"
}

func (c *GenerationController) InterviewOpen() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.interviewOpen
}

func (c *GenerationController) retireIdleGateSession() error {
	c.mu.Lock()
	session, active := c.session, c.active
	c.mu.Unlock()
	if active != nil {
		return errors.New("previous generation Codex turn is still active")
	}
	if session == nil {
		return nil
	}
	mode := session.Mode()
	if mode != agent.GenerationModeRetainedAtGate && mode != agent.GenerationModeClosed {
		return errors.New("previous generation Codex session is not idle at the gate")
	}
	closeErr := session.Close()
	transcript, transcriptOK := session.ExitInterviewTranscript()
	c.mu.Lock()
	if c.session == session {
		if transcriptOK {
			c.lastInterview = transcript
			c.interviewSet = true
		}
		c.session = nil
		c.generationSet = false
		c.interviewOpen = false
	}
	if closeErr != nil {
		c.retirementErr = closeErr
	}
	c.mu.Unlock()
	return closeErr
}

func (c *GenerationController) retireSession(interrupt bool) error {
	c.mu.Lock()
	session, turn := c.session, c.active
	c.mu.Unlock()
	if session == nil {
		return nil
	}
	if interrupt && turn != nil {
		// Interrupt failure already fail-closes the session. Close below remains
		// authoritative and mirrors the Python operator's hard-retirement path.
		_ = session.InterruptTurn(c.options.InterruptTimeout)
	}
	closeErr := session.Close()
	if turn != nil {
		deadline := time.Now().Add(c.options.InterruptTimeout)
		if waitErr := waitControlledTurn(context.Background(), turn.done, deadline); closeErr == nil {
			closeErr = waitErr
		}
	}
	transcript, transcriptOK := session.ExitInterviewTranscript()
	c.mu.Lock()
	if c.session == session {
		if transcriptOK {
			c.lastInterview = transcript
			c.interviewSet = true
		}
		c.session = nil
		c.generationSet = false
		c.interviewOpen = false
	}
	if c.active == turn {
		c.active = nil
	}
	if closeErr != nil {
		c.retirementErr = closeErr
	}
	c.mu.Unlock()
	return closeErr
}

func (c *GenerationController) requireOpen() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("CodexOS generation controller is closed")
	}
	if c.retirementErr != nil {
		return fmt.Errorf("Codex session retirement previously failed: %w", c.retirementErr)
	}
	return nil
}

func (c *GenerationController) clearGenerationOwnership() {
	c.mu.Lock()
	c.session = nil
	c.generationSet = false
	c.unavailable = nil
	c.active = nil
	c.resumeTurn = false
	c.interviewOpen = false
	c.lastInterview = provenance.ExitInterviewTranscriptSnapshot{}
	c.interviewSet = false
	c.mu.Unlock()
}

// Close retires Codex before QEMU and is idempotent. Both cleanup paths are
// attempted so a session error cannot orphan the live runtime.
func (c *GenerationController) Close() error {
	if c == nil {
		return nil
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	c.mu.Lock()
	if c.closed {
		if c.runtimeClosed {
			err := c.retirementErr
			c.mu.Unlock()
			return err
		}
		c.mu.Unlock()
		runtimeErr := c.runtime.Close()
		c.mu.Lock()
		if runtimeErr == nil {
			c.runtimeClosed = true
		}
		sessionErr := c.retirementErr
		c.mu.Unlock()
		return errors.Join(sessionErr, runtimeErr)
	}
	c.closed = true
	c.mu.Unlock()
	sessionErr := c.retireSession(true)
	runtimeErr := c.runtime.Close()
	c.mu.Lock()
	if runtimeErr == nil {
		c.runtimeClosed = true
	}
	c.mu.Unlock()
	return errors.Join(sessionErr, runtimeErr)
}

func waitControlledTurn(ctx context.Context, done <-chan struct{}, deadline time.Time) error {
	remaining := time.Until(deadline)
	if remaining < 0 {
		remaining = 0
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("Codex turn cleanup did not finish before timeout")
	}
}

func waitForTurnAdmission(ctx context.Context, session *agent.GenerationSession, done <-chan struct{}, deadline time.Time) (bool, error) {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(maxDuration(time.Until(deadline)))
	defer timer.Stop()
	for {
		if session.TurnAdmitted() {
			return true, nil
		}
		select {
		case <-done:
			return false, nil
		case <-ctx.Done():
			return false, ctx.Err()
		case <-timer.C:
			return false, errors.New("Codex turn admission did not finish before timeout")
		case <-ticker.C:
		}
	}
}

func maxDuration(value time.Duration) time.Duration {
	if value < 0 {
		return 0
	}
	return value
}
