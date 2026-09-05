package experiment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"codexos/internal/build"
	"codexos/internal/guest"
	"codexos/internal/qemu"
)

func (r *CodexOSRun) finishLiveIfRequested(generation *liveGeneration, number uint64) error {
	if generation == nil || generation.hostServices == nil {
		return nil
	}
	pending := generation.hostServices.PendingGenerationFinish()
	if pending == nil {
		return nil
	}
	return r.completeLiveGeneration(generation, number, pending, generation.hostServices.LatestSuccessfulBuild())
}

func (r *CodexOSRun) completeLiveGeneration(generation *liveGeneration, number uint64, pending *build.PendingGenerationFinish, validated *build.StagedBuildArtifacts) error {
	if pending == nil || validated == nil || pending.KernelELF != validated.KernelELF || pending.ISO != validated.ISO ||
		!bytes.Equal(pending.SourceSnapshot, validated.SourceSnapshot) {
		return &GenerationRuntimeError{Reason: "accepted generation finish has no matching validated successor"}
	}
	r.gateMu.Lock()
	if r.state != RuntimeStateRunning || r.generationNumber == nil || *r.generationNumber != number ||
		r.currentTransition == "" || r.currentHardware.SchemaVersion == 0 {
		r.gateMu.Unlock()
		return &GenerationRuntimeError{Reason: "CodexOS generation state is unavailable"}
	}
	parent := cloneUint64Pointer(r.currentParent)
	transition := r.currentTransition
	hardware := r.currentHardware
	r.gateMu.Unlock()
	if err := r.recordHarnessGenerationStart(number); err != nil {
		return err
	}

	if r.liveGeneration() != generation {
		return &GenerationRuntimeError{Reason: "CodexOS live generation ownership changed"}
	}
	closeErr := closeLiveGeneration(generation, defaultGenerationStopTimeout)
	if closeErr != nil {
		return &GenerationRuntimeError{Reason: "could not stop completed generation", Err: closeErr}
	}
	if !r.detachExpectedLiveGeneration(generation) {
		return &GenerationRuntimeError{Reason: "CodexOS live generation ownership changed after shutdown"}
	}
	archive, err := writeCompletedArchiveFiles(r.runDirectory, completedArchiveFiles{
		Generation: number, ParentGeneration: parent, Transition: transition, Hardware: hardware,
		HarnessIdentity: r.HarnessIdentity(),
		SourceCapacity:  r.SourceCapacity(),
		BootISO:         generation.bootISO, Handoff: pending.HandoffMessage, SourceSnapshot: pending.SourceSnapshot,
		KernelELF: pending.KernelELF, SuccessorISO: pending.ISO,
		KernelIdentity: validated.KernelIdentity, ISOIdentity: validated.ISOIdentity,
		QEMUStdout: generation.stdoutPath, QEMUStderr: generation.stderrPath,
	})
	if err != nil {
		_ = os.RemoveAll(generation.workspace)
		r.failLiveGeneration(generation, false)
		return err
	}
	_ = os.RemoveAll(generation.workspace)
	handoff := pending.HandoffMessage
	r.gateMu.Lock()
	r.pendingFinish = &PendingGenerationFinish{
		HandoffMessage: handoff,
		SourceSnapshot: append([]byte(nil), pending.SourceSnapshot...),
		KernelELF:      filepath.Join(archive.ArchivePath, successorName, "kernel.elf"),
		ISO:            filepath.Join(archive.ArchivePath, successorName, "codexos.iso"),
	}
	r.previousHandoff = &handoff
	r.currentOperatorFeedback = nil
	r.currentBootImage = ""
	r.currentParent = nil
	r.currentTransition = ""
	r.currentHardware = qemu.HardwareManifest{}
	r.state = RuntimeStateAwaitingNextGeneration
	r.gateMu.Unlock()
	r.setObservedLiveState()
	r.recordLive("generation_completed", &number, map[string]any{
		"transition":             transition,
		"duration_seconds":       nonNegativeLiveDuration(generation.startedAt),
		"source_snapshot_sha256": sha256Hex(pending.SourceSnapshot),
		"source_snapshot_bytes":  len(pending.SourceSnapshot),
	})
	return nil
}

