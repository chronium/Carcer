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

	"codexos/internal/guest"
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
