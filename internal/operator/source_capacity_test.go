package operator

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"codexos/internal/experiment"
	"codexos/internal/guest"
	"codexos/internal/qemu"
	"codexos/internal/sourcecapacity"
	"codexos/internal/store"
)

func TestSourceCapacityConsoleProvisionsSeparatelyFromFeatureApproval(t *testing.T) {
	directory := t.TempDir()
	hardware, err := qemu.TestHardwareProfile.Manifest("QEMU test")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := guest.EncodeSourceSnapshot([]guest.SnapshotFile{{Path: "seed/kernel.c", Content: []byte("source")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := experiment.WriteCompletedArchive(directory, experiment.CompletedArchive{Generation: 0, Transition: "initial", Hardware: hardware, SourceSnapshot: snapshot}); err != nil {
		t.Fatal(err)
	}
	runtime, err := experiment.NewLiveCodexOSRun(directory, experiment.LiveRunOptions{HardwareProfile: qemu.TestHardwareProfile})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Stop)
	if err := runtime.ReopenAtGate(); err != nil {
		t.Fatal(err)
	}
	ledger, err := store.NewFeatureRequestStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	request, err := ledger.Create(0, "Source capacity", "One MiB serialized source capacity")
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	console, err := NewPlainConsole(runtime, PlainConsoleOptions{Output: &output, ConfirmationHandler: func(string) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"source-capacity 1048576", "status", "inspect 0"} {
		if _, err := console.ExecuteLine(context.Background(), line); err != nil {
			t.Fatalf("%s: %v", line, err)
		}
	}
	feature, err := ledger.Request(request.ID)
	if err != nil || feature.Status != store.FeaturePending {
		t.Fatal("provisioning implicitly approved request")
	}
	if !strings.Contains(output.String(), "Source content capacity: 1048576 bytes") || !strings.Contains(output.String(), "Source content capacity: 65536 bytes") {
		t.Fatalf("current/archive capacities absent: %s", output.String())
	}
	if _, err := console.ExecuteLine(context.Background(), "feature-approve 1"); err != nil {
		t.Fatal(err)
	}
	feature, err = ledger.Request(request.ID)
	if err != nil || feature.Status != store.FeatureApproved {
		t.Fatal("existing feature mechanism did not record availability")
	}
	if runtime.SourceCapacity().Bytes() != sourcecapacity.Expanded || runtime.State() != experiment.RuntimeStateAwaitingNextGeneration {
		t.Fatal("approval changed capacity or started generation")
	}
	if pid, active := runtime.ActivePID(); active {
		t.Fatalf("provisioning started QEMU %d", pid)
	}
	if _, err := console.ExecuteLine(context.Background(), "source-capacity 9999999999999999999"); err == nil {
		t.Fatal("unsupported numeric budget accepted")
	}
}
