package operator

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codexos/internal/agent"
	"codexos/internal/experiment"
	"codexos/internal/guest"
	"codexos/internal/provenance"
	"codexos/internal/qemu"
	"codexos/internal/store"
)

type consoleTestRuntime struct {
	*operatorTestRuntime
	pending  *experiment.PendingGenerationFinish
	handoff  *string
	archives []experiment.ArchivedGeneration
	features []store.FeatureRequest
	pid      int
}

func newConsoleTestRuntime(t *testing.T) *consoleTestRuntime {
	t.Helper()
	return &consoleTestRuntime{operatorTestRuntime: newOperatorTestRuntime(t), pid: 4242}
}

func (r *consoleTestRuntime) State() experiment.RuntimeState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return experiment.RuntimeState(r.state)
}

func (r *consoleTestRuntime) ActivePID() (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pid, r.state == string(experiment.RuntimeStateRunning) || r.state == string(experiment.RuntimeStatePaused)
}

func (r *consoleTestRuntime) HardwareProfile() qemu.HardwareProfile {
	return qemu.TestHardwareProfile
}

func (r *consoleTestRuntime) PendingGenerationFinish() (*experiment.PendingGenerationFinish, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil {
		return nil, false
	}
	copy := *r.pending
	copy.SourceSnapshot = append([]byte(nil), copy.SourceSnapshot...)
	return &copy, true
}

func (r *consoleTestRuntime) PreviousHandoff() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handoff == nil {
		return "", false
	}
	return *r.handoff, true
}

func (r *consoleTestRuntime) ArchivedGenerations() ([]experiment.ArchivedGeneration, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]experiment.ArchivedGeneration(nil), r.archives...), nil
}

func (r *consoleTestRuntime) InspectGeneration(generation uint64) (experiment.ArchivedGeneration, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, item := range r.archives {
		if item.Generation == generation {
			return item, nil
		}
	}
	return experiment.ArchivedGeneration{}, errors.New("generation is not archived")
}

func (r *consoleTestRuntime) FeatureRequests() ([]store.FeatureRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]store.FeatureRequest(nil), r.features...), nil
}

func (r *consoleTestRuntime) FeatureRequest(requestID uint64) (store.FeatureRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, request := range r.features {
		if request.ID == requestID {
			return request, nil
		}
	}
	return store.FeatureRequest{}, errors.New("feature request does not exist")
}

func (r *consoleTestRuntime) ApproveFeatureRequest(requestID uint64) (store.FeatureRequest, error) {
	return r.decideFeature(requestID, store.FeatureApproved)
}

func (r *consoleTestRuntime) DenyFeatureRequest(requestID uint64) (store.FeatureRequest, error) {
	return r.decideFeature(requestID, store.FeatureDenied)
}

func (r *consoleTestRuntime) decideFeature(requestID uint64, status string) (store.FeatureRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != string(experiment.RuntimeStateAwaitingNextGeneration) {
		return store.FeatureRequest{}, errors.New("feature requests can be decided only at a generation gate")
	}
	for index := range r.features {
		if r.features[index].ID == requestID {
			r.features[index].Status = status
			return r.features[index], nil
		}
	}
	return store.FeatureRequest{}, errors.New("feature request does not exist")
}

func (r *consoleTestRuntime) InvokeTool(ctx context.Context, name string, arguments [][]byte) (guest.ToolResult, error) {
	result, err := r.operatorTestRuntime.InvokeTool(ctx, name, arguments)
	if err != nil || name != "finish_generation" {
		return result, err
	}
	r.mu.Lock()
	handoff := "next"
	r.pending = &experiment.PendingGenerationFinish{HandoffMessage: handoff, SourceSnapshot: []byte("snapshot"), KernelELF: "kernel.elf", ISO: "codexos.iso"}
	r.handoff = &handoff
	r.mu.Unlock()
	return result, nil
}

