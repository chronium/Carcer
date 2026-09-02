package build

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"codexos/internal/guest"
	"codexos/internal/observability"
	"codexos/internal/provenance"
	"codexos/internal/qemu"
)

const candidateHelperEnvironment = "CODEXOS_GO_CANDIDATE_HELPER"

func TestCandidateBootValidationUsesPausedControlsAndReapsQEMU(t *testing.T) {
	helper := candidateHelperExecutable(t)
	for _, test := range []struct {
		name       string
		mode       string
		wantStatus BuildStatus
		wantText   string
	}{
		{name: "success", mode: "success", wantStatus: BuildStatusSuccess},
		{name: "no-ready", mode: "no-ready", wantStatus: BuildStatusBuildFailure, wantText: "timed out waiting for CODEXOS-SEED-READY"},
		{name: "bad-protocol", mode: "bad-protocol", wantStatus: BuildStatusBuildFailure, wantText: "canonical list-tools exchange failed"},
		{name: "early-exit", mode: "early-exit", wantStatus: BuildStatusBuildFailure, wantText: "before CODEXOS-SEED-READY"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(candidateHelperEnvironment, test.mode)
			root := candidateRoot(t)
			configureCandidateHelperPID(t, root)
			iso := filepath.Join(root, "candidate.iso")
			if err := os.WriteFile(iso, []byte("synthetic ISO"), 0o600); err != nil {
				t.Fatal(err)
			}
			validator, err := NewCandidateBootValidator(CandidateBootConfig{
				QEMUExecutable:  helper,
				HardwareProfile: qemu.TestHardwareProfile,
				ReadyTimeout:    100 * time.Millisecond,
				TemporaryParent: root,
			})
			if err != nil {
				t.Fatal(err)
			}
			result := validator.Validate(context.Background(), iso, nil, nil)
			if result.Status != test.wantStatus {
				t.Fatalf("status = %s, diagnostics = %s", result.Status, result.Diagnostics)
			}
			if test.wantText != "" && !strings.Contains(result.Diagnostics, test.wantText) {
				t.Fatalf("diagnostics = %q, want %q", result.Diagnostics, test.wantText)
			}
			if test.mode == "no-ready" {
				if !strings.Contains(result.Diagnostics, `CODEXOS-NOT-READY\n\x1b[2J\x00`) || strings.ContainsRune(result.Diagnostics, '\x1b') {
					t.Fatalf("unsafe readiness diagnostics = %q", result.Diagnostics)
				}
			}
			assertCandidateHelperReaped(t)
			if entries, err := filepath.Glob(filepath.Join(root, "codexos-candidate-*")); err != nil {
				t.Fatal(err)
			} else if len(entries) != 0 {
				t.Fatalf("candidate workspaces survived: %v", entries)
			}
		})
	}
}

