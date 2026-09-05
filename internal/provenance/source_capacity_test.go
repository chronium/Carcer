package provenance

import (
	"bytes"
	"path/filepath"
	"testing"

	"codexos/internal/guest"
	"codexos/internal/sourcecapacity"
)

func TestSourceCapacityGitReconcilesLargeHistoricalArchive(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	createGenerationGitRepository(t, repository)
	run := filepath.Join(root, "capacity-run")
	files := []guest.SnapshotFile{{Path: "seed/kernel.c", Content: []byte("small")}}
	archiveGenerationGitCompleted(t, run, 0, nil, "initial", files, "handoff")
	archive := filepath.Join(run, "generation-0000")
	files[0].Content = bytes.Repeat([]byte("x"), sourcecapacity.Expanded)
	snapshot, err := guest.EncodeSourceSnapshotWithBudget(files, sourcecapacity.Expanded)
	if err != nil {
		t.Fatal(err)
	}
	writeGenerationGitFile(t, filepath.Join(archive, "source.snapshot"), snapshot)
	writeGenerationGitFile(t, filepath.Join(archive, "source/seed/kernel.c"), files[0].Content)
	if err := sourcecapacity.Save(archive, sourcecapacity.Expanded); err != nil {
		t.Fatal(err)
	}
	// The run's current/default budget must not invalidate its older archive.
	if err := sourcecapacity.Save(run, sourcecapacity.Default); err != nil {
		t.Fatal(err)
	}
	recorder, err := NewGenerationGitRecorder(repository, run, "test-base")
	if err != nil {
		t.Fatal(err)
	}
	records, err := recorder.Reconcile()
	if err != nil || len(records) != 1 {
		t.Fatalf("large reconcile: %v %v", records, err)
	}
	if got := generationGitCommand(t, repository, "show", records[0].Commit+":seed/kernel.c"); !bytes.Equal([]byte(got), files[0].Content) {
		t.Fatal("Git did not preserve full expanded source")
	}
	again, err := recorder.Reconcile()
	if err != nil || len(again) != 1 || !again[0].AlreadyRecorded {
		t.Fatalf("reopen reconcile: %v %v", again, err)
	}
}
