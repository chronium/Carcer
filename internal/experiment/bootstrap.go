package experiment

import (
	"codexos/internal/provenance"
	"codexos/internal/store"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	if e := r.live.options.BootstrapClient.Probe(ctx); e != nil {
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
	availability := "Bootstrap execution is disabled; new jobs are not permitted. Retained references do not enable execution."
	if c.Enabled {
		availability = "Bootstrap execution is enabled and permits new jobs under existing service limits without a separate batch grant, including when no previous jobs exist. Retained job references count previous jobs, not execution permissions or job credits."
	}
	return fmt.Sprintf("%s TCC asset=%s; image=%s; retained job references=%d; limits=%s. Runtime availability is verified at provisioning and job admission. This does not fulfill feature request #3.", availability, c.TCCAsset, bootstrap.Image, jobs, b), nil
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
		if e := g.bootstrap.Recover(ctx); e != nil {
			return e
		}
		if r.State() == RuntimeStateRunning {
			g.bootstrap.Resume()
		}
		return nil
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

// ProvisionInitialBootstrap is the explicit pre-boot exception to the archived
// gate rule. It is restricted to a validated, unstarted inherited destination.
func (r *CodexOSRun) ProvisionInitialBootstrap(ctx context.Context, asset, initialISO string) error {
	if r == nil || r.live == nil {
		return errors.New("initial bootstrap provisioning requires a configured live owner")
	}
	r.live.operationMu.Lock()
	defer r.live.operationMu.Unlock()
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	if r.state != RuntimeStateStopped || r.generationNumber != nil || r.transitioning || r.retainedFinish != nil || r.live.closed || r.live.started || r.liveGeneration() != nil {
		return errors.New("initial bootstrap provisioning requires an unstarted inherited destination")
	}
	inherited, e := store.LoadCrossRunBootstrap(r.runDirectory)
	if e != nil {
		return e
	}
	if inherited == nil {
		return errors.New("initial bootstrap provisioning requires cross-run provenance")
	}
	if e = inherited.VerifyInitialISO(initialISO); e != nil {
		return e
	}
	archives, e := LoadArchivedGenerations(r.runDirectory)
	if e != nil {
		return e
	}
	partial, e := partialGenerationState(r.runDirectory)
	if e != nil {
		return e
	}
	if len(archives) != 0 || len(partial) != 0 {
		return errors.New("initial bootstrap destination already has generation state")
	}
	if _, e = os.Lstat(provenance.HarnessGenerationRecordPath(r.runDirectory, 0)); !errors.Is(e, os.ErrNotExist) {
		return errors.New("initial bootstrap destination already has a generation start record")
	}
	if e = r.configureLiveAssets(0); e != nil {
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
	if e = bootstrap.ProvisionInherited(ctx, r.runDirectory, asset, r.live.options.BootstrapClient); e != nil {
		return e
	}
	r.recordLive("bootstrap_service_provisioned", nil, map[string]any{"initial_destination": true, "image": bootstrap.Image, "tcc_commit": bootstrap.TCCCommit, "tcc_sha256": bootstrap.TCCSHA256, "tcc_asset": asset, "limits": bootstrap.Baseline()})
	return nil
}
