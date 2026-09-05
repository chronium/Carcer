package bootstrap

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codexos/internal/guest"
)

func fixtureSnapshot(t *testing.T) []byte {
	t.Helper()
	s, e := guest.NewSourceSnapshot([]guest.SnapshotFile{{Path: "seed/bootstrap.sh", Content: []byte("echo hello")}})
	if e != nil {
		t.Fatal(e)
	}
	return s.Bytes()
}
func fixtureRequest() Request {
	return Request{Version: 1, Argv: []string{"/bin/sh", "/inputs/source/seed/bootstrap.sh"}, Outputs: []string{"tool"}}
}
func TestRequestRejectsUntrustedShape(t *testing.T) {
	good := mustJSON(fixtureRequest())
	if _, _, e := ParseRequest(good, fixtureSnapshot(t), 0); e != nil {
		t.Fatal(e)
	}
	for _, b := range [][]byte{
		[]byte(`{"version":1,"version":1,"argv":["true"]}`), []byte(`{"version":1,"argv":["true"],"image":"other"}`),
		[]byte(`{"version":1,"argv":["true"],"assets":[{"id":"a","id":"b","sha256":"x"}]}`),
		[]byte(`{"version":1,"argv":["true"],"outputs":["../escape"]}`), []byte(`{"version":1,"argv":["true"],"outputs":["a//b"]}`),
		[]byte(`{"version":1,"argv":["true"],"outputs":["/etc/passwd"]}`), []byte(`{"version":1,"argv":["true"],"outputs":["a","a"]}`),
		append(good, []byte(` {}`)...), bytes.Repeat([]byte(" "), MaxRequest+1),
	} {
		if _, _, e := ParseRequest(b, fixtureSnapshot(t), 0); e == nil {
			t.Fatalf("accepted %s", b)
		}
	}
	r := fixtureRequest()
	r.Argv = []string{"sh", strings.Repeat("x", 1025)}
	if e := r.Validate(); e == nil {
		t.Fatal("oversized argv accepted")
	}
	if _, _, e := ParseRequest(good, []byte("not a snapshot"), 0); e == nil {
		t.Fatal("malformed snapshot accepted")
	}
}
func fixtureStore(t *testing.T) (string, *Storage, References) {
	t.Helper()
	run := t.TempDir()
	root := filepath.Join(t.TempDir(), "store")
	if e := Provision(run, root, "tcc"); e != nil {
		t.Fatal(e)
	}
	c, e := LoadConfig(run)
	if e != nil {
		t.Fatal(e)
	}
	s, e := LockStorage(*c)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return run, s, NewReferences(*c, 0)
}
func publishFixture(t *testing.T, s *Storage, r *References, data string) Manifest {
	t.Helper()
	now := time.Now().UTC()
	m := Manifest{Version: 1, RunID: s.Config.RunID, ID: randomID(), Generation: r.Generation, Image: Image, ImageID: ImageID, TCCCommit: TCCCommit, Request: fixtureRequest(), SnapshotSHA256: Digest(fixtureSnapshot(t)), SourceContentBytes: 65536, Limits: Baseline(), Started: now, Finished: now, Result: Result{Status: 0, Reason: "completed", Cleaned: true, Artifacts: []Artifact{{ID: Digest([]byte(data)), Name: "tool", Size: int64(len(data))}}}}
	if e := s.Publish(context.Background(), m, []Input{{"tool", []byte(data)}}, r); e != nil {
		t.Fatal(e)
	}
	return m
}
func TestImmutablePublicationRangesAndLineage(t *testing.T) {
	run, s, refs := fixtureStore(t)
	first := publishFixture(t, s, &refs, "compiler")
	old := refs
	old.Jobs = append([]JobRef(nil), refs.Jobs...)
	if e := Freeze(run, t.TempDir(), 0); e != nil {
		t.Fatal(e)
	}
	second := publishFixture(t, s, &refs, "later output")
	if _, e := s.Read(old, second.Result.Artifacts[0].ID, 0, 1); e == nil {
		t.Fatal("rollback saw discarded later artifact")
	}
	got, e := s.Read(refs, first.Result.Artifacts[0].ID, 2, 3)
	if e != nil || string(got) != "mpi" {
		t.Fatalf("range %q %v", got, e)
	}
	for _, r := range [][2]uint64{{0, MaxRead + 1}, {8, 1}, {^uint64(0), 1}} {
		if _, e := s.Read(refs, first.Result.Artifacts[0].ID, r[0], r[1]); e == nil {
			t.Fatal("bad range accepted")
		}
	}
	if _, e := s.Read(refs, Digest([]byte("unknown")), 0, 0); e == nil {
		t.Fatal("unknown artifact authorized")
	}
	if e := s.Validate(&refs); e != nil {
		t.Fatal(e)
	}
	c := s.Config
	s.Close()
	reopened, e := LockStorage(c)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	saved, e := ReadReferences(reopened.Directory)
	if e != nil || len(saved.Jobs) != 2 {
		t.Fatalf("restart %v %v", saved, e)
	}
	if e = reopened.Validate(saved); e != nil {
		t.Fatal(e)
	}
	artifactPath := filepath.Join(reopened.Directory, "jobs", first.ID, first.Result.Artifacts[0].ID)
	if e = os.Chmod(artifactPath, 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(artifactPath, []byte("tampered"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = reopened.Validate(saved); e == nil {
		t.Fatal("corrupt immutable artifact accepted")
	}
}
func TestInterruptedPublicationAndQuota(t *testing.T) {
	_, s, refs := fixtureStore(t)
	publishFixture(t, s, &refs, "kept")
	dir := s.Directory
	c := s.Config
	stage := filepath.Join(dir, "jobs", ".stage-interrupted")
	if e := os.Mkdir(stage, 0700); e != nil {
		t.Fatal(e)
	}
	_ = os.WriteFile(filepath.Join(stage, "partial"), []byte("partial"), 0600)
	if e := s.Reserve(MaxRunBytes, MaxRunBytes); e == nil {
		t.Fatal("run quota allowed")
	}
	if e := s.Reserve(0, MaxGlobalBytes); e == nil {
		t.Fatal("global reservation allowed")
	}
	// Published data without the synced authorization index stays unreachable.
	orphan := publishFixture(t, s, &refs, "orphan")
	prior := refs
	prior.Jobs = prior.Jobs[:1]
	if e := atomicJSON(filepath.Join(dir, ReferencesFilename), prior); e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e := LockStorage(c)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e := os.Stat(stage); !os.IsNotExist(e) {
		t.Fatalf("partial staging survived: %v", e)
	}
	if _, e := s.Read(prior, orphan.Result.Artifacts[0].ID, 0, 1); e == nil {
		t.Fatal("orphan published by recovery")
	}
}

func TestRecoveryRemovesInterruptedStoreInitialization(t *testing.T) {
	_, s, refs := fixtureStore(t)
	m := publishFixture(t, s, &refs, "committed compiler")
	c := s.Config
	init := filepath.Join(c.Storage, ".init-"+randomID())
	if e := os.Mkdir(init, 0700); e != nil {
		t.Fatal(e)
	}
	// Simulate interruption before even owner.json was durably published.
	if e := os.WriteFile(filepath.Join(init, ".write-partial"), []byte("{"), 0600); e != nil {
		t.Fatal(e)
	}
	uncommitted := Config{1, false, c.Storage, randomID(), "tcc"}
	orphan := &Storage{Config: uncommitted, Directory: filepath.Join(c.Storage, uncommitted.RunID)}
	if e := initializeStorage(orphan, filepath.Join(t.TempDir(), "unpublished-run")); e != nil {
		t.Fatal(e)
	}
	partialIndex := filepath.Join(s.Directory, ".write-partial")
	if e := os.WriteFile(partialIndex, []byte("{"), 0600); e != nil {
		t.Fatal(e)
	}
	s.Close()
	reopened, e := LockStorage(c)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	for _, p := range []string{init, orphan.Directory, partialIndex} {
		if _, e = os.Lstat(p); !os.IsNotExist(e) {
			t.Fatalf("unpublished storage survived: %s: %v", p, e)
		}
	}
	if b, e := reopened.Read(refs, m.Result.Artifacts[0].ID, 0, 18); e != nil || string(b) != "committed compiler" {
		t.Fatalf("committed storage changed: %q %v", b, e)
	}
}
func TestCrossRunCopiesSelectedReferencesAtomically(t *testing.T) {
	run, s, refs := fixtureStore(t)
	m := publishFixture(t, s, &refs, "source and binary bytes are both opaque")
	archive := t.TempDir()
	if e := Freeze(run, archive, 0); e != nil {
		t.Fatal(e)
	}
	s.Close()
	parent := t.TempDir()
	candidate := filepath.Join(parent, "staging")
	destination := filepath.Join(parent, "destination")
	_ = os.Mkdir(candidate, 0700)
	finish, e := Inherit(run, archive, candidate, destination)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(destination); !os.IsNotExist(e) {
		t.Fatal("destination appeared before publication")
	}
	if e = os.Rename(candidate, destination); e != nil {
		t.Fatal(e)
	}
	if e = finish(true); e != nil {
		t.Fatal(e)
	}
	c, e := LoadConfig(destination)
	if e != nil || c.Enabled || c.RunID == s.Config.RunID {
		t.Fatalf("inherit config %+v %v", c, e)
	}
	dest, e := LockStorage(*c)
	if e != nil {
		t.Fatal(e)
	}
	defer dest.Close()
	inherited, e := ReadReferences(dest.Directory)
	if e != nil {
		t.Fatal(e)
	}
	got, e := dest.Read(*inherited, m.Result.Artifacts[0].ID, 0, uint64(len("source and binary bytes are both opaque")))
	if e != nil || string(got) != "source and binary bytes are both opaque" {
		t.Fatalf("inherit read %q %v", got, e)
	}
	if e = dest.Validate(inherited); e != nil {
		t.Fatal(e)
	}
}
func TestFailedInheritanceLeavesNoDestinationStore(t *testing.T) {
	run, s, refs := fixtureStore(t)
	publishFixture(t, s, &refs, "compiler")
	archive := t.TempDir()
	if e := Freeze(run, archive, 0); e != nil {
		t.Fatal(e)
	}
	s.Close()
	candidate := t.TempDir()
	destination := filepath.Join(t.TempDir(), "absent")
	finish, e := Inherit(run, archive, candidate, destination)
	if e != nil {
		t.Fatal(e)
	}
	c, e := LoadConfig(candidate)
	if e != nil {
		t.Fatal(e)
	}
	if e = finish(false); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(c.Storage, c.RunID)); !os.IsNotExist(e) {
		t.Fatal("failed inheritance retained a destination store")
	}
}

// This boundary uses at most 32 MiB of input at once and is deliberately
// serialized. A source store close to its quota can be valid while destination
// metadata/reservations make inheritance too large; nothing may be published.
func TestExpandedArtifactInheritanceChecksDestinationQuota(t *testing.T) {
	run, s, refs := fixtureStore(t)
	for job := 0; job < 4; job++ {
		size := MaxOutputs
		if job == 3 {
			used, e := directoryBytes(s.Directory)
			if e != nil {
				t.Fatal(e)
			}
			size = MaxRunBytes - int(used) - 6000
		}
		a := bytes.Repeat([]byte{byte(job * 2)}, MaxOutput)
		b := bytes.Repeat([]byte{byte(job*2 + 1)}, size-MaxOutput)
		now := time.Now().UTC()
		req := fixtureRequest()
		req.Outputs = []string{"a", "b"}
		m := Manifest{Version: 1, RunID: s.Config.RunID, ID: randomID(), Generation: 0, Image: Image, ImageID: ImageID, TCCCommit: TCCCommit, Request: req, SnapshotSHA256: Digest(fixtureSnapshot(t)), SourceContentBytes: 65536, Limits: Baseline(), Started: now, Finished: now, Result: Result{Status: 0, Cleaned: true, Artifacts: []Artifact{{Digest(a), "a", int64(len(a))}, {Digest(b), "b", int64(len(b))}}}}
		if e := s.Publish(context.Background(), m, []Input{{"a", a}, {"b", b}}, &refs); e != nil {
			t.Fatal(e)
		}
	}
	archive := t.TempDir()
	if e := Freeze(run, archive, 0); e != nil {
		t.Fatal(e)
	}
	s.Close()
	candidate := t.TempDir()
	destination := filepath.Join(t.TempDir(), "not-published")
	finish, e := Inherit(run, archive, candidate, destination)
	if e == nil {
		finish(false)
		t.Fatal("inheritance exceeded destination reservation")
	}
	if !strings.Contains(e.Error(), "destination/run artifact quota") {
		t.Fatalf("unclear quota rejection: %v", e)
	}
	if c, e := LoadConfig(candidate); e != nil || c != nil {
		t.Fatalf("partial destination config %+v %v", c, e)
	}
	if _, e := os.Stat(destination); !os.IsNotExist(e) {
		t.Fatal("partial destination published")
	}
}

func TestGateGCAndCancelledPublication(t *testing.T) {
	run, s, refs := fixtureStore(t)
	kept := publishFixture(t, s, &refs, "kept compiler")
	archive := filepath.Join(run, "generation-0000")
	if e := os.Mkdir(archive, 0700); e != nil {
		t.Fatal(e)
	}
	if e := Freeze(run, archive, 0); e != nil {
		t.Fatal(e)
	}
	orphan := publishFixture(t, s, &refs, "unreferenced output")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if e := s.Publish(cancelled, Manifest{}, nil, &refs); e != context.Canceled {
		t.Fatalf("cancelled publication: %v", e)
	}
	s.Close()
	n, e := GarbageCollect(run)
	if e != nil || n != 1 {
		t.Fatalf("GC %d %v", n, e)
	}
	c, e := LoadConfig(run)
	if e != nil {
		t.Fatal(e)
	}
	storage, e := LockStorage(*c)
	if e != nil {
		t.Fatal(e)
	}
	defer storage.Close()
	current, e := ReadReferences(storage.Directory)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = storage.Read(*current, kept.Result.Artifacts[0].ID, 0, 1); e != nil {
		t.Fatal(e)
	}
	if _, e = storage.Read(*current, orphan.Result.Artifacts[0].ID, 0, 1); e == nil {
		t.Fatal("GC retained orphan authorization")
	}
	if e = ValidateArchive(run, archive); e != nil {
		t.Fatal("GC damaged immutable archive", e)
	}
}