func (r *consoleTestRuntime) ContinueGeneration() error {
	if err := r.operatorTestRuntime.ContinueGeneration(); err != nil {
		return err
	}
	r.mu.Lock()
	r.pending = nil
	r.mu.Unlock()
	return nil
}

func (r *consoleTestRuntime) ForkFromGeneration(uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forkAttempts++
	if r.state != string(experiment.RuntimeStateAwaitingNextGeneration) || r.finishRetained {
		return errors.New("run is not available for rollback")
	}
	r.generation++
	r.state = string(experiment.RuntimeStateRunning)
	r.pending = nil
	return nil
}

func newTestPlainConsole(t *testing.T, runtime *consoleTestRuntime, input *strings.Reader, output *bytes.Buffer, interviews *provenance.ExitInterviewArtifactStore) *PlainConsole {
	t.Helper()
	auth := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(auth, []byte("fake login"), 0o600); err != nil {
		t.Fatal(err)
	}
	console, err := newPlainConsole(runtime, PlainConsoleOptions{
		Input: input, Output: output, InterviewStore: interviews,
		Controller: GenerationControllerOptions{
			Session: agent.GenerationSessionOptions{
				Executable: os.Args[0], AuthFile: auth, StopTimeout: time.Second,
			},
			InterruptTimeout: 2 * time.Second,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return console
}

func executeConsoleLine(t *testing.T, console *PlainConsole, line string) bool {
	t.Helper()
	quit, err := console.ExecuteLine(context.Background(), line)
	if err != nil {
		t.Fatalf("execute %q: %v", line, err)
	}
	return quit
}

func waitConsoleIdle(t *testing.T, console *PlainConsole, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if console.currentTurn() == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for console turn")
}

func TestPlainConsoleCommandsExposeStateAndRequireLiteralConfirmation(t *testing.T) {
	runtime := newConsoleTestRuntime(t)
	handoff := "archived handoff"
	runtime.archives = []experiment.ArchivedGeneration{{
		Generation: 11, Transition: "continue", Outcome: "completed", ArchivePath: filepath.Join(runtime.root, "generations", "generation-0011"), Handoff: &handoff,
		Hardware: qemu.HardwareManifest{Profile: "test-v1", Machine: "q35", CPUModel: "qemu64", VCPUs: 1, MemoryMiB: 128, Graphics: "std-vga", Network: "none"},
	}}
	runtime.features = []store.FeatureRequest{{ID: 7, Generation: 11, Status: store.FeaturePending, Title: "Needs \x1b[31mdevice", Description: "Trusted capability"}}
	input := strings.NewReader("yes\nY\nn\n")
	var output bytes.Buffer
	console := newTestPlainConsole(t, runtime, input, &output, nil)
	t.Cleanup(func() { _ = console.Shutdown() })

	executeConsoleLine(t, console, "status")
	executeConsoleLine(t, console, "history")
	executeConsoleLine(t, console, "inspect 11")
	executeConsoleLine(t, console, "features")
	executeConsoleLine(t, console, "feature 7")
	executeConsoleLine(t, console, "inspect -1")

	runtime.mu.Lock()
	runtime.state = string(experiment.RuntimeStateAwaitingNextGeneration)
	runtime.pending = &experiment.PendingGenerationFinish{HandoffMessage: handoff}
	runtime.mu.Unlock()
	executeConsoleLine(t, console, "feature-approve 7")
	if request, _ := runtime.FeatureRequest(7); request.Status != store.FeaturePending {
		t.Fatalf("non-literal confirmation changed request: %#v", request)
	}
	executeConsoleLine(t, console, "feature-approve 7")
	executeConsoleLine(t, console, "feature-deny 7")
	executeConsoleLine(t, console, "interview")

	text := output.String()
	for _, want := range []string{
		"Generation: 12", "State: RUNNING", "Hardware profile: test-v1",
		"GEN   PARENT   TRANSITION   OUTCOME", "Outcome: completed",
		"Feature request: #7", "Feature approval cancelled.", "Feature request #7 approved.",
		"Feature denial cancelled.", "Usage: inspect N",
		"No live generation session is available for an exit interview.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("console output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "\x1b[31m") {
		t.Fatalf("console emitted a raw terminal escape:\n%s", text)
	}
	if strings.Contains(text, "Observability:") {
		t.Fatalf("console reported unconfigured observability:\n%s", text)
	}
}

func TestPlainConsoleRunExecutesUnterminatedFinalCommand(t *testing.T) {
	runtime := newConsoleTestRuntime(t)
	var output bytes.Buffer
	console := newTestPlainConsole(t, runtime, strings.NewReader("help"), &output, nil)
	if err := console.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "status      show current runtime state") || !strings.Contains(output.String(), "Input closed; stopping CodexOS run.") {
		t.Fatalf("unterminated command was not executed before EOF:\n%s", output.String())
	}
	runtime.mu.Lock()
	closeCalls := runtime.closeCalls
	runtime.mu.Unlock()
	if closeCalls != 1 {
		t.Fatalf("console shutdown calls = %d, want 1", closeCalls)
	}
}

func TestPlainConsolePauseResumeTracksTheSameSession(t *testing.T) {
	runtime := newConsoleTestRuntime(t)
	ready := filepath.Join(t.TempDir(), "ready")
	record := filepath.Join(t.TempDir(), "record.json")
	setOperatorHelper(t, "pause", ready, record)
	var output bytes.Buffer
	console := newTestPlainConsole(t, runtime, strings.NewReader(""), &output, nil)
	t.Cleanup(func() { _ = console.Shutdown() })

	executeConsoleLine(t, console, "agent")
	waitOperatorFile(t, ready, 3*time.Second)
	pid := waitOperatorSessionPID(t, console.controller, 3*time.Second)
	threadID := console.controller.SessionThreadID()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := console.ExecuteLine(ctx, "pause"); err != nil {
		t.Fatal(err)
	}
	if _, err := console.ExecuteLine(ctx, "resume"); err != nil {
		t.Fatal(err)
	}
	waitConsoleIdle(t, console, 3*time.Second)
	resumedPID, ok := console.controller.SessionPID()
	if !ok || resumedPID != pid || console.controller.SessionThreadID() != threadID {
		t.Fatalf("resume replaced session: PID %d/%d, thread %q/%q", pid, resumedPID, threadID, console.controller.SessionThreadID())
	}
	if runtime.pauseCalls != 1 || runtime.resumeCalls != 1 {
		t.Fatalf("runtime pause/resume calls = %d/%d", runtime.pauseCalls, runtime.resumeCalls)
	}
	for _, want := range []string{"Generation 12 paused.", "Codex continued in the same session."} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("console output missing %q:\n%s", want, output.String())
		}
	}
}

