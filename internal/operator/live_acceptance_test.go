package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"codexos/internal/agent"
	"codexos/internal/build"
	"codexos/internal/experiment"
	"codexos/internal/guest"
	"codexos/internal/observability"
	"codexos/internal/provenance"
	"codexos/internal/qemu"
	"codexos/internal/sourcecapacity"
	"codexos/internal/store"
)

const disposableGenerationHandoff = "The disposable generation completed its validated build."

func TestRunnerCompletesContinuesAndRollsBackDisposableGeneration(t *testing.T) {
	t.Setenv("CODEXOS_DISPOSABLE_QEMU_LIFECYCLE", "lifecycle")
	processRecords := t.TempDir()
	t.Setenv("CODEXOS_DISPOSABLE_PROCESS_RECORDS", processRecords)
	qemuExecutable := buildDisposableRunnerQEMU(t)
	codexExecutable := buildDisposableOperatorFixture(t, "fake-codex", "./internal/operator/testdata/fakecodex")
	buildConfig := disposableTrustedBuildConfig(t)
	authFile := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Keep the root short: the candidate workspace adds two random path
	// components and Linux Unix-domain socket paths are limited to 108 bytes.
	runDirectory, err := os.MkdirTemp("/tmp", "co-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDirectory) })
	initialISO := filepath.Join(t.TempDir(), "initial.iso")
	if err := os.WriteFile(initialISO, []byte("disposable initial image"), 0o600); err != nil {
		t.Fatal(err)
	}

	input, inputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer inputWriter.Close()
	var output synchronizedBuffer
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- runWithIOConfigured(ctx, Options{
			RunDirectory: runDirectory,
			InitialISO:   initialISO,
		}, input, &output, runnerConfiguration{
			live: experiment.LiveRunOptions{
				QEMUExecutable:        qemuExecutable,
				HardwareProfile:       qemu.TestHardwareProfile,
				BuildConfig:           buildConfig,
				ReadyTimeout:          3 * time.Second,
				CandidateReadyTimeout: 3 * time.Second,
			},
			session: agent.GenerationSessionOptions{
				Executable:  codexExecutable,
				AuthFile:    authFile,
				StopTimeout: 3 * time.Second,
			},
		})
	}()
	if _, err := io.WriteString(inputWriter, "agent\n"); err != nil {
		stopErr := stopDisposableRunner(cancel, inputWriter, result)
		t.Fatalf("send agent command: %v; stop runner: %v", err, stopErr)
	}

	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	var stoppedEarly bool
	var runnerErr error
	var completionTimedOut bool
	for !strings.Contains(output.String(), "Generation 0 completed cooperatively.") && !stoppedEarly && !completionTimedOut {
		select {
		case err := <-result:
			stoppedEarly = true
			runnerErr = err
		case <-poll.C:
		case <-deadline.C:
			completionTimedOut = true
		}
	}
	if stoppedEarly || completionTimedOut {
		if !stoppedEarly {
			runnerErr = stopDisposableRunner(cancel, inputWriter, result)
		}
		if stoppedEarly {
			t.Fatalf("runner stopped before reaching the completed gate: %v\n%s", runnerErr, output.String())
		}
		t.Fatalf("runner did not reach the completed gate: %v\n%s", runnerErr, output.String())
	}

	// The completion report is emitted just before the console releases its
	// asynchronous turn reservation. Probe through the public status command
	// until the gate reports the retained session as idle, then transition.
	for attempts := 0; attempts < 100 && !strings.Contains(output.String(), "Codex turn: idle"); attempts++ {
		if _, err := io.WriteString(inputWriter, "status\n"); err != nil {
			stopErr := stopDisposableRunner(cancel, inputWriter, result)
			t.Fatalf("send gate status command: %v; stop runner: %v", err, stopErr)
		}
		select {
		case err := <-result:
			t.Fatalf("runner stopped while waiting for the completed turn to settle: %v\n%s", err, output.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if !strings.Contains(output.String(), "Codex turn: idle") {
		stopErr := stopDisposableRunner(cancel, inputWriter, result)
		t.Fatalf("completed turn did not settle at the gate: %v\n%s", stopErr, output.String())
	}

	sendDisposableCommand(t, cancel, inputWriter, result, "continue\n")
	waitForDisposableOutput(t, cancel, inputWriter, result, &output, "Generation 1: RUNNING")
	sendDisposableCommand(t, cancel, inputWriter, result, "abort first acceptance stop\ny\n")
	waitForDisposableOutput(t, cancel, inputWriter, result, &output, "Generation 1 aborted.")
	sendDisposableCommand(t, cancel, inputWriter, result, "rollback 0\ny\n")
	waitForDisposableOutput(t, cancel, inputWriter, result, &output, "Generation 2 started from generation 0.")
	sendDisposableCommand(t, cancel, inputWriter, result, "abort second acceptance stop\ny\n")
	waitForDisposableOutput(t, cancel, inputWriter, result, &output, "Generation 2 aborted.")
	if _, err := io.WriteString(inputWriter, "quit\n"); err != nil {
		stopErr := stopDisposableRunner(cancel, inputWriter, result)
		t.Fatalf("send quit command: %v; stop runner: %v", err, stopErr)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("run disposable completed generation: %v\n%s", err, output.String())
		}
	case <-time.After(15 * time.Second):
		stopErr := stopDisposableRunner(cancel, inputWriter, result)
		t.Fatalf("runner did not stop after quit: %v\n%s", stopErr, output.String())
	}
	if ctx.Err() != nil {
		t.Fatalf("runner exceeded acceptance deadline: %v", ctx.Err())
	}
	assertDisposableProcessesStopped(t, processRecords, 3)

	for _, want := range []string{
		"Codex planning and implementation started for generation 0.",
		"Generation 0 completed cooperatively.",
		"A successor is selected.",
		disposableGenerationHandoff,
		"Starting generation 1 from selected successor...",
		"Generation 1 aborted.",
		"Generation 2 started from generation 0.",
		"Generation 2 aborted.",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("operator output missing %q:\n%s", want, output.String())
		}
	}
	loaded, err := experiment.NewCodexOSRun(runDirectory)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := loaded.InspectGeneration(0)
	if err != nil {
		t.Fatal(err)
	}
	if archive.Outcome != "completed" || archive.Transition != "initial" || archive.ParentGeneration != nil || archive.Handoff == nil || *archive.Handoff != disposableGenerationHandoff {
		t.Fatalf("completed archive = %#v", archive)
	}
	bootISO, err := os.ReadFile(filepath.Join(archive.ArchivePath, "boot", "codexos.iso"))
	if err != nil || string(bootISO) != "disposable initial image" {
		t.Fatalf("archived boot ISO = %q, %v", bootISO, err)
	}
	snapshot, err := os.ReadFile(filepath.Join(archive.ArchivePath, "source.snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	files, err := guest.DecodeSourceSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 4 {
		t.Fatalf("archived source files = %#v", files)
	}
	for _, file := range files {
		materialized, err := os.ReadFile(filepath.Join(archive.ArchivePath, "source", filepath.FromSlash(file.Path)))
		if err != nil || !bytes.Equal(materialized, file.Content) {
			t.Fatalf("materialized source %s differs from snapshot: %v", file.Path, err)
		}
	}
	successorISO, err := os.ReadFile(filepath.Join(archive.ArchivePath, "successor", "codexos.iso"))
	if err != nil || string(successorISO) != "synthetic-iso\nlimine-installed\n" {
		t.Fatalf("validated successor ISO = %q, %v", successorISO, err)
	}
	for _, path := range []string{
		filepath.Join(archive.ArchivePath, "successor", "kernel.elf"),
		filepath.Join(archive.ArchivePath, "successor", "codexos.iso"),
		filepath.Join(runDirectory, "planning-evidence", "generation-0000", "manifest.json"),
	} {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("required completed artifact %s: %v", path, err)
		}
	}
	buildManifests, err := filepath.Glob(filepath.Join(runDirectory, "build-review-provenance", "generation-0000", "build-*", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(buildManifests) != 1 {
		t.Fatalf("build evidence manifests = %v", buildManifests)
	}
	manifest, err := os.ReadFile(buildManifests[0])
	if err != nil {
		t.Fatal(err)
	}
	var buildEvidence map[string]any
	if err := json.Unmarshal(manifest, &buildEvidence); err != nil {
		t.Fatal(err)
	}
	candidate, _ := buildEvidence["candidate_validation"].(map[string]any)
	if buildEvidence["outcome"] != "success" || candidate["protocol_validated"] != true {
		t.Fatalf("build evidence does not prove validated success: %s", manifest)
	}
	workspaces, err := filepath.Glob(filepath.Join(runDirectory, ".generation-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(workspaces) != 0 {
		t.Fatalf("runner left generation workspaces: %v", workspaces)
	}
	for generation, want := range map[uint64]struct {
		transition string
		parent     uint64
		reason     string
	}{
		1: {transition: "successor", parent: 0, reason: "first acceptance stop"},
		2: {transition: "rollback", parent: 0, reason: "second acceptance stop"},
	} {
		item, err := loaded.InspectGeneration(generation)
		if err != nil {
			t.Fatal(err)
		}
		if item.Outcome != "aborted" || item.Transition != want.transition || item.ParentGeneration == nil || *item.ParentGeneration != want.parent || item.AbortReason == nil || *item.AbortReason != want.reason {
			t.Fatalf("generation %d archive = %#v", generation, item)
		}
	}
	events, err := os.ReadFile(filepath.Join(runDirectory, observability.EventLogFilename))
	if err != nil {
		t.Fatal(err)
	}
	previous := -1
	for _, event := range []string{
		"run_started", "generation_started", "codex_session_started", "planning_started", "planning_completed",
		"build_attempt_received", "build_candidate_validation_started", "build_candidate_qemu_started",
		"build_candidate_ready_observed", "build_protocol_validation_started", "build_protocol_validation_completed",
		"build_attempt_completed", "build_completed", "generation_completed", "codex_session_stopped", "run_stopped",
	} {
		index := bytes.Index(events, []byte(`"event":"`+event+`"`))
		if index < 0 || index <= previous {
			t.Fatalf("event %q missing or out of order:\n%s", event, events)
		}
		previous = index
	}
	if !bytes.Contains(events, []byte(`"event":"operator_abort_feedback_attached"`)) ||
		!bytes.Contains(events, []byte(`"source_abort_generation":1`)) ||
		!bytes.Contains(events, []byte(`"reason":"first acceptance stop"`)) {
		t.Fatalf("feedback attachment event is incomplete:\n%s", events)
	}
	attachment, err := os.ReadFile(filepath.Join(runDirectory, "operator-feedback", "generation-0002.json"))
	if err != nil || !bytes.Contains(attachment, []byte(`"reason": "first acceptance stop"`)) {
		t.Fatalf("durable feedback attachment = %s, %v", attachment, err)
	}
}

func TestRunnerBootsCrossRunInheritanceWithGitProvenance(t *testing.T) {
	for _, budget := range []sourcecapacity.Budget{0, sourcecapacity.Expanded} {
		t.Run(fmt.Sprint(budget.Bytes()), func(t *testing.T) { testRunnerBootsCrossRunInheritanceWithGitProvenance(t, budget) })
	}
}

func testRunnerBootsCrossRunInheritanceWithGitProvenance(t *testing.T, budget sourcecapacity.Budget) {
	processRecords := t.TempDir()
	t.Setenv("CODEXOS_DISPOSABLE_PROCESS_RECORDS", processRecords)
	qemuExecutable := buildDisposableRunnerQEMU(t)
	root, err := os.MkdirTemp("/tmp", "co-xrun-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	sourceRun := filepath.Join(root, "source")
	if err := os.Mkdir(sourceRun, 0o755); err != nil {
		t.Fatal(err)
	}
	hardware, err := qemu.TestHardwareProfile.Manifest("QEMU emulator version disposable-cross-run")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := guest.EncodeSourceSnapshotWithBudget([]guest.SnapshotFile{{
		Path: "seed/kernel.c", Content: bytes.Repeat([]byte("x"), budget.Bytes()),
	}}, budget)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := experiment.WriteCompletedArchive(sourceRun, experiment.CompletedArchive{
		Generation: 0, Transition: "initial", Hardware: hardware, SourceCapacity: budget,
		BootISO: []byte("source boot image"), Handoff: "Inherited handoff λ.\n",
		SourceSnapshot: snapshot, KernelELF: []byte("inherited kernel"),
		SuccessorISO: []byte("inherited successor image"),
	}); err != nil {
		t.Fatal(err)
	}
	featureStore, err := store.NewFeatureRequestStore(sourceRun)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := featureStore.Create(0, "Pending inherited capability", "Keep this request pending.")
	if err != nil {
		t.Fatal(err)
	}
	approved, err := featureStore.Create(0, "Approved inherited capability", "Preserve this decision.")
	if err != nil {
		t.Fatal(err)
	}
	approved, err = featureStore.Approve(approved.ID)
	if err != nil {
		t.Fatal(err)
	}

	repository := filepath.Join(root, "repository")
	initializeDisposableGitRepository(t, repository)
	sourceRecorder, err := provenance.NewGenerationGitRecorder(repository, sourceRun, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	records, err := sourceRecorder.Reconcile()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Tag != "source/generation-0000" {
		t.Fatalf("source Git records = %#v", records)
	}

	initialISO := filepath.Join(sourceRun, "generation-0000", "successor", "codexos.iso")
	destinationRun := filepath.Join(root, "destination")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var output bytes.Buffer
	err = runWithIOConfigured(ctx, Options{
		RunDirectory:          destinationRun,
		InitialISO:            initialISO,
		GitRepository:         repository,
		GitBaseRef:            "source/generation-0000",
		InheritFromRun:        sourceRun,
		InheritFromGeneration: 0, InheritSourceCapacity: budget,
	}, strings.NewReader("status\nfeatures\nfeature 1\nfeature 2\nabort cross-run acceptance stop\ny\nquit\n"), &output, runnerConfiguration{
		live: experiment.LiveRunOptions{
			QEMUExecutable: qemuExecutable, HardwareProfile: qemu.TestHardwareProfile,
			ReadyTimeout: 3 * time.Second,
		},
	})
	if err != nil {
		t.Fatalf("run inherited generation: %v\n%s", err, output.String())
	}
	if ctx.Err() != nil {
		t.Fatalf("inherited runner exceeded acceptance deadline: %v", ctx.Err())
	}
	assertDisposableProcessesStopped(t, processRecords, 1)

	for _, wanted := range []string{
		"Generation 0: RUNNING",
		"Pending feature requests: 1",
		"Previous handoff:\n  Inherited handoff λ.",
		"1    0     pending    Pending inherited capability",
		"2    0     approved   Approved inherited capability",
		"Feature request: #1",
		"Feature request: #2",
		"Generation 0 aborted.",
	} {
		if !strings.Contains(output.String(), wanted) {
			t.Fatalf("inherited operator output missing %q:\n%s", wanted, output.String())
		}
	}
	bootstrap, err := store.LoadCrossRunBootstrap(destinationRun)
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap == nil || bootstrap.SourceRun != "source" || bootstrap.SourceGeneration != 0 ||
		bootstrap.Handoff != "Inherited handoff λ.\n" || bootstrap.GitBaseRef != "source/generation-0000" ||
		bootstrap.GitBaseCommit != records[0].Commit ||
		len(bootstrap.InheritedRequestIDs) != 2 || bootstrap.InheritedRequestIDs[0] != 1 || bootstrap.InheritedRequestIDs[1] != 2 {
		t.Fatalf("destination bootstrap = %#v", bootstrap)
	}
	destinationFeatures, err := store.NewFeatureRequestStore(destinationRun)
	if err != nil {
		t.Fatal(err)
	}
	requests, err := destinationFeatures.Requests()
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[0] != pending || requests[1] != approved {
		t.Fatalf("destination feature requests = %#v", requests)
	}
	loaded, err := experiment.NewCodexOSRun(destinationRun)
	if err != nil {
		t.Fatal(err)
	}
	if err := loaded.ReopenAtGate(); err != nil {
		t.Fatal(err)
	}
	if loaded.SourceCapacity().Bytes() != budget.Bytes() {
		t.Fatal("inherited budget lost on gate reopen")
	}
	archive, err := loaded.InspectGeneration(0)
	if err != nil {
		t.Fatal(err)
	}
	if archive.SourceCapacity.Bytes() != budget.Bytes() {
		t.Fatal("inherited generation budget missing from archive")
	}
	if archive.Outcome != "aborted" || archive.Transition != "initial" || archive.ParentGeneration != nil {
		t.Fatalf("destination archive = %#v", archive)
	}
	bootISO, err := os.ReadFile(filepath.Join(archive.ArchivePath, "boot", "codexos.iso"))
	if err != nil || string(bootISO) != "inherited successor image" {
		t.Fatalf("destination boot ISO = %q, %v", bootISO, err)
	}
	workspaces, err := filepath.Glob(filepath.Join(destinationRun, ".generation-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(workspaces) != 0 {
		t.Fatalf("inherited runner left generation workspaces: %v", workspaces)
	}
	staging, err := filepath.Glob(filepath.Join(root, ".cross-run-bootstrap-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(staging) != 0 {
		t.Fatalf("inherited runner left bootstrap staging directories: %v", staging)
	}
}

func TestRunnerAbortsDuringBlockedLargeHostResponse(t *testing.T) {
	t.Setenv("CODEXOS_DISPOSABLE_QEMU_LIFECYCLE", "large-shutdown")
	processRecords := t.TempDir()
	t.Setenv("CODEXOS_DISPOSABLE_PROCESS_RECORDS", processRecords)
	qemuExecutable := buildDisposableRunnerQEMU(t)
	runDirectory, err := os.MkdirTemp("/tmp", "co-large-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDirectory) })
	initialISO := filepath.Join(t.TempDir(), "initial.iso")
	if err := os.WriteFile(initialISO, []byte("large-transfer initial image"), 0o600); err != nil {
		t.Fatal(err)
	}
	providedAssets := filepath.Join(t.TempDir(), "assets")
	bulkAsset := filepath.Join(providedAssets, "bulk")
	if err := os.MkdirAll(bulkAsset, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, store.MaxProvidedAssetReadBytes)
	for index := range payload {
		payload[index] = byte(index)
	}
	if err := os.WriteFile(filepath.Join(bulkAsset, "payload.bin"), payload, 0o600); err != nil {
		t.Fatal(err)
	}

	input, inputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer inputWriter.Close()
	var output synchronizedBuffer
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- runWithIOConfigured(ctx, Options{
			RunDirectory: runDirectory, InitialISO: initialISO, ProvidedAssets: providedAssets,
		}, input, &output, runnerConfiguration{
			live: experiment.LiveRunOptions{
				QEMUExecutable: qemuExecutable, HardwareProfile: qemu.TestHardwareProfile,
				ReadyTimeout: 3 * time.Second,
			},
		})
	}()

	eventPath := filepath.Join(runDirectory, observability.EventLogFilename)
	started := waitForDisposableEvent(t, cancel, inputWriter, result, eventPath, func(event disposableEvent) bool {
		return event.Event == "serial_protocol_write" && event.Data["connection"] == "active" &&
			event.Data["request_id"] == float64(1) && event.Data["write_kind"] == "host_response" &&
			event.Data["phase"] == "write_started"
	})
	if started.Data["total_bytes"] != float64(store.MaxProvidedAssetReadBytes+20) {
		stopErr := stopDisposableRunner(cancel, inputWriter, result)
		t.Fatalf("large host-response bytes = %v, want %d; stop runner: %v", started.Data["total_bytes"], store.MaxProvidedAssetReadBytes+20, stopErr)
	}
	markers, err := filepath.Glob(filepath.Join(processRecords, "qemu-large-response-requested-*.marker"))
	if err != nil || len(markers) != 1 {
		stopErr := stopDisposableRunner(cancel, inputWriter, result)
		t.Fatalf("large-response request markers = %v, %v; stop runner: %v", markers, err, stopErr)
	}

	abortStarted := time.Now()
	sendDisposableCommand(t, cancel, inputWriter, result, "abort blocked exchange acceptance stop\ny\n")
	waitForDisposableOutput(t, cancel, inputWriter, result, &output, "Generation 0 aborted.")
	if elapsed := time.Since(abortStarted); elapsed > 10*time.Second {
		stopErr := stopDisposableRunner(cancel, inputWriter, result)
		t.Fatalf("abort during large host response took %s; stop runner: %v", elapsed, stopErr)
	}
	sendDisposableCommand(t, cancel, inputWriter, result, "quit\n")
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("large-response runner failed: %v\n%s", err, output.String())
		}
	case <-time.After(15 * time.Second):
		stopErr := stopDisposableRunner(cancel, inputWriter, result)
		t.Fatalf("large-response runner did not stop: %v\n%s", stopErr, output.String())
	}
	if ctx.Err() != nil {
		t.Fatalf("large-response runner exceeded acceptance deadline: %v", ctx.Err())
	}
	assertDisposableProcessesStopped(t, processRecords, 1)

	events := readDisposableEvents(t, eventPath)
	phases := make([]string, 0, 3)
	for _, event := range events {
		if event.Event == "serial_protocol_write" && event.Data["connection"] == "active" &&
			event.Data["request_id"] == float64(1) && event.Data["write_kind"] == "host_response" {
			if phase, ok := event.Data["phase"].(string); ok {
				phases = append(phases, phase)
			}
		}
	}
	if len(phases) < 3 || phases[0] != "write_started" || phases[len(phases)-1] != "write_failed" {
		t.Fatalf("large host-response write phases = %v", phases)
	}
	for _, phase := range phases[1 : len(phases)-1] {
		if phase != "write_progress" {
			t.Fatalf("large host-response write phases = %v", phases)
		}
	}
	loaded, err := experiment.NewCodexOSRun(runDirectory)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := loaded.InspectGeneration(0)
	if err != nil {
		t.Fatal(err)
	}
	if archive.Outcome != "aborted" || archive.Transition != "initial" {
		t.Fatalf("large-response abort archive = %#v", archive)
	}
	workspaces, err := filepath.Glob(filepath.Join(runDirectory, ".generation-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(workspaces) != 0 {
		t.Fatalf("large-response runner left generation workspaces: %v", workspaces)
	}
}

type disposableEvent struct {
	Event string         `json:"event"`
	Data  map[string]any `json:"data"`
}

func waitForDisposableEvent(
	t *testing.T,
	cancel context.CancelFunc,
	input *os.File,
	result <-chan error,
	path string,
	match func(disposableEvent) bool,
) disposableEvent {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		for _, event := range readDisposableEventsIfAvailable(t, path) {
			if match(event) {
				return event
			}
		}
		select {
		case err := <-result:
			t.Fatalf("runner stopped before expected event: %v", err)
		case <-ticker.C:
		case <-timer.C:
			stopErr := stopDisposableRunner(cancel, input, result)
			t.Fatalf("runner did not record expected event: %v", stopErr)
		}
	}
}

func readDisposableEvents(t *testing.T, path string) []disposableEvent {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return decodeDisposableEvents(t, contents)
}

func readDisposableEventsIfAvailable(t *testing.T, path string) []disposableEvent {
	t.Helper()
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return decodeDisposableEvents(t, contents)
}

func decodeDisposableEvents(t *testing.T, contents []byte) []disposableEvent {
	t.Helper()
	lines := bytes.Split(contents, []byte{'\n'})
	if len(contents) != 0 && contents[len(contents)-1] != '\n' {
		lines = lines[:len(lines)-1]
	}
	events := make([]disposableEvent, 0, len(lines))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		var event disposableEvent
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("decode disposable event: %v\n%s", err, line)
		}
		events = append(events, event)
	}
	return events
}

func sendDisposableCommand(t *testing.T, cancel context.CancelFunc, input *os.File, result <-chan error, command string) {
	t.Helper()
	if _, err := io.WriteString(input, command); err != nil {
		stopErr := stopDisposableRunner(cancel, input, result)
		t.Fatalf("send disposable operator command %q: %v; stop runner: %v", strings.TrimSpace(command), err, stopErr)
	}
}

func waitForDisposableOutput(
	t *testing.T,
	cancel context.CancelFunc,
	input *os.File,
	result <-chan error,
	output *synchronizedBuffer,
	wanted string,
) {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !strings.Contains(output.String(), wanted) {
		select {
		case err := <-result:
			t.Fatalf("runner stopped before output %q: %v\n%s", wanted, err, output.String())
		case <-ticker.C:
		case <-timer.C:
			stopErr := stopDisposableRunner(cancel, input, result)
			t.Fatalf("runner did not produce output %q: %v\n%s", wanted, stopErr, output.String())
		}
	}
}

func stopDisposableRunner(cancel context.CancelFunc, input *os.File, result <-chan error) error {
	cancel()
	_ = input.Close()
	select {
	case err := <-result:
		return err
	case <-time.After(15 * time.Second):
		return context.DeadlineExceeded
	}
}

func assertDisposableProcessesStopped(t *testing.T, directory string, minimum int) {
	t.Helper()
	records, err := filepath.Glob(filepath.Join(directory, "*.pid"))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) < minimum {
		t.Fatalf("disposable process records = %v, want at least %d", records, minimum)
	}
	for _, record := range records {
		encoded, err := os.ReadFile(record)
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(encoded)))
		if err != nil {
			t.Fatalf("invalid disposable PID record %s: %v", record, err)
		}
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("disposable process %d from %s survived runner shutdown: %v", pid, record, err)
		}
	}
}

