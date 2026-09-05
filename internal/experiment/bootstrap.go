package experiment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"codexos/internal/bootstrap"
)

func (r *CodexOSRun) suspendBootstrap() {
	if r != nil {
		if g := r.liveGeneration(); g != nil {
			g.bootstrap.Suspend()
		}
	}
}

// requireBootstrapGate is called with gateMu and (when present) operationMu.
func (r *CodexOSRun) requireBootstrapGate() error {
	if r.state != RuntimeStateAwaitingNextGeneration || r.generationNumber == nil || r.retainedFinish != nil || r.transitioning {
		return errors.New("bootstrap provisioning/GC requires a validated inactive generation gate without a retained interview")
	}
	if r.live != nil && (r.live.closed || r.liveGeneration() != nil) {
		return errors.New("bootstrap requires an inactive generation gate")
	}
	partial, e := partialGenerationState(r.runDirectory)
	if e != nil {
		return e
	}
	if len(partial) != 0 {
		return errors.New("run contains partial generation state")
	}
	archives, e := LoadArchivedGenerations(r.runDirectory)
	if e != nil {
		return e
	}
	if e = ValidateArchivedHistory(archives); e != nil {
		return e
	}
	if len(archives) == 0 || archives[len(archives)-1].Generation != *r.generationNumber {
		return errors.New("bootstrap gate does not match archive history")
	}
	return nil
}
func (r *CodexOSRun) ProvisionBootstrap(ctx context.Context, asset string) error {
	if r == nil || r.live == nil {
		return errors.New("bootstrap provisioning requires the configured Go live owner at an inactive gate")
	}
	r.live.operationMu.Lock()
	defer r.live.operationMu.Unlock()
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	if e := r.requireBootstrapGate(); e != nil {
		return e
	}
	found := false
	if r.live.provided != nil {
		for _, a := range r.live.provided.Metadata() {
			if a.ID == asset && a.SHA256 == bootstrap.TCCSHA256 {
				found = true
			}
		}
	}
	if !found {
		return errors.New("provided assets do not contain the pinned upstream TCC archive under that ID")
	}
	if e := bootstrap.Probe(ctx); e != nil {
		return e
	}
	if e := bootstrap.Provision(r.runDirectory, bootstrap.StorageDirectory, asset); e != nil {
		return e
	}
	r.recordLive("bootstrap_service_provisioned", r.generationNumber, map[string]any{"image": bootstrap.Image, "tcc_commit": bootstrap.TCCCommit, "tcc_sha256": bootstrap.TCCSHA256, "tcc_asset": asset, "limits": bootstrap.Baseline()})
	return nil
}
func (r *CodexOSRun) BootstrapStatus() (string, error) {
	if r == nil {
		return "", errors.New("run is nil")
	}
	c, e := bootstrap.LoadConfig(r.runDirectory)
	if e != nil {
		return "", e
	}
	if c == nil {
		return "Bootstrap service: not provisioned.", nil
	}
	refs, e := bootstrap.ReadReferences(filepath.Join(c.Storage, c.RunID))
	if e != nil {
		return "", e
	}
	jobs := 0
	if refs != nil {
		jobs = len(refs.Jobs)
	}
	b, _ := json.Marshal(bootstrap.Baseline())
	return fmt.Sprintf("Bootstrap configured=%t; TCC asset=%s; image=%s; authorized jobs=%d; limits=%s. Runtime availability is verified at provisioning and job admission. This does not fulfill feature request #3.", c.Enabled, c.TCCAsset, bootstrap.Image, jobs, b), nil
}
func (r *CodexOSRun) RecoverBootstrap(ctx context.Context) error {
	if r == nil {
		return errors.New("run is nil")
	}
	r.suspendBootstrap()
	if r.live != nil {
		r.live.operationMu.Lock()
		defer r.live.operationMu.Unlock()
	}
	if g := r.liveGeneration(); g != nil {
		return g.bootstrap.Recover(ctx)
	}
	return bootstrap.RecoverRun(ctx, r.runDirectory)
}
func (r *CodexOSRun) GarbageCollectBootstrap() error {
	if r == nil {
		return errors.New("run is nil")
	}
	if r.live != nil {
		r.live.operationMu.Lock()
		defer r.live.operationMu.Unlock()
	}
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	if e := r.requireBootstrapGate(); e != nil {
		return e
	}
	n, e := bootstrap.GarbageCollect(r.runDirectory)
	if e == nil {
		r.recordLive("bootstrap_artifacts_reclaimed", r.generationNumber, map[string]any{"jobs": n})
	}
	return e
}
