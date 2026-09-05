package bootstrap

import (
	"context"
	"encoding/binary"
	"testing"

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
