package experiment

import (
	"fmt"
	"path/filepath"

	"codexos/internal/guest"
	"codexos/internal/sourcecapacity"
)

func (r *CodexOSRun) SourceCapacity() sourcecapacity.Budget {
	if r == nil {
		return 0
	}
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	return r.sourceCapacity
}

// SetSourceCapacity provisions this run only at a validated, inactive gate.
// It neither approves a feature request nor starts a generation.
func (r *CodexOSRun) SetSourceCapacity(budget sourcecapacity.Budget) error {
	if r == nil {
		return &GenerationRuntimeError{Reason: "run is nil"}
	}
	if err := budget.Validate(); err != nil {
		return err
	}
	if r.live != nil {
		r.live.operationMu.Lock()
		defer r.live.operationMu.Unlock()
		if r.live.closed || r.liveGeneration() != nil {
			return &GenerationRuntimeError{Reason: "source capacity requires an inactive generation gate"}
		}
	}
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	if r.state != RuntimeStateAwaitingNextGeneration || r.generationNumber == nil || r.retainedFinish != nil || r.transitioning {
		return &GenerationRuntimeError{Reason: "source capacity requires a validated inactive generation gate with no retained interview"}
	}
	partial, err := partialGenerationState(r.runDirectory)
	if err != nil {
		return err
	}
	if len(partial) != 0 {
		return &GenerationRuntimeError{Reason: "run contains partial generation state"}
	}
	archives, err := LoadArchivedGenerations(r.runDirectory)
	if err != nil {
		return err
	}
	if err := ValidateArchivedHistory(archives); err != nil {
		return err
	}
	if len(archives) == 0 || archives[len(archives)-1].Generation != *r.generationNumber {
		return &GenerationRuntimeError{Reason: "generation gate does not match archived history"}
	}
	latest := archives[len(archives)-1]
	if latest.Outcome == "completed" {
		if err := validateInheritedSource(latest, budget); err != nil {
			return err
		}
	}
	if r.pendingFinish != nil {
		if _, err := guest.ParseSourceSnapshotWithBudget(r.pendingFinish.SourceSnapshot, budget); err != nil {
			return err
		}
	}
	previous := r.sourceCapacity
	if err := sourcecapacity.Save(r.runDirectory, budget); err != nil {
		return err
	}
	r.sourceCapacity = sourcecapacity.Budget(budget.Bytes())
	r.recordLive("source_capacity_provisioned", r.generationNumber, map[string]any{
		"previous_source_content_bytes": previous.Bytes(), "source_content_bytes": budget.Bytes(),
		"source_snapshot_max_bytes": budget.SnapshotLimit(),
	})
	return nil
}

func validateInheritedSource(archive ArchivedGeneration, destination sourcecapacity.Budget) error {
	snapshot, err := sourcecapacity.ReadFile(filepath.Join(archive.ArchivePath, sourceSnapshotName), archive.SourceCapacity.SnapshotLimit())
	if err != nil {
		return err
	}
	if _, err := guest.ParseSourceSnapshotWithBudget(snapshot, destination); err != nil {
		return fmt.Errorf("generation %d source cannot enter run with %d-byte source content capacity: %w", archive.Generation, destination.Bytes(), err)
	}
	return nil
}