func (r *CodexOSRun) continueLiveGeneration(ctx context.Context) error {
	r.live.operationMu.Lock()
	defer r.live.operationMu.Unlock()
	operationContext, cancelOperation := r.liveOperationContext(ctx)
	defer cancelOperation()
	r.gateMu.Lock()
	if r.transitioning {
		r.gateMu.Unlock()
		return &GenerationRuntimeError{Reason: "CodexOS generation transition is already active"}
	}
	if r.retainedFinish != nil {
		r.gateMu.Unlock()
		return &GenerationRuntimeError{Reason: "completed generation is retained for an exit interview"}
	}
	if r.state != RuntimeStateAwaitingNextGeneration {
		r.gateMu.Unlock()
		return &GenerationRuntimeError{Reason: "CodexOS run is not awaiting a generation"}
	}
	if r.pendingFinish == nil || r.generationNumber == nil {
		r.gateMu.Unlock()
		return &GenerationRuntimeError{Reason: "CodexOS run has no selected successor"}
	}
	if *r.generationNumber == ^uint64(0) {
		r.gateMu.Unlock()
		return &GenerationRuntimeError{Reason: "generation number space is exhausted"}
	}
	if _, err := guest.ParseSourceSnapshotWithBudget(r.pendingFinish.SourceSnapshot, r.sourceCapacity); err != nil {
		r.gateMu.Unlock()
		return err
	}
	parent := *r.generationNumber
	next := parent + 1
	image := r.pendingFinish.ISO
	handoff := r.pendingFinish.HandoffMessage
	r.transitioning = true
	r.gateMu.Unlock()
	if !isRegularWithoutSymlink(image) {
		r.clearLiveTransition()
		return &GenerationRuntimeError{Reason: "selected successor artifact is missing: " + image}
	}
	generation, hardware, err := r.bootLiveGeneration(operationContext, next, image, &parent)
	if err != nil {
		r.clearLiveTransition()
		return err
	}
	if err := r.recordHarnessGenerationStart(next); err != nil {
		closeErr := closeLiveGeneration(generation, defaultGenerationStopTimeout)
		if closeErr == nil {
			_ = os.RemoveAll(generation.workspace)
		}
		r.clearLiveTransition()
		return errors.Join(err, closeErr)
	}
	feedback, err := r.attachOperatorFeedback(next)
	if err != nil {
		closeErr := closeLiveGeneration(generation, defaultGenerationStopTimeout)
		if closeErr == nil {
			_ = os.RemoveAll(generation.workspace)
		}
		r.clearLiveTransition()
		return errors.Join(err, closeErr)
	}
	r.installLiveGeneration(generation)
	r.gateMu.Lock()
	r.generationNumber = &next
	r.currentParent = &parent
	r.currentTransition = "successor"
	r.currentBootImage = generation.bootISO
	r.currentHardware = hardware
	r.previousHandoff = &handoff
	r.currentOperatorFeedback = feedback
	r.pendingFinish = nil
	r.gateHarnessTransition = nil
	r.transitioning = false
	r.state = RuntimeStateRunning
	r.gateMu.Unlock()
	r.setObservedLiveState()
	r.recordLiveGenerationStarted(next, &parent, "successor", generation.controller)
	return nil
}

