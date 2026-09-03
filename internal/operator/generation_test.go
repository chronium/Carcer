package operator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"codexos/internal/agent"
	"codexos/internal/experiment"
	"codexos/internal/guest"
	"codexos/internal/observability"
)

type operatorTestRuntime struct {
	mu               sync.Mutex
	root             string
	state            string
	generation       uint64
	finishRetained   bool
	pauseCalls       int
	resumeCalls      int
	continueCalls    int
	abortCalls       int
	closeCalls       int
	continueCheck    func() error
	pauseAttempts    int
	resumeAttempts   int
	continueAttempts int
	forkAttempts     int
	abortAttempts    int
	abortReason      string
}

func newOperatorTestRuntime(t *testing.T) *operatorTestRuntime {
	t.Helper()
	return &operatorTestRuntime{root: t.TempDir(), state: "running", generation: 12}
}

func (r *operatorTestRuntime) GenerationRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state == "running"
}

func (r *operatorTestRuntime) GenerationNumber() (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.generation, true
}

func (r *operatorTestRuntime) GenerationState() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

func (r *operatorTestRuntime) PresentationSnapshot() experiment.RunPresentationSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return experiment.RunPresentationSnapshot{
		RunDirectory: r.root, State: experiment.RuntimeState(r.state),
		Generation: r.generation, HasGeneration: true,
	}
}

func (r *operatorTestRuntime) ListTools(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []string{"finish_generation"}, nil
}

func (r *operatorTestRuntime) InvokeTool(ctx context.Context, name string, _ [][]byte) (guest.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return guest.ToolResult{}, err
	}
	if name != "finish_generation" {
		return guest.ToolResult{}, errors.New("unexpected operator test tool")
	}
	r.mu.Lock()
	r.state = "awaiting_next_generation"
	r.mu.Unlock()
	return guest.ToolResult{Status: 0}, nil
}

func (r *operatorTestRuntime) RunDirectory() string              { return r.root }
func (r *operatorTestRuntime) EventLog() *observability.EventLog { return nil }
func (r *operatorTestRuntime) Metrics() *observability.Metrics   { return nil }

func (r *operatorTestRuntime) RetainGenerationFinish(generation uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != "awaiting_next_generation" || r.finishRetained || generation != r.generation {
		return false
	}
	r.finishRetained = true
	return true
}

func (r *operatorTestRuntime) GenerationFinishRetained(generation uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.finishRetained && generation == r.generation && r.state == "awaiting_next_generation"
}

func (r *operatorTestRuntime) ReleaseGenerationFinish(generation uint64) {
	r.mu.Lock()
	if generation == r.generation {
		r.finishRetained = false
	}
	r.mu.Unlock()
}

func (r *operatorTestRuntime) Pause(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pauseAttempts++
	if r.state != "running" {
		return errors.New("generation is not running")
	}
	r.pauseCalls++
	r.state = "paused"
	return nil
}

func (r *operatorTestRuntime) Resume(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resumeAttempts++
	if r.state != "paused" {
		return errors.New("generation is not paused")
	}
	r.resumeCalls++
	r.state = "running"
	return nil
}

func (r *operatorTestRuntime) ContinueGeneration() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.continueAttempts++
	if r.continueCheck != nil {
		if err := r.continueCheck(); err != nil {
			return err
		}
	}
	if r.finishRetained {
		return errors.New("completed generation is retained for an exit interview")
	}
	if r.state != "awaiting_next_generation" {
		return errors.New("run is not awaiting a generation")
	}
	r.continueCalls++
	r.generation++
	r.state = "running"
	return nil
}

func (r *operatorTestRuntime) ForkFromGeneration(uint64) error {
	r.mu.Lock()
	r.forkAttempts++
	r.mu.Unlock()
	return errors.New("unused rollback")
}

func (r *operatorTestRuntime) AbortGeneration(reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.abortAttempts++
	r.abortCalls++
	r.abortReason = reason
	r.state = "awaiting_next_generation"
	return nil
}

func (r *operatorTestRuntime) Close() error {
	r.mu.Lock()
	r.closeCalls++
	r.state = "stopped"
	r.mu.Unlock()
	return nil
}

