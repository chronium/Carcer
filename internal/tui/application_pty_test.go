//go:build linux

package tui

import (
	"context"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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