func TestPlainConsolePersistsCompletedExitInterviewAndRetiresSession(t *testing.T) {
	runtime := newConsoleTestRuntime(t)
	ready := filepath.Join(t.TempDir(), "ready")
	record := filepath.Join(t.TempDir(), "record.json")
	setOperatorHelper(t, "interview", ready, record)
	repository := t.TempDir()
	interviews, err := provenance.NewExitInterviewArtifactStore(repository, runtime.root)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	console := newTestPlainConsole(t, runtime, strings.NewReader(""), &output, interviews)
	t.Cleanup(func() { _ = console.Shutdown() })

	executeConsoleLine(t, console, "agent")
	waitConsoleIdle(t, console, 4*time.Second)
	pid := waitOperatorSessionPID(t, console.controller, time.Second)
	executeConsoleLine(t, console, "interview")
	executeConsoleLine(t, console, "What did you verify?")
	waitConsoleIdle(t, console, 4*time.Second)
	executeConsoleLine(t, console, "end")
	waitOperatorProcessExit(t, pid, 3*time.Second)

	artifact := filepath.Join(repository, "artifacts", "interviews", filepath.Base(runtime.root), "generation-0012.md")
	contents, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	text := string(contents)
	for _, want := range []string{"Interview status: completed", "What did you verify?", "Retrospective answer."} {
		if !strings.Contains(text, want) {
			t.Fatalf("interview artifact missing %q:\n%s", want, text)
		}
	}
	if _, ok := console.controller.SessionPID(); ok {
		t.Fatal("exit interview left the app-server process owned")
	}
}

