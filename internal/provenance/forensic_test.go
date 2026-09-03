package provenance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBuildForensicEvidencePreservesExactSuccessIdentity(t *testing.T) {
	run := t.TempDir()
	snapshot := []byte("snapshot\x00bytes\xff λ")
	evidence, err := NewBuildReviewProvenance(run).BeginBuild(12, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.AttemptID() != "build-000001" || evidence.Generation() != 12 {
		t.Fatalf("identity = %s/%d", evidence.AttemptID(), evidence.Generation())
	}
	if _, err := evidence.SourceIdentity(); err != nil {
		t.Fatal(err)
	}
	if err := evidence.RecordDecoded(2, 37); err != nil {
		t.Fatal(err)
	}
	kernel := FileIdentity{SHA256: strings.Repeat("a", 64), Size: 123}
	iso := FileIdentity{SHA256: strings.Repeat("b", 64), Size: 456}
	if err := evidence.RecordArtifacts(kernel, iso); err != nil {
		t.Fatal(err)
	}
	if err := evidence.RecordCandidateStage(
		"build_protocol_validation_completed",
		"protocol_validated",
		map[string]any{"outcome": "success", "protocol_validated": true},
	); err != nil {
		t.Fatal(err)
	}
	if err := evidence.RecordLatestSuccess(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := evidence.RecordLatestSuccessUpdate(); err != nil {
		t.Fatal(err)
	}

	directory := filepath.Join(run, "build-review-provenance", "generation-0012", "build-000001")
	if got, err := os.ReadFile(filepath.Join(directory, "source.snapshot")); err != nil || !bytes.Equal(got, snapshot) {
		t.Fatalf("source snapshot = %q, %v", got, err)
	}
	manifest := readForensicManifest(t, filepath.Join(directory, "manifest.json"))
	if manifest["outcome"] != "success" || manifest["stage"] != "latest_success" {
		t.Fatalf("manifest completion = %#v", manifest)
	}
	source, ok := manifest["source_snapshot"].(map[string]any)
	if !ok || source["decoded"] != true || source["file_count"] != float64(2) || source["content_size"] != float64(37) {
		t.Fatalf("source evidence = %#v", manifest["source_snapshot"])
	}
	latest, ok := manifest["latest_success"].(map[string]any)
	if !ok || latest["ready"] != true || latest["protocol_validated"] != true {
		t.Fatalf("latest success = %#v", manifest["latest_success"])
	}
	archive, err := evidence.AbortedArchiveManifest()
	if err != nil {
		t.Fatal(err)
	}
	if archive["build_attempt_id"] != "build-000001" || archive["generation"] != uint64(12) {
		t.Fatalf("archive identity = %#v", archive)
	}
	if _, ok := archive["kernel_bytes"]; ok {
		t.Fatal("archive copied kernel bytes")
	}
	if _, err := NewBuildReviewProvenance(run).BeginBuild(12, snapshot); err != nil {
		t.Fatal(err)
	} else if got := readForensicManifest(t, filepath.Join(run, "build-review-provenance", "generation-0012", "build-000002", "manifest.json")); got["attempt_id"] != "build-000002" {
		t.Fatalf("second attempt = %#v", got["attempt_id"])
	}
}

func TestBuildForensicFailuresRemainIncompleteAndReserveIDs(t *testing.T) {
	run := t.TempDir()
	evidence, err := NewBuildReviewProvenance(run).BeginBuild(4, []byte("source"))
	if err != nil {
		t.Fatal(err)
	}
	if err := evidence.RecordCompileFailure("build_failure"); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(run, "build-review-provenance", "generation-0004", "build-000001")
	manifest := readForensicManifest(t, filepath.Join(directory, "manifest.json"))
	if manifest["outcome"] != "build_failure" || manifest["stage"] != "completed" {
		t.Fatalf("failed manifest = %#v", manifest)
	}
	if _, ok := manifest["latest_success"]; ok {
		t.Fatal("failed build claimed latest success")
	}
	if _, err := os.Stat(filepath.Join(directory, "source.snapshot")); !os.IsNotExist(err) {
		t.Fatalf("failed build retained source snapshot: %v", err)
	}

	generation := filepath.Dir(directory)
	future := filepath.Join(generation, "build-000003")
	if err := os.Mkdir(future, 0o777); err != nil {
		t.Fatal(err)
	}
	if next, err := NewBuildReviewProvenance(run).BeginBuild(4, []byte("source")); err != nil {
		t.Fatal(err)
	} else if next.AttemptID() != "build-000004" {
		t.Fatalf("reserved ID = %s", next.AttemptID())
	}

	malformed := filepath.Join(generation, "build-000005")
	if err := os.Mkdir(malformed, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(malformed, "manifest.json"), []byte(`{"schema_version":2,"kind":"build_attempt","generation":4,"attempt_id":"build-000005"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewBuildReviewProvenance(run).BeginBuild(4, []byte("source")); err == nil || !strings.Contains(err.Error(), "unsupported or inconsistent") {
		t.Fatalf("future schema error = %v", err)
	}
}

func TestReviewForensicEvidenceSeparatesOutcomeAndCapture(t *testing.T) {
	run := t.TempDir()
	evidence, err := NewBuildReviewProvenance(run).BeginReview(9)
	if err != nil {
		t.Fatal(err)
	}
	output := []byte("source\x00bytes\xff")
	if err := evidence.RecordSourceRead("seed/tasks.c", 7, 14, 0, output); err != nil {
		t.Fatal(err)
	}
	if err := evidence.Complete("completed"); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(run, "build-review-provenance", "generation-0009", "review-000001")
	manifest := readForensicManifest(t, filepath.Join(directory, "manifest.json"))
	if manifest["review_outcome"] != "completed" || manifest["capture_outcome"] != "complete" || manifest["evidence_complete"] != true {
		t.Fatalf("review completion = %#v", manifest)
	}
	reads, ok := manifest["source_reads"].([]any)
	if !ok || len(reads) != 1 {
		t.Fatalf("source reads = %#v", manifest["source_reads"])
	}
	read, ok := reads[0].(map[string]any)
	if !ok || read["path"] != "seed/tasks.c" || read["offset"] != float64(7) || read["length"] != float64(14) || read["status"] != float64(0) {
		t.Fatalf("source read = %#v", reads[0])
	}
	if read["returned_bytes"] != float64(len(output)) || read["sha256"] != hashBytes(output) {
		t.Fatalf("source identity = %#v", read)
	}
	if got, err := os.ReadFile(filepath.Join(directory, "read-000001.bin")); err != nil || !bytes.Equal(got, output) {
		t.Fatalf("source bytes = %q, %v", got, err)
	}

	if err := os.Remove(filepath.Join(directory, "read-000001.bin")); err != nil {
		t.Fatal(err)
	}
	if err := evidence.Complete("completed"); err != nil {
		t.Fatal(err)
	}
	manifest = readForensicManifest(t, filepath.Join(directory, "manifest.json"))
	if manifest["review_outcome"] != "completed" || manifest["capture_outcome"] != "incomplete" || manifest["evidence_complete"] != false {
		t.Fatalf("corrupt review completion = %#v", manifest)
	}
}

func TestReviewYieldEvidencePreservesPrivateContentAndExactlyOneContinuation(t *testing.T) {
	var events []map[string]any
	run := t.TempDir()
	store := NewBuildReviewProvenance(run, func(_ string, _ uint64, data map[string]any) {
		events = append(events, data)
	})
	evidence, err := store.BeginReview(3)
	if err != nil {
		t.Fatal(err)
	}
	request := "check the locking boundary"
	proposal := "1. Quiesce the turn.\n2. Resume exactly once."
	if err := evidence.RecordYieldRequested(ReviewYieldOrigin{
		RequestID: 17, CallID: "call-17", ThreadID: "thread-3", TurnID: "turn-4",
		Phase: "planning", Focus: "correctness", Request: &request, Proposal: &proposal,
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := []byte("stable snapshot\x00bytes")
	if err := evidence.RecordAwaitingReview(snapshot, "interrupted"); err != nil {
		t.Fatal(err)
	}
	if err := evidence.Complete("completed"); err != nil {
		t.Fatal(err)
	}
	findings := "Blocking: preserve the originating identity."
	if err := evidence.RecordReviewResult("completed", findings); err != nil {
		t.Fatal(err)
	}
	if err := evidence.RecordContinuationStarted("turn-5"); err != nil {
		t.Fatal(err)
	}
	if err := evidence.RecordContinuationStarted("turn-6"); err == nil || !strings.Contains(err.Error(), "current state") {
		t.Fatalf("duplicate continuation error = %v", err)
	}
	if err := evidence.RecordContinuationFinished("completed"); err != nil {
		t.Fatal(err)
	}

	directory := filepath.Join(run, "build-review-provenance", "generation-0003", "review-000001")
	for name, want := range map[string][]byte{
		"request.txt": []byte(request), "proposal.txt": []byte(proposal),
		"source.snapshot": snapshot, "result.txt": []byte(findings),
	} {
		got, readErr := os.ReadFile(filepath.Join(directory, name))
		if readErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s = %q, %v", name, got, readErr)
		}
	}
	manifest := readForensicManifest(t, filepath.Join(directory, "manifest.json"))
	if manifest["stage"] != "completed" || manifest["origin_status"] != "interrupted" || manifest["continuation_turn_id"] != "turn-5" || manifest["continuation_status"] != "completed" {
		t.Fatalf("yield completion = %#v", manifest)
	}
	yield, ok := manifest["yield"].(map[string]any)
	if !ok || yield["call_id"] != "call-17" || yield["phase"] != "planning" {
		t.Fatalf("yield identity = %#v", manifest["yield"])
	}
	for _, event := range events {
		encoded, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		for _, private := range []string{request, proposal, findings, string(snapshot)} {
			if bytes.Contains(encoded, []byte(private)) {
				t.Fatalf("operational event leaked private content: %s", encoded)
			}
		}
	}
}

func TestReviewForensicContentWriteFailureLeavesSafeIncompleteMarker(t *testing.T) {
	run := t.TempDir()
	evidence, err := NewBuildReviewProvenance(run).BeginReview(10)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(run, "build-review-provenance", "generation-0010", "review-000001")
	if err := os.Mkdir(filepath.Join(directory, "read-000001.bin"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := evidence.RecordSourceRead("seed/tasks.c", 0, 5, 0, []byte("exact")); err == nil || !strings.Contains(err.Error(), "cannot write forensic provenance") {
		t.Fatalf("content write error = %v", err)
	}
	if err := evidence.Complete("completed"); err != nil {
		t.Fatal(err)
	}
	manifest := readForensicManifest(t, filepath.Join(directory, "manifest.json"))
	if manifest["review_outcome"] != "completed" || manifest["capture_outcome"] != "incomplete" || manifest["evidence_complete"] != false {
		t.Fatalf("failed capture manifest = %#v", manifest)
	}
	reads, ok := manifest["source_reads"].([]any)
	if !ok || len(reads) != 0 {
		t.Fatalf("failed capture source reads = %#v", manifest["source_reads"])
	}
}

func TestForensicEventRecorderNeverReceivesContentBytes(t *testing.T) {
	var events []map[string]any
	recorder := ForensicEventRecorder(func(_ string, _ uint64, data map[string]any) {
		events = append(events, data)
	})
	run := t.TempDir()
	evidence, err := NewBuildReviewProvenance(run, recorder).BeginBuild(2, []byte("private snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	if err := evidence.RecordDecoded(0, 0); err != nil {
		t.Fatal(err)
	}
	review, err := NewBuildReviewProvenance(run, recorder).BeginReview(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := review.RecordSourceRead("seed/private.c", 0, 7, 0, []byte("private")); err != nil {
		t.Fatal(err)
	}
	for _, data := range events {
		if _, ok := data["source_snapshot"]; ok {
			t.Fatal("event exposed source snapshot bytes")
		}
		if _, ok := data["returned_bytes_content"]; ok {
			t.Fatal("event exposed returned bytes")
		}
	}
}

func TestFileIdentityFromPathHashesExactBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact")
	contents := []byte("artifact\x00\xff")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := FileIdentityFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	if identity.SHA256 != hex.EncodeToString(digest[:]) || identity.Size != uint64(len(contents)) {
		t.Fatalf("identity = %#v", identity)
	}
	if _, err := FileIdentityFromPath(filepath.Join(filepath.Dir(path), "missing")); err == nil {
		t.Fatal("missing artifact unexpectedly hashed")
	}
}

func TestForensicGenerationSymlinkIsRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks are not uniformly available on Windows")
	}
	run := t.TempDir()
	root := filepath.Join(run, "build-review-provenance")
	if err := os.MkdirAll(root, 0o777); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(root, "generation-0001")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := NewBuildReviewProvenance(run).BeginBuild(1, []byte("source")); err == nil || !strings.Contains(err.Error(), "generation provenance directory is unsafe") {
		t.Fatalf("symlink error = %v", err)
	}
}

func readForensicManifest(t *testing.T, path string) map[string]any {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(contents, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestForensicProvenanceErrorUnwrapsCause(t *testing.T) {
	cause := errors.New("cause")
	err := &ForensicProvenanceError{Reason: "reason", Err: cause}
	if !errors.Is(err, cause) || err.Error() != "reason: cause" {
		t.Fatalf("error = %v", err)
	}
}

func TestBuildAndReviewEvidenceCarryFixedHarnessIdentity(t *testing.T) {
	identity := testHarnessIdentity()
	store := NewBuildReviewProvenanceWithHarnessIdentity(t.TempDir(), &identity)
	build, err := store.BeginBuild(8, []byte("snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	review, err := store.BeginReview(8)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(build.directory, "manifest.json"), filepath.Join(review.directory, "manifest.json")} {
		manifest := readForensicManifest(t, path)
		encoded, err := json.Marshal(manifest["harness_identity"])
		if err != nil {
			t.Fatal(err)
		}
		actual, err := ParseHarnessIdentity(encoded)
		if err != nil || !actual.Equal(identity) {
			t.Fatalf("%s harness identity = %#v, %v", path, actual, err)
		}
	}
}
