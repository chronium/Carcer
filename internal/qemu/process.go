package qemu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const qemuStopTimeout = 5 * time.Second

type QEMUProcessError struct {
	Reason string
	Err    error
}

func (e *QEMUProcessError) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *QEMUProcessError) Unwrap() error { return e.Err }

type QEMUStartOptions struct {
	Arguments        []string
	StdoutPath       string
	StderrPath       string
	QMPSocketPath    *string
	SerialSocketPath *string
}

// QEMUProcessController owns one direct QEMU child and the parent copies of its
// log files. The child is deliberately not placed in a new process group,
// matching the Python harness.
type QEMUProcessController struct {
	mutex      sync.Mutex
	stopMutex  sync.Mutex
	executable string
	command    *exec.Cmd
	done       chan struct{}
	stdout     *os.File
	stderr     *os.File
}

func NewQEMUProcessController(executable string) *QEMUProcessController {
	if executable == "" {
		executable = "qemu-system-x86_64"
	}
	return &QEMUProcessController{executable: executable}
}

func (c *QEMUProcessController) Start(options QEMUStartOptions) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.reapExitedLocked()
	if c.command != nil {
		return &QEMUProcessError{Reason: "QEMU is already running"}
	}

	stdout, err := os.OpenFile(options.StdoutPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o666)
	if err != nil {
		return &QEMUProcessError{Reason: "could not open QEMU stdout log", Err: err}
	}
	c.stdout = stdout
	stderr, err := os.OpenFile(options.StderrPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o666)
	if err != nil {
		c.closeLogsLocked()
		return &QEMUProcessError{Reason: "could not open QEMU stderr log", Err: err}
	}
	c.stderr = stderr

	arguments := append([]string(nil), options.Arguments...)
	if options.QMPSocketPath != nil {
		arguments = append(arguments, "-qmp", "unix:"+*options.QMPSocketPath+",server=on,wait=off")
	}
	if options.SerialSocketPath != nil {
		arguments = append(arguments, "-serial", "unix:"+*options.SerialSocketPath+",server=on,wait=off")
	}
	command := exec.Command(c.executable, arguments...)
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		c.closeLogsLocked()
		return &QEMUProcessError{Reason: "could not start QEMU", Err: err}
	}
	done := make(chan struct{})
	c.command = command
	c.done = done
	go c.reap(command, done)
	return nil
}

func (c *QEMUProcessController) IsRunning() bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.reapExitedLocked()
	return c.command != nil
}

func (c *QEMUProcessController) PID() (int, bool) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.reapExitedLocked()
	if c.command == nil || c.command.Process == nil {
		return 0, false
	}
	return c.command.Process.Pid, true
}

func (c *QEMUProcessController) Stop(ctx context.Context, timeout time.Duration) error {
	if ctx == nil {
		return &QEMUProcessError{Reason: "QEMU stop context is nil"}
	}
	c.stopMutex.Lock()
	defer c.stopMutex.Unlock()
	c.mutex.Lock()
	c.reapExitedLocked()
	command, done := c.command, c.done
	if command == nil {
		c.closeLogsLocked()
		c.mutex.Unlock()
		return nil
	}
	if timeout < 0 {
		c.mutex.Unlock()
		return &QEMUProcessError{Reason: "QEMU stop timeout must not be negative"}
	}
	c.mutex.Unlock()
	defer c.closeLogsFor(command)

	if err := command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return &QEMUProcessError{Reason: "could not terminate QEMU", Err: err}
	}
	exited, waitErr := waitForQEMU(ctx, done, timeout)
	if exited {
		return nil
	}
	if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return &QEMUProcessError{Reason: "could not kill QEMU", Err: err}
	}
	killed, _ := waitForQEMU(context.Background(), done, timeout)
	if !killed {
		return &QEMUProcessError{Reason: fmt.Sprintf("QEMU did not exit within %s after kill", timeout)}
	}
	if waitErr != nil {
		return waitErr
	}
	return nil
}

func (c *QEMUProcessController) Close() error {
	return c.Stop(context.Background(), qemuStopTimeout)
}

func (c *QEMUProcessController) reap(command *exec.Cmd, done chan struct{}) {
	_ = command.Wait()
	close(done)
	c.mutex.Lock()
	if c.command == command {
		c.command = nil
		c.done = nil
		c.closeLogsLocked()
	}
	c.mutex.Unlock()
}

func (c *QEMUProcessController) reapExitedLocked() {
	if c.command == nil || c.done == nil {
		return
	}
	select {
	case <-c.done:
		c.command = nil
		c.done = nil
		c.closeLogsLocked()
	default:
	}
}

func (c *QEMUProcessController) closeLogsFor(command *exec.Cmd) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.command == command {
		c.closeLogsLocked()
	}
}

func (c *QEMUProcessController) closeLogsLocked() {
	if c.stdout != nil {
		_ = c.stdout.Close()
		c.stdout = nil
	}
	if c.stderr != nil {
		_ = c.stderr.Close()
		c.stderr = nil
	}
}

func waitForQEMU(ctx context.Context, done <-chan struct{}, timeout time.Duration) (bool, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timer.C:
		return false, nil
	}
}
