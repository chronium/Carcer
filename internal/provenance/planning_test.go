package provenance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCompletedPlanningEvidenceIsExactAndImmutable(t *testing.T) {
	run := t.TempDir()
	evidence, err := NewPlanningEvidenceStore(run).Begin(16, "thread-sol")
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Generation() != 16 {
		t.Fatalf("generation = %d", evidence.Generation())
	}
	if err := evidence.RecordStarted("turn-plan"); err != nil {
		t.Fatal(err)
	}
	response := "Inspect first.\n\nThen choose independently. Ω"
	identity, err := evidence.Complete("completed", &response)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(response))
	if identity.Size != uint64(len([]byte(response))) || identity.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("identity = %#v", identity)
	}
	directory := filepath.Join(run, "planning-evidence", "generation-0016")
	contents, err := os.ReadFile(filepath.Join(directory, "response.txt"))
	if err != nil || !bytes.Equal(contents, []byte(response)) {
		t.Fatalf("response = %q, %v", contents, err)
	}
	manifest := readManifest(t, filepath.Join(directory, "manifest.json"))
	if manifest.Outcome != "completed" || manifest.Stage != "completed" || manifest.ThreadID != "thread-sol" || manifest.TurnID == nil || *manifest.TurnID != "turn-plan" {
		t.Fatalf("manifest = %#v", manifest)
	}
	if len(manifest.Attempts) != 1 || manifest.Attempts[0].Outcome != "completed" {
		t.Fatalf("attempts = %#v", manifest.Attempts)
	}
	if _, err := NewPlanningEvidenceStore(run).Begin(16, "another-thread"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate Begin() error = %v", err)
	}
	if err := evidence.RecordStarted("another-turn"); err == nil {
		t.Fatal("completed evidence started another attempt")
	}
}

func TestInterruptedResumedAndFailedPlanningEvidence(t *testing.T) {
	run := t.TempDir()
	interrupted, err := NewPlanningEvidenceStore(run).Begin(4, "thread-4")
	if err != nil {
		t.Fatal(err)
	}
	if err := interrupted.RecordStarted("turn-4"); err != nil {
		t.Fatal(err)
	}
	partial := "Partial plan."
	if _, err := interrupted.Complete("interrupted", &partial); err != nil {
		t.Fatal(err)
	}
	manifest := readManifest(t, filepath.Join(run, "planning-evidence", "generation-0004", "manifest.json"))
	if manifest.Outcome != "incomplete" || manifest.Stage != "awaiting_resume" || manifest.Attempts[0].Outcome != "interrupted" {
		t.Fatalf("interrupted manifest = %#v", manifest)
	}
	if manifest.Attempts[0].ResponseFile != "attempt-0001-response.txt" {
		t.Fatalf("response file = %q", manifest.Attempts[0].ResponseFile)
	}
	if _, err := os.Stat(filepath.Join(run, "planning-evidence", "generation-0004", "response.txt")); !os.IsNotExist(err) {
		t.Fatalf("interruption published final response: %v", err)
	}
	if err := interrupted.RecordStarted("turn-5"); err != nil {
		t.Fatal(err)
	}
	final := "Final successful plan."
	if _, err := interrupted.Complete("completed", &final); err != nil {
		t.Fatal(err)
	}

	failed, err := NewPlanningEvidenceStore(run).Begin(5, "thread-5")
	if err != nil {
		t.Fatal(err)
	}
	if err := failed.RecordStarted("turn-5"); err != nil {
		t.Fatal(err)
	}
	if err := failed.Fail(); err != nil {
		t.Fatal(err)
	}
	if err := failed.Fail(); err != nil {
		t.Fatal(err)
	}
	failedManifest := readManifest(t, filepath.Join(run, "planning-evidence", "generation-0005", "manifest.json"))
	if failedManifest.Outcome != "failed" || failedManifest.Attempts[0].Outcome != "failed" || failedManifest.ResponseFile != "" {
		t.Fatalf("failed manifest = %#v", failedManifest)
	}
}

