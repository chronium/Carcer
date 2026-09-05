package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"codexos/internal/guest"
	"codexos/internal/sourcecapacity"
)

// Client always uses the fixed installed worker in production. The unexported
// command is populated only by package-local disposable acceptance fixtures.
type Client struct{ command []string }

func (c *Client) call(ctx context.Context, r wireRequest) (wireResponse, error) {
	argv := []string{"/usr/bin/sudo", "-n", "-u", Account, WorkerExecutable}
	if c != nil && len(c.command) > 0 {
		argv = c.command
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	var diag boundedBuffer
	diag.limit = MaxDiagnostics
	cmd.Stderr = &diag
	in, e := cmd.StdinPipe()
	if e != nil {
		return wireResponse{}, e
	}
	out, e := cmd.StdoutPipe()
	if e != nil {
		in.Close()
		return wireResponse{}, e
	}
	if e = cmd.Start(); e != nil {
		in.Close()
		out.Close()
		return wireResponse{}, e
	}
	defer in.Close()
	defer out.Close()
	sent := make(chan error, 1)
	go func() { sent <- writeWire(in, r) }()
	type received struct {
		v wireResponse
		e error
	}
	got := make(chan received, 1)
	go func() { var v wireResponse; e := readWire(out, &v, maxWireOutput); got <- received{v, e} }()
	stop := context.AfterFunc(ctx, func() { _ = in.Close() })
	defer stop()
	var result received
	select {
	case result = <-got:
	case <-ctx.Done():
		_ = in.Close()
		select {
		case result = <-got:
		case <-time.After(12 * time.Second):
			_ = out.Close()
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			result = <-got
			result.e = errors.New("bootstrap worker did not confirm cleanup after cancellation")
		}
	}
	_ = in.Close()
	writeErr := <-sent
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waited:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		waitErr = <-waited
		result.e = errors.New("bootstrap worker did not retire")
	}
	if result.e != nil || writeErr != nil || waitErr != nil {
		return result.v, fmt.Errorf("bootstrap worker: %w; %s", errors.Join(result.e, writeErr, waitErr), diag.Bytes())
	}
	if len(result.v.Result.Diagnostics) > MaxDiagnostics || result.v.Result.Status > 2 || len(result.v.Outputs) > 32 {
		return wireResponse{}, errors.New("invalid worker response")
	}
	return result.v, nil
}
func Probe(ctx context.Context) error {
	v, e := (*Client)(nil).call(ctx, wireRequest{Kind: "probe"})
	if e != nil {
		return e
	}
	if v.Result.Status != 0 || !v.Result.Cleaned {
		return fmt.Errorf("bootstrap unavailable: %s: %s", v.Result.Reason, v.Result.Diagnostics)
	}
	return nil
}

type Asset struct {
	ID     string
	SHA256 string
	Size   uint64
}
type Service struct {
	mu         sync.Mutex
	run        string
	config     Config
	generation uint64
	budget     sourcecapacity.Budget
	refs       References
	assets     map[string]Asset
	readAsset  func(string, uint64, uint64) ([]byte, error)
	client     *Client
	invocation context.Context
	suspended  bool
	active     context.CancelFunc
	poisoned   error
}

// NewService selects exactly the completed parent's references. Restart at a
// gate never recovers authorization from discarded later generation work.
func NewService(run string, generation uint64, parent *uint64, assets []Asset, readAsset func(string, uint64, uint64) ([]byte, error)) (*Service, error) {
	c, e := LoadConfig(run)
	if e != nil || c == nil {
		return nil, e
	}
	budget, e := sourcecapacity.Load(run)
	if e != nil {
		return nil, e
	}
	refs := References{Version: 1, RunID: c.RunID, Generation: generation, Limits: Baseline()}
	var inherited *References
	if parent != nil {
		inherited, e = ReadReferences(filepath.Join(run, fmt.Sprintf("generation-%04d", *parent)))
	} else if generation == 0 {
		var value References
		e = readJSON(filepath.Join(run, "bootstrap-inherited.json"), &value, MaxManifest)
		if errors.Is(e, os.ErrNotExist) {
			e = nil
		} else if e == nil {
			inherited = &value
		}
	}
	if e != nil {
		return nil, e
	}
	if inherited != nil {
		refs.Jobs = append([]JobRef(nil), inherited.Jobs...)
	}
	s, e := LockStorage(*c)
	if e != nil {
		return nil, e
	}
	defer s.Close()
	if e = s.Validate(&refs); e != nil {
		return nil, e
	}
	if e = atomicJSON(filepath.Join(s.Directory, ReferencesFilename), refs); e != nil {
		return nil, e
	}
	svc := &Service{run: run, config: *c, generation: generation, budget: budget, refs: refs, assets: map[string]Asset{}, readAsset: readAsset}
	for _, a := range assets {
		svc.assets[a.ID] = a
	}
	if c.Enabled {
		a, ok := svc.assets[c.TCCAsset]
		if !ok || a.SHA256 != TCCSHA256 {
			return nil, errors.New("provisioned upstream TCC asset is missing or changed")
		}
	}
	if _, e = os.Stat(filepath.Join(s.Directory, "blocked.json")); e == nil {
		svc.poisoned = errors.New("bootstrap cleanup requires recovery")
	}
	return svc, nil
}
func (s *Service) Activate(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.suspended {
		s.invocation = ctx
	}
}
func (s *Service) Deactivate() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invocation = nil
}

