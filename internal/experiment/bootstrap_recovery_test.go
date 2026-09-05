package experiment

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"codexos/internal/bootstrap"
	"codexos/internal/build"
	"codexos/internal/guest"
)

// Only the isolated worker transport is synthetic. Runtime state, serial host
// service routing, admission and immutable publication are exercised normally.
func bootstrapRuntimeWorker(failure string) int {
	var n uint32
	if binary.Read(os.Stdin, binary.BigEndian, &n) != nil || n > 1<<20 {
		return 2
	}
	b := make([]byte, n)
	if _, e := io.ReadFull(os.Stdin, b); e != nil {
		return 2
	}
	var request struct{ Kind string }
	if json.Unmarshal(b, &request) != nil {
		return 2
	}
	result := bootstrap.Result{Status: 0, Reason: "completed", Cleaned: true}
	if _, e := os.Stat(failure); e == nil {
		result.Status = 2
		result.Reason = "cleanup_failed"
		result.Cleaned = false
	}
	b, _ = json.Marshal(map[string]any{"result": result, "outputs": []any{}})
	if binary.Write(os.Stdout, binary.BigEndian, uint32(len(b))) != nil {
		return 2
	}
	if _, e := os.Stdout.Write(b); e != nil {
		return 2
	}
	return 0
}
func liveBootstrapExchange(connection net.Conn) ([]byte, error) {
	snapshot, e := guest.NewSourceSnapshot([]guest.SnapshotFile{{Path: "seed/job.sh", Content: []byte("true")}})
	if e != nil {
		return nil, e
	}
	arguments := [][]byte{[]byte(`{"version":1,"argv":["true"],"outputs":[]}`), snapshot.Bytes()}
	name := "bootstrap_job"
	payload := make([]byte, 2+len(name)+2)
	binary.LittleEndian.PutUint16(payload, uint16(len(name)))
	copy(payload[2:], name)
	binary.LittleEndian.PutUint16(payload[2+len(name):], uint16(len(arguments)))
	for _, a := range arguments {
		b := make([]byte, 4+len(a))
		binary.LittleEndian.PutUint32(b, uint32(len(a)))
		copy(b[4:], a)
		payload = append(payload, b...)
	}
	encoded, e := guest.EncodeFrame(guest.Frame{MessageType: guest.HostServiceRequest, RequestID: 99, Payload: payload})
	if e != nil {
		return nil, e
	}
	if _, e = connection.Write(encoded); e != nil {
		return nil, e
	}
	f, e := guest.ReadFrame(connection)
	return f.Payload, e
}
func TestRuntimeBootstrapRecoveryRestoresOnlyRunningAdmission(t *testing.T) {
	run := startLiveTestRunMode(t, 2*time.Second, "bootstrap")
	failure := filepath.Join(t.TempDir(), "fail-recovery")
	if e := bootstrap.Provision(run.runDirectory, filepath.Join(t.TempDir(), "artifacts"), "tcc"); e != nil {
		t.Fatal(e)
	}
	svc, e := bootstrap.NewService(run.runDirectory, 0, nil, []bootstrap.Asset{{ID: "tcc", SHA256: bootstrap.TCCSHA256}}, nil, &bootstrap.Client{Command: []string{os.Args[0], "--bootstrap-runtime-worker", failure}})
	if e != nil {
		t.Fatal(e)
	}
	g := run.liveGeneration()
	g.bootstrap = svc
	services, e := build.NewCodexOSHostServices(build.HostServicesConfig{StagingDirectory: t.TempDir(), Bootstrap: svc})
	if e != nil {
		t.Fatal(e)
	}
	*g.hostServices = *services
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	job := func(want uint32) {
		t.Helper()
		r, e := run.InvokeTool(ctx, "bootstrap_job", nil)
		if e != nil || r.Status != want {
			t.Fatalf("job status=%d body=%s error=%v", r.Status, r.Output, e)
		}
	}
	job(0)
	if e = run.RecoverBootstrap(ctx); e != nil {
		t.Fatal(e)
	}
	job(0) // Regression: successful running recovery must reopen admission.
	if e = run.Pause(ctx); e != nil {
		t.Fatal(e)
	}
	if e = run.RecoverBootstrap(ctx); e != nil {
		t.Fatal(e)
	}
	if run.State() != RuntimeStatePaused {
		t.Fatal("recovery resumed the VM")
	}
	// Admission itself stays suspended, even if a queued invocation activates.
	svc.Activate(ctx)
	snapshot, _ := guest.NewSourceSnapshot([]guest.SnapshotFile{{Path: "seed/job.sh", Content: []byte("true")}})
	frame, e := svc.HandleRequest(ctx, guest.HostRequest{RequestID: 1, ServiceName: "bootstrap_job", Arguments: [][]byte{[]byte(`{"version":1,"argv":["true"]}`), snapshot.Bytes()}})
	if e != nil || binary.LittleEndian.Uint32(frame.Payload) != 2 {
		t.Fatalf("paused admission: %+v %v", frame, e)
	}
	if e = run.Resume(ctx); e != nil {
		t.Fatal(e)
	}
	job(0)
	if e = os.WriteFile(failure, []byte("fail"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = run.RecoverBootstrap(ctx); e == nil {
		t.Fatal("failed recovery succeeded")
	}
	job(2)
	if e = os.Remove(failure); e != nil {
		t.Fatal(e)
	}
	if e = run.RecoverBootstrap(ctx); e != nil {
		t.Fatal(e)
	}
	job(0)
}