func TestCandidateBootValidationCancellationStillCleansUp(t *testing.T) {
	helper := candidateHelperExecutable(t)
	t.Setenv(candidateHelperEnvironment, "no-ready")
	root := candidateRoot(t)
	configureCandidateHelperPID(t, root)
	iso := filepath.Join(root, "candidate.iso")
	if err := os.WriteFile(iso, []byte("synthetic ISO"), 0o600); err != nil {
		t.Fatal(err)
	}
	validator, err := NewCandidateBootValidator(CandidateBootConfig{
		QEMUExecutable:  helper,
		HardwareProfile: qemu.TestHardwareProfile,
		ReadyTimeout:    5 * time.Second,
		TemporaryParent: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	resultChannel := make(chan CandidateBootResult, 1)
	go func() { resultChannel <- validator.Validate(ctx, iso, nil, nil) }()
	waitCandidateHelperPID(t)
	cancel()
	select {
	case result := <-resultChannel:
		if result.Status != BuildStatusHarnessFailure || !strings.Contains(result.Diagnostics, "context canceled") {
			t.Fatalf("cancelled validation = %#v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled validation did not return")
	}
	assertCandidateHelperReaped(t)
}

func TestCandidateBootValidationPublishesActivityAndEvidence(t *testing.T) {
	helper := candidateHelperExecutable(t)
	t.Setenv(candidateHelperEnvironment, "success")
	root := candidateRoot(t)
	configureCandidateHelperPID(t, root)
	iso := filepath.Join(root, "candidate.iso")
	if err := os.WriteFile(iso, []byte("synthetic ISO"), 0o600); err != nil {
		t.Fatal(err)
	}
	generation := uint64(4)
	activity := observability.NewActivityStream()
	provenanceRoot := filepath.Join(root, "run")
	evidence, err := newTestCandidateEvidence(provenanceRoot, generation)
	if err != nil {
		t.Fatal(err)
	}
	isoIdentity := testFileIdentity
	validator, err := NewCandidateBootValidator(CandidateBootConfig{
		QEMUExecutable:  helper,
		HardwareProfile: qemu.TestHardwareProfile,
		TemporaryParent: root,
		ActivityStream:  activity,
		Generation:      &generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := validator.Validate(context.Background(), iso, evidence, &isoIdentity)
	if result.Status != BuildStatusSuccess {
		t.Fatalf("validation = %#v", result)
	}
	events := activity.Drain()
	wantKinds := []observability.ActivityKind{
		observability.ActivityBuildCandidateStarted,
		observability.ActivityBuildCandidateReady,
		observability.ActivityBuildProtocolValidated,
	}
	if len(events) != len(wantKinds) {
		t.Fatalf("activity events = %#v", events)
	}
	for index, event := range events {
		if event.Kind != wantKinds[index] || event.Generation == nil || *event.Generation != generation || event.Role != observability.ActivityHarness {
			t.Fatalf("activity[%d] = %#v", index, event)
		}
		if event.Data["build_attempt_id"] != evidence.AttemptID() {
			t.Fatalf("activity[%d] identity = %#v", index, event.Data)
		}
	}
	manifestPath := filepath.Join(
		provenanceRoot,
		"build-review-provenance",
		"generation-0004",
		evidence.AttemptID(),
		"manifest.json",
	)
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(manifest, &object); err != nil {
		t.Fatal(err)
	}
	candidate, ok := object["candidate_validation"].(map[string]any)
	if !ok || candidate["ready"] != true || candidate["protocol_validated"] != true || candidate["outcome"] != "success" {
		t.Fatalf("candidate evidence = %#v", object["candidate_validation"])
	}
	assertCandidateHelperReaped(t)
}

func TestCandidateBootValidationInfrastructureFailureUsesHarnessStatus(t *testing.T) {
	root := candidateRoot(t)
	validator, err := NewCandidateBootValidator(CandidateBootConfig{
		QEMUExecutable:  filepath.Join(root, "missing-qemu"),
		HardwareProfile: qemu.TestHardwareProfile,
		TemporaryParent: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := validator.Validate(context.Background(), filepath.Join(root, "missing.iso"), nil, nil)
	if result.Status != BuildStatusHarnessFailure || !strings.Contains(result.Diagnostics, "could not start candidate QEMU") {
		t.Fatalf("infrastructure validation = %#v", result)
	}
	if entries, err := filepath.Glob(filepath.Join(root, "codexos-candidate-*")); err != nil {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("candidate workspaces survived: %v", entries)
	}
}

func TestCandidateBootValidationLeavesEvidenceIncompleteWhenStageWriteFails(t *testing.T) {
	root := candidateRoot(t)
	generation := uint64(5)
	evidence, err := newTestCandidateEvidence(filepath.Join(root, "run"), generation)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(
		root,
		"run",
		"build-review-provenance",
		"generation-0005",
		evidence.AttemptID(),
		"manifest.json",
	)
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(manifestPath, 0o700); err != nil {
		t.Fatal(err)
	}
	validator, err := NewCandidateBootValidator(CandidateBootConfig{
		QEMUExecutable:  filepath.Join(root, "missing-qemu"),
		HardwareProfile: qemu.TestHardwareProfile,
		TemporaryParent: root,
		Generation:      &generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := validator.Validate(context.Background(), filepath.Join(root, "missing.iso"), evidence, nil)
	if !result.provenanceFailure || result.Status != BuildStatusHarnessFailure {
		t.Fatalf("stage write failure = %#v, want an unfinalized provenance failure", result)
	}
}

func TestCandidateProtocolErrorsUsePythonFailureCategories(t *testing.T) {
	for _, test := range []struct {
		name      string
		err       error
		readiness bool
		guest     bool
	}{
		{name: "serial", err: &guest.SerialError{Reason: "closed"}, guest: true},
		{name: "framing", err: &guest.FramingError{Reason: "bad frame"}, guest: true},
		{name: "tool protocol", err: &guest.ToolProtocolError{Reason: "bad response"}, guest: true},
		{name: "ready timeout", err: &guest.DispatcherError{Reason: "timed out waiting for CODEXOS-SEED-READY"}, readiness: true, guest: true},
		{name: "ready timeout in exchange", err: &guest.DispatcherError{Reason: "timed out waiting for CODEXOS-SEED-READY"}, guest: false},
		{name: "response timeout", err: &guest.DispatcherError{Reason: "timed out waiting for tool response"}, guest: true},
		{name: "write timeout", err: &guest.DispatcherError{Reason: "timed out writing serial protocol frame"}, guest: true},
		{name: "dispatcher state", err: &guest.DispatcherError{Reason: "guest serial protocol is not ready"}, guest: false},
		{name: "unknown", err: errors.New("internal failure"), guest: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := candidateGuestProtocolError(test.err, test.readiness); got != test.guest {
				t.Fatalf("candidateGuestProtocolError() = %v, want %v", got, test.guest)
			}
		})
	}
}

var testFileIdentity = provenance.FileIdentity{SHA256: strings.Repeat("a", 64), Size: 12}

func candidateRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "cb-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

func configureCandidateHelperPID(t *testing.T, root string) {
	t.Helper()
	t.Setenv("CODEXOS_GO_CANDIDATE_PID", filepath.Join(root, "candidate.pid"))
}

// newTestCandidateEvidence keeps the integration fixture independent of the
// trusted source compiler while still exercising candidate provenance.
func newTestCandidateEvidence(root string, generation uint64) (*provenance.BuildAttemptEvidence, error) {
	return provenance.NewBuildReviewProvenance(root).BeginBuild(generation, []byte("snapshot"))
}

func candidateHelperExecutable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "candidate-qemu-helper.sh")
	contents := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=^TestCandidateQEMUHelper$ -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCandidateQEMUHelper(t *testing.T) {
	if os.Getenv(candidateHelperEnvironment) == "" {
		return
	}
	qmpPath, serialPath := candidateHelperSocketPaths(os.Args[1:])
	if qmpPath == "" || serialPath == "" {
		t.Fatal("candidate helper could not locate control sockets")
	}
	qmpListener, err := net.Listen("unix", qmpPath)
	if err != nil {
		t.Fatal(err)
	}
	defer qmpListener.Close()
	serialListener, err := net.Listen("unix", serialPath)
	if err != nil {
		t.Fatal(err)
	}
	defer serialListener.Close()
	pidPath := os.Getenv("CODEXOS_GO_CANDIDATE_PID")
	if pidPath != "" {
		if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	qmpConnection, err := qmpListener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer qmpConnection.Close()
	if _, err := qmpConnection.Write([]byte(`{"QMP":{"version":{},"capabilities":[]}}` + "\r\n")); err != nil {
		t.Fatal(err)
	}
	qmpReader := bufio.NewReader(qmpConnection)
	if err := candidateHelperQMPCommand(qmpReader, qmpConnection, "qmp_capabilities", 1); err != nil {
		t.Fatal(err)
	}
	serialConnection, err := serialListener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer serialConnection.Close()
	if err := candidateHelperQMPCommand(qmpReader, qmpConnection, "cont", 2); err != nil {
		t.Fatal(err)
	}
	mode := os.Getenv(candidateHelperEnvironment)
	if mode == "no-ready" {
		if _, err := serialConnection.Write([]byte("CODEXOS-NOT-READY\n\x1b[2J\x00")); err != nil {
			t.Fatal(err)
		}
	} else if mode == "early-exit" {
		return
	} else {
		if _, err := serialConnection.Write([]byte(guest.ReadyMarker)); err != nil {
			t.Fatal(err)
		}
		request, err := guest.ReadFrame(serialConnection)
		if err != nil {
			return
		}
		if mode == "bad-protocol" {
			_, _ = serialConnection.Write([]byte("BROKEN-PROTOCOL!"))
		} else if request.MessageType == guest.ListToolsRequest {
			response, err := guest.EncodeFrame(guest.Frame{
				MessageType: guest.ListToolsResponse,
				RequestID:   request.RequestID,
				Payload:     []byte{0, 0},
			})
			if err != nil {
				t.Fatal(err)
			}
			_, _ = serialConnection.Write(response)
		}
	}
	for {
		line, err := qmpReader.ReadString('\n')
		if err != nil {
			return
		}
		var request map[string]any
		if json.Unmarshal([]byte(line), &request) != nil {
			return
		}
		if request["execute"] == "quit" {
			id := request["id"]
			response, _ := json.Marshal(map[string]any{"return": map[string]any{}, "id": id})
			_, _ = qmpConnection.Write(append(response, '\r', '\n'))
			return
		}
	}
}

func candidateHelperQMPCommand(reader *bufio.Reader, connection net.Conn, command string, id int) error {
	line, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	var request map[string]any
	if err := json.Unmarshal([]byte(line), &request); err != nil {
		return err
	}
	if request["execute"] != command {
		return fmt.Errorf("QMP command = %v, want %s", request["execute"], command)
	}
	response, _ := json.Marshal(map[string]any{"return": map[string]any{}, "id": id})
	_, err = connection.Write(append(response, '\r', '\n'))
	return err
}

func candidateHelperSocketPaths(arguments []string) (string, string) {
	qmpPath := ""
	serialPath := ""
	for index, argument := range arguments {
		if argument == "-qmp" && index+1 < len(arguments) {
			qmpPath = strings.TrimPrefix(strings.Split(arguments[index+1], ",")[0], "unix:")
		}
		if strings.HasPrefix(argument, "socket,id=codexos-com1,path=") {
			serialPath = strings.Split(strings.TrimPrefix(argument, "socket,id=codexos-com1,path="), ",")[0]
		}
	}
	return qmpPath, serialPath
}

func waitCandidateHelperPID(t *testing.T) int {
	t.Helper()
	path := os.Getenv("CODEXOS_GO_CANDIDATE_PID")
	if path == "" {
		t.Fatal("candidate helper PID path is not configured")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(string(data))
			if parseErr == nil {
				return pid
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("candidate helper did not start")
	return 0
}

func assertCandidateHelperReaped(t *testing.T) {
	t.Helper()
	path := os.Getenv("CODEXOS_GO_CANDIDATE_PID")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("candidate helper %d survived cleanup: %v", pid, err)
	}
}
