//go:build linux && !race

package tui

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/sys/unix"
)

// Bubble Tea's Linux cancelreader has an upstream race in its bounded
// read-loop fallback, so this real terminal restoration proof runs outside the
// race build. The ordinary application tests retain race coverage for our
// shutdown state and handler cancellation.
func TestApplicationExternalShutdownRestoresPseudoTerminal(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
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
	app.Shutdown()
	select {
	case runErr := <-runDone:
		if runErr != nil && !errors.Is(runErr, tea.ErrProgramKilled) {
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
}