func initializeDisposableGitRepository(t *testing.T, repository string) {
	t.Helper()
	if err := os.Mkdir(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{
		{"init", "-q"},
		{"config", "user.name", "Disposable acceptance"},
		{"config", "user.email", "disposable@example.invalid"},
	} {
		runDisposableGit(t, repository, arguments...)
	}
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("trusted base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runDisposableGit(t, repository, "add", "README.md")
	runDisposableGit(t, repository, "commit", "-q", "-m", "Trusted disposable base")
}

func runDisposableGit(t *testing.T, repository string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repository}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", arguments, err, output)
	}
	return string(output)
}

type synchronizedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *synchronizedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func buildDisposableOperatorFixture(t *testing.T, name, packagePath string) string {
	t.Helper()
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), name)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "build", "-o", executable, packagePath)
	command.Dir = repositoryRoot
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build disposable %s fixture: %v\n%s", name, err, output)
	}
	return executable
}

func disposableTrustedBuildConfig(t *testing.T) build.Config {
	t.Helper()
	repository := t.TempDir()
	limineDirectory := filepath.Join(repository, "third_party", "limine")
	if err := os.MkdirAll(limineDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"limine.c", "limine-bios-hdd.h", "limine-bios.sys", "limine-bios-cd.bin"} {
		if err := os.WriteFile(filepath.Join(limineDirectory, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	bin := filepath.Join(repository, "toolchain", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	cc1 := disposableExecutable(t, "#!/bin/sh\nexit 0\n", filepath.Join(bin, "cc1"))
	assembler := disposableExecutable(t, "#!/bin/sh\nexit 0\n", filepath.Join(bin, "as"))
	crossCompiler := filepath.Join(bin, "x86_64-elf-gcc")
	disposableExecutable(t, `#!/bin/sh
case "$1" in
  -print-prog-name=cc1) printf '%s\n' '`+cc1+`'; exit 0 ;;
  -print-prog-name=as) printf '%s\n' '`+assembler+`'; exit 0 ;;
esac
output=
previous=
for argument in "$@"; do
  if [ "$previous" = "-o" ]; then output="$argument"; break; fi
  previous="$argument"
done
if [ -z "$output" ]; then exit 9; fi
printf '%s\n' "$@" > "$output"
`, crossCompiler)
	crossLinker := filepath.Join(bin, "x86_64-elf-ld")
	disposableExecutable(t, `#!/bin/sh
output=
previous=
for argument in "$@"; do
  if [ "$previous" = "-o" ]; then output="$argument"; break; fi
  previous="$argument"
done
if [ -z "$output" ]; then exit 9; fi
printf 'synthetic-kernel\n' > "$output"
for argument in "$@"; do
  case "$argument" in *.o) cat "$argument" >> "$output" ;; esac
done
`, crossLinker)
	ldd := disposableExecutable(t, "#!/bin/sh\nexit 0\n", filepath.Join(bin, "ldd"))
	xorriso := disposableExecutable(t, `#!/bin/sh
output=
previous=
for argument in "$@"; do
  if [ "$previous" = "-o" ]; then output="$argument"; break; fi
  previous="$argument"
done
if [ -z "$output" ]; then exit 9; fi
printf 'synthetic-iso\n' > "$output"
`, filepath.Join(bin, "xorriso"))
	cc := disposableExecutable(t, `#!/bin/sh
output=
previous=
for argument in "$@"; do
  if [ "$previous" = "-o" ]; then output="$argument"; break; fi
  previous="$argument"
done
if [ -z "$output" ]; then exit 9; fi
{
  printf '%s\n' '#!/bin/sh'
  printf '%s\n' 'if [ "$1" != "bios-install" ] || [ "$#" -ne 2 ]; then exit 17; fi'
  printf '%s\n' 'printf "%s\\n" "limine-installed" >> "$2"'
} > "$output"
chmod 755 "$output"
`, filepath.Join(bin, "cc"))
	bwrap := disposableExecutable(t, `#!/bin/bash
workspace=
while [[ $# -gt 0 ]]; do
  if [[ "$1" == "--bind" ]]; then workspace="$2"; shift 3; continue; fi
  if [[ "$1" == "--" ]]; then shift; break; fi
  shift
done
mapped=()
for argument in "$@"; do
  if [[ "$argument" == /workspace/* ]]; then
    mapped+=("$workspace${argument#/workspace}")
  else
    mapped+=("$argument")
  fi
done
exec "${mapped[@]}"
`, filepath.Join(bin, "bwrap"))
	return build.Config{
		RepositoryRoot: repository,
		Tools: build.ToolPaths{
			Bwrap: bwrap, CC: cc, LDD: ldd, CrossCompiler: crossCompiler,
			CrossLinker: crossLinker, Xorriso: xorriso,
		},
		StepTimeout: 3 * time.Second,
	}
}

func disposableExecutable(t *testing.T, contents, path string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
