package experiment

import (
	"bytes"
	"codexos/internal/build"
	"codexos/internal/observability"
	"codexos/internal/qemu"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codexos/internal/guest"
	"codexos/internal/sourcecapacity"
)

func capacityArchive(t *testing.T, run string, number uint64, budget sourcecapacity.Budget, size int) ArchivedGeneration {
	t.Helper()
	snapshot, err := guest.EncodeSourceSnapshotWithBudget([]guest.SnapshotFile{{Path: "seed/kernel.c", Content: bytes.Repeat([]byte("x"), size)}}, budget)
	if err != nil {
		t.Fatal(err)
	}
	input := CompletedArchive{Generation: number, Transition: "initial", Hardware: testHardware(t), BootISO: []byte("boot"), SourceSnapshot: snapshot, Handoff: "handoff", KernelELF: []byte("kernel"), SuccessorISO: []byte("iso"), SourceCapacity: budget}
	if number > 0 {
		parent := number - 1
		input.ParentGeneration = &parent
		input.Transition = "successor"
	}
	archive, err := WriteCompletedArchive(run, input)
	if err != nil {
		t.Fatal(err)
	}
	return archive
}

func capacityGate(t *testing.T, directory string) *CodexOSRun {
	t.Helper()
	run, err := NewCodexOSRun(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.ReopenAtGate(); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestSourceCapacityGatePersistenceAndHistoricalArchives(t *testing.T) {
	directory := t.TempDir()
	legacy := capacityArchive(t, directory, 0, 0, sourcecapacity.Default)
	before := archiveBytes(t, legacy.ArchivePath)
	run, err := NewCodexOSRun(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.SetSourceCapacity(sourcecapacity.Expanded); err == nil {
		t.Fatal("provisioned before validating gate")
	}
	if err := run.ReopenAtGate(); err != nil {
		t.Fatal(err)
	}
	if run.SourceCapacity().Bytes() != sourcecapacity.Default {
		t.Fatal("legacy default changed")
	}
	if !run.RetainGenerationFinish(0) {
		t.Fatal("cannot retain gate")
	}
	if err := run.SetSourceCapacity(sourcecapacity.Expanded); err == nil {
		t.Fatal("provisioned during interview")
	}
	run.ReleaseGenerationFinish(0)
	if err := run.SetSourceCapacity(sourcecapacity.Expanded); err != nil {
		t.Fatal(err)
	}
	if !sameArchiveBytes(before, archiveBytes(t, legacy.ArchivePath)) {
		t.Fatal("provisioning changed legacy archive")
	}
	run = capacityGate(t, directory)
	if run.PresentationSnapshot().SourceCapacity.Bytes() != sourcecapacity.Expanded {
		t.Fatal("reopen lost budget")
	}
	if err := run.ContinueGeneration(); err != nil {
		t.Fatal(err)
	}
	if err := run.SetSourceCapacity(sourcecapacity.Default); err == nil {
		t.Fatal("changed active generation budget")
	}
	if run.SourceCapacity().Bytes() != sourcecapacity.Expanded {
		t.Fatal("continue lost budget")
	}
	if err := run.AbortGeneration("test stop"); err != nil {
		t.Fatal(err)
	}
	aborted, err := run.InspectGeneration(1)
	if err != nil || aborted.SourceCapacity.Bytes() != sourcecapacity.Expanded {
		t.Fatalf("abort provenance: %v %v", aborted.SourceCapacity, err)
	}
	run = capacityGate(t, directory)
	if err := run.ForkFromGeneration(0); err != nil {
		t.Fatal(err)
	}
	if run.SourceCapacity().Bytes() != sourcecapacity.Expanded {
		t.Fatal("rollback lost budget")
	}
	fresh, err := NewCodexOSRun(t.TempDir())
	if err != nil || fresh.SourceCapacity().Bytes() != sourcecapacity.Default {
		t.Fatal("another experiment inherited provisioning")
	}
}

func TestSourceCapacityLargeArchiveReopenAndDownsizeRejection(t *testing.T) {
	directory := t.TempDir()
	capacityArchive(t, directory, 0, 0, 1)
	run := capacityGate(t, directory)
	if err := run.SetSourceCapacity(sourcecapacity.Expanded); err != nil {
		t.Fatal(err)
	}
	large := capacityArchive(t, directory, 1, sourcecapacity.Expanded, sourcecapacity.Expanded)
	info, err := os.Stat(filepath.Join(large.ArchivePath, sourceSnapshotName))
	if err != nil || info.Size() <= sourcecapacity.Expanded {
		t.Fatal("fixture did not exceed old archive read ceiling")
	}
	before := archiveBytes(t, large.ArchivePath)
	run = capacityGate(t, directory)
	pending, ok := run.PendingGenerationFinish()
	if !ok || len(pending.SourceSnapshot) != int(info.Size()) {
		t.Fatal("large snapshot not restored")
	}
	if err := run.SetSourceCapacity(sourcecapacity.Default); err == nil || !strings.Contains(err.Error(), "65536") {
		t.Fatalf("downsize = %v", err)
	}
	persisted, err := sourcecapacity.Load(directory)
	if err != nil || persisted.Bytes() != sourcecapacity.Expanded {
		t.Fatal("failed downsize changed persisted budget")
	}
	if !sameArchiveBytes(before, archiveBytes(t, large.ArchivePath)) {
		t.Fatal("reopen/downsize changed archive")
	}
	if err := run.ContinueGeneration(); err != nil {
		t.Fatal(err)
	}
	if err := run.AbortGeneration("test stop"); err != nil {
		t.Fatal(err)
	}
	run = capacityGate(t, directory)
	if err := run.SetSourceCapacity(sourcecapacity.Default); err != nil {
		t.Fatal(err)
	}
	if err := run.ForkFromGeneration(1); err == nil || !strings.Contains(err.Error(), "65536") {
		t.Fatalf("oversized rollback = %v", err)
	}
	if run.State() != RuntimeStateAwaitingNextGeneration {
		t.Fatal("failed rollback changed state")
	}
	if _, err := run.InspectGeneration(1); err != nil {
		t.Fatalf("historical expanded archive invalid under smaller current budget: %v", err)
	}
	if err := run.ForkFromGeneration(0); err != nil {
		t.Fatal(err)
	}
}

func TestSourceCapacityAbortedLargeEvidenceAndNoPartialArchive(t *testing.T) {
	directory := t.TempDir()
	snapshot, err := guest.EncodeSourceSnapshotWithBudget([]guest.SnapshotFile{{Path: "seed/kernel.c", Content: make([]byte, sourcecapacity.Expanded)}}, sourcecapacity.Expanded)
	if err != nil {
		t.Fatal(err)
	}
	identity := map[string]any{"sha256": sha256Hex(snapshot), "size": len(snapshot)}
	manifest, err := json.Marshal(map[string]any{"schema_version": 1, "generation": 0, "ready": true, "protocol_validated": true, "build_attempt_id": "build-1", "source_snapshot": identity, "kernel": identity, "iso": identity})
	if err != nil {
		t.Fatal(err)
	}
	input := AbortedArchive{Generation: 0, Transition: "initial", Hardware: testHardware(t), BootISO: []byte("boot"), AbortReason: "test", LatestSuccess: &AbortedSuccessEvidence{Manifest: manifest, Snapshot: snapshot}}
	if _, err := WriteAbortedArchive(directory, input); err == nil {
		t.Fatal("default accepted large forensic snapshot")
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatal("rejected archive published partial state")
	}
	input.SourceCapacity = sourcecapacity.Expanded
	archived, err := WriteAbortedArchive(directory, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InspectGeneration(directory, 0); err != nil {
		t.Fatal(err)
	}
	if archived.SourceCapacity.Bytes() != sourcecapacity.Expanded {
		t.Fatal("lost forensic budget")
	}
}

func TestSourceCapacityLiveCaptureAndBuildUseEffectiveRunBudget(t *testing.T) {
	t.Setenv(liveQEMUHelperEnvironment, "capacity")
	for _, expanded := range []bool{false, true} {
		directory := liveTestRunDirectory(t)
		eventLog, err := observability.OpenEventLog(directory)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(eventLog.Close)
		run, err := NewLiveCodexOSRun(directory, LiveRunOptions{QEMUExecutable: os.Args[0], HardwareProfile: qemu.TestHardwareProfile, ReadyTimeout: 2 * time.Second, EventLog: eventLog,
			// A caller's static build configuration must never expand a fresh run.
			BuildConfig: build.Config{SourceCapacity: sourcecapacity.Expanded}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(run.Stop)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		t.Cleanup(cancel)
		if expanded {
			capacityArchive(t, directory, 0, 0, 1)
			if err := run.ReopenAtGate(); err != nil {
				t.Fatal(err)
			}
			if err := run.SetSourceCapacity(sourcecapacity.Expanded); err != nil {
				t.Fatal(err)
			}
			if err := run.ContinueGeneration(); err != nil {
				t.Fatal(err)
			}
		} else {
			iso := filepath.Join(t.TempDir(), "initial.iso")
			if err := os.WriteFile(iso, []byte("synthetic boot"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := run.Start(ctx, iso); err != nil {
				t.Fatal(err)
			}
		}
		captured, err := run.CaptureReviewSource(ctx)
		if expanded && err != nil {
			t.Fatal(err)
		}
		if !expanded && (err == nil || !strings.Contains(err.Error(), "64 KiB")) {
			t.Fatalf("fresh capture: %v", err)
		}
		if expanded && len(captured) <= sourcecapacity.Expanded {
			t.Fatal("capture lost framing or content")
		}
		snapshot, err := guest.EncodeSourceSnapshotWithBudget([]guest.SnapshotFile{{Path: "seed/kernel.c", Content: make([]byte, sourcecapacity.Expanded)}}, sourcecapacity.Expanded)
		if err != nil {
			t.Fatal(err)
		}
		frame, err := run.liveGeneration().hostServices.HandleRequest(ctx, guest.HostRequest{RequestID: 1, ServiceName: "build", Arguments: [][]byte{snapshot}})
		if err != nil {
			t.Fatal(err)
		}
		// This deliberately incomplete source cannot build. Its diagnostic proves
		// the installed generation service applied the run budget before fixed
		// build prerequisites, without invoking a host compiler.
		diagnostic := string(frame.Payload[4:])
		if expanded && strings.Contains(diagnostic, "content exceeds") {
			t.Fatalf("expanded live build rejected source: %s", diagnostic)
		}
		if !expanded && !strings.Contains(diagnostic, "64 KiB") {
			t.Fatalf("fresh build used static config budget: %s", diagnostic)
		}
		if err := run.Pause(ctx); err != nil {
			t.Fatal(err)
		}
		if err := run.SetSourceCapacity(sourcecapacity.Default); err == nil {
			t.Fatal("provisioned paused live generation")
		}
		if err := run.Resume(ctx); err != nil {
			t.Fatal(err)
		}
		if err := run.AbortGeneration("capacity test stop"); err != nil {
			t.Fatal(err)
		}
		number, _ := run.GenerationNumber()
		archive, err := run.InspectGeneration(number)
		if err != nil || archive.SourceCapacity.Bytes() != run.SourceCapacity().Bytes() {
			t.Fatalf("live archive capacity: %v", err)
		}
		contents, err := os.ReadFile(eventLog.Path())
		if err != nil {
			t.Fatal(err)
		}
		expected := []byte(`"source_content_bytes":65536`)
		if expanded {
			expected = []byte(`"source_content_bytes":1048576`)
		}
		if !bytes.Contains(contents, expected) {
			t.Fatal("generation provenance omitted effective source budget")
		}
		run.Stop()
	}
}