func (r *CodexOSRun) forkLiveGeneration(ctx context.Context, forkParent uint64) error {
	r.live.operationMu.Lock()
	defer r.live.operationMu.Unlock()
	operationContext, cancelOperation := r.liveOperationContext(ctx)
	defer cancelOperation()
	r.gateMu.Lock()
	if r.transitioning {
		r.gateMu.Unlock()
		return &GenerationRuntimeError{Reason: "CodexOS generation transition is already active"}
	}
	if r.retainedFinish != nil {
		r.gateMu.Unlock()
		return &GenerationRuntimeError{Reason: "completed generation is retained for an exit interview"}
	}
	if r.state != RuntimeStateAwaitingNextGeneration {
		r.gateMu.Unlock()
		return &GenerationRuntimeError{Reason: "CodexOS run is not awaiting a generation"}
	}
	if r.generationNumber == nil {
		r.gateMu.Unlock()
		return &GenerationRuntimeError{Reason: "CodexOS generation number is unavailable"}
	}
	if forkParent >= *r.generationNumber {
		r.gateMu.Unlock()
		return &GenerationRuntimeError{Reason: "fork parent must be an earlier generation"}
	}
	if *r.generationNumber == ^uint64(0) {
		r.gateMu.Unlock()
		return &GenerationRuntimeError{Reason: "generation number space is exhausted"}
	}
	next := *r.generationNumber + 1
	r.transitioning = true
	r.gateMu.Unlock()
	archived, err := InspectGeneration(r.runDirectory, forkParent)
	if err != nil {
		r.clearLiveTransition()
		return err
	}
	if archived.Outcome != "completed" || archived.Handoff == nil {
		r.clearLiveTransition()
		return &GenerationRuntimeError{Reason: "aborted generation cannot be a rollback parent"}
	}
	if err := validateInheritedSource(archived, r.SourceCapacity()); err != nil {
		r.clearLiveTransition()
		return err
	}
	image := filepath.Join(archived.ArchivePath, successorName, "codexos.iso")
	if !isRegularWithoutSymlink(image) {
		r.clearLiveTransition()
		return &GenerationRuntimeError{Reason: "generation archive artifact is missing: " + image}
	}
	generation, hardware, err := r.bootLiveGeneration(operationContext, next, image, &forkParent)
	if err != nil {
		r.clearLiveTransition()
		return err
	}
	if err := r.recordHarnessGenerationStart(next); err != nil {
		closeErr := closeLiveGeneration(generation, defaultGenerationStopTimeout)
		if closeErr == nil {
			_ = os.RemoveAll(generation.workspace)
		}
		r.clearLiveTransition()
		return errors.Join(err, closeErr)
	}
	feedback, err := r.attachOperatorFeedback(next)
	if err != nil {
		closeErr := closeLiveGeneration(generation, defaultGenerationStopTimeout)
		if closeErr == nil {
			_ = os.RemoveAll(generation.workspace)
		}
		r.clearLiveTransition()
		return errors.Join(err, closeErr)
	}
	r.installLiveGeneration(generation)
	handoff := *archived.Handoff
	r.gateMu.Lock()
	r.generationNumber = &next
	r.currentParent = &forkParent
	r.currentTransition = "rollback"
	r.currentBootImage = generation.bootISO
	r.currentHardware = hardware
	r.previousHandoff = &handoff
	r.currentOperatorFeedback = feedback
	r.pendingFinish = nil
	r.gateHarnessTransition = nil
	r.transitioning = false
	r.state = RuntimeStateRunning
	r.gateMu.Unlock()
	r.setObservedLiveState()
	r.recordLive("generation_rollback_started", &next, map[string]any{"parent_generation": forkParent})
	r.recordLiveGenerationStarted(next, &forkParent, "rollback", generation.controller)
	return nil
}

