package experiment

import (
	"bufio"
	"bytes"
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
	"codexos/internal/observability"
	"codexos/internal/provenance"
	"codexos/internal/qemu"
	"codexos/internal/store"
)

const liveQEMUHelperEnvironment = "CODEXOS_GO_LIVE_QEMU_HELPER"

func TestLiveGenerationFixesHarnessIdentityAcrossEventsAndArchive(t *testing.T) {
	t.Setenv(liveQEMUHelperEnvironment, "1")
	runDirectory := liveTestRunDirectory(t)
	eventLog, err := observability.OpenEventLog(runDirectory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eventLog.Close)
	initialISO := filepath.Join(t.TempDir(), "initial.iso")
	if err := os.WriteFile(initialISO, []byte("synthetic boot image"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity := experimentHarnessIdentity()
	expected := identity
	if err := provenance.NewHarnessIdentityStore(runDirectory).RecordRunCreation(identity); err != nil {
		t.Fatal(err)
	}
	run, err := NewLiveCodexOSRun(runDirectory, LiveRunOptions{
		QEMUExecutable: os.Args[0], HardwareProfile: qemu.TestHardwareProfile,
		ReadyTimeout: 2 * time.Second, EventLog: eventLog, HarnessIdentity: &identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.Stop)
	identity.RepositoryCommit = strings.Repeat("d", 40)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := run.Start(ctx, initialISO); err != nil {
		t.Fatal(err)
	}
	recordBytes, err := os.ReadFile(provenance.HarnessGenerationRecordPath(runDirectory, 0))
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := provenance.ParseHarnessGenerationRecord(recordBytes, 0)
	if err != nil || !recorded.Equal(expected) {
		t.Fatalf("generation identity = %#v, %v", recorded, err)
	}
	if err := run.AbortGeneration(); err != nil {
		t.Fatal(err)
	}
	archive, err := InspectGeneration(runDirectory, 0)
	if err != nil || archive.HarnessIdentity == nil || !archive.HarnessIdentity.Equal(expected) {
		t.Fatalf("archive identity = %#v, %v", archive.HarnessIdentity, err)
	}
	contents, err := os.ReadFile(eventLog.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, eventName := range []string{"run_started", "generation_started", "generation_aborted"} {
		if !eventHasHarnessIdentity(t, contents, eventName, expected) {
			t.Fatalf("event %s lacks the fixed harness identity", eventName)
		}
	}
}

func TestLiveGateRejectsUnacknowledgedHarnessReplacementAndRecordsAcknowledgement(t *testing.T) {
	runDirectory := liveTestRunDirectory(t)
	first := experimentHarnessIdentity()
	if err := provenance.NewHarnessIdentityStore(runDirectory).RecordRunCreation(first); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteAbortedArchive(runDirectory, AbortedArchive{
		Generation: 0, Transition: "initial", Hardware: testHardware(t), BootISO: []byte("boot"), HarnessIdentity: &first,
	}); err != nil {
		t.Fatal(err)
	}
	same, err := NewLiveCodexOSRun(runDirectory, LiveRunOptions{
		HardwareProfile: qemu.TestHardwareProfile, HarnessIdentity: &first,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := same.ReopenAtGate(); err != nil {
		t.Fatalf("same identity reopen: %v", err)
	}
	if err := same.Close(); err != nil {
		t.Fatal(err)
	}
	replacement := first
	replacement.Executable.SHA256 = strings.Repeat("e", 64)
	unacknowledged, err := NewLiveCodexOSRun(runDirectory, LiveRunOptions{
		HardwareProfile: qemu.TestHardwareProfile, HarnessIdentity: &replacement,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := unacknowledged.ReopenAtGate(); err == nil || !strings.Contains(err.Error(), "--acknowledge-harness-change") {
		t.Fatalf("unacknowledged gate error = %v", err)
	}
	_ = unacknowledged.Close()
	eventLog, err := observability.OpenEventLog(runDirectory)
	if err != nil {
		t.Fatal(err)
	}
	acknowledged, err := NewLiveCodexOSRun(runDirectory, LiveRunOptions{
		HardwareProfile: qemu.TestHardwareProfile, EventLog: eventLog, HarnessIdentity: &replacement,
		AcknowledgeHarnessChange: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := acknowledged.ReopenAtGate(); err != nil {
		t.Fatal(err)
	}
	if err := acknowledged.Close(); err != nil {
		t.Fatal(err)
	}
	eventLog.Close()
	events, err := os.ReadFile(eventLog.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !eventHasHarnessIdentity(t, events, "harness_identity_transition_acknowledged", replacement) {
		t.Fatal("acknowledged identity transition event is missing")
	}
	transitions, err := filepath.Glob(filepath.Join(runDirectory, "harness-transitions", "transition-*.json"))
	if err != nil || len(transitions) != 1 {
		t.Fatalf("identity transitions = %v, %v", transitions, err)
	}
}

func eventHasHarnessIdentity(t *testing.T, contents []byte, wanted string, expected provenance.HarnessIdentity) bool {
	t.Helper()
	scanner := bufio.NewScanner(bytes.NewReader(contents))
	for scanner.Scan() {
		var envelope struct {
			Event string         `json:"event"`
			Data  map[string]any `json:"data"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Event != wanted {
			continue
		}
		encoded, err := json.Marshal(envelope.Data["harness_identity"])
		if err != nil {
			t.Fatal(err)
		}
		identity, err := provenance.ParseHarnessIdentity(encoded)
		return err == nil && identity.Equal(expected)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return false
}

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

func TestLiveRunRecordsOversizedInvocationBeforeDispatch(t *testing.T) {
	t.Setenv(liveQEMUHelperEnvironment, "1")
	runDirectory := liveTestRunDirectory(t)
	eventLog, err := observability.OpenEventLog(runDirectory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eventLog.Close)
	initialISO := filepath.Join(t.TempDir(), "initial.iso")
	if err := os.WriteFile(initialISO, []byte("synthetic boot image"), 0o600); err != nil {
		t.Fatal(err)
	}
	run, err := NewLiveCodexOSRun(runDirectory, LiveRunOptions{
		QEMUExecutable: os.Args[0], HardwareProfile: qemu.TestHardwareProfile,
		ReadyTimeout: 2 * time.Second, EventLog: eventLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := run.Start(ctx, initialISO); err != nil {
		t.Fatal(err)
	}

	path := []byte("seed/target")
	offset := []byte("0")
	baseSize, err := guest.InvokeRequestPayloadSize("write", [][]byte{path, offset, nil})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(strings.Repeat("x", int(uint64(guest.V1GuestInvocationPayloadCapacity)+1-baseSize)))
	result, err := run.InvokeTool(ctx, "write", [][]byte{path, offset, data})
	if err != nil || result.Status != 1 || !strings.Contains(string(result.Output), "accepted_bytes:0") {
		t.Fatalf("oversized invocation = %#v, %v", result, err)
	}

	contents, err := os.ReadFile(eventLog.Path())
	if err != nil {
		t.Fatal(err)
	}
	type recordedEvent struct {
		Event string         `json:"event"`
		Data  map[string]any `json:"data"`
	}
	var lifecycle []string
	var rejection recordedEvent
	scanner := bufio.NewScanner(strings.NewReader(string(contents)))
	for scanner.Scan() {
		var event recordedEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		if event.Event == "tool_invocation_rejected_before_dispatch" || strings.HasPrefix(event.Event, "tool_guest_invocation_") {
			lifecycle = append(lifecycle, event.Event)
		}
		if event.Event == "serial_protocol_write" && event.Data["write_kind"] == "tool_request" {
			lifecycle = append(lifecycle, event.Event)
		}
		if event.Event == "tool_invocation_rejected_before_dispatch" {
			rejection = event
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"tool_invocation_rejected_before_dispatch"}; !reflect.DeepEqual(lifecycle, want) {
		t.Fatalf("oversized invocation event sequence = %v, want %v", lifecycle, want)
	}
	encodedBytes, encodedOK := rejection.Data["encoded_bytes"].(float64)
	maximumBytes, maximumOK := rejection.Data["maximum_bytes"].(float64)
	acceptedBytes, acceptedOK := rejection.Data["accepted_bytes"].(float64)
	if rejection.Data["tool"] != "write" || !encodedOK || !maximumOK || !acceptedOK ||
		uint64(encodedBytes) != uint64(guest.V1GuestInvocationPayloadCapacity+1) ||
		uint64(maximumBytes) != uint64(guest.V1GuestInvocationPayloadCapacity) || uint64(acceptedBytes) != 0 {
		t.Fatalf("oversized invocation rejection = %#v", rejection.Data)
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

func TestLiveRunAppliesCrossRunHandoffAndVerifiesInitialISO(t *testing.T) {
	t.Setenv(liveQEMUHelperEnvironment, "1")
	run, err := NewLiveCodexOSRun(liveTestRunDirectory(t), LiveRunOptions{
		QEMUExecutable: os.Args[0], HardwareProfile: qemu.TestHardwareProfile, ReadyTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.Stop)
	image := filepath.Join(t.TempDir(), "inherited.iso")
	if err := os.WriteFile(image, []byte("inherited successor"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := provenance.FileIdentityFromPath(image)
	if err != nil {
		t.Fatal(err)
	}
	run.live.bootstrap = &store.CrossRunBootstrap{
		Handoff: "inherited context", SuccessorISOSHA256: identity.SHA256, SuccessorISOSize: identity.Size,
	}
	link := filepath.Join(t.TempDir(), "initial-link.iso")
	if err := os.Symlink(image, link); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := run.Start(ctx, link); err != nil {
		t.Fatalf("start inherited run: %v", err)
	}
	if handoff, ok := run.PreviousHandoff(); !ok || handoff != "inherited context" {
		t.Fatalf("inherited handoff = %q, %v", handoff, ok)
	}
}

func TestLiveRunDecidesFeatureRequestsOnlyAtGate(t *testing.T) {
	run := startLiveTestRun(t, 2*time.Second)
	generation := run.liveGeneration()
	response, err := generation.hostServices.HandleRequest(context.Background(), guest.HostRequest{
		RequestID: 1, ServiceName: "request_feature",
		Arguments: [][]byte{[]byte("Need capability"), []byte("Please record this")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Payload) < 4 || binary.LittleEndian.Uint32(response.Payload[:4]) != build.FeatureResponseRecorded {
		t.Fatalf("feature response = %#v", response)
	}
	requests, err := run.FeatureRequests()
	if err != nil || len(requests) != 1 {
		t.Fatalf("feature requests = %#v, %v", requests, err)
	}
	request := requests[0]
	if snapshot := run.PresentationSnapshot(); snapshot.PendingFeatureRequests != 1 {
		t.Fatalf("pending feature presentation after create = %#v", snapshot)
	}
	if _, err := run.ApproveFeatureRequest(request.ID); err == nil || !strings.Contains(err.Error(), "only while awaiting") {
		t.Fatalf("running feature decision error = %v", err)
	}
	if err := run.AbortGeneration(); err != nil {
		t.Fatal(err)
	}
	approved, err := run.ApproveFeatureRequest(request.ID)
	if err != nil {
		t.Fatalf("approve at gate: %v", err)
	}
	if approved.Status != store.FeatureApproved {
		t.Fatalf("approved status = %q", approved.Status)
	}
	if snapshot := run.PresentationSnapshot(); snapshot.PendingFeatureRequests != 0 {
		t.Fatalf("pending feature presentation after approval = %#v", snapshot)
	}
}

func TestLiveRunRestoresProvidedAssetsAtReopenedGate(t *testing.T) {
	t.Setenv(liveQEMUHelperEnvironment, "1")
	assets := filepath.Join(t.TempDir(), "assets")
	assetDirectory := filepath.Join(assets, "manual")
	if err := os.MkdirAll(assetDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assetDirectory, "manual.txt"), []byte("trusted bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	runDirectory := liveTestRunDirectory(t)
	options := LiveRunOptions{
		QEMUExecutable: os.Args[0], HardwareProfile: qemu.TestHardwareProfile,
		ReadyTimeout: 2 * time.Second, ProvidedAssetsDirectory: &assets,
	}
	run, err := NewLiveCodexOSRun(runDirectory, options)
	if err != nil {
		t.Fatal(err)
	}
	initialISO := filepath.Join(t.TempDir(), "initial.iso")
	if err := os.WriteFile(initialISO, []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := run.Start(ctx, initialISO); err != nil {
		t.Fatal(err)
	}
	if err := run.AbortGeneration(); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewLiveCodexOSRun(runDirectory, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Stop)
	if err := reopened.ReopenAtGate(); err != nil {
		t.Fatal(err)
	}
	if reopened.live.provided == nil || reopened.live.provided.Provenance == nil {
		t.Fatal("provided assets were not restored")
	}
	got, err := reopened.live.provided.ReadAsset("manual", 0, uint64(len("trusted bytes")))
	if err != nil || string(got) != "trusted bytes" {
		t.Fatalf("restored asset = %q, %v", got, err)
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
