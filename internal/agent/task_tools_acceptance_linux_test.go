//go:build linux

package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"codexos/internal/guest"
	"codexos/internal/qemu"
	"codexos/internal/store"
)

// This opt-in exercise copies an operator-selected image, boots one disposable
// VM and uses actual serial tools. It opens no experiment run or Codex session.
func TestGuestTaskToolsRealGuest(t *testing.T) {
	imagePath := os.Getenv("CODEXOS_GUEST_TASK_TOOL_ISO")
	if imagePath == "" {
		t.Skip("set CODEXOS_GUEST_TASK_TOOL_ISO to an image advertising run, reap and import_provided_asset")
	}
	executable, err := exec.LookPath("qemu-system-x86_64")
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	imageHash := sha256.Sum256(image)
	root, err := os.MkdirTemp("/tmp", "co-task-tools-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	iso := filepath.Join(root, "guest.iso")
	if err := os.WriteFile(iso, image, 0400); err != nil {
		t.Fatal(err)
	}
	// A sleeping CXE1 program exits above the signed 64-bit range. The fault
	// fixture executes UD2. Both enter the guest's ordinary loader and scheduler.
	sleeper := []byte{0xb8, 11, 0, 0, 0, 0xbf, 0xe8, 3, 0, 0, 0xcd, 0x80, 0x48, 0xbf}
	sleeper = binary.LittleEndian.AppendUint64(sleeper, 0x8000000000000001)
	sleeper = append(sleeper, 0x31, 0xc0, 0xcd, 0x80, 0x0f, 0x0b)
	fixtures := map[string][]byte{"fixture-exit": taskToolCXE1(sleeper), "fixture-fault": taskToolCXE1([]byte{0x0f, 0x0b}), "fixture-empty": {}}
	assetDir := filepath.Join(root, "assets")
	for id, data := range fixtures {
		dir := filepath.Join(assetDir, id)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "fixture.bin"), data, 0400); err != nil {
			t.Fatal(err)
		}
	}
	assets, err := store.LoadProvidedAssets(assetDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	version, err := qemu.DiscoverQEMUVersion(ctx, executable)
	if err != nil {
		t.Fatal(err)
	}
	profile := qemu.TestHardwareProfile
	t.Logf("image sha256=%x; %s; profile=%s CPU=%s vCPUs=%d RAM=%dMiB", imageHash, version, profile.Profile, profile.CPUModel, profile.VCPUs, profile.MemoryMiB)
	qmpPath, serialPath := filepath.Join(root, "qmp.sock"), filepath.Join(root, "serial.sock")
	qmp := qemu.NewQMPClient(qmpPath)
	serial := guest.NewSerialConnection(serialPath)
	controller := qemu.NewQEMUProcessController(executable)
	var dispatcher *guest.SerialProtocolDispatcher
	pid := 0
	defer func() {
		if dispatcher != nil {
			if err := dispatcher.Close(); err != nil {
				t.Error(err)
			}
		} else {
			_ = serial.Close()
		}
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = qmp.Quit(stopCtx)
		_ = qmp.Close()
		if err := controller.Stop(stopCtx, 2*time.Second); err != nil {
			t.Error(err)
		}
		if pid > 0 {
			waitForGenerationProcessExit(t, pid)
		}
		if controller.IsRunning() {
			t.Error("disposable QEMU survived cleanup")
		}
		original, err := os.ReadFile(imagePath)
		if err != nil || sha256.Sum256(original) != imageHash {
			t.Errorf("input image changed: %v", err)
		}
	}()
	arguments, err := profile.QEMUCommandArguments(iso, qmpPath, serialPath)
	if err != nil {
		t.Fatal(err)
	}
	arguments = append(arguments, "-S")
	if err := controller.Start(qemu.QEMUStartOptions{Arguments: arguments, StdoutPath: filepath.Join(root, "qemu.stdout"), StderrPath: filepath.Join(root, "qemu.stderr")}); err != nil {
		t.Fatal(err)
	}
	pid, _ = controller.PID()
	if err := qmp.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := serial.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	handler := func(_ context.Context, request guest.HostRequest) (guest.Frame, error) {
		return assets.HandleRequest(request)
	}
	dispatcher = guest.NewSerialProtocolDispatcher(serial, guest.DispatcherOptions{StartupHostServices: handler, BackgroundHostServices: handler, ExchangeHostServices: handler})
	if err := dispatcher.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := qmp.Continue(ctx); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.WaitUntilReady(ctx, 25*time.Second); err != nil {
		stderr, _ := os.ReadFile(filepath.Join(root, "qemu.stderr"))
		t.Fatalf("READY: %v; serial=%s; QEMU=%s", err, guest.EscapeDiagnosticBytes(dispatcher.StartupDiagnostic()), stderr)
	}
	client := guest.NewToolClient(dispatcher)
	advertised, err := client.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	selected, _, err := advertisedGuestToolsInOrder(advertised)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"run", "reap", "import_provided_asset", "read", "write", "truncate", "remove"} {
		if _, ok := selected[name]; !ok {
			t.Fatalf("real guest does not advertise %s: %v", name, advertised)
		}
	}
	runtime := newGenerationTestRuntime(t)
	runtime.invoke = client.InvokeTool
	session := NewGenerationSession(runtime, GenerationSessionOptions{})
	session.runCtx = ctx
	session.availableTools = selected
	call := func(tool string, args map[string]any, wantStatus uint32) guest.ToolResult {
		t.Helper()
		result, err := session.dispatchTool(tool, args)
		if err != nil || result.Status != wantStatus {
			t.Fatalf("%s(%v): status=%d output=%q err=%v", tool, args, result.Status, result.Output, err)
		}
		return result
	}
	importAsset := func(id, path string) {
		t.Helper()
		result := call("import_provided_asset", map[string]any{"id": id, "path": path}, 0)
		if len(result.Output) != 0 {
			t.Fatalf("import output=%q", result.Output)
		}
		read := call("read", map[string]any{"path": path, "offset": 0, "length": len(fixtures[id])}, 0)
		if !bytes.Equal(read.Output, fixtures[id]) {
			t.Fatal("imported bytes changed")
		}
	}
	const path = "ram/bridge-exit.cxe"
	importAsset("fixture-exit", path)
	call("import_provided_asset", map[string]any{"id": "fixture-exit", "path": path}, 1)
	call("import_provided_asset", map[string]any{"id": "missing", "path": "ram/missing"}, 1)
	call("read", map[string]any{"path": "ram/missing", "offset": 0, "length": 1}, 1)
	call("write", map[string]any{"path": path, "offset": 0, "data": "changed"}, 1)
	call("truncate", map[string]any{"path": path, "size": 0}, 1)
	call("remove", map[string]any{"path": path}, 1)
	unchanged := call("read", map[string]any{"path": path, "offset": 0, "length": len(fixtures["fixture-exit"])}, 0)
	if !bytes.Equal(unchanged.Output, fixtures["fixture-exit"]) {
		t.Fatal("immutable import changed")
	}
	importAsset("fixture-empty", "ram/empty")
	call("write", map[string]any{"path": "ram/empty", "offset": 0, "data": "x"}, 1)
	call("run", map[string]any{"path": "ram/empty"}, 1)
	call("run", map[string]any{"path": "ram/missing"}, 1)
	call("reap", map[string]any{"task_id": 0}, 1)
	call("reap", map[string]any{"task_id": uint64(4294967295)}, 1)
	runTask := func(path string) uint64 {
		t.Helper()
		result := call("run", map[string]any{"path": path}, 0)
		task, err := strconv.ParseUint(string(result.Output), 10, 32)
		if err != nil || task == 0 {
			t.Fatalf("task ID=%q err=%v", result.Output, err)
		}
		t.Logf("run(%q) task ID=%s", path, result.Output)
		return task
	}
	reapUntilExit := func(task uint64, want string) {
		t.Helper()
		deadline := time.NewTimer(20 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			result := call("reap", map[string]any{"task_id": task}, 0)
			if string(result.Output) != "running" {
				if string(result.Output) != want {
					t.Fatalf("exit=%q want=%q", result.Output, want)
				}
				t.Logf("reap(%d) exit=%s tool status=%d", task, result.Output, result.Status)
				break
			}
			select {
			case <-ticker.C:
			case <-deadline.C:
				t.Fatal("task did not complete")
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		call("reap", map[string]any{"task_id": task}, 1)
	}
	task := runTask(path)
	running := call("reap", map[string]any{"task_id": task}, 0)
	if string(running.Output) != "running" {
		t.Fatalf("sleeping task=%q", running.Output)
	}
	t.Logf("reap(%d) output=%q", task, running.Output)
	reapUntilExit(task, "9223372036854775809")
	importAsset("fixture-fault", "ram/fault.cxe")
	reapUntilExit(runTask("ram/fault.cxe"), "18446744073709551615")
	t.Log("actual guest import/readback/sealing, running/completed reap, 64-bit exits, consumed-ID and invalid-input failures passed")
}

func taskToolCXE1(code []byte) []byte {
	image := append([]byte("CXE1"), make([]byte, 12)...)
	binary.LittleEndian.PutUint32(image[4:8], uint32(len(code)))
	return append(image, code...)
}
