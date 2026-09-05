package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codexos/internal/guest"
	"codexos/internal/sourcecapacity"
)

func TestSourceCapacityInheritanceRejectsBeforePublishingDestination(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := createPythonCrossRunFixture(source, repositoryRootForCrossRunTest(t)); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(source, "generation-0000")
	snapshot, err := guest.EncodeSourceSnapshotWithBudget([]guest.SnapshotFile{{Path: "seed/kernel.c", Content: make([]byte, sourcecapacity.Expanded)}}, sourcecapacity.Expanded)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archive, "source.snapshot"), snapshot, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archive, "source/seed/kernel.c"), make([]byte, sourcecapacity.Expanded), 0600); err != nil {
		t.Fatal(err)
	}
	if err := sourcecapacity.Save(archive, sourcecapacity.Expanded); err != nil {
		t.Fatal(err)
	}
	if err := sourcecapacity.Save(source, sourcecapacity.Expanded); err != nil {
		t.Fatal(err)
	}
	if _, err := readCrossRunArchives(source); err != nil {
		t.Fatalf("recorded expanded source archive rejected: %v", err)
	}
	destination := filepath.Join(root, "uncreated-parent", "destination")
	_, err = InitializeCrossRunBootstrap(destination, filepath.Join(archive, "successor/codexos.iso"), source, 0, filepath.Join(root, "repository"), "source/generation-0000")
	if err == nil || !strings.Contains(err.Error(), "destination source content capacity (65536 bytes)") {
		t.Fatalf("inheritance = %v", err)
	}
	if _, err := os.Lstat(filepath.Dir(destination)); !os.IsNotExist(err) {
		t.Fatalf("failed inheritance published state: %v", err)
	}

	repository := filepath.Join(root, "repository")
	createCrossRunGitRepository(t, repository, "source/generation-0000")
	expandedDestination := filepath.Join(root, "expanded-destination")
	if _, err := InitializeCrossRunBootstrapWithCapacity(expandedDestination, filepath.Join(archive, "successor/codexos.iso"), source, 0, repository, "source/generation-0000", nil, sourcecapacity.Expanded); err != nil {
		t.Fatal(err)
	}
	budget, err := sourcecapacity.Load(expandedDestination)
	if err != nil || budget.Bytes() != sourcecapacity.Expanded {
		t.Fatalf("expanded destination persistence: %v %v", budget, err)
	}
	if _, err := LoadCrossRunBootstrap(expandedDestination); err != nil {
		t.Fatalf("expanded bootstrap reopen: %v", err)
	}
	sourceBytes, err := os.ReadFile(filepath.Join(archive, "source.snapshot"))
	if err != nil || string(sourceBytes) != string(snapshot) {
		t.Fatal("inheritance changed source snapshot")
	}
	for _, budget := range []sourcecapacity.Budget{1, sourcecapacity.Expanded} {
		failedDestination := filepath.Join(root, "failed-destination")
		if _, err := InitializeCrossRunBootstrapWithCapacity(failedDestination, filepath.Join(archive, "successor/codexos.iso"), source, 0, repository, "wrong-base", nil, budget); err == nil {
			t.Fatal("invalid bootstrap succeeded")
		}
		if _, err := os.Lstat(failedDestination); !os.IsNotExist(err) {
			t.Fatal("failed bootstrap published capacity or other destination state")
		}
		staging, err := filepath.Glob(filepath.Join(root, ".cross-run-bootstrap-*"))
		if err != nil || len(staging) != 0 {
			t.Fatal("failed bootstrap left staging state")
		}
	}
	// Provisioning a source run does not expand a fresh destination, even when
	// the selected snapshot fits and inheritance succeeds.
	small, err := guest.EncodeSourceSnapshot([]guest.SnapshotFile{{Path: "seed/kernel.c", Content: []byte("source\n")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archive, "source.snapshot"), small, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archive, "source/seed/kernel.c"), []byte("source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeCrossRunBootstrap(destination, filepath.Join(archive, "successor/codexos.iso"), source, 0, repository, "source/generation-0000"); err != nil {
		t.Fatal(err)
	}
	budget, err = sourcecapacity.Load(destination)
	if err != nil || budget.Bytes() != sourcecapacity.Default {
		t.Fatalf("fresh destination expanded: %v %v", budget, err)
	}
}
