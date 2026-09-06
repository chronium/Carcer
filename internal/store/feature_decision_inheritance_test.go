package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCrossRunFeatureDecisionNotesPreservedAndProtected(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := createCrossRunFixture(source); err != nil {
		t.Fatal(err)
	}
	s, err := NewFeatureRequestStore(source)
	if err != nil {
		t.Fatal(err)
	}
	const note = "Already provisioned. Four jobs is scope, not a new quota. λ"
	if _, err := s.Approve(1, note); err != nil {
		t.Fatal(err)
	}
	pending, err := s.Create(0, "Later request", "Guest-authored description")
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repository")
	createCrossRunGitRepository(t, repo, "source/generation-0000")
	dest := filepath.Join(root, "destination")
	if _, err := InitializeCrossRunBootstrap(dest, filepath.Join(source, "generation-0000", "successor", "codexos.iso"), source, 0, repo, "source/generation-0000"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCrossRunBootstrap(dest); err != nil {
		t.Fatal(err)
	}
	inherited, err := NewFeatureRequestStore(dest)
	if err != nil {
		t.Fatal(err)
	}
	got, err := inherited.Request(1)
	if err != nil || got.DecisionNote != note {
		t.Fatalf("inherited note: %+v, %v", got, err)
	}
	// Final decisions are immutable against the inherited snapshot.
	forged := got
	forged.DecisionNote = "changed"
	if err := inherited.write(forged, true); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCrossRunBootstrap(dest); err == nil {
		t.Fatal("changed inherited note accepted")
	}
	if err := inherited.write(got, true); err != nil {
		t.Fatal(err)
	}
	// Archive a disposable destination generation before deciding its pending request.
	if err := createCrossRunHistory(dest, 0, -1); err != nil {
		t.Fatal(err)
	}
	if _, err := inherited.Deny(pending.ID, "Unavailable for now. λ"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCrossRunBootstrap(dest); err != nil {
		t.Fatalf("destination decision rejected: %v", err)
	}
	runCrossRunGit(t, repo, "tag", "--annotate", "--no-sign", "--cleanup=verbatim", "-m", "Cross-run base", "destination/generation-0000")
	next := filepath.Join(root, "next")
	if _, err := InitializeCrossRunBootstrap(next, filepath.Join(dest, "generation-0000", "successor", "codexos.iso"), dest, 0, repo, "destination/generation-0000"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCrossRunBootstrap(next); err != nil {
		t.Fatal(err)
	}
	ns, err := NewFeatureRequestStore(next)
	if err != nil {
		t.Fatal(err)
	}
	all, err := ns.Requests()
	if err != nil || len(all) != 3 || all[0].DecisionNote != note || all[2].DecisionNote != "Unavailable for now. λ" || all[2].Description != pending.Description {
		t.Fatalf("chained inheritance: %+v, %v", all, err)
	}
	// Immutable ledger identity includes note bytes.
	path := filepath.Join(next, CrossRunBootstrapFeatureLedger)
	if err := os.WriteFile(path, []byte(`{"requests":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCrossRunBootstrap(next); err == nil {
		t.Fatal("changed ledger accepted")
	}
}
