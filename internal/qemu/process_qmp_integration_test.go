package qemu

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestDisposableQEMUProcessAndQMPLifecycle(t *testing.T) {
	executable, err := exec.LookPath("qemu-system-x86_64")
	if err != nil {
		t.Skip("qemu-system-x86_64 is not installed")
	}
	temporary := t.TempDir()
	qmpPath := filepath.Join(temporary, "qmp.sock")
	controller := NewQEMUProcessController(executable)
	if err := controller.Start(QEMUStartOptions{
		Arguments: []string{
			"-display", "none",
			"-monitor", "none",
			"-serial", "none",
			"-nodefaults",
			"-machine", "none",
		},
		StdoutPath:    filepath.Join(temporary, "qemu.stdout"),
		StderrPath:    filepath.Join(temporary, "qemu.stderr"),
		QMPSocketPath: &qmpPath,
	}); err != nil {
		t.Fatalf("start disposable QEMU: %v", err)
	}
	t.Cleanup(func() { _ = controller.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	qmp := NewQMPClient(qmpPath)
	if err := qmp.Connect(ctx); err != nil {
		t.Fatalf("connect QMP: %v", err)
	}
	t.Cleanup(func() { _ = qmp.Close() })
	status, err := qmp.QueryStatus(ctx)
	if err != nil || status != "running" {
		t.Fatalf("initial status = %q, %v", status, err)
	}
	if err := qmp.Stop(ctx); err != nil {
		t.Fatalf("QMP stop: %v", err)
	}
	status, err = qmp.QueryStatus(ctx)
	if err != nil || status != "paused" {
		t.Fatalf("paused status = %q, %v", status, err)
	}
	if err := qmp.Continue(ctx); err != nil {
		t.Fatalf("QMP continue: %v", err)
	}
	if err := qmp.Quit(ctx); err != nil {
		t.Fatalf("QMP quit: %v", err)
	}
	if err := qmp.Close(); err != nil {
		t.Fatalf("close QMP: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for controller.IsRunning() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if controller.IsRunning() {
		t.Fatal("QEMU did not exit after QMP quit")
	}
}