func TestOperatorHelperSubprocessCannotEnterTestSuite(t *testing.T) {
	root := t.TempDir()
	record := filepath.Join(root, "record.json")
	sentinel := filepath.Join(root, "suite-entered")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOperatorSuiteSentinel$", "-test.count=1")
	command.Env = operatorHelperEnvironmentValues(map[string]string{
		operatorHelperEnvironment: "1",
		operatorHelperMode:        "probe",
		operatorHelperRecord:      record,
		operatorHelperSentinel:    sentinel,
	})
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("bounded operator helper probe did not exit: %v", ctx.Err())
	}
	if err != nil {
		t.Fatalf("operator helper probe failed: %v\n%s", err, output)
	}
	if value := readOperatorRecord(t, record); value["mode"] != "probe" {
		t.Fatalf("operator helper record = %#v", value)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("operator helper entered the Go test suite: %v", err)
	}
}

func TestOperatorSuiteSentinel(t *testing.T) {
	if path := os.Getenv(operatorHelperSentinel); path != "" {
		if err := os.WriteFile(path, []byte("entered"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGenerationControllerPauseResumeReusesOneSession(t *testing.T) {
	runtime := newOperatorTestRuntime(t)
	ready := filepath.Join(t.TempDir(), "ready")
	record := filepath.Join(t.TempDir(), "record.json")
	setOperatorHelper(t, "pause", ready, record)
	controller := newTestGenerationController(t, runtime)
	t.Cleanup(func() { _ = controller.Close() })

	first, err := controller.StartTurn("")
	if err != nil {
		t.Fatal(err)
	}
	waitOperatorFile(t, ready, 3*time.Second)
	pid, ok := controller.SessionPID()
	if !ok {
		t.Fatal("Codex session PID is unavailable before pause")
	}
	threadID := controller.SessionThreadID()
	if threadID == "" {
		t.Fatal("Codex session thread ID is unavailable before pause")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := controller.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	firstOutcome := receiveTurnOutcome(t, first)
	if firstOutcome.Err != nil || firstOutcome.Result.TurnStatus != "interrupted" {
		t.Fatalf("paused turn outcome = %#v", firstOutcome)
	}
	if runtime.GenerationState() != "paused" || runtime.pauseCalls != 1 {
		t.Fatalf("pause state/calls = %q/%d", runtime.GenerationState(), runtime.pauseCalls)
	}

	resumed, err := controller.Resume(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if resumed == nil {
		t.Fatal("resume did not continue the interrupted Codex turn")
	}
	resumedOutcome := receiveTurnOutcome(t, resumed)
	if resumedOutcome.Err != nil || resumedOutcome.Result.TurnStatus != "completed" {
		t.Fatalf("resumed turn outcome = %#v", resumedOutcome)
	}
	resumedPID, ok := controller.SessionPID()
	if !ok || resumedPID != pid {
		t.Fatalf("resumed session PID = %d, %v; want %d", resumedPID, ok, pid)
	}
	if got := controller.SessionThreadID(); got != threadID {
		t.Fatalf("resumed thread ID = %q, want %q", got, threadID)
	}
	if runtime.GenerationState() != "running" || runtime.resumeCalls != 1 {
		t.Fatalf("resume state/calls = %q/%d", runtime.GenerationState(), runtime.resumeCalls)
	}
	waitOperatorFile(t, record, 3*time.Second)
	value := readOperatorRecord(t, record)
	messages, _ := value["messages"].([]any)
	var threadStarts, turnStarts, interrupts int
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		switch message["method"] {
		case "thread/start":
			threadStarts++
		case "turn/start":
			turnStarts++
		case "turn/interrupt":
			interrupts++
		}
	}
	if threadStarts != 1 || turnStarts != 3 || interrupts != 1 {
		t.Fatalf("helper protocol counts thread/turn/interrupt = %d/%d/%d", threadStarts, turnStarts, interrupts)
	}
}

func TestGenerationControllerImmediatePauseWaitsForTurnAdmission(t *testing.T) {
	runtime := newOperatorTestRuntime(t)
	ready := filepath.Join(t.TempDir(), "ready")
	record := filepath.Join(t.TempDir(), "record.json")
	setOperatorHelper(t, "admission-pause", ready, record)
	controller := newTestGenerationController(t, runtime)
	t.Cleanup(func() { _ = controller.Close() })

	outcomes, err := controller.StartTurn("")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := controller.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	outcome := receiveTurnOutcome(t, outcomes)
	if outcome.Err != nil || outcome.Result.TurnStatus != "interrupted" {
		t.Fatalf("immediately paused turn outcome = %#v, error = %v", outcome, outcome.Err)
	}
	if runtime.pauseCalls != 1 || runtime.GenerationState() != "paused" {
		t.Fatalf("runtime pause calls/state = %d/%q", runtime.pauseCalls, runtime.GenerationState())
	}
	waitOperatorFile(t, record, 3*time.Second)
}

func TestGenerationControllerDoesNotPauseBeforeFailedInterruptQuiesces(t *testing.T) {
	runtime := newOperatorTestRuntime(t)
	ready := filepath.Join(t.TempDir(), "ready")
	record := filepath.Join(t.TempDir(), "record.json")
	setOperatorHelper(t, "stuck-interrupt", ready, record)
	controller := newTestGenerationControllerWithTimeout(t, runtime, 100*time.Millisecond)
	t.Cleanup(func() { _ = controller.Close() })

	outcomes, err := controller.StartTurn("")
	if err != nil {
		t.Fatal(err)
	}
	waitOperatorFile(t, ready, 3*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := controller.Pause(ctx); err == nil {
		t.Fatal("pause unexpectedly succeeded after a stuck Codex interrupt")
	}
	if runtime.pauseCalls != 0 || runtime.GenerationState() != "running" {
		t.Fatalf("runtime was touched after failed interrupt: calls/state = %d/%q", runtime.pauseCalls, runtime.GenerationState())
	}
	outcome := receiveTurnOutcome(t, outcomes)
	if outcome.Err == nil {
		t.Fatalf("stuck interrupted turn outcome = %#v", outcome)
	}
}

func TestGenerationControllerRetainsGateThenRetiresBeforeSuccessor(t *testing.T) {
	runtime := newOperatorTestRuntime(t)
	ready := filepath.Join(t.TempDir(), "ready")
	record := filepath.Join(t.TempDir(), "record.json")
	setOperatorHelper(t, "finish", ready, record)
	controller := newTestGenerationController(t, runtime)
	t.Cleanup(func() { _ = controller.Close() })

	outcomes, err := controller.StartTurn("")
	if err != nil {
		t.Fatal(err)
	}
	outcome := receiveTurnOutcome(t, outcomes)
	if outcome.Err != nil || outcome.Result.TurnStatus != "completed" || !outcome.Retained {
		t.Fatalf("completion outcome = %#v", outcome)
	}
	if !controller.ExitInterviewAvailable() || !runtime.GenerationFinishRetained(12) {
		t.Fatal("completed session was not retained at the frozen gate")
	}
	oldPID, ok := controller.SessionPID()
	if !ok {
		t.Fatal("retained session PID is unavailable")
	}
	if err := runtime.ContinueGeneration(); err == nil {
		t.Fatal("runtime bypassed the retained-session gate lease")
	}
	runtime.mu.Lock()
	runtime.continueCheck = func() error {
		if operatorProcessAlive(oldPID) {
			return errors.New("predecessor Codex process is still alive")
		}
		return nil
	}
	runtime.mu.Unlock()
	if err := controller.ContinueGeneration(); err != nil {
		t.Fatal(err)
	}
	if runtime.GenerationState() != "running" || runtime.generation != 13 || runtime.continueCalls != 1 {
		t.Fatalf("continued runtime = state %q generation %d calls %d", runtime.GenerationState(), runtime.generation, runtime.continueCalls)
	}
	if _, ok := controller.SessionPID(); ok {
		t.Fatal("previous generation session still has an owned process")
	}
	waitOperatorProcessExit(t, oldPID, 3*time.Second)

	// A successor turn must create a fresh process and thread, never reuse the
	// retained predecessor conversation.
	second, err := controller.StartTurn("")
	if err != nil {
		t.Fatal(err)
	}
	waitOperatorFile(t, ready, 3*time.Second)
	newPID := waitOperatorSessionPID(t, controller, 3*time.Second)
	if newPID == oldPID {
		t.Fatalf("successor reused predecessor app-server PID %d", oldPID)
	}
	secondOutcome := receiveTurnOutcome(t, second)
	if secondOutcome.Err != nil || !secondOutcome.Retained {
		t.Fatalf("successor completion outcome = %#v", secondOutcome)
	}
}

func TestGenerationControllerRejectsLifecycleCommandsAfterClose(t *testing.T) {
	runtime := newOperatorTestRuntime(t)
	controller := newTestGenerationController(t, runtime)
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := controller.Resume(ctx); err == nil {
		t.Fatal("closed controller resumed the runtime")
	}
	for name, call := range map[string]func() error{
		"pause":    func() error { return controller.Pause(ctx) },
		"continue": controller.ContinueGeneration,
		"rollback": func() error { return controller.Rollback(0) },
		"abort":    func() error { return controller.Abort("operator stopped the generation") },
	} {
		if err := call(); err == nil {
			t.Fatalf("closed controller accepted %s", name)
		}
	}
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	attempts := [5]int{
		runtime.pauseAttempts,
		runtime.resumeAttempts,
		runtime.continueAttempts,
		runtime.forkAttempts,
		runtime.abortAttempts,
	}
	closeCalls := runtime.closeCalls
	runtime.mu.Unlock()
	if attempts != [5]int{} {
		t.Fatalf("closed controller reached runtime mutations: %#v", attempts)
	}
	if closeCalls != 1 {
		t.Fatalf("idempotent close called runtime %d times", closeCalls)
	}
}

func newTestGenerationController(t *testing.T, runtime *operatorTestRuntime) *GenerationController {
	return newTestGenerationControllerWithTimeout(t, runtime, 2*time.Second)
}

func newTestGenerationControllerWithTimeout(t *testing.T, runtime *operatorTestRuntime, interruptTimeout time.Duration) *GenerationController {
	t.Helper()
	auth := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(auth, []byte("fake login"), 0o600); err != nil {
		t.Fatal(err)
	}
	controller, err := newGenerationController(runtime, GenerationControllerOptions{
		Session: agent.GenerationSessionOptions{
			Executable: os.Args[0], AuthFile: auth, StopTimeout: time.Second,
		},
		InterruptTimeout: interruptTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func setOperatorHelper(t *testing.T, mode, ready, record string) {
	t.Helper()
	t.Setenv(operatorHelperEnvironment, "1")
	t.Setenv(operatorHelperMode, mode)
	t.Setenv(operatorHelperReady, ready)
	t.Setenv(operatorHelperRecord, record)
}

func operatorHelperEnvironmentValues(overrides map[string]string) []string {
	environment := append([]string(nil), os.Environ()...)
	for name, value := range overrides {
		prefix := name + "="
		for index := 0; index < len(environment); index++ {
			if len(environment[index]) >= len(prefix) && environment[index][:len(prefix)] == prefix {
				environment = append(environment[:index], environment[index+1:]...)
				index--
			}
		}
		environment = append(environment, prefix+value)
	}
	return environment
}

func receiveTurnOutcome(t *testing.T, outcomes <-chan TurnOutcome) TurnOutcome {
	t.Helper()
	select {
	case outcome, ok := <-outcomes:
		if !ok {
			t.Fatal("turn outcome channel closed without a result")
		}
		return outcome
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for Codex turn outcome")
		return TurnOutcome{}
	}
}

func waitOperatorFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		runtime.Gosched()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for helper file %s", path)
}

func readOperatorRecord(t *testing.T, path string) map[string]any {
	t.Helper()
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func waitOperatorSessionPID(t *testing.T, controller *GenerationController, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pid, ok := controller.SessionPID(); ok {
			return pid
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for Codex session PID")
	return 0
}

func waitOperatorProcessExit(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !operatorProcessAlive(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("helper process %d survived retirement", pid)
}

func operatorProcessAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	return err == nil && process.Signal(syscall.Signal(0)) == nil
}