func TestPlainConsoleEndInterruptsActiveInterviewAndPersistsPartialTurn(t *testing.T) {
	runtime := newConsoleTestRuntime(t)
	ready := filepath.Join(t.TempDir(), "ready")
	record := filepath.Join(t.TempDir(), "record.json")
	setOperatorHelper(t, "interview-hold", ready, record)
	repository := t.TempDir()
	interviews, err := provenance.NewExitInterviewArtifactStore(repository, runtime.root)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	console := newTestPlainConsole(t, runtime, strings.NewReader(""), &output, interviews)
	t.Cleanup(func() { _ = console.Shutdown() })

	executeConsoleLine(t, console, "agent")
	waitConsoleIdle(t, console, 4*time.Second)
	executeConsoleLine(t, console, "interview")
	if err := os.Remove(ready); err != nil {
		t.Fatal(err)
	}
	executeConsoleLine(t, console, "Why stop now?")
	waitOperatorFile(t, ready, 3*time.Second)
	executeConsoleLine(t, console, "end-interview")

	artifact := filepath.Join(repository, "artifacts", "interviews", filepath.Base(runtime.root), "generation-0012.md")
	contents, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	text := string(contents)
	for _, want := range []string{"Interview status: interrupted", "Why stop now?", "Partial retrospective.", "Turn status: interrupted"} {
		if !strings.Contains(text, want) {
			t.Fatalf("partial interview artifact missing %q:\n%s", want, text)
		}
	}
}

func TestPlainConsoleShutdownPersistsAndReapsActiveInterview(t *testing.T) {
	runtime := newConsoleTestRuntime(t)
	ready := filepath.Join(t.TempDir(), "ready")
	record := filepath.Join(t.TempDir(), "record.json")
	setOperatorHelper(t, "interview-hold", ready, record)
	repository := t.TempDir()
	interviews, err := provenance.NewExitInterviewArtifactStore(repository, runtime.root)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	console := newTestPlainConsole(t, runtime, strings.NewReader(""), &output, interviews)

	executeConsoleLine(t, console, "agent")
	waitConsoleIdle(t, console, 4*time.Second)
	pid := waitOperatorSessionPID(t, console.controller, time.Second)
	executeConsoleLine(t, console, "interview")
	if err := os.Remove(ready); err != nil {
		t.Fatal(err)
	}
	executeConsoleLine(t, console, "What remains?")
	waitOperatorFile(t, ready, 3*time.Second)
	if err := console.Shutdown(); err != nil {
		t.Fatal(err)
	}
	waitOperatorProcessExit(t, pid, 3*time.Second)

	artifact := filepath.Join(repository, "artifacts", "interviews", filepath.Base(runtime.root), "generation-0012.md")
	contents, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	text := string(contents)
	for _, want := range []string{"Interview status: interrupted", "What remains?", "Partial retrospective.", "Turn status: interrupted"} {
		if !strings.Contains(text, want) {
			t.Fatalf("shutdown interview artifact missing %q:\n%s", want, text)
		}
	}
	runtime.mu.Lock()
	closeCalls := runtime.closeCalls
	runtime.mu.Unlock()
	if closeCalls != 1 {
		t.Fatalf("runtime close calls = %d, want 1", closeCalls)
	}
}
