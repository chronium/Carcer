package bootstrap

import (
	"context"
	"encoding/binary"
	"os"
	"testing"
	"time"

	"codexos/internal/guest"
)

func TestJobScopeRejectsBackgroundReviewAndSuspendedAdmission(t *testing.T) {
	run, s, _ := fixtureStore(t)
	s.Close()
	svc, e := NewService(run, 0, nil, []Asset{{ID: "tcc", SHA256: TCCSHA256}}, nil)
	if e != nil {
		t.Fatal(e)
	}
	request := guest.HostRequest{RequestID: 9, ServiceName: "bootstrap_job", Arguments: [][]byte{mustJSON(fixtureRequest()), fixtureSnapshot(t)}}
	reject := func() {
		t.Helper()
		f, e := svc.HandleRequest(context.Background(), request)
		if e != nil || binary.LittleEndian.Uint32(f.Payload[:4]) != 2 {
			t.Fatalf("scope admitted job %+v %v", f, e)
		}
	}
	reject() // No development invocation: background or review capture.
	svc.Activate(context.Background())
	svc.Suspend()
	reject()
	svc.Activate(context.Background())
	reject() // Queued invocation cannot undo pause.
	svc.Resume()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc.Activate(ctx)
	reject()
	svc.Deactivate()
	reject()
}

func TestClientDoesNotInheritHarnessWorkingDirectory(t *testing.T) {
	// A private checkout must not become the dedicated worker's cwd. Remove it
	// after entering to also catch reliance on the caller's cwd remaining valid.
	caller := t.TempDir()
	executable, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	t.Chdir(caller)
	if e = os.Remove(caller); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := Client{Command: []string{executable, "--bootstrap-client-cwd-fixture"}}
	result, e := c.call(ctx, wireRequest{Kind: "probe"})
	if e != nil || result.Result.Status != 0 || result.Result.Diagnostics != "/" {
		t.Fatalf("worker inherited caller cwd: %+v %v", result.Result, e)
	}
}
