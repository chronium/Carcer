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
	"strconv"
	"strings"
	"sync"
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

func TestCodexOSBinaryCompletesDisposableGenerationThroughTUI(t *testing.T) {
	if err := qemu.ExperimentHardwareProfile.RequireAvailable(); err != nil {
		t.Skipf("actual binary requires its production KVM profile: %v", err)
	}
	binary := buildCodexOSBinary(t)
	repositoryRoot := codexOSRepositoryRoot(t)
	fixtureBin := writeDisposableBinaryLiveFixtures(t, repositoryRoot)
	processRecords := t.TempDir()
	authDirectory := t.TempDir()
	if err := os.WriteFile(filepath.Join(authDirectory, "auth.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runDirectory, err := os.MkdirTemp("/tmp", "co-bin-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDirectory) })
	initialISO := filepath.Join(t.TempDir(), "initial.iso")
	if err := os.WriteFile(initialISO, []byte("binary live initial image"), 0o600); err != nil {
		t.Fatal(err)
	}

	master, slave := openPseudoTerminal(t)
	before, err := unix.IoctlGetTermios(int(master.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	command := exec.CommandContext(ctx, binary,
		"--run-directory", runDirectory,
		"--initial-iso", initialISO,
		"--tui",
	)
	command.Dir = repositoryRoot
	command.Stdin = slave
	command.Stdout = slave
	var stderr synchronizedBinaryBuffer
	command.Stderr = &stderr
	command.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"PATH="+fixtureBin+":/usr/bin:/bin",
		"CODEX_HOME="+authDirectory,
		"CODEXOS_DISPOSABLE_QEMU_LIFECYCLE=lifecycle",
		"CODEXOS_DISPOSABLE_PROCESS_RECORDS="+processRecords,
	)
	if err := command.Start(); err != nil {
		_ = slave.Close()
		t.Fatal(err)
	}
	if err := slave.Close(); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal(err)
	}
	var rendered synchronizedBinaryBuffer
	drainDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(&rendered, master)
		drainDone <- copyErr
	}()
	waitDone := make(chan error, 1)
	go func() { waitDone <- command.Wait() }()
	commandFinished := false
	drainFinished := false
	t.Cleanup(func() {
		if !commandFinished {
			_ = command.Process.Signal(syscall.SIGTERM)
			select {
			case <-waitDone:
				commandFinished = true
			case <-time.After(5 * time.Second):
				cancel()
				_ = command.Process.Kill()
				select {
				case <-waitDone:
					commandFinished = true
				case <-time.After(5 * time.Second):
				}
			}
		}
		cancel()
		_ = master.Close()
		if !drainFinished {
			select {
			case <-drainDone:
				drainFinished = true
			case <-time.After(time.Second):
			}
		}
	})

	if !waitForRawTerminal(master, 3*time.Second) {
		t.Fatalf("live binary did not enter raw terminal mode\nstderr:\n%s", stderr.String())
	}
	if _, err := master.Write([]byte("agent\r")); err != nil {
		t.Fatal(err)
	}

	archiveDeadline := time.NewTimer(45 * time.Second)
	defer archiveDeadline.Stop()
	archivePoll := time.NewTicker(20 * time.Millisecond)
	defer archivePoll.Stop()
	var archive experiment.ArchivedGeneration
	for archive.Outcome != "completed" {
		select {
		case err := <-waitDone:
			commandFinished = true
			t.Fatalf("live binary stopped before completion: %v\nstderr:\n%s\nterminal:\n%s", err, stderr.String(), rendered.String())
		case <-archivePoll.C:
			loaded, openErr := experiment.NewCodexOSRun(runDirectory)
			if openErr != nil {
				continue
			}
			item, inspectErr := loaded.InspectGeneration(0)
			if inspectErr == nil {
				archive = item
			}
		case <-archiveDeadline.C:
			t.Fatalf("live binary did not complete generation\nstderr:\n%s\nterminal:\n%s", stderr.String(), rendered.String())
		}
	}

	gateDeadline := time.NewTimer(5 * time.Second)
	defer gateDeadline.Stop()
	gatePoll := time.NewTicker(20 * time.Millisecond)
	defer gatePoll.Stop()
	for {
		output := rendered.Bytes()
		if bytes.Contains(output, []byte("Generation 0 complete.")) &&
			bytes.Contains(output, []byte("The disposable generation completed its validated build.")) &&
			bytes.Contains(output, []byte("A successor is selected.")) {
			break
		}
		select {
		case err := <-waitDone:
			commandFinished = true
			t.Fatalf("live binary stopped before rendering completed gate: %v\nstderr:\n%s\nterminal:\n%s", err, stderr.String(), rendered.String())
		case <-gatePoll.C:
		case <-gateDeadline.C:
			t.Fatalf("live binary did not render completed gate\nstderr:\n%s\nterminal:\n%s", stderr.String(), rendered.String())
		}
	}

	quitDeadline := time.NewTimer(15 * time.Second)
	defer quitDeadline.Stop()
	quitPoll := time.NewTicker(100 * time.Millisecond)
	defer quitPoll.Stop()
	for !commandFinished {
		if _, err := master.Write([]byte{3}); err != nil && !errors.Is(err, os.ErrClosed) && !errors.Is(err, syscall.EIO) {
			t.Fatal(err)
		}
		select {
		case err := <-waitDone:
			commandFinished = true
			if err != nil {
				t.Fatalf("live binary TUI shutdown: %v\nstderr:\n%s\nterminal:\n%s", err, stderr.String(), rendered.String())
			}
		case <-quitPoll.C:
		case <-quitDeadline.C:
			t.Fatal("live binary did not quit from completed gate")
		}
	}
	if ctx.Err() != nil {
		t.Fatalf("live binary exceeded acceptance deadline: %v", ctx.Err())
	}
	after, err := unix.IoctlGetTermios(int(master.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	if *after != *before {
		t.Fatalf("live binary did not restore terminal state:\nbefore=%#v\nafter=%#v", *before, *after)
	}
	if stderr.Len() != 0 {
		t.Fatalf("live binary wrote stderr: %s", stderr.String())
	}
	if archive.Transition != "initial" || archive.ParentGeneration != nil || archive.Handoff == nil ||
		*archive.Handoff != "The disposable generation completed its validated build." {
		t.Fatalf("live binary completed archive = %#v", archive)
	}
	successorISO, err := os.ReadFile(filepath.Join(archive.ArchivePath, "successor", "codexos.iso"))
	if err != nil || string(successorISO) != "synthetic-iso\nlimine-installed\n" {
		t.Fatalf("live binary successor ISO = %q, %v", successorISO, err)
	}
	assertDisposableBinaryProcessesStopped(t, processRecords, 3)
	assertDisposableBinaryLifecycleEvents(t, runDirectory)
	workspaces, err := filepath.Glob(filepath.Join(runDirectory, ".generation-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(workspaces) != 0 {
		t.Fatalf("live binary left generation workspaces: %v", workspaces)
	}
	_ = master.Close()
	select {
	case readErr := <-drainDone:
		drainFinished = true
		if !isExpectedPTYReadEnd(readErr) {
			t.Fatalf("live terminal output reader: %v", readErr)
		}
	case <-time.After(time.Second):
		t.Fatal("live terminal output reader did not stop")
	}
	if !bytes.Contains(rendered.Bytes(), []byte("CodexOS")) {
		t.Fatalf("live TUI did not render its application chrome: %q", rendered.String())
	}
}

type synchronizedBinaryBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *synchronizedBinaryBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *synchronizedBinaryBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func (b *synchronizedBinaryBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Len()
}

func (b *synchronizedBinaryBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buffer.Bytes()...)
}

func buildCodexOSBinary(t *testing.T) string {
	t.Helper()
	repositoryRoot := codexOSRepositoryRoot(t)
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

func codexOSRepositoryRoot(t *testing.T) string {
	t.Helper()
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return repositoryRoot
}

func writeDisposableBinaryLiveFixtures(t *testing.T, repositoryRoot string) string {
	t.Helper()
	bin := t.TempDir()
	for output, packagePath := range map[string]string{
		"qemu-system-x86_64": "./internal/operator/testdata/fakeqemu",
		"codex":              "./internal/operator/testdata/fakecodex",
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		command := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(bin, output), packagePath)
		command.Dir = repositoryRoot
		combined, err := command.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("build disposable %s: %v\n%s", output, err, combined)
		}
	}
	cc1 := writeDisposableBinaryExecutable(t, filepath.Join(bin, "cc1"), "#!/bin/sh\nexit 0\n")
	assembler := writeDisposableBinaryExecutable(t, filepath.Join(bin, "as"), "#!/bin/sh\nexit 0\n")
	writeDisposableBinaryExecutable(t, filepath.Join(bin, "x86_64-elf-gcc"), `#!/bin/sh
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
`)
	writeDisposableBinaryExecutable(t, filepath.Join(bin, "x86_64-elf-ld"), `#!/bin/sh
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
`)
	writeDisposableBinaryExecutable(t, filepath.Join(bin, "ldd"), "#!/bin/sh\nexit 0\n")
	writeDisposableBinaryExecutable(t, filepath.Join(bin, "xorriso"), `#!/bin/sh
output=
previous=
for argument in "$@"; do
  if [ "$previous" = "-o" ]; then output="$argument"; break; fi
  previous="$argument"
done
if [ -z "$output" ]; then exit 9; fi
printf 'synthetic-iso\n' > "$output"
`)
	writeDisposableBinaryExecutable(t, filepath.Join(bin, "cc"), `#!/bin/sh
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
`)
	writeDisposableBinaryExecutable(t, filepath.Join(bin, "bwrap"), `#!/bin/bash
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
`)
	return bin
}