func TestPlanningReviewYieldKeepsProposalPrivateAndPlanIncomplete(t *testing.T) {
	run := t.TempDir()
	evidence, err := NewPlanningEvidenceStore(run).Begin(7, "thread-7")
	if err != nil {
		t.Fatal(err)
	}
	if err := evidence.RecordStarted("turn-origin"); err != nil {
		t.Fatal(err)
	}
	proposal := "Exact proposed plan.\nReview this before implementation."
	identity, err := evidence.RecordYielded(proposal)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(run, "planning-evidence", "generation-0007")
	manifest := readManifest(t, filepath.Join(directory, "manifest.json"))
	if manifest.Stage != "awaiting_resume" || manifest.Outcome != "incomplete" || manifest.Attempts[0].Outcome != "yielded_for_review" {
		t.Fatalf("yielded planning manifest = %#v", manifest)
	}
	if identity.Size != uint64(len(proposal)) || manifest.Attempts[0].ResponseFile != "attempt-0001-proposal.txt" {
		t.Fatalf("proposal identity = %#v, attempt = %#v", identity, manifest.Attempts[0])
	}
	if _, err := os.Stat(filepath.Join(directory, "response.txt")); !os.IsNotExist(err) {
		t.Fatalf("review proposal was published as the final plan: %v", err)
	}
	if err := evidence.RecordStarted("turn-continuation"); err != nil {
		t.Fatal(err)
	}
	final := "Final plan after advisory review."
	if _, err := evidence.Complete("completed", &final); err != nil {
		t.Fatal(err)
	}
}

func TestPlanningEvidenceWriteFailurePreservesManifest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory mode failure is Unix-specific")
	}
	run := t.TempDir()
	evidence, err := NewPlanningEvidenceStore(run).Begin(1, "thread")
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(run, "planning-evidence", "generation-0001")
	manifestPath := filepath.Join(directory, "manifest.json")
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(directory, 0o700)
	if err := evidence.RecordStarted("turn"); err == nil || !strings.Contains(err.Error(), "cannot write planning evidence") {
		t.Fatalf("RecordStarted() error = %v", err)
	}
	after, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed atomic update replaced the allocated manifest")
	}
}

func TestPlanningEvidenceNonDirectoryRootMatchesAllocationConflict(t *testing.T) {
	run := t.TempDir()
	if err := os.WriteFile(filepath.Join(run, "planning-evidence"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPlanningEvidenceStore(run).Begin(3, "thread"); err == nil || !strings.Contains(err.Error(), "already exists for generation 3") {
		t.Fatalf("Begin() error = %v", err)
	}
}

func TestRetryablePlanningFailurePreservesLaterAttempt(t *testing.T) {
	run := t.TempDir()
	evidence, err := NewPlanningEvidenceStore(run).Begin(8, "thread-8")
	if err != nil {
		t.Fatal(err)
	}
	if err := evidence.RecordStarted("turn-abandoned"); err != nil {
		t.Fatal(err)
	}
	if err := evidence.RecordRetryableFailure(); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(run, "planning-evidence", "generation-0008", "manifest.json")
	failed := readManifest(t, manifestPath)
	if failed.Stage != "awaiting_resume" || failed.Outcome != "incomplete" || len(failed.Attempts) != 1 || failed.Attempts[0].Outcome != "failed" {
		t.Fatalf("retryable manifest = %#v", failed)
	}
	if failed.Attempts[0].ResponseFile != "" {
		t.Fatalf("retryable attempt has response file %q", failed.Attempts[0].ResponseFile)
	}
	if err := evidence.RecordStarted("turn-retry"); err != nil {
		t.Fatal(err)
	}
	if _, err := evidence.Complete("completed", stringPointer("Exact successful plan.")); err != nil {
		t.Fatal(err)
	}
	completed := readManifest(t, manifestPath)
	if len(completed.Attempts) != 2 || completed.Attempts[0].Outcome != "failed" || completed.Attempts[1].Outcome != "completed" {
		t.Fatalf("completed manifest = %#v", completed)
	}
}

func readManifest(t *testing.T, path string) planningManifest {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest planningManifest
	if err := json.Unmarshal(contents, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}
