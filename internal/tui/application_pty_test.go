//go:build linux

package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/sys/unix"

	"codexos/internal/observability"
)

// This exercises the real Linux cancel reader under the race detector. Caller
// cancellation and direct shutdown must both take Bubble Tea's graceful path,
// which joins that reader before closing its descriptor.
func TestApplicationExternalShutdownRestoresPseudoTerminal(t *testing.T) {
	for _, test := range []struct {
		name          string
		cancelContext bool
	}{
		{name: "application shutdown"},
		{name: "caller context cancellation", cancelContext: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			terminal, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
			if err != nil {
				t.Skipf("pseudo-terminal unavailable: %v", err)
			}
			defer terminal.Close()
			before, err := unix.IoctlGetTermios(int(terminal.Fd()), unix.TCGETS)
			if err != nil {
				t.Skipf("pseudo-terminal attributes unavailable: %v", err)
			}
			app, err := NewApplication(ApplicationOptions{ActivityPoll: time.Millisecond, StatusPoll: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			defer app.Shutdown()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runDone := make(chan error, 1)
			go func() { runDone <- app.Run(ctx, terminal, terminal) }()
			rawObserved := false
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				current, getErr := unix.IoctlGetTermios(int(terminal.Fd()), unix.TCGETS)
				if getErr == nil && current.Lflag&unix.ICANON == 0 {
					rawObserved = true
					break
				}
				time.Sleep(time.Millisecond)
			}
			if !rawObserved {
				t.Fatal("Bubble Tea did not put the pseudo-terminal into raw mode")
			}
			if test.cancelContext {
				cancel()
			} else {
				app.Shutdown()
			}
			select {
			case runErr := <-runDone:
				if runErr != nil {
					t.Fatalf("Run after external shutdown: %v", runErr)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Bubble Tea did not return after external shutdown")
			}
			after, err := unix.IoctlGetTermios(int(terminal.Fd()), unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}
			if *after != *before {
				t.Fatalf("pseudo-terminal attributes were not restored:\nbefore=%#v\nafter=%#v", *before, *after)
			}
		})
	}
}

func TestApplicationRetainedLayoutThroughPseudoTerminal(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	master, slave := openApplicationPseudoTerminal(t, 52, 14)
	defer master.Close()
	defer slave.Close()
	before, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	stream := observability.NewActivityStream()
	app, err := NewApplication(ApplicationOptions{
		Activity: stream,
		Status: func() StatusSnapshot {
			return StatusSnapshot{RunName: "pty-run", Generation: 2, HasGeneration: true, RuntimeState: "running", ActiveAgent: "Sol", ActivePhase: "implementation"}
		},
		ActivityPoll: time.Hour,
		StatusPoll:   time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(context.Background(), slave, slave) }()
	var rendered bytes.Buffer
	copyDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(&rendered, master)
		copyDone <- copyErr
	}()
	if !waitForApplicationRawTerminal(slave, time.Second) {
		t.Fatal("Bubble Tea did not enter raw mode")
	}
	program := waitForApplicationProgram(t, app)
	program.Send(tea.WindowSizeMsg{Width: 52, Height: 14})
	generation := uint64(2)
	for index := 0; index < 150; index++ {
		if _, err := stream.Publish(&generation, observability.ActivityImplementor, observability.ActivityAgentMessage,
			map[string]any{"text": fmt.Sprintf("pty output %03d %s", index, strings.Repeat("界", 30)), "turn_phase": "implementation"}, "thread", "turn", fmt.Sprintf("item-%03d", index)); err != nil {
			t.Fatal(err)
		}
	}
	program.Send(activityPollMsg{})
	waitForApplication(t, time.Second, func() bool { return len(app.ActivityModel().Entries()) > 100 })
	if _, err := master.Write([]byte("\x1b[5~")); err != nil {
		t.Fatal(err)
	}
	waitForApplication(t, time.Second, func() bool { return !app.FollowState().Following })
	if _, err := master.Write([]byte("\x1b[F")); err != nil {
		t.Fatal(err)
	}
	waitForApplication(t, time.Second, func() bool { return app.FollowState().Following })
	if _, err := master.Write([]byte("\x1b[<64;3;4M")); err != nil {
		t.Fatal(err)
	}
	waitForApplication(t, time.Second, func() bool { return !app.FollowState().Following })
	if _, err := master.Write([]byte("\x1b[F")); err != nil {
		t.Fatal(err)
	}
	waitForApplication(t, time.Second, func() bool { return app.FollowState().Following })
	if _, err := master.Write([]byte("\x1b[200~first line\nsecond line\x1b[201~")); err != nil {
		t.Fatal(err)
	}
	waitForApplication(t, time.Second, func() bool { return app.Input() == "first line\nsecond line" })
	if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 12, Col: 44}); err != nil {
		t.Fatal(err)
	}
	program.Send(tea.WindowSizeMsg{Width: 44, Height: 12})
	waitForApplication(t, time.Second, func() bool {
		view := app.View()
		return len(strings.Split(view.Content, "\n")) == 12 && app.Input() == "first line\nsecond line" && view.Cursor != nil
	})
	if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 14, Col: 52}); err != nil {
		t.Fatal(err)
	}
	program.Send(tea.WindowSizeMsg{Width: 52, Height: 14})
	waitForApplication(t, time.Second, func() bool { return len(strings.Split(app.View().Content, "\n")) == 14 })
	view := app.View()
	assertFixedFrame(t, view.Content, 52, 14)
	if view.Cursor == nil || !strings.Contains(ansi.Strip(strings.Split(view.Content, "\n")[0]), "Sol implementation") {
		t.Fatalf("retained PTY frame has no cursor or live phase:\n%s", ansi.Strip(view.Content))
	}
	app.viewMu.Lock()
	app.composer.Blur()
	app.viewMu.Unlock()
	if _, err := master.Write([]byte("\x1b[<0;52;14M")); err != nil {
		t.Fatal(err)
	}
	waitForApplication(t, time.Second, func() bool {
		app.viewMu.RLock()
		defer app.viewMu.RUnlock()
		return app.composer.Focused()
	})

	confirmationDone := make(chan bool, 1)
	go func() { confirmationDone <- app.RequestConfirmation("Proceed? [y/N] ") }()
	waitForApplication(t, time.Second, func() bool { return app.ConfirmationPrompt() != "" })
	if got := strings.Count(ansi.Strip(app.View().Content), "[y/N]"); got != 1 {
		t.Fatalf("PTY confirmation indicators = %d", got)
	}
	if _, err := master.Write([]byte("n")); err != nil {
		t.Fatal(err)
	}
	select {
	case accepted := <-confirmationDone:
		if accepted {
			t.Fatal("PTY confirmation accepted n")
		}
	case <-time.After(time.Second):
		t.Fatal("PTY confirmation did not complete")
	}
	if app.Input() != "first line\nsecond line" {
		t.Fatalf("confirmation changed composer input to %q", app.Input())
	}

	app.Shutdown()
	select {
	case runErr := <-runDone:
		if runErr != nil {
			t.Fatal(runErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PTY application did not stop")
	}
	after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	if *after != *before {
		t.Fatalf("PTY attributes were not restored:\nbefore=%#v\nafter=%#v", *before, *after)
	}
	_ = slave.Close()
	_ = master.Close()
	select {
	case copyErr := <-copyDone:
		if copyErr != nil && !errors.Is(copyErr, syscall.EIO) && !errors.Is(copyErr, os.ErrClosed) {
			t.Fatal(copyErr)
		}
	case <-time.After(time.Second):
		t.Fatal("PTY output reader did not stop")
	}
	if !bytes.Contains(rendered.Bytes(), []byte("\x1b")) {
		t.Fatal("PTY renderer emitted no terminal control sequences")
	}
}

func openApplicationPseudoTerminal(t *testing.T, width, height uint16) (*os.File, *os.File) {
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
	if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: height, Col: width}); err != nil {
		slave.Close()
		master.Close()
		t.Fatal(err)
	}
	return master, slave
}

func waitForApplicationRawTerminal(terminal *os.File, timeout time.Duration) bool {
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

func waitForApplicationProgram(t *testing.T, app *Application) *tea.Program {
	t.Helper()
	var program *tea.Program
	waitForApplication(t, time.Second, func() bool {
		app.stateMu.Lock()
		defer app.stateMu.Unlock()
		program = app.program
		return app.started && program != nil
	})
	return program
}

func waitForApplication(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true before deadline")
}