func writeDisposableBinaryExecutable(t *testing.T, path, contents string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertDisposableBinaryProcessesStopped(t *testing.T, directory string, minimum int) {
	t.Helper()
	records, err := filepath.Glob(filepath.Join(directory, "*.pid"))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) < minimum {
		t.Fatalf("disposable binary process records = %v, want at least %d", records, minimum)
	}
	for _, record := range records {
		encoded, err := os.ReadFile(record)
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(encoded)))
		if err != nil {
			t.Fatalf("invalid disposable binary PID record %s: %v", record, err)
		}
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("disposable binary process %d from %s survived shutdown: %v", pid, record, err)
		}
	}
}

func assertDisposableBinaryLifecycleEvents(t *testing.T, runDirectory string) {
	t.Helper()
	events, err := os.ReadFile(filepath.Join(runDirectory, observability.EventLogFilename))
	if err != nil {
		t.Fatal(err)
	}
	previous := -1
	for _, event := range []string{
		"run_started", "generation_started", "codex_session_started", "planning_started", "planning_completed",
		"build_attempt_received", "build_candidate_validation_started", "build_candidate_qemu_started",
		"build_candidate_ready_observed", "build_protocol_validation_started", "build_protocol_validation_completed",
		"build_attempt_completed", "build_completed", "generation_completed",
		"codex_session_stopped", "run_stopped",
	} {
		index := bytes.Index(events, []byte(`"event":"`+event+`"`))
		if index < 0 || index <= previous {
			t.Fatalf("binary lifecycle event %q missing or out of order:\n%s", event, events)
		}
		previous = index
	}
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
