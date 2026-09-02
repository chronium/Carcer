package experiment

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"codexos/internal/build"
	"codexos/internal/guest"
	"codexos/internal/provenance"
	"codexos/internal/qemu"
)

const liveQEMUHelperEnvironment = "CODEXOS_GO_LIVE_QEMU_HELPER"

func TestLiveRunOwnsBootToolsPauseResumeAndStop(t *testing.T) {
	t.Setenv(liveQEMUHelperEnvironment, "1")
	runDirectory := liveTestRunDirectory(t)
	initialISO := filepath.Join(t.TempDir(), "initial.iso")
	if err := os.WriteFile(initialISO, []byte("synthetic boot image"), 0o600); err != nil {
		t.Fatal(err)
	}
	run, err := NewLiveCodexOSRun(runDirectory, LiveRunOptions{
		QEMUExecutable: os.Args[0], HardwareProfile: qemu.TestHardwareProfile,
		ReadyTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("new live run: %v", err)
	}
	t.Cleanup(run.Stop)
	startContext, cancelStart := context.WithTimeout(context.Background(), 5*time.Second)
	if err := run.Start(startContext, initialISO); err != nil {
		t.Fatalf("start live run: %v", err)
	}
	cancelStart()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if state := run.State(); state != RuntimeStateRunning {
		t.Fatalf("state after start = %q", state)
	}
	if number, ok := run.GenerationNumber(); !ok || number != 0 {
		t.Fatalf("generation after start = %d, %v", number, ok)
	}
	if _, ok := run.ActivePID(); !ok {
		t.Fatal("live run has no QEMU PID")
	}
	tools, err := run.ListTools(ctx)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if want := []string{"read", "write"}; !reflect.DeepEqual(tools, want) {
		t.Fatalf("tools = %#v, want %#v", tools, want)
	}
	result, err := run.InvokeTool(ctx, "read", [][]byte{[]byte("seed/kernel.c")})
	if err != nil {
		t.Fatalf("invoke tool: %v", err)
	}
	if result.Status != 0 || string(result.Output) != "tool:read" {
		t.Fatalf("tool result = %#v", result)
	}
	if err := run.Pause(ctx); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if state := run.State(); state != RuntimeStatePaused {
		t.Fatalf("state after pause = %q", state)
	}
	if err := run.Resume(ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}
	run.Stop()
	if state := run.State(); state != RuntimeStateStopped {
		t.Fatalf("state after stop = %q", state)
	}
	if _, ok := run.ActivePID(); ok {
		t.Fatal("QEMU PID survived stop")
	}
	workspaces, err := filepath.Glob(filepath.Join(runDirectory, ".generation-*-"+"*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(workspaces) != 0 {
		t.Fatalf("generation workspaces survived stop: %v", workspaces)
	}
}

func TestLiveRunStartupFailureReapsQEMUAndRemovesWorkspace(t *testing.T) {
	t.Setenv(liveQEMUHelperEnvironment, "no-ready")
	runDirectory := liveTestRunDirectory(t)
	initialISO := filepath.Join(t.TempDir(), "initial.iso")
	if err := os.WriteFile(initialISO, []byte("synthetic boot image"), 0o600); err != nil {
		t.Fatal(err)
	}
	run, err := NewLiveCodexOSRun(runDirectory, LiveRunOptions{
		QEMUExecutable: os.Args[0], HardwareProfile: qemu.TestHardwareProfile,
		ReadyTimeout: 40 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := run.Start(ctx, initialISO); err == nil || !strings.Contains(err.Error(), "timed out waiting") {
		t.Fatalf("start error = %v", err)
	}
	if state := run.State(); state != RuntimeStateStopped {
		t.Fatalf("state after failed start = %q", state)
	}
	if _, ok := run.ActivePID(); ok {
		t.Fatal("QEMU PID survived failed start")
	}
	workspaces, err := filepath.Glob(filepath.Join(runDirectory, ".generation-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(workspaces) != 0 {
		t.Fatalf("generation workspaces survived failed start: %v", workspaces)
	}
}

func TestLiveRunCompletesStreamsArchiveAndBootsSuccessor(t *testing.T) {
	run := startLiveTestRun(t, 2*time.Second)
	generation := run.liveGeneration()
	artifacts := t.TempDir()
	kernel := filepath.Join(artifacts, "kernel.elf")
	iso := filepath.Join(artifacts, "codexos.iso")
	if err := os.WriteFile(kernel, []byte("successor kernel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(iso, []byte("successor iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot := testSnapshot(t, "successor source")
	run.live.operationMu.Lock()
	err := run.completeLiveGeneration(generation, 0, &build.PendingGenerationFinish{
		HandoffMessage: "continue here", SourceSnapshot: snapshot, KernelELF: kernel, ISO: iso,
	}, liveValidatedArtifacts(t, kernel, iso, snapshot))
	run.live.operationMu.Unlock()
	if err != nil {
		t.Fatalf("complete generation: %v", err)
	}
	if state := run.State(); state != RuntimeStateAwaitingNextGeneration {
		t.Fatalf("state after completion = %q", state)
	}
	if _, ok := run.ActivePID(); ok {
		t.Fatal("QEMU PID survived completion")
	}
	archived, err := run.InspectGeneration(0)
	if err != nil {
		t.Fatalf("inspect completed archive: %v", err)
	}
	if archived.Outcome != "completed" || archived.Handoff == nil || *archived.Handoff != "continue here" {
		t.Fatalf("completed archive = %#v", archived)
	}
	if got, err := os.ReadFile(filepath.Join(archived.ArchivePath, successorName, "codexos.iso")); err != nil || string(got) != "successor iso" {
		t.Fatalf("archived successor ISO = %q, %v", got, err)
	}
	if err := run.ContinueGeneration(); err != nil {
		t.Fatalf("continue generation: %v", err)
	}
	if number, ok := run.GenerationNumber(); !ok || number != 1 {
		t.Fatalf("successor generation = %d, %v", number, ok)
	}
	if transition, ok := run.CurrentTransition(); !ok || transition != "successor" {
		t.Fatalf("successor transition = %q, %v", transition, ok)
	}
	if handoff, ok := run.PreviousHandoff(); !ok || handoff != "continue here" {
		t.Fatalf("successor handoff = %q, %v", handoff, ok)
	}
	if _, ok := run.ActivePID(); !ok {
		t.Fatal("successor QEMU is not running")
	}
}

func TestLiveRunAbortPausedGenerationStreamsArchive(t *testing.T) {
	run := startLiveTestRun(t, 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := run.Pause(ctx); err != nil {
		t.Fatalf("pause before abort: %v", err)
	}
	if err := run.AbortGeneration(); err != nil {
		t.Fatalf("abort generation: %v", err)
	}
	if state := run.State(); state != RuntimeStateAwaitingNextGeneration {
		t.Fatalf("state after abort = %q", state)
	}
	if _, ok := run.ActivePID(); ok {
		t.Fatal("QEMU PID survived abort")
	}
	archived, err := run.InspectGeneration(0)
	if err != nil {
		t.Fatalf("inspect aborted archive: %v", err)
	}
	if archived.Outcome != "aborted" {
		t.Fatalf("abort outcome = %q", archived.Outcome)
	}
	if err := run.ContinueGeneration(); err == nil || !strings.Contains(err.Error(), "no selected successor") {
		t.Fatalf("continue after abort error = %v", err)
	}
}

func TestLiveRunFailedSuccessorBootPreservesFrozenGate(t *testing.T) {
	run := startLiveTestRun(t, 60*time.Millisecond)
	generation := run.liveGeneration()
	artifacts := t.TempDir()
	kernel := filepath.Join(artifacts, "kernel.elf")
	iso := filepath.Join(artifacts, "codexos.iso")
	if err := os.WriteFile(kernel, []byte("kernel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(iso, []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	run.live.operationMu.Lock()
	snapshot := testSnapshot(t, "source")
	err := run.completeLiveGeneration(generation, 0, &build.PendingGenerationFinish{
		HandoffMessage: "preserve me", SourceSnapshot: snapshot, KernelELF: kernel, ISO: iso,
	}, liveValidatedArtifacts(t, kernel, iso, snapshot))
	run.live.operationMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(liveQEMUHelperEnvironment, "no-ready")
	if err := run.ContinueGeneration(); err == nil || !strings.Contains(err.Error(), "timed out waiting") {
		t.Fatalf("failed successor error = %v", err)
	}
	if state := run.State(); state != RuntimeStateAwaitingNextGeneration {
		t.Fatalf("state after failed successor = %q", state)
	}
	if number, ok := run.GenerationNumber(); !ok || number != 0 {
		t.Fatalf("generation changed after failed successor = %d, %v", number, ok)
	}
	pending, ok := run.PendingGenerationFinish()
	if !ok || pending.HandoffMessage != "preserve me" {
		t.Fatalf("pending finish after failed successor = %#v, %v", pending, ok)
	}
	if run.transitioning {
		t.Fatal("failed successor left transition reserved")
	}
}

func TestLiveRunCloseCancelsBlockedToolExchangeBeforeJoining(t *testing.T) {
	run := startLiveTestRunMode(t, 2*time.Second, "stall-invoke")
	invoked := make(chan error, 1)
	go func() {
		_, err := run.InvokeTool(context.Background(), "read", [][]byte{[]byte("seed/kernel.c")})
		invoked <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if !run.live.operationMu.TryLock() {
			break
		}
		run.live.operationMu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("tool exchange did not start")
		}
		time.Sleep(time.Millisecond)
	}
	started := time.Now()
	if err := run.Close(); err != nil {
		t.Fatalf("close live run: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("close took %s", elapsed)
	}
	select {
	case err := <-invoked:
		if err == nil {
			t.Fatal("blocked invocation unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked invocation survived close")
	}
}

func TestLiveRunRejectsSuccessorChangedAfterValidation(t *testing.T) {
	run := startLiveTestRun(t, 2*time.Second)
	generation := run.liveGeneration()
	artifacts := t.TempDir()
	kernel := filepath.Join(artifacts, "kernel.elf")
	iso := filepath.Join(artifacts, "codexos.iso")
	if err := os.WriteFile(kernel, []byte("validated kernel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(iso, []byte("validated iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot := testSnapshot(t, "validated source")
	validated := liveValidatedArtifacts(t, kernel, iso, snapshot)
	if err := os.WriteFile(iso, []byte("replaced after proof"), 0o600); err != nil {
		t.Fatal(err)
	}
	run.live.operationMu.Lock()
	err := run.completeLiveGeneration(generation, 0, &build.PendingGenerationFinish{
		HandoffMessage: "must fail", SourceSnapshot: snapshot, KernelELF: kernel, ISO: iso,
	}, validated)
	run.live.operationMu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "changed before archival") {
		t.Fatalf("completion error = %v", err)
	}
	if _, inspectErr := run.InspectGeneration(0); inspectErr == nil {
		t.Fatal("changed successor produced a completed archive")
	}
	if state := run.State(); state != RuntimeStateStopped {
		t.Fatalf("state after successor identity failure = %q", state)
	}
}

func TestLiveRunReopensFrozenGateBeforeBootingSuccessor(t *testing.T) {
	run := startLiveTestRun(t, 2*time.Second)
	generation := run.liveGeneration()
	artifacts := t.TempDir()
	kernel := filepath.Join(artifacts, "kernel.elf")
	iso := filepath.Join(artifacts, "codexos.iso")
	if err := os.WriteFile(kernel, []byte("reopen kernel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(iso, []byte("reopen iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot := testSnapshot(t, "reopen source")
	run.live.operationMu.Lock()
	err := run.completeLiveGeneration(generation, 0, &build.PendingGenerationFinish{
		HandoffMessage: "reopened handoff", SourceSnapshot: snapshot, KernelELF: kernel, ISO: iso,
	}, liveValidatedArtifacts(t, kernel, iso, snapshot))
	run.live.operationMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewLiveCodexOSRun(run.RunDirectory(), LiveRunOptions{
		QEMUExecutable: os.Args[0], HardwareProfile: qemu.TestHardwareProfile, ReadyTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Stop)
	if err := reopened.ReopenAtGate(); err != nil {
		t.Fatalf("reopen gate: %v", err)
	}
	if _, ok := reopened.ActivePID(); ok {
		t.Fatal("reopen booted QEMU before operator continuation")
	}
	if transition, ok := reopened.CurrentTransition(); ok || transition != "" {
		t.Fatalf("reopened gate exposed stale transition %q, %v", transition, ok)
	}
	if err := reopened.ContinueGeneration(); err != nil {
		t.Fatalf("continue reopened gate: %v", err)
	}
	if number, ok := reopened.GenerationNumber(); !ok || number != 1 {
		t.Fatalf("reopened successor generation = %d, %v", number, ok)
	}
}

func TestLiveRunRollbackBootsEarlierCompletedSuccessor(t *testing.T) {
	run := startLiveTestRun(t, 2*time.Second)
	completeLiveTestGeneration(t, run, 0, "zero", "handoff zero")
	if err := run.ContinueGeneration(); err != nil {
		t.Fatal(err)
	}
	completeLiveTestGeneration(t, run, 1, "one", "handoff one")
	if err := run.ForkFromGeneration(0); err != nil {
		t.Fatalf("fork generation zero: %v", err)
	}
	if number, ok := run.GenerationNumber(); !ok || number != 2 {
		t.Fatalf("rollback generation = %d, %v", number, ok)
	}
	if transition, ok := run.CurrentTransition(); !ok || transition != "rollback" {
		t.Fatalf("rollback transition = %q, %v", transition, ok)
	}
	if handoff, ok := run.PreviousHandoff(); !ok || handoff != "handoff zero" {
		t.Fatalf("rollback handoff = %q, %v", handoff, ok)
	}
}

func TestLiveRunStopBeforeStartIsTerminal(t *testing.T) {
	run, err := NewLiveCodexOSRun(liveTestRunDirectory(t), LiveRunOptions{
		QEMUExecutable: os.Args[0], HardwareProfile: qemu.TestHardwareProfile,
	})
	if err != nil {
		t.Fatal(err)
	}
	run.Stop()
	initialISO := filepath.Join(t.TempDir(), "initial.iso")
	if err := os.WriteFile(initialISO, []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run.Start(context.Background(), initialISO); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("start after stop error = %v", err)
	}
}

func startLiveTestRun(t *testing.T, readyTimeout time.Duration) *CodexOSRun {
	return startLiveTestRunMode(t, readyTimeout, "1")
}

func startLiveTestRunMode(t *testing.T, readyTimeout time.Duration, helperMode string) *CodexOSRun {
	t.Helper()
	t.Setenv(liveQEMUHelperEnvironment, helperMode)
	runDirectory := liveTestRunDirectory(t)
	initialISO := filepath.Join(t.TempDir(), "initial.iso")
	if err := os.WriteFile(initialISO, []byte("synthetic boot image"), 0o600); err != nil {
		t.Fatal(err)
	}
	run, err := NewLiveCodexOSRun(runDirectory, LiveRunOptions{
		QEMUExecutable: os.Args[0], HardwareProfile: qemu.TestHardwareProfile, ReadyTimeout: readyTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := run.Start(ctx, initialISO); err != nil {
		t.Fatalf("start live run: %v", err)
	}
	return run
}

func liveValidatedArtifacts(t *testing.T, kernel, iso string, snapshot []byte) *build.StagedBuildArtifacts {
	t.Helper()
	kernelIdentity, err := provenance.FileIdentityFromPath(kernel)
	if err != nil {
		t.Fatal(err)
	}
	isoIdentity, err := provenance.FileIdentityFromPath(iso)
	if err != nil {
		t.Fatal(err)
	}
	return &build.StagedBuildArtifacts{
		KernelELF: kernel, ISO: iso, SourceSnapshot: append([]byte(nil), snapshot...),
		KernelIdentity: kernelIdentity, ISOIdentity: isoIdentity,
	}
}

func completeLiveTestGeneration(t *testing.T, run *CodexOSRun, number uint64, contents, handoff string) {
	t.Helper()
	generation := run.liveGeneration()
	artifacts := t.TempDir()
	kernel := filepath.Join(artifacts, "kernel.elf")
	iso := filepath.Join(artifacts, "codexos.iso")
	if err := os.WriteFile(kernel, []byte("kernel "+contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(iso, []byte("iso "+contents), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot := testSnapshot(t, "source "+contents)
	run.live.operationMu.Lock()
	err := run.completeLiveGeneration(generation, number, &build.PendingGenerationFinish{
		HandoffMessage: handoff, SourceSnapshot: snapshot, KernelELF: kernel, ISO: iso,
	}, liveValidatedArtifacts(t, kernel, iso, snapshot))
	run.live.operationMu.Unlock()
	if err != nil {
		t.Fatalf("complete generation %d: %v", number, err)
	}
}

func liveTestRunDirectory(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "codexos-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}

func runLiveQEMUHelper() int {
	for _, argument := range os.Args[1:] {
		if argument == "--version" {
			fmt.Println("QEMU emulator version 9.2.0")
			return 0
		}
	}
	qmpPath, serialPath := liveHelperSocketPaths(os.Args[1:])
	if qmpPath == "" || serialPath == "" {
		return 2
	}
	qmpListener, err := net.Listen("unix", qmpPath)
	if err != nil {
		return 3
	}
	defer qmpListener.Close()
	serialListener, err := net.Listen("unix", serialPath)
	if err != nil {
		return 4
	}
	defer serialListener.Close()
	done := make(chan struct{})
	results := make(chan error, 2)
	go func() { results <- serveLiveHelperQMP(qmpListener, done) }()
	go func() { results <- serveLiveHelperSerial(serialListener, done) }()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	for completed := 0; completed < 2; completed++ {
		select {
		case err := <-results:
			if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
				return 5
			}
		case <-deadline.C:
			return 6
		}
	}
	return 0
}

func liveHelperSocketPaths(arguments []string) (string, string) {
	var qmpPath, serialPath string
	for index, argument := range arguments {
		if argument == "-qmp" && index+1 < len(arguments) {
			qmpPath = strings.TrimPrefix(strings.Split(arguments[index+1], ",")[0], "unix:")
		}
		if argument == "-chardev" && index+1 < len(arguments) {
			for _, field := range strings.Split(arguments[index+1], ",") {
				if strings.HasPrefix(field, "path=") {
					serialPath = strings.TrimPrefix(field, "path=")
				}
			}
		}
	}
	return qmpPath, serialPath
}

func serveLiveHelperQMP(listener net.Listener, done chan struct{}) error {
	connection, err := listener.Accept()
	if err != nil {
		return err
	}
	defer connection.Close()
	if _, err := io.WriteString(connection, `{"QMP":{"version":{},"capabilities":[]}}`+"\r\n"); err != nil {
		return err
	}
	reader := bufio.NewReader(connection)
	status := "running"
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return err
		}
		var request map[string]any
		decoder := json.NewDecoder(strings.NewReader(string(line)))
		decoder.UseNumber()
		if err := decoder.Decode(&request); err != nil {
			return err
		}
		command, _ := request["execute"].(string)
		var result any = map[string]any{}
		switch command {
		case "stop":
			status = "paused"
		case "cont":
			status = "running"
		case "query-status":
			result = map[string]any{"status": status}
		case "quit":
			response := map[string]any{"return": result, "id": request["id"]}
			if err := json.NewEncoder(connection).Encode(response); err != nil {
				return err
			}
			close(done)
			_ = listener.Close()
			return nil
		}
		response := map[string]any{"return": result, "id": request["id"]}
		if err := json.NewEncoder(connection).Encode(response); err != nil {
			return err
		}
	}
}

func serveLiveHelperSerial(listener net.Listener, done <-chan struct{}) error {
	connection, err := listener.Accept()
	if err != nil {
		return err
	}
	defer connection.Close()
	go func() {
		<-done
		_ = connection.Close()
		_ = listener.Close()
	}()
	if os.Getenv(liveQEMUHelperEnvironment) != "no-ready" {
		if _, err := io.WriteString(connection, guest.ReadyMarker); err != nil {
			return err
		}
	}
	for {
		frame, err := guest.ReadFrame(connection)
		if err != nil {
			return err
		}
		var response guest.Frame
		switch frame.MessageType {
		case guest.ListToolsRequest:
			response = guest.Frame{MessageType: guest.ListToolsResponse, RequestID: frame.RequestID, Payload: liveToolList("read", "write")}
		case guest.InvokeToolRequest:
			name, err := liveInvokeName(frame.Payload)
			if err != nil {
				return err
			}
			if os.Getenv(liveQEMUHelperEnvironment) == "stall-invoke" {
				<-done
				return net.ErrClosed
			}
			payload := make([]byte, 4+len("tool:")+len(name))
			copy(payload[4:], "tool:"+name)
			response = guest.Frame{MessageType: guest.InvokeToolResponse, RequestID: frame.RequestID, Payload: payload}
		default:
			return fmt.Errorf("unexpected serial message type %#x", frame.MessageType)
		}
		encoded, err := guest.EncodeFrame(response)
		if err != nil {
			return err
		}
		if _, err := connection.Write(encoded); err != nil {
			return err
		}
	}
}

func liveToolList(names ...string) []byte {
	size := 2
	for _, name := range names {
		size += 2 + len(name)
	}
	payload := make([]byte, size)
	binary.LittleEndian.PutUint16(payload[:2], uint16(len(names)))
	offset := 2
	for _, name := range names {
		binary.LittleEndian.PutUint16(payload[offset:offset+2], uint16(len(name)))
		offset += 2
		copy(payload[offset:], name)
		offset += len(name)
	}
	return payload
}

func liveInvokeName(payload []byte) (string, error) {
	if len(payload) < 2 {
		return "", errors.New("short invoke request")
	}
	length := int(binary.LittleEndian.Uint16(payload[:2]))
	if length == 0 || length > len(payload)-2 {
		return "", errors.New("invalid invoke tool name")
	}
	return string(payload[2 : 2+length]), nil
}
