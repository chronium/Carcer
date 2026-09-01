package qemu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

const qemuHelperEnvironment = "CODEXOS_GO_QEMU_HELPER"

func TestQEMUProcessControllerLifecycle(t *testing.T) {
	t.Setenv(qemuHelperEnvironment, "1")
	temporary := t.TempDir()
	stdoutPath := filepath.Join(temporary, "qemu.stdout")
	stderrPath := filepath.Join(temporary, "qemu.stderr")
	qmpPath := filepath.Join(temporary, "qmp.sock")
	serialPath := filepath.Join(temporary, "serial.sock")
	controller := NewQEMUProcessController(os.Args[0])
	options := helperQEMUOptions("wait", stdoutPath, stderrPath)
	options.QMPSocketPath = &qmpPath
	options.SerialSocketPath = &serialPath
	if err := controller.Start(options); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	waitForFileText(t, stdoutPath, "ready")

	if !controller.IsRunning() {
		t.Fatal("controller does not report its child running")
	}
	pid, ok := controller.PID()
	if !ok || pid <= 0 {
		t.Fatalf("PID = %d, %v", pid, ok)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("child PID is unavailable: %v", err)
	}
	if err := controller.Start(options); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("duplicate Start error = %v, want already running", err)
	}

	if err := controller.Stop(context.Background(), time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if controller.IsRunning() {
		t.Fatal("controller still reports running after Stop")
	}
	if _, ok := controller.PID(); ok {
		t.Fatal("controller retained a PID after Stop")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("stopped child still exists or returned unexpected error: %v", err)
	}
	if err := controller.Stop(context.Background(), time.Second); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if err := controller.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	stdout, err := os.ReadFile(stdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	gotArguments := strings.Split(strings.TrimSpace(string(stdout)), "\n")
	wantArguments := []string{
		"wait",
		"-qmp",
		"unix:" + qmpPath + ",server=on,wait=off",
		"-serial",
		"unix:" + serialPath + ",server=on,wait=off",
		"ready",
	}
	if !reflect.DeepEqual(gotArguments, wantArguments) {
		t.Fatalf("child arguments = %#v, want %#v", gotArguments, wantArguments)
	}
	stderr, err := os.ReadFile(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stderr), "helper stderr") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestQEMUProcessControllerKillsAfterTerminateTimeout(t *testing.T) {
	t.Setenv(qemuHelperEnvironment, "1")
	temporary := t.TempDir()
	stdoutPath := filepath.Join(temporary, "qemu.stdout")
	controller := NewQEMUProcessController(os.Args[0])
	if err := controller.Start(helperQEMUOptions("ignore-term", stdoutPath, filepath.Join(temporary, "qemu.stderr"))); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	waitForFileText(t, stdoutPath, "ready")
	started := time.Now()
	if err := controller.Stop(context.Background(), 20*time.Millisecond); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond || elapsed > time.Second {
		t.Fatalf("fallback kill took %s", elapsed)
	}
	if controller.IsRunning() {
		t.Fatal("killed child still reports running")
	}
}

func TestQEMUProcessControllerCancellationStillKillsAndReaps(t *testing.T) {
	t.Setenv(qemuHelperEnvironment, "1")
	temporary := t.TempDir()
	stdoutPath := filepath.Join(temporary, "qemu.stdout")
	controller := NewQEMUProcessController(os.Args[0])
	if err := controller.Start(helperQEMUOptions("ignore-term", stdoutPath, filepath.Join(temporary, "qemu.stderr"))); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	waitForFileText(t, stdoutPath, "ready")
	pid, ok := controller.PID()
	if !ok {
		t.Fatal("controller has no PID before canceled Stop")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := controller.Stop(ctx, time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop error = %v, want context canceled", err)
	}
	if controller.IsRunning() {
		t.Fatal("canceled Stop abandoned the child")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("canceled Stop did not reap PID %d: %v", pid, err)
	}
}

func TestQEMUProcessControllerReapsSpontaneousExitAndRestarts(t *testing.T) {
	t.Setenv(qemuHelperEnvironment, "1")
	temporary := t.TempDir()
	controller := NewQEMUProcessController(os.Args[0])
	first := helperQEMUOptions("exit", filepath.Join(temporary, "first.stdout"), filepath.Join(temporary, "first.stderr"))
	if err := controller.Start(first); err != nil {
		t.Fatal(err)
	}
	controller.mutex.Lock()
	firstDone := controller.done
	controller.mutex.Unlock()
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first child was not reaped")
	}
	second := helperQEMUOptions("exit", filepath.Join(temporary, "second.stdout"), filepath.Join(temporary, "second.stderr"))
	if err := controller.Start(second); err != nil {
		t.Fatalf("restart after spontaneous exit: %v", err)
	}
	waitForControllerExit(t, controller)
}

func TestQEMUProcessControllerStartFailuresLeaveItReusable(t *testing.T) {
	t.Setenv(qemuHelperEnvironment, "1")
	temporary := t.TempDir()
	controller := NewQEMUProcessController(os.Args[0])
	bad := helperQEMUOptions(
		"exit",
		filepath.Join(temporary, "failed.stdout"),
		filepath.Join(temporary, "missing", "failed.stderr"),
	)
	if err := controller.Start(bad); err == nil || !strings.Contains(err.Error(), "stderr log") {
		t.Fatalf("Start error = %v, want stderr log failure", err)
	}
	if controller.IsRunning() {
		t.Fatal("failed Start left the controller running")
	}
	if controller.stdout != nil || controller.stderr != nil {
		t.Fatal("failed Start retained a log descriptor")
	}
	good := helperQEMUOptions("exit", filepath.Join(temporary, "good.stdout"), filepath.Join(temporary, "good.stderr"))
	if err := controller.Start(good); err != nil {
		t.Fatalf("Start after log failure: %v", err)
	}
	waitForControllerExit(t, controller)

	missing := NewQEMUProcessController(filepath.Join(temporary, "missing-qemu"))
	missingOptions := QEMUStartOptions{
		StdoutPath: filepath.Join(temporary, "missing.stdout"),
		StderrPath: filepath.Join(temporary, "missing.stderr"),
	}
	if err := missing.Start(missingOptions); err == nil || !strings.Contains(err.Error(), "could not start QEMU") {
		t.Fatalf("missing executable error = %v", err)
	}
	if missing.IsRunning() {
		t.Fatal("missing executable left the controller running")
	}
}

func TestQEMUProcessControllerRejectsNegativeStopTimeout(t *testing.T) {
	controller := NewQEMUProcessController("unused")
	if err := controller.Stop(context.Background(), -time.Second); err != nil {
		t.Fatalf("idle Stop with a negative timeout should be a no-op: %v", err)
	}
}

func TestQEMUProcessControllerRejectsNilStopContext(t *testing.T) {
	controller := NewQEMUProcessController("unused")
	err := controller.Stop(nil, time.Second)
	if err == nil || !strings.Contains(err.Error(), "context is nil") {
		t.Fatalf("Stop error = %v, want nil context error", err)
	}
}

func TestQEMUProcessHelper(t *testing.T) {
	if os.Getenv(qemuHelperEnvironment) != "1" {
		return
	}
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		fmt.Fprintln(os.Stderr, "missing helper mode")
		os.Exit(2)
	}
	arguments := os.Args[separator+1:]
	mode := arguments[0]
	if mode == "ignore-term" {
		signal.Ignore(syscall.SIGTERM)
	}
	for _, argument := range arguments {
		fmt.Fprintln(os.Stdout, argument)
	}
	fmt.Fprintln(os.Stdout, "ready")
	fmt.Fprintln(os.Stderr, "helper stderr")
	switch mode {
	case "exit":
		os.Exit(0)
	case "ignore-term":
		select {}
	case "wait":
		terminated := make(chan os.Signal, 1)
		signal.Notify(terminated, syscall.SIGTERM)
		<-terminated
		signal.Stop(terminated)
		os.Exit(0)
	default:
		os.Exit(3)
	}
}

func helperQEMUOptions(mode, stdoutPath, stderrPath string) QEMUStartOptions {
	return QEMUStartOptions{
		Arguments:  []string{"-test.run=^TestQEMUProcessHelper$", "--", mode},
		StdoutPath: stdoutPath,
		StderrPath: stderrPath,
	}
}

func waitForFileText(t *testing.T, path, text string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		contents, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(contents), text) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s did not contain %q before timeout", path, text)
}

func waitForControllerExit(t *testing.T, controller *QEMUProcessController) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for controller.IsRunning() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if controller.IsRunning() {
		t.Fatal("controller did not reap its exited child")
	}
}