// Suspend is intentionally independent of the runtime operation mutex.
func (s *Service) Suspend() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.suspended = true
	s.invocation = nil
	if s.active != nil {
		s.active()
	}
}
func (s *Service) Resume() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.suspended = false
	s.mu.Unlock()
}
func (s *Service) Healthy() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.poisoned
}
func (s *Service) Recover(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil {
		return errors.New("bootstrap job still active")
	}
	v, e := s.client.call(ctx, wireRequest{Kind: "recover"})
	if e != nil {
		return e
	}
	if !v.Result.Cleaned || v.Result.Status != 0 {
		return errors.New("bootstrap recovery failed: " + v.Result.Diagnostics)
	}
	p := filepath.Join(s.config.Storage, s.config.RunID, "blocked.json")
	if e = os.Remove(p); e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	s.poisoned = nil
	return nil
}
func (s *Service) HandleRequest(ctx context.Context, r guest.HostRequest) (guest.Frame, error) {
	response := func(v Result) (guest.Frame, error) {
		return guest.CreateHostServiceResponse(r.RequestID, v.Status, mustJSON(v))
	}
	if s == nil || !s.config.Enabled {
		return response(Result{Status: 2, Reason: "not_provisioned", Cleaned: true})
	}
	if r.ServiceName == "read_bootstrap_artifact" {
		if len(r.Arguments) != 3 {
			return response(Result{Status: 2, Reason: "invalid_arguments", Cleaned: true})
		}
		offset, e := canonicalUint(r.Arguments[1])
		if e != nil {
			return response(Result{Status: 2, Reason: "invalid_range", Cleaned: true})
		}
		length, e := canonicalUint(r.Arguments[2])
		if e != nil {
			return response(Result{Status: 2, Reason: "invalid_range", Cleaned: true})
		}
		s.mu.Lock()
		refs := s.refs
		refs.Jobs = append([]JobRef(nil), refs.Jobs...)
		s.mu.Unlock()
		data, e := (&Storage{Config: s.config, Directory: filepath.Join(s.config.Storage, s.config.RunID)}).Read(refs, string(r.Arguments[0]), offset, length)
		if e != nil {
			return response(Result{Status: 2, Reason: "artifact_read_rejected", Diagnostics: e.Error(), Cleaned: true})
		}
		return guest.CreateHostServiceResponse(r.RequestID, 0, data)
	}
	if r.ServiceName != "bootstrap_job" || len(r.Arguments) != 2 {
		return response(Result{Status: 2, Reason: "invalid_arguments", Cleaned: true})
	}
	request, snapshot, e := ParseRequest(r.Arguments[0], r.Arguments[1], s.budget)
	if e != nil {
		return response(Result{Status: 2, Reason: "invalid_request", Diagnostics: e.Error(), Cleaned: true})
	}
	s.mu.Lock()
	if s.poisoned != nil || s.suspended || s.active != nil || s.invocation == nil || s.invocation.Err() != nil {
		s.mu.Unlock()
		return response(Result{Status: 2, Reason: "not_in_development_or_busy", Cleaned: true})
	}
	jobCtx, cancel := context.WithCancel(s.invocation)
	s.active = cancel
	s.mu.Unlock()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	defer cancel()
	defer func() { s.mu.Lock(); s.active = nil; s.mu.Unlock() }()
	storage, e := LockStorage(s.config)
	if e != nil {
		return response(Result{Status: 2, Reason: "busy_or_storage_failure", Diagnostics: e.Error(), Cleaned: true})
	}
	defer storage.Close()
	if e = storage.Admit(len(request.Outputs)); e != nil {
		return response(Result{Status: 2, Reason: "quota", Diagnostics: e.Error(), Cleaned: true})
	}
	// Reserve destination publication plus worker input/helper and output staging
	// before reading any selected large immutable input or admitting a process.
	if e = storage.Reserve(MaxOutputs+MaxManifest, MaxInputs+(1<<20)+(16<<20)+2*MaxOutputs+MaxManifest); e != nil {
		return response(Result{Status: 2, Reason: "quota", Diagnostics: e.Error(), Cleaned: true})
	}
	wire := wireRequest{Kind: "job", Request: request, Snapshot: snapshot.Bytes(), Budget: s.budget, TCCAsset: s.config.TCCAsset}
	var inputBytes uint64
	failInput := func(e error) (guest.Frame, error) {
		return response(Result{Status: 2, Reason: "invalid_inputs", Diagnostics: e.Error(), Cleaned: true})
	}
	for _, a := range request.Assets {
		meta, ok := s.assets[a.ID]
		if !ok || meta.SHA256 != a.SHA256 || meta.Size > MaxInputs-inputBytes || s.readAsset == nil {
			return failInput(errors.New("asset identity/size is unavailable"))
		}
		inputBytes += meta.Size
		data := make([]byte, 0, int(meta.Size))
		for offset := uint64(0); offset < meta.Size; {
			n := min(uint64(MaxRead), meta.Size-offset)
			b, e := s.readAsset(a.ID, offset, n)
			if e != nil {
				return failInput(e)
			}
			if uint64(len(b)) != n {
				return failInput(io.ErrUnexpectedEOF)
			}
			data = append(data, b...)
			offset += n
		}
		if Digest(data) != a.SHA256 {
			return failInput(errors.New("captured asset digest mismatch"))
		}
		wire.Inputs = append(wire.Inputs, Input{"assets/" + a.ID, data})
	}
	for _, id := range request.Artifacts {
		data, e := storage.artifact(s.refs, id)
		if e != nil {
			return failInput(e)
		}
		inputBytes += uint64(len(data))
		if inputBytes > MaxInputs {
			return failInput(errors.New("mounted input quota exceeded"))
		}
		wire.Inputs = append(wire.Inputs, Input{"artifacts/" + id, data})
	}
	m := Manifest{Version: 1, RunID: s.config.RunID, ID: randomID(), Generation: s.generation, Image: Image, ImageID: ImageID, TCCCommit: TCCCommit, Request: request, SnapshotSHA256: snapshot.SHA256(), SourceContentBytes: s.budget.Bytes(), Limits: Baseline(), Started: time.Now().UTC()}
	for _, in := range wire.Inputs {
		m.Inputs = append(m.Inputs, AssetRef{in.Path, Digest(in.Data)})
	}
	result, callErr := s.client.call(jobCtx, wire)
	m.Finished = time.Now().UTC()
	m.Result = result.Result
	if callErr != nil {
		m.Result = Result{Status: 2, Reason: "worker_transport_failure", Diagnostics: callErr.Error()}
	}
	if !m.Result.Cleaned {
		s.mu.Lock()
		s.poisoned = errors.New("bootstrap cleanup was not confirmed")
		s.mu.Unlock()
		_ = atomicJSON(filepath.Join(storage.Directory, "blocked.json"), map[string]string{"reason": m.Result.Reason})
	}
	if m.Result.Status == 0 && jobCtx.Err() != nil {
		m.Result.Status = 1
		m.Result.Reason = "cancelled"
		m.Result.Artifacts = nil
	}
	if m.Result.Status == 0 {
		s.mu.Lock()
		e = storage.Publish(m, result.Outputs, &s.refs)
		s.mu.Unlock()
		if e != nil {
			m.Result.Status = 2
			m.Result.Reason = "publication_failed"
			m.Result.Diagnostics = e.Error()
			m.Result.Artifacts = nil
		}
	}
	if m.Result.Status != 0 {
		m.Result.Artifacts = nil
		if e = storage.Failure(m); e != nil {
			m.Result.Diagnostics += "; failure evidence: " + e.Error()
		}
	}
	return response(m.Result)
}
func canonicalUint(b []byte) (uint64, error) {
	s := string(b)
	v, e := strconv.ParseUint(s, 10, 64)
	if e != nil || strconv.FormatUint(v, 10) != s {
		return 0, errors.New("noncanonical unsigned decimal")
	}
	return v, nil
}