func (r *CodexOSRun) abortLiveGeneration(reason string) error {
	r.suspendBootstrap()
	if err := ValidateAbortReason(reason); err != nil {
		return err
	}
	r.live.operationMu.Lock()
	defer r.live.operationMu.Unlock()
	r.gateMu.Lock()
	if r.state != RuntimeStateRunning && r.state != RuntimeStatePaused {
		r.gateMu.Unlock()
		return &GenerationRuntimeError{Reason: "CodexOS generation cannot be aborted"}
	}
	if r.generationNumber == nil || r.currentTransition == "" || r.currentHardware.SchemaVersion == 0 {
		r.gateMu.Unlock()
		return &GenerationRuntimeError{Reason: "CodexOS generation state is unavailable"}
	}
	number := *r.generationNumber
	parent := cloneUint64Pointer(r.currentParent)
	transition := r.currentTransition
	hardware := r.currentHardware
	r.gateMu.Unlock()
	if err := r.recordHarnessGenerationStart(number); err != nil {
		return err
	}
	generation := r.liveGeneration()
	if generation == nil {
		return &GenerationRuntimeError{Reason: "CodexOS live generation is unavailable"}
	}
	var latest *AbortedSuccessEvidence
	if successful := generation.hostServices.LatestSuccessfulBuild(); successful != nil && successful.Evidence != nil {
		manifest, err := successful.Evidence.AbortedArchiveManifest()
		if err != nil {
			return errors.Join(err, r.failLiveGeneration(generation, true))
		}
		encoded, err := json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			return errors.Join(err, r.failLiveGeneration(generation, true))
		}
		encoded = append(encoded, '\n')
		latest = &AbortedSuccessEvidence{Manifest: encoded, Snapshot: successful.SourceSnapshot}
	}
	closeErr := closeLiveGeneration(generation, defaultGenerationStopTimeout)
	if closeErr != nil {
		return &GenerationRuntimeError{Reason: "could not stop aborted generation", Err: closeErr}
	}
	if !r.detachExpectedLiveGeneration(generation) {
		return &GenerationRuntimeError{Reason: "CodexOS live generation ownership changed after shutdown"}
	}
	_, err := writeAbortedArchiveFiles(r.runDirectory, abortedArchiveFiles{
		Generation: number, ParentGeneration: parent, Transition: transition, Hardware: hardware,
		HarnessIdentity: r.HarnessIdentity(),
		SourceCapacity:  r.SourceCapacity(),
		BootISO:         generation.bootISO, QEMUStdout: generation.stdoutPath, QEMUStderr: generation.stderrPath,
		AbortReason:   reason,
		LatestSuccess: latest,
	})
	if err != nil {
		_ = os.RemoveAll(generation.workspace)
		r.failLiveGeneration(generation, false)
		return err
	}
	_ = os.RemoveAll(generation.workspace)
	r.gateMu.Lock()
	r.pendingFinish = nil
	r.retainedFinish = nil
	r.previousHandoff = nil
	r.currentOperatorFeedback = nil
	r.currentBootImage = ""
	r.currentParent = nil
	r.currentTransition = ""
	r.currentHardware = qemu.HardwareManifest{}
	r.state = RuntimeStateAwaitingNextGeneration
	r.gateMu.Unlock()
	r.setObservedLiveState()
	r.recordLive("generation_aborted", &number, map[string]any{
		"transition": transition, "duration_seconds": nonNegativeLiveDuration(generation.startedAt), "reason": reason,
	})
	return nil
}

func (r *CodexOSRun) clearLiveTransition() {
	r.gateMu.Lock()
	r.transitioning = false
	r.gateMu.Unlock()
}

func (r *CodexOSRun) failLiveGeneration(generation *liveGeneration, cleanup bool) error {
	var cleanupErr error
	if cleanup {
		cleanupErr = closeLiveGeneration(generation, defaultGenerationStopTimeout)
	}
	if generation != nil && cleanup && cleanupErr == nil {
		_ = r.detachExpectedLiveGeneration(generation)
		_ = os.RemoveAll(generation.workspace)
	}
	if cleanupErr != nil {
		return cleanupErr
	}
	r.gateMu.Lock()
	r.pendingFinish = nil
	r.retainedFinish = nil
	r.previousHandoff = nil
	r.currentOperatorFeedback = nil
	r.currentBootImage = ""
	r.currentParent = nil
	r.currentTransition = ""
	r.currentHardware = qemu.HardwareManifest{}
	r.gateHarnessTransition = nil
	r.transitioning = false
	r.state = RuntimeStateStopped
	r.gateMu.Unlock()
	r.setObservedLiveState()
	return cleanupErr
}

func nonNegativeLiveDuration(started time.Time) float64 {
	if started.IsZero() {
		return 0
	}
	duration := time.Since(started).Seconds()
	if duration < 0 {
		return 0
	}
	return duration
}
