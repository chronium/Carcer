package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"codexos/internal/bootstrap"
)

func TestCrossRunBootstrapCopiesOpaqueArtifactsBeforePublishing(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if e := createPythonCrossRunFixture(source, repositoryRootForCrossRunTest(t)); e != nil {
		t.Fatal(e)
	}
	archive := filepath.Join(source, "generation-0000")
	if e := bootstrap.Provision(source, filepath.Join(root, "artifacts"), "tcc"); e != nil {
		t.Fatal(e)
	}
	cfg, e := bootstrap.LoadConfig(source)
	if e != nil {
		t.Fatal(e)
	}
	storage, e := bootstrap.LockStorage(*cfg)
	if e != nil {
		t.Fatal(e)
	}
	data := []byte("opaque compiler or SDK source")
	id := bootstrap.Digest(data)
	refs := bootstrap.References{Version: 1, RunID: cfg.RunID, Limits: bootstrap.Baseline()}
	snapshot, e := os.ReadFile(filepath.Join(archive, "source.snapshot"))
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC()
	manifest := bootstrap.Manifest{Version: 1, RunID: cfg.RunID, ID: bootstrap.Digest([]byte("fixture job")), Image: bootstrap.Image, ImageID: bootstrap.ImageID, TCCCommit: bootstrap.TCCCommit, Request: bootstrap.Request{Version: 1, Argv: []string{"true"}, Outputs: []string{"tool"}}, SnapshotSHA256: bootstrap.Digest(snapshot), SourceContentBytes: 65536, Limits: bootstrap.Baseline(), Started: now, Finished: now, Result: bootstrap.Result{Status: 0, Cleaned: true, Artifacts: []bootstrap.Artifact{{ID: id, Name: "tool", Size: int64(len(data))}}}}
	if e = storage.Publish(manifest, []bootstrap.Input{{Path: "tool", Data: data}}, &refs); e != nil {
		t.Fatal(e)
	}
	storage.Close()
	if e = bootstrap.Freeze(source, archive, 0); e != nil {
		t.Fatal(e)
	}
	repository := filepath.Join(root, "repository")
	createCrossRunGitRepository(t, repository, "source/generation-0000")
	destination := filepath.Join(root, "destination")
	if _, e = InitializeCrossRunBootstrap(destination, filepath.Join(archive, "successor/codexos.iso"), source, 0, repository, "source/generation-0000"); e != nil {
		t.Fatal(e)
	}
	if _, e = LoadCrossRunBootstrap(destination); e != nil {
		t.Fatal(e)
	}
	c, e := bootstrap.LoadConfig(destination)
	if e != nil || c == nil || c.Enabled || c.RunID == cfg.RunID {
		t.Fatalf("destination config %+v %v", c, e)
	}
	inherited, e := bootstrap.ReadReferences(filepath.Join(c.Storage, c.RunID))
	if e != nil {
		t.Fatal(e)
	}
	dst, e := bootstrap.LockStorage(*c)
	if e != nil {
		t.Fatal(e)
	}
	defer dst.Close()
	got, e := dst.Read(*inherited, id, 0, uint64(len(data)))
	if e != nil || string(got) != string(data) {
		t.Fatalf("copied artifact %q %v", got, e)
	}
	// The copied blob remains independent of source-run storage and permissions.
	if e = os.Remove(filepath.Join(cfg.Storage, cfg.RunID, "jobs", manifest.ID, id)); e != nil {
		t.Fatal(e)
	}
	got, e = dst.Read(*inherited, id, 0, uint64(len(data)))
	if e != nil || string(got) != string(data) {
		t.Fatal("destination depended on source blob")
	}
}
