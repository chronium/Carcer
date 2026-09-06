package experiment

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"codexos/internal/guest"
	"codexos/internal/provenance"
	"codexos/internal/qemu"
)

func TestGenerationArchiveCarriesHarnessIdentityWithoutChangingLegacyMetadata(t *testing.T) {
	run := t.TempDir()
	identity := experimentHarnessIdentity()
	archive, err := WriteCompletedArchive(run, CompletedArchive{
		Generation: 0, Transition: "initial", Hardware: testHardware(t), BootISO: []byte("boot"),
		Handoff: "handoff", SourceSnapshot: testSnapshot(t, "source\n"), KernelELF: []byte("kernel"),
		SuccessorISO: []byte("iso"), HarnessIdentity: &identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	if archive.HarnessIdentity == nil || !archive.HarnessIdentity.Equal(identity) {
		t.Fatalf("archive harness identity = %#v", archive.HarnessIdentity)
	}
	metadata, err := os.ReadFile(filepath.Join(archive.ArchivePath, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(metadata, []byte("harness")) {
		t.Fatalf("legacy archive metadata was changed: %s", metadata)
	}
}

func TestCompletedArchiveGateAndContinue(t *testing.T) {
	run := t.TempDir()
	hardware := testHardware(t)
	snapshot := testSnapshot(t, "generation zero\n")
	archive, err := WriteCompletedArchive(run, CompletedArchive{
		Generation:     0,
		Transition:     "initial",
		Hardware:       hardware,
		BootISO:        []byte("boot-0"),
		Handoff:        "handoff zero λ",
		SourceSnapshot: snapshot,
		KernelELF:      []byte("kernel-0"),
		SuccessorISO:   []byte("successor-0"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if archive.HarnessIdentity != nil {
		t.Fatalf("legacy archive acquired a fabricated harness identity: %#v", archive.HarnessIdentity)
	}
	before := archiveBytes(t, archive.ArchivePath)
	runState, err := NewCodexOSRun(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := runState.ReopenAtGate(); err != nil {
		t.Fatal(err)
	}
	if runState.State() != RuntimeStateAwaitingNextGeneration {
		t.Fatalf("state = %q", runState.State())
	}
	if generation, ok := runState.GenerationNumber(); !ok || generation != 0 {
		t.Fatalf("generation = %d, %t", generation, ok)
	}
	pending, ok := runState.PendingGenerationFinish()
	if !ok || pending.HandoffMessage != "handoff zero λ" || !bytes.Equal(pending.SourceSnapshot, snapshot) {
		t.Fatalf("pending = %#v, %t", pending, ok)
	}
	if !runState.GenerationFinishFrozen() {
		t.Fatal("completed gate did not expose its frozen finish invariant")
	}
	if !runState.RetainGenerationFinish(0) || !runState.GenerationFinishRetained(0) {
		t.Fatal("completed gate could not be retained atomically")
	}
	if runState.RetainGenerationFinish(0) {
		t.Fatal("completed gate accepted a second retention lease")
	}
	if err := runState.ContinueGeneration(); err == nil || !strings.Contains(err.Error(), "retained for an exit interview") {
		t.Fatalf("continue under retained gate error = %v", err)
	}
	runState.ReleaseGenerationFinish(0)
	if runState.GenerationFinishRetained(0) {
		t.Fatal("released gate remains retained")
	}
	if err := runState.ContinueGeneration(); err != nil {
		t.Fatal(err)
	}
	if runState.State() != RuntimeStateRunning {
		t.Fatalf("continued state = %q", runState.State())
	}
	if generation, ok := runState.GenerationNumber(); !ok || generation != 1 {
		t.Fatalf("continued generation = %d, %t", generation, ok)
	}
	if got := archiveBytes(t, archive.ArchivePath); !sameArchiveBytes(got, before) {
		t.Fatal("continue changed immutable archive")
	}
	if _, ok := runState.PendingGenerationFinish(); ok {
		t.Fatal("continue retained selected successor")
	}
	if runState.GenerationFinishFrozen() {
		t.Fatal("running generation still reported a frozen finish")
	}
}

func TestAbortedGateHasNoSuccessor(t *testing.T) {
	run := t.TempDir()
	archive, err := WriteAbortedArchive(run, AbortedArchive{
		Generation:  0,
		Transition:  "initial",
		Hardware:    testHardware(t),
		BootISO:     []byte("boot"),
		AbortReason: "operator stopped the generation",
	})
	if err != nil {
		t.Fatal(err)
	}
	if archive.Outcome != "aborted" {
		t.Fatalf("outcome = %q", archive.Outcome)
	}
	runState, err := NewCodexOSRun(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := runState.ReopenAtGate(); err != nil {
		t.Fatal(err)
	}
	if runState.State() != RuntimeStateAwaitingNextGeneration {
		t.Fatalf("state = %q", runState.State())
	}
	if _, ok := runState.PendingGenerationFinish(); ok {
		t.Fatal("aborted gate selected a successor")
	}
	if runState.GenerationFinishFrozen() {
		t.Fatal("aborted gate reported a frozen completed finish")
	}
	if err := runState.ContinueGeneration(); err == nil || !strings.Contains(err.Error(), "no selected successor") {
		t.Fatalf("continue error = %v", err)
	}
}

func TestLegacyAbortedArchiveDoesNotFabricateReason(t *testing.T) {
	run := t.TempDir()
	archive, err := WriteAbortedArchive(run, AbortedArchive{
		Generation: 0, Transition: "initial", Hardware: testHardware(t),
		BootISO: []byte("boot"), AbortReason: "temporary fixture reason",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(archive.ArchivePath, abortReasonName)); err != nil {
		t.Fatal(err)
	}
	legacy, err := InspectGeneration(run, 0)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.AbortReason != nil {
		t.Fatalf("legacy archive acquired abort reason: %q", *legacy.AbortReason)
	}
}

func TestAbortRejectsInvalidReasonWithoutChangingRunningGeneration(t *testing.T) {
	run := t.TempDir()
	hardware := testHardware(t)
	if _, err := WriteCompletedArchive(run, CompletedArchive{
		Generation: 0, Transition: "initial", Hardware: hardware, BootISO: []byte("boot"),
		Handoff: "handoff", SourceSnapshot: testSnapshot(t, "source\n"), KernelELF: []byte("kernel"), SuccessorISO: []byte("iso"),
	}); err != nil {
		t.Fatal(err)
	}
	runtime := reopenProcessFreeRun(t, run)
	if err := runtime.ContinueGeneration(); err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{"", " \t", strings.Repeat("x", MaxAbortReasonBytes+1), string([]byte{0xff})} {
		if err := runtime.AbortGeneration(reason); err == nil {
			t.Fatalf("invalid reason was accepted: %q", reason)
		}
		if runtime.State() != RuntimeStateRunning {
			t.Fatalf("invalid reason changed runtime state to %s", runtime.State())
		}
		if _, err := InspectGeneration(run, 1); err == nil {
			t.Fatal("invalid reason published an aborted archive")
		}
	}
}

func TestAbortRunningProcessFreeGenerationPublishesArchive(t *testing.T) {
	run := t.TempDir()
	hardware := testHardware(t)
	snapshot := testSnapshot(t, "source\n")
	if _, err := WriteCompletedArchive(run, CompletedArchive{
		Generation: 0, Transition: "initial", Hardware: hardware, BootISO: []byte("boot"),
		Handoff: "handoff", SourceSnapshot: snapshot, KernelELF: []byte("kernel"), SuccessorISO: []byte("iso"),
	}); err != nil {
		t.Fatal(err)
	}
	runState, err := NewCodexOSRun(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := runState.ReopenAtGate(); err != nil {
		t.Fatal(err)
	}
	if err := runState.ContinueGeneration(); err != nil {
		t.Fatal(err)
	}
	reason := "operator observed λ\nretry scheduler"
	if err := runState.AbortGeneration(reason); err != nil {
		t.Fatal(err)
	}
	if runState.State() != RuntimeStateAwaitingNextGeneration {
		t.Fatalf("state = %q", runState.State())
	}
	aborted, err := InspectGeneration(run, 1)
	if err != nil {
		t.Fatal(err)
	}
	if aborted.Outcome != "aborted" || aborted.Handoff != nil || aborted.AbortReason == nil || *aborted.AbortReason != reason {
		t.Fatalf("aborted archive = %#v", aborted)
	}
	persisted, err := os.ReadFile(filepath.Join(aborted.ArchivePath, abortReasonName))
	if err != nil || string(persisted) != reason {
		t.Fatalf("persisted abort reason = %q, %v", persisted, err)
	}
}

func TestAbortFeedbackAttachesOnceAcrossGateReopenAndRollback(t *testing.T) {
	run := t.TempDir()
	hardware := testHardware(t)
	snapshot := testSnapshot(t, "source\n")
	if _, err := WriteCompletedArchive(run, CompletedArchive{
		Generation: 0, Transition: "initial", Hardware: hardware, BootISO: []byte("boot-0"),
		Handoff: "completed handoff", SourceSnapshot: snapshot, KernelELF: []byte("kernel-0"), SuccessorISO: []byte("iso-0"),
	}); err != nil {
		t.Fatal(err)
	}

	first := reopenProcessFreeRun(t, run)
	if err := first.ContinueGeneration(); err != nil {
		t.Fatal(err)
	}
	firstReason := "first abort reason λ\nkeep this verbatim"
	if err := first.AbortGeneration(firstReason); err != nil {
		t.Fatal(err)
	}
	first.Stop()

	// Reopening the same gate does not consume feedback. It is attached only
	// when generation 2 actually starts from a completed rollback parent.
	reopened := reopenProcessFreeRun(t, run)
	reopened.Stop()
	reopened = reopenProcessFreeRun(t, run)
	if err := reopened.ForkFromGeneration(0); err != nil {
		t.Fatal(err)
	}
	if source, reason, ok := reopened.OperatorFeedback(); !ok || source != 1 || reason != firstReason {
		t.Fatalf("generation 2 feedback = (%d, %q, %t)", source, reason, ok)
	}
	if handoff, ok := reopened.PreviousHandoff(); !ok || handoff != "completed handoff" {
		t.Fatalf("rollback handoff = %q, %t", handoff, ok)
	}
	records, err := loadOperatorFeedbackRecords(run)
	if err != nil || len(records) != 1 || records[0].TargetGeneration != 2 {
		t.Fatalf("feedback records = %#v, %v", records, err)
	}
	reopened.Stop()
	reopened = reopenProcessFreeRun(t, run)
	if err := reopened.ForkFromGeneration(0); err != nil {
		t.Fatal(err)
	}
	if source, reason, ok := reopened.OperatorFeedback(); !ok || source != 1 || reason != firstReason {
		t.Fatalf("restarted generation 2 feedback = (%d, %q, %t)", source, reason, ok)
	}

	secondReason := "newer abort supersedes pending feedback"
	if err := reopened.AbortGeneration(secondReason); err != nil {
		t.Fatal(err)
	}
	reopened.Stop()

	third := reopenProcessFreeRun(t, run)
	if err := third.ForkFromGeneration(0); err != nil {
		t.Fatal(err)
	}
	if source, reason, ok := third.OperatorFeedback(); !ok || source != 2 || reason != secondReason {
		t.Fatalf("generation 3 feedback = (%d, %q, %t)", source, reason, ok)
	}
	third.Stop()

	parent := uint64(0)
	if _, err := WriteCompletedArchive(run, CompletedArchive{
		Generation: 3, ParentGeneration: &parent, Transition: "rollback", Hardware: hardware, BootISO: []byte("boot-3"),
		Handoff: "generation three handoff", SourceSnapshot: snapshot, KernelELF: []byte("kernel-3"), SuccessorISO: []byte("iso-3"),
	}); err != nil {
		t.Fatal(err)
	}
	fourth := reopenProcessFreeRun(t, run)
	if err := fourth.ContinueGeneration(); err != nil {
		t.Fatal(err)
	}
	if source, reason, ok := fourth.OperatorFeedback(); ok {
		t.Fatalf("generation 4 inherited consumed feedback = (%d, %q)", source, reason)
	}
	archives, err := LoadArchivedGenerations(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateArchivedHistory(archives); err != nil {
		t.Fatal(err)
	}
	for _, generation := range []uint64{2, 3} {
		archive := archives[generation]
		if archive.ParentGeneration == nil || *archive.ParentGeneration != 0 {
			t.Fatalf("generation %d parent = %#v; aborted generation entered lineage", generation, archive.ParentGeneration)
		}
	}
}

func TestOperatorFeedbackAttachmentCannotBeReplaced(t *testing.T) {
	run := t.TempDir()
	original := OperatorFeedback{
		TargetGeneration: 2, SourceAbortGeneration: 1,
		Reason: "immutable operator feedback", SchemaVersion: 1,
	}
	if err := persistOperatorFeedback(run, original); err != nil {
		t.Fatal(err)
	}
	replacement := original
	replacement.Reason = "replacement feedback"
	if err := persistOperatorFeedback(run, replacement); err == nil || !strings.Contains(err.Error(), "conflicts with immutable state") {
		t.Fatalf("replacement error = %v", err)
	}
	encoded, err := os.ReadFile(filepath.Join(run, operatorFeedbackDirName, operatorFeedbackFilename(2)))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeOperatorFeedback(encoded)
	if err != nil || decoded.Reason != original.Reason {
		t.Fatalf("persisted feedback = %#v, %v", decoded, err)
	}
}

func TestOperatorFeedbackAttachmentRecoversAcrossInterruptedPublication(t *testing.T) {
	for _, test := range []struct {
		name      string
		published bool
	}{
		{name: "before final link"},
		{name: "after final link", published: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			run, feedback := abortFeedbackGateFixture(t)
			canonical := filepath.Join(run, operatorFeedbackDirName)
			if err := os.Mkdir(canonical, 0o700); err != nil {
				t.Fatal(err)
			}
			encoded, err := encodeOperatorFeedback(feedback)
			if err != nil {
				t.Fatal(err)
			}
			staging, err := stageOperatorFeedback(run, encoded)
			if err != nil {
				t.Fatal(err)
			}
			if filepath.Dir(staging) != run {
				t.Fatalf("feedback staging path entered canonical directory: %s", staging)
			}
			final := filepath.Join(canonical, operatorFeedbackFilename(feedback.TargetGeneration))
			if test.published {
				if err := os.Link(staging, final); err != nil {
					t.Fatal(err)
				}
			}

			runtime := reopenProcessFreeRun(t, run)
			if err := runtime.ForkFromGeneration(0); err != nil {
				t.Fatal(err)
			}
			if source, reason, ok := runtime.OperatorFeedback(); !ok || source != feedback.SourceAbortGeneration || reason != feedback.Reason {
				t.Fatalf("recovered feedback = (%d, %q, %t)", source, reason, ok)
			}
			persisted, err := os.ReadFile(final)
			if err != nil || !bytes.Equal(persisted, encoded) {
				t.Fatalf("final attachment = %s, %v", persisted, err)
			}
			if _, err := os.Stat(staging); err != nil {
				t.Fatalf("simulated crash staging unexpectedly affected recovery: %v", err)
			}
		})
	}
}

func TestOperatorFeedbackCanonicalDirectoryRejectsUnexpectedEntry(t *testing.T) {
	run, _ := abortFeedbackGateFixture(t)
	directory := filepath.Join(run, operatorFeedbackDirName)
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, ".operator-feedback-interrupted"), []byte("not canonical"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime, err := NewCodexOSRun(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.ReopenAtGate(); err == nil || !strings.Contains(err.Error(), "unexpected entry") {
		t.Fatalf("unexpected canonical entry reopen error = %v", err)
	}
}

func abortFeedbackGateFixture(t *testing.T) (string, OperatorFeedback) {
	t.Helper()
	run := t.TempDir()
	hardware := testHardware(t)
	if _, err := WriteCompletedArchive(run, CompletedArchive{
		Generation: 0, Transition: "initial", Hardware: hardware, BootISO: []byte("boot-0"),
		Handoff: "handoff-0", SourceSnapshot: testSnapshot(t, "source\n"), KernelELF: []byte("kernel-0"), SuccessorISO: []byte("iso-0"),
	}); err != nil {
		t.Fatal(err)
	}
	parent := uint64(0)
	feedback := OperatorFeedback{
		TargetGeneration: 2, SourceAbortGeneration: 1,
		Reason: "restart-safe operator feedback", SchemaVersion: 1,
	}
	if _, err := WriteAbortedArchive(run, AbortedArchive{
		Generation: 1, ParentGeneration: &parent, Transition: "successor", Hardware: hardware,
		BootISO: []byte("boot-1"), AbortReason: feedback.Reason,
	}); err != nil {
		t.Fatal(err)
	}
	return run, feedback
}

func TestPendingAbortFeedbackAttachesThroughContinue(t *testing.T) {
	run := t.TempDir()
	hardware := testHardware(t)
	snapshot := testSnapshot(t, "source\n")
	if _, err := WriteCompletedArchive(run, CompletedArchive{
		Generation: 0, Transition: "initial", Hardware: hardware, BootISO: []byte("boot-0"),
		Handoff: "handoff-0", SourceSnapshot: snapshot, KernelELF: []byte("kernel-0"), SuccessorISO: []byte("iso-0"),
	}); err != nil {
		t.Fatal(err)
	}
	parent := uint64(0)
	reason := "pending reason survives until a continue start"
	if _, err := WriteAbortedArchive(run, AbortedArchive{
		Generation: 1, ParentGeneration: &parent, Transition: "successor", Hardware: hardware,
		BootISO: []byte("boot-1"), AbortReason: reason,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteCompletedArchive(run, CompletedArchive{
		Generation: 2, ParentGeneration: &parent, Transition: "rollback", Hardware: hardware, BootISO: []byte("boot-2"),
		Handoff: "handoff-2", SourceSnapshot: snapshot, KernelELF: []byte("kernel-2"), SuccessorISO: []byte("iso-2"),
	}); err != nil {
		t.Fatal(err)
	}
	runtime := reopenProcessFreeRun(t, run)
	if err := runtime.ContinueGeneration(); err != nil {
		t.Fatal(err)
	}
	if source, got, ok := runtime.OperatorFeedback(); !ok || source != 1 || got != reason {
		t.Fatalf("continued feedback = (%d, %q, %t)", source, got, ok)
	}
	if handoff, ok := runtime.PreviousHandoff(); !ok || handoff != "handoff-2" {
		t.Fatalf("continued handoff = %q, %t", handoff, ok)
	}
}

func reopenProcessFreeRun(t *testing.T, directory string) *CodexOSRun {
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

func TestRollbackSelectionRequiresEarlierCompletedArchive(t *testing.T) {
	run := t.TempDir()
	hardware := testHardware(t)
	snapshot := testSnapshot(t, "zero\n")
	if _, err := WriteCompletedArchive(run, CompletedArchive{
		Generation: 0, Transition: "initial", Hardware: hardware, BootISO: []byte("boot-0"),
		Handoff: "handoff-0", SourceSnapshot: snapshot, KernelELF: []byte("kernel-0"), SuccessorISO: []byte("iso-0"),
	}); err != nil {
		t.Fatal(err)
	}
	parent := uint64(0)
	if _, err := WriteCompletedArchive(run, CompletedArchive{
		Generation: 1, ParentGeneration: &parent, Transition: "successor", Hardware: hardware, BootISO: []byte("boot-1"),
		Handoff: "handoff-1", SourceSnapshot: snapshot, KernelELF: []byte("kernel-1"), SuccessorISO: []byte("iso-1"),
	}); err != nil {
		t.Fatal(err)
	}
	runState, err := NewCodexOSRun(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := runState.ReopenAtGate(); err != nil {
		t.Fatal(err)
	}
	if err := runState.ForkFromGeneration(1); err == nil || !strings.Contains(err.Error(), "earlier") {
		t.Fatalf("same-generation fork error = %v", err)
	}
	if !runState.RetainGenerationFinish(1) {
		t.Fatal("latest completed gate could not be retained")
	}
	if err := runState.ForkFromGeneration(0); err == nil || !strings.Contains(err.Error(), "retained for an exit interview") {
		t.Fatalf("fork under retained gate error = %v", err)
	}
	runState.ReleaseGenerationFinish(1)
	if err := runState.ForkFromGeneration(0); err != nil {
		t.Fatal(err)
	}
	if runState.State() != RuntimeStateRunning {
		t.Fatalf("forked state = %q", runState.State())
	}
	if generation, ok := runState.GenerationNumber(); !ok || generation != 2 {
		t.Fatalf("forked generation = %d, %t", generation, ok)
	}
	if transition, ok := runState.CurrentTransition(); !ok || transition != "rollback" {
		t.Fatalf("transition = %q, %t", transition, ok)
	}
	if handoff, ok := runState.PreviousHandoff(); !ok || handoff != "handoff-0" {
		t.Fatalf("handoff = %q, %t", handoff, ok)
	}
}

func TestHistoryAndPartialStateFailClosed(t *testing.T) {
	run := t.TempDir()
	hardware := testHardware(t)
	snapshot := testSnapshot(t, "source\n")
	parent := uint64(0)
	if _, err := WriteCompletedArchive(run, CompletedArchive{
		Generation: 0, Transition: "initial", Hardware: hardware, BootISO: []byte("boot"),
		Handoff: "handoff", SourceSnapshot: snapshot, KernelELF: []byte("kernel"), SuccessorISO: []byte("iso"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteCompletedArchive(run, CompletedArchive{
		Generation: 2, ParentGeneration: &parent, Transition: "rollback", Hardware: hardware, BootISO: []byte("boot"),
		Handoff: "handoff", SourceSnapshot: snapshot, KernelELF: []byte("kernel"), SuccessorISO: []byte("iso"),
	}); err != nil {
		t.Fatal(err)
	}
	runState, err := NewCodexOSRun(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := runState.ReopenAtGate(); err == nil || !strings.Contains(err.Error(), "not contiguous") {
		t.Fatalf("history error = %v", err)
	}
	partial := filepath.Join(run, ".generation-0003-active")
	if err := os.Mkdir(partial, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadArchivedGenerations(run); err != nil {
		t.Fatal(err)
	}
	if err := runState.ReopenAtGate(); err == nil || !strings.Contains(err.Error(), "partial generation state") {
		t.Fatalf("partial-state error = %v", err)
	}
}

func TestSymlinkAndCollisionAreRejectedWithoutMutation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink test requires Unix semantics")
	}
	run := t.TempDir()
	hardware := testHardware(t)
	snapshot := testSnapshot(t, "source\n")
	if _, err := WriteCompletedArchive(run, CompletedArchive{
		Generation: 0, Transition: "initial", Hardware: hardware, BootISO: []byte("boot"),
		Handoff: "handoff", SourceSnapshot: snapshot, KernelELF: []byte("kernel"), SuccessorISO: []byte("iso"),
	}); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(run, "generation-0000")
	before := archiveBytes(t, archive)
	if _, err := WriteAbortedArchive(run, AbortedArchive{
		Generation: 0, Transition: "initial", Hardware: hardware,
		AbortReason: "operator stopped the generation",
	}); err == nil {
		t.Fatal("collision publication succeeded")
	}
	if got := archiveBytes(t, archive); !sameArchiveBytes(got, before) {
		t.Fatal("collision changed existing archive")
	}
	sourceFile := filepath.Join(archive, "source", "seed", "kernel.c")
	if err := os.Remove(sourceFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(run, "outside"), sourceFile); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadArchivedGenerations(run); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("nested source symlink error = %v", err)
	}
	if err := os.Remove(sourceFile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourceFile, []byte("source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(archive, filepath.Join(run, "generation-0001")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadArchivedGenerations(run); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("symlink archive error = %v", err)
	}
}

func TestLegacyCompletedArchiveInspectionAndGateReopen(t *testing.T) {
	run := t.TempDir()
	archive := filepath.Join(run, "generation-0000")
	hardware, err := qemu.EncodeHardwareManifest(testHardware(t))
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately omit later optional harness, source-capacity, and bootstrap
	// records: existing archives remain readable without rewriting them.
	files := map[string][]byte{
		"metadata.json": []byte(`{"generation":0,"outcome":"completed","parent_generation":null,"transition":"initial"}`),
		"hardware.json": hardware, "source.snapshot": testSnapshot(t, "Legacy source\n"),
		"source/seed/kernel.c": []byte("Legacy source\n"), "handoff.txt": []byte("Legacy handoff λ.\n"),
		"boot/codexos.iso": []byte("boot"), "successor/kernel.elf": []byte("kernel"),
		"successor/codexos.iso": []byte("successor"), "qemu.stdout": {}, "qemu.stderr": {},
	}
	for name, contents := range files {
		path := filepath.Join(archive, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, contents, 0600); err != nil {
			t.Fatal(err)
		}
	}
	before := archiveBytes(t, archive)
	archives, err := LoadArchivedGenerations(run)
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 1 || archives[0].Handoff == nil || *archives[0].Handoff != "Legacy handoff λ.\n" {
		t.Fatalf("archives = %#v", archives)
	}
	opened, err := NewCodexOSRun(run)
	if err != nil {
		t.Fatal(err)
	}
	if err = opened.ReopenAtGate(); err != nil {
		t.Fatal(err)
	}
	pending, ok := opened.PendingGenerationFinish()
	if !ok || pending.HandoffMessage != "Legacy handoff λ.\n" || !bytes.Equal(pending.SourceSnapshot, files["source.snapshot"]) {
		t.Fatalf("reopened successor = %#v", pending)
	}
	if !sameArchiveBytes(before, archiveBytes(t, archive)) {
		t.Fatal("inspection or gate reopening changed legacy archive")
	}
}

func testHardware(t *testing.T) qemu.HardwareManifest {
	t.Helper()
	hardware, err := qemu.TestHardwareProfile.Manifest("QEMU emulator version test")
	if err != nil {
		t.Fatal(err)
	}
	return hardware
}

func experimentHarnessIdentity() provenance.HarnessIdentity {
	return provenance.HarnessIdentity{
		SchemaVersion:    provenance.HarnessIdentitySchemaVersion,
		RepositoryCommit: strings.Repeat("a", 40),
		Executable:       provenance.FileIdentity{SHA256: strings.Repeat("b", 64), Size: 42},
		Build: provenance.HarnessBuildIdentity{
			GoVersion: "go-test", ModulePath: "codexos", ModuleVersion: "v-test",
			SettingsSHA256: strings.Repeat("c", 64),
		},
	}
}

func testSnapshot(t *testing.T, source string) []byte {
	t.Helper()
	snapshot, err := guest.EncodeSourceSnapshot([]guest.SnapshotFile{{Path: "seed/kernel.c", Content: []byte(source)}})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func archiveBytes(t *testing.T, root string) map[string][]byte {
	t.Helper()
	result := make(map[string][]byte)
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			value, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			result[filepath.ToSlash(relative)] = value
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

func sameArchiveBytes(left, right map[string][]byte) bool {
	if len(left) != len(right) {
		return false
	}
	for path, value := range left {
		if !bytes.Equal(value, right[path]) {
			return false
		}
	}
	return true
}
