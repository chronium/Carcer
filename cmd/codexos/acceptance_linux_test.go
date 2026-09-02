//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"codexos/internal/experiment"
	"codexos/internal/observability"
	"codexos/internal/qemu"
)

// TestCodexOSBinaryOperatesAtDisposableGate builds and executes the real
// command. Unlike subprocess helpers that re-enter a test binary, this cannot
// recursively start the test suite.
func TestCodexOSBinaryOperatesAtDisposableGate(t *testing.T) {
	binary := buildCodexOSBinary(t)

	t.Run("plain frontend", func(t *testing.T) {
		runDirectory := writeDisposableBinaryGate(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary,
			"--run-directory", runDirectory,
			"--resume-at-gate",
			"--plain",
		)
		command.Stdin = strings.NewReader("quit\n")
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		command.Stdout = &stdout
		command.Stderr = &stderr
		if err := command.Run(); err != nil {
			t.Fatalf("run binary: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
		if ctx.Err() != nil {
			t.Fatalf("binary did not stop before deadline: %v", ctx.Err())
		}
		for _, want := range []string{
			"CodexOS operator console",
			"Generation 0 aborted.",
			"No successor was selected.",
		} {
			if !strings.Contains(stdout.String(), want) {
				t.Fatalf("stdout missing %q:\n%s", want, stdout.String())
			}
		}
		if stderr.Len() != 0 {
			t.Fatalf("binary wrote stderr: %s", stderr.String())
		}
		assertBinaryGateEventOrder(t, runDirectory)
	})

	t.Run("TUI signal shutdown restores terminal", func(t *testing.T) {
		runDirectory := writeDisposableBinaryGate(t)
		master, slave := openPseudoTerminal(t)
		defer master.Close()
		before, err := unix.IoctlGetTermios(int(master.Fd()), unix.TCGETS)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary,
			"--run-directory", runDirectory,
			"--resume-at-gate",
			"--tui",
		)
		command.Stdin = slave
		command.Stdout = slave
		var stderr bytes.Buffer
		command.Stderr = &stderr
		command.Env = append(os.Environ(), "TERM=xterm-256color")
		if err := command.Start(); err != nil {
			_ = slave.Close()
			t.Fatal(err)
		}
		if err := slave.Close(); err != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatal(err)
		}
		var rendered bytes.Buffer
		drainDone := make(chan error, 1)
		go func() {
			_, copyErr := io.Copy(&rendered, master)
			drainDone <- copyErr
		}()

		if !waitForRawTerminal(master, 2*time.Second) {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatalf("binary did not enter raw terminal mode\nstderr:\n%s", stderr.String())
		}
		if err := command.Process.Signal(syscall.SIGTERM); err != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatal(err)
		}
		waitDone := make(chan error, 1)
		go func() { waitDone <- command.Wait() }()
		select {
		case err := <-waitDone:
			if err != nil {
				t.Fatalf("binary signal shutdown: %v\nstderr:\n%s", err, stderr.String())
			}
		case <-time.After(3 * time.Second):
			_ = command.Process.Kill()
			<-waitDone
			t.Fatal("binary did not stop after SIGTERM")
		}
		after, err := unix.IoctlGetTermios(int(master.Fd()), unix.TCGETS)
		if err != nil {
			t.Fatal(err)
		}
		if *after != *before {
			t.Fatalf("binary did not restore terminal state:\nbefore=%#v\nafter=%#v", *before, *after)
		}
		if stderr.Len() != 0 {
			t.Fatalf("binary wrote stderr: %s", stderr.String())
		}
		assertBinaryGateEventOrder(t, runDirectory)
		_ = master.Close()
		select {
		case readErr := <-drainDone:
			if !isExpectedPTYReadEnd(readErr) {
				t.Fatalf("terminal output reader: %v", readErr)
			}
		case <-time.After(time.Second):
			t.Fatal("terminal output reader did not stop")
		}
		if !bytes.Contains(rendered.Bytes(), []byte("\x1b")) {
			t.Fatalf("TUI binary emitted no terminal presentation: %q", rendered.String())
		}
	})
}

func buildCodexOSBinary(t *testing.T) string {
	t.Helper()
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "codexos")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/codexos")
	command.Dir = repositoryRoot
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build cmd/codexos: %v\n%s", err, output)
	}
	return binary
}

func writeDisposableBinaryGate(t *testing.T) string {
	t.Helper()
	runDirectory := filepath.Join(t.TempDir(), "run")
	hardware, err := qemu.TestHardwareProfile.Manifest("QEMU emulator version binary-acceptance")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := experiment.WriteAbortedArchive(runDirectory, experiment.AbortedArchive{
		Generation: 0,
		Transition: "initial",
		Hardware:   hardware,
		BootISO:    []byte("disposable archived boot image"),
	}); err != nil {
		t.Fatal(err)
	}
	return runDirectory
}

func assertBinaryGateEventOrder(t *testing.T, runDirectory string) {
	t.Helper()
	events, err := os.ReadFile(filepath.Join(runDirectory, observability.EventLogFilename))
	if err != nil {
		t.Fatal(err)
	}
	reopened := bytes.Index(events, []byte(`"event":"run_reopened_at_gate"`))
	stopped := bytes.Index(events, []byte(`"event":"run_stopped"`))
	if reopened < 0 || stopped < 0 || reopened >= stopped {
		t.Fatalf("binary startup/shutdown events are out of order:\n%s", events)
	}
}

func openPseudoTerminal(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("pseudo-terminal unavailable: %v", err)
	}
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		master.Close()
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		t.Fatal(err)
	}
	window := &unix.Winsize{Row: 24, Col: 80}
	if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, window); err != nil {
		slave.Close()
		master.Close()
		t.Fatal(err)
	}
	return master, slave
}

func waitForRawTerminal(terminal *os.File, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, err := unix.IoctlGetTermios(int(terminal.Fd()), unix.TCGETS)
		if err == nil && state.Lflag&unix.ICANON == 0 {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func isExpectedPTYReadEnd(err error) bool {
	return err == nil || errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrClosed)
}
