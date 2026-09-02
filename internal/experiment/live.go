package experiment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"codexos/internal/build"
	"codexos/internal/guest"
	"codexos/internal/observability"
	"codexos/internal/provenance"
	"codexos/internal/qemu"
	"codexos/internal/store"
)

const (
	defaultGenerationReadyTimeout = 10 * time.Second
	defaultGenerationStopTimeout  = 5 * time.Second
)

// LiveRunOptions contains trusted harness inputs for concrete generation
// execution. No field is populated from guest state.
type LiveRunOptions struct {
	QEMUExecutable          string
	HardwareProfile         qemu.HardwareProfile
	BuildConfig             build.Config
	ReadyTimeout            time.Duration
	CandidateReadyTimeout   time.Duration
	ProvidedAssetsDirectory *string
	EventLog                *observability.EventLog
	Metrics                 *observability.Metrics
	ActivityStream          *observability.ActivityStream
}

type liveRun struct {
	operationMu sync.Mutex
	resourceMu  sync.RWMutex
	context     context.Context
	cancel      context.CancelFunc

	options      LiveRunOptions
	featureStore *store.FeatureRequestStore
	provenance   *provenance.BuildReviewProvenance
	bootstrap    *store.CrossRunBootstrap
	provided     *store.ProvidedAssets
	assetsReady  bool
	generation   *liveGeneration
	closed       bool
	started      bool
}

type liveGeneration struct {
	workspace    string
	bootISO      string
	stdoutPath   string
	stderrPath   string
	controller   *qemu.QEMUProcessController
	qmp          *qemu.QMPClient
	serial       *guest.SerialConnection
	dispatcher   *guest.SerialProtocolDispatcher
	hostServices *build.CodexOSHostServices
	toolClient   *guest.ToolClient
	cancel       context.CancelFunc
	startedAt    time.Time
}

// NewLiveCodexOSRun creates a concrete process owner. It does not start QEMU.
func NewLiveCodexOSRun(runDirectory string, options LiveRunOptions) (*CodexOSRun, error) {
	if options.QEMUExecutable == "" {
		options.QEMUExecutable = "qemu-system-x86_64"
	}
	if options.HardwareProfile.Profile == "" {
		options.HardwareProfile = qemu.ExperimentHardwareProfile
	}
	if err := options.HardwareProfile.Validate(); err != nil {
		return nil, err
	}
	if options.ReadyTimeout < 0 {
		return nil, &GenerationRuntimeError{Reason: "generation readiness timeout must not be negative"}
	}
	if options.CandidateReadyTimeout < 0 {
		return nil, &GenerationRuntimeError{Reason: "candidate readiness timeout must not be negative"}
	}
	if options.ReadyTimeout == 0 {
		options.ReadyTimeout = defaultGenerationReadyTimeout
	}
	run, err := NewCodexOSRun(runDirectory)
	if err != nil {
		return nil, err
	}
	features, err := store.NewFeatureRequestStore(run.runDirectory)
	if err != nil {
		return nil, err
	}
	bootstrap, err := store.LoadCrossRunBootstrap(run.runDirectory)
	if err != nil {
		return nil, err
	}
	forensics := provenance.NewBuildReviewProvenance(run.runDirectory, func(event string, generation uint64, data map[string]any) {
		run.recordLive(event, &generation, data)
	})
	liveContext, cancelLive := context.WithCancel(context.Background())
	run.live = &liveRun{
		options: options, featureStore: features, provenance: forensics, bootstrap: bootstrap,
		context: liveContext, cancel: cancelLive,
	}
	if options.Metrics != nil {
		requests, requestErr := features.Requests()
		if requestErr != nil {
			return nil, requestErr
		}
		pending := 0
		for _, request := range requests {
			if request.Status == store.FeaturePending {
				pending++
			}
		}
		options.Metrics.SetFeatureRequestsPending(pending)
	}
	return run, nil
}

// Start boots generation zero and establishes both trusted control channels
// and the sole serial reader before exposing the run as running.
func (r *CodexOSRun) Start(ctx context.Context, initialISO string) error {
	if r == nil {
		return &GenerationRuntimeError{Reason: "run is nil"}
	}
	if ctx == nil {
		return &GenerationRuntimeError{Reason: "generation start context is nil"}
	}
	if r.live == nil {
		return &GenerationRuntimeError{Reason: "CodexOS run has no live runtime configuration"}
	}
	r.live.operationMu.Lock()
	defer r.live.operationMu.Unlock()
	if r.live.closed {
		return &GenerationRuntimeError{Reason: "CodexOS run is closed"}
	}
	r.gateMu.Lock()
	if r.state != RuntimeStateStopped {
		r.gateMu.Unlock()
		return &GenerationRuntimeError{Reason: "CodexOS run is not stopped"}
	}
	if r.generationNumber != nil {
		r.gateMu.Unlock()
		return &GenerationRuntimeError{Reason: "CodexOS run has already been started"}
	}
	r.gateMu.Unlock()
	resolvedISO, err := filepath.Abs(initialISO)
	if err == nil {
		resolvedISO, err = filepath.EvalSymlinks(resolvedISO)
	}
	if err != nil || !isRegularWithoutSymlink(resolvedISO) {
		return &GenerationRuntimeError{Reason: "initial ISO is unavailable: " + initialISO}
	}
	if r.live.bootstrap != nil {
		if err := r.live.bootstrap.VerifyInitialISO(initialISO); err != nil {
			return err
		}
	}
	if err := r.configureLiveAssets(0); err != nil {
		return err
	}
	operationContext, cancelOperation := r.liveOperationContext(ctx)
	defer cancelOperation()
	generation, hardware, err := r.bootLiveGeneration(operationContext, 0, resolvedISO)
	if err != nil {
		return err
	}
	r.installLiveGeneration(generation)
	zero := uint64(0)
	r.gateMu.Lock()
	r.generationNumber = &zero
	if r.live.bootstrap != nil {
		handoff := r.live.bootstrap.Handoff
		r.previousHandoff = &handoff
	}
	r.currentTransition = "initial"
	r.currentHardware = hardware
	r.currentBootImage = generation.bootISO
	r.state = RuntimeStateRunning
	r.gateMu.Unlock()
	r.setObservedLiveState()
	r.recordLive("run_started", nil, map[string]any{})
	r.live.started = true
	r.recordLiveGenerationStarted(0, nil, "initial", generation.controller)
	return nil
}

// GenerationRunning implements the agent runtime boundary.
func (r *CodexOSRun) GenerationRunning() bool {
	return r != nil && r.State() == RuntimeStateRunning && r.liveGeneration() != nil
}

// GenerationState implements the string-valued agent state boundary.
func (r *CodexOSRun) GenerationState() string { return string(r.State()) }

// HardwareProfile returns the fixed profile selected by trusted configuration.
func (r *CodexOSRun) HardwareProfile() qemu.HardwareProfile {
	if r == nil || r.live == nil {
		return qemu.HardwareProfile{}
	}
	return r.live.options.HardwareProfile
}

func (r *CodexOSRun) EventLog() *observability.EventLog {
	if r == nil || r.live == nil {
		return nil
	}
	return r.live.options.EventLog
}

func (r *CodexOSRun) Metrics() *observability.Metrics {
	if r == nil || r.live == nil {
		return nil
	}
	return r.live.options.Metrics
}

func (r *CodexOSRun) ForensicProvenance() *provenance.BuildReviewProvenance {
	if r == nil || r.live == nil {
		return nil
	}
	return r.live.provenance
}

func (r *CodexOSRun) FeatureRequests() ([]store.FeatureRequest, error) {
	if r == nil || r.live == nil || r.live.featureStore == nil {
		return []store.FeatureRequest{}, nil
	}
	r.live.operationMu.Lock()
	defer r.live.operationMu.Unlock()
	return r.live.featureStore.Requests()
}

func (r *CodexOSRun) FeatureRequest(requestID uint64) (store.FeatureRequest, error) {
	if r == nil || r.live == nil || r.live.featureStore == nil {
		return store.FeatureRequest{}, &GenerationRuntimeError{Reason: "feature-request store is unavailable"}
	}
	r.live.operationMu.Lock()
	defer r.live.operationMu.Unlock()
	return r.live.featureStore.Request(requestID)
}

func (r *CodexOSRun) ApproveFeatureRequest(requestID uint64) (store.FeatureRequest, error) {
	return r.decideFeatureRequest(requestID, true)
}

func (r *CodexOSRun) DenyFeatureRequest(requestID uint64) (store.FeatureRequest, error) {
	return r.decideFeatureRequest(requestID, false)
}

func (r *CodexOSRun) decideFeatureRequest(requestID uint64, approve bool) (store.FeatureRequest, error) {
	if r == nil || r.live == nil || r.live.featureStore == nil {
		return store.FeatureRequest{}, &GenerationRuntimeError{Reason: "feature-request store is unavailable"}
	}
	r.live.operationMu.Lock()
	defer r.live.operationMu.Unlock()
	r.gateMu.Lock()
	if r.state != RuntimeStateAwaitingNextGeneration || r.generationNumber == nil || r.transitioning {
		r.gateMu.Unlock()
		return store.FeatureRequest{}, &GenerationRuntimeError{Reason: "feature requests may be decided only while awaiting a generation"}
	}
	generation := *r.generationNumber
	r.gateMu.Unlock()
	var request store.FeatureRequest
	var err error
	event := "feature_denied"
	if approve {
		request, err = r.live.featureStore.Approve(requestID)
		event = "feature_approved"
	} else {
		request, err = r.live.featureStore.Deny(requestID)
	}
	if err != nil {
		return store.FeatureRequest{}, err
	}
	r.recordLive(event, &generation, map[string]any{
		"request_id": request.ID, "request_generation": request.Generation,
	})
	if r.live.options.Metrics != nil {
		requests, requestErr := r.live.featureStore.Requests()
		if requestErr == nil {
			pending := 0
			for _, item := range requests {
				if item.Status == store.FeaturePending {
					pending++
				}
			}
			r.live.options.Metrics.SetFeatureRequestsPending(pending)
		}
	}
	return request, nil
}

// ListTools performs the canonical discovery exchange through the sole serial
// dispatcher.
func (r *CodexOSRun) ListTools(ctx context.Context) ([]string, error) {
	if r == nil || r.live == nil {
		return nil, &GenerationRuntimeError{Reason: "CodexOS run has no live runtime configuration"}
	}
	r.live.operationMu.Lock()
	defer r.live.operationMu.Unlock()
	generation, number, err := r.requireRunningLiveGeneration()
	if err != nil {
		return nil, err
	}
	r.recordLive("tool_guest_invocation_started", &number, map[string]any{"tool": "list_tools"})
	operationContext, cancelOperation := r.liveOperationContext(ctx)
	defer cancelOperation()
	tools, err := generation.toolClient.ListTools(operationContext)
	if err != nil {
		r.recordLive("tool_guest_invocation_failed", &number, map[string]any{"tool": "list_tools"})
		return nil, err
	}
	r.recordLive("tool_guest_invocation_completed", &number, map[string]any{"tool": "list_tools"})
	if err := r.finishLiveIfRequested(generation, number); err != nil {
		return nil, err
	}
	return tools, nil
}

// InvokeTool performs one serialized guest exchange.
func (r *CodexOSRun) InvokeTool(ctx context.Context, name string, arguments [][]byte) (guest.ToolResult, error) {
	if r == nil || r.live == nil {
		return guest.ToolResult{}, &GenerationRuntimeError{Reason: "CodexOS run has no live runtime configuration"}
	}
	r.live.operationMu.Lock()
	defer r.live.operationMu.Unlock()
	generation, number, err := r.requireRunningLiveGeneration()
	if err != nil {
		return guest.ToolResult{}, err
	}
	r.recordLive("tool_guest_invocation_started", &number, map[string]any{"tool": name})
	operationContext, cancelOperation := r.liveOperationContext(ctx)
	defer cancelOperation()
	result, err := generation.toolClient.InvokeTool(operationContext, name, arguments)
	if err != nil {
		r.recordLive("tool_guest_invocation_failed", &number, map[string]any{"tool": name})
		return guest.ToolResult{}, err
	}
	r.recordLive("tool_guest_invocation_completed", &number, map[string]any{
		"tool": name, "status": result.Status, "output_bytes": len(result.Output),
	})
	if err := r.finishLiveIfRequested(generation, number); err != nil {
		return guest.ToolResult{}, err
	}
	return result, nil
}

func (r *CodexOSRun) Pause(ctx context.Context) error {
	if r == nil || r.live == nil {
		return &GenerationRuntimeError{Reason: "CodexOS run has no live runtime configuration"}
	}
	if ctx == nil {
		return &GenerationRuntimeError{Reason: "generation pause context is nil"}
	}
	r.live.operationMu.Lock()
	defer r.live.operationMu.Unlock()
	operationContext, cancelOperation := r.liveOperationContext(ctx)
	defer cancelOperation()
	generation, number, err := r.requireRunningLiveGeneration()
	if err != nil {
		return err
	}
	if err := generation.qmp.Stop(operationContext); err != nil {
		return err
	}
	r.gateMu.Lock()
	r.state = RuntimeStatePaused
	r.gateMu.Unlock()
	r.setObservedLiveState()
	status, err := generation.qmp.QueryStatus(operationContext)
	if err != nil {
		return err
	}
	if status != "paused" {
		return &GenerationRuntimeError{Reason: fmt.Sprintf("QEMU did not pause; status is %q", status)}
	}
	r.recordLive("generation_paused", &number, map[string]any{})
	return nil
}

func (r *CodexOSRun) Resume(ctx context.Context) error {
	if r == nil || r.live == nil {
		return &GenerationRuntimeError{Reason: "CodexOS run has no live runtime configuration"}
	}
	if ctx == nil {
		return &GenerationRuntimeError{Reason: "generation resume context is nil"}
	}
	r.live.operationMu.Lock()
	defer r.live.operationMu.Unlock()
	operationContext, cancelOperation := r.liveOperationContext(ctx)
	defer cancelOperation()
	r.gateMu.Lock()
	if r.state != RuntimeStatePaused || r.generationNumber == nil {
		r.gateMu.Unlock()
		return &GenerationRuntimeError{Reason: "CodexOS generation is not paused"}
	}
	number := *r.generationNumber
	r.gateMu.Unlock()
	generation := r.liveGeneration()
	if generation == nil {
		return &GenerationRuntimeError{Reason: "CodexOS QMP connection is unavailable"}
	}
	if err := generation.qmp.Continue(operationContext); err != nil {
		return err
	}
	status, err := generation.qmp.QueryStatus(operationContext)
	if err != nil {
		return err
	}
	if status != "running" {
		return &GenerationRuntimeError{Reason: fmt.Sprintf("QEMU did not resume; status is %q", status)}
	}
	r.gateMu.Lock()
	r.state = RuntimeStateRunning
	r.gateMu.Unlock()
	r.setObservedLiveState()
	r.recordLive("generation_resumed", &number, map[string]any{})
	return nil
}

func (r *CodexOSRun) bootLiveGeneration(ctx context.Context, number uint64, image string) (_ *liveGeneration, _ qemu.HardwareManifest, resultErr error) {
	options := r.live.options
	if err := options.HardwareProfile.RequireAvailable(); err != nil {
		return nil, qemu.HardwareManifest{}, &GenerationRuntimeError{Reason: "could not start QEMU", Err: err}
	}
	version, err := qemu.DiscoverQEMUVersion(ctx, options.QEMUExecutable)
	if err != nil {
		return nil, qemu.HardwareManifest{}, err
	}
	hardware, err := options.HardwareProfile.Manifest(version)
	if err != nil {
		return nil, qemu.HardwareManifest{}, err
	}
	workspace, err := os.MkdirTemp(r.runDirectory, fmt.Sprintf(".generation-%04d-", number))
	if err != nil {
		return nil, qemu.HardwareManifest{}, &GenerationRuntimeError{Reason: "could not create generation workspace", Err: err}
	}
	generation := &liveGeneration{
		workspace:  workspace,
		bootISO:    filepath.Join(workspace, "boot.iso"),
		stdoutPath: filepath.Join(workspace, stdoutName),
		stderrPath: filepath.Join(workspace, stderrName),
	}
	dispatcherContext, cancelDispatcher := context.WithCancel(r.live.context)
	generation.cancel = cancelDispatcher
	defer func() {
		if resultErr != nil {
			if closeErr := closeLiveGeneration(generation, defaultGenerationStopTimeout); closeErr != nil {
				resultErr = errors.Join(resultErr, closeErr)
				return
			}
			_ = os.RemoveAll(workspace)
		}
	}()
	if err := copyLiveFile(image, generation.bootISO); err != nil {
		return nil, qemu.HardwareManifest{}, &GenerationRuntimeError{Reason: "could not stage generation boot image", Err: err}
	}
	qmpPath := filepath.Join(workspace, "qmp.sock")
	serialPath := filepath.Join(workspace, "serial.sock")
	generation.controller = qemu.NewQEMUProcessController(options.QEMUExecutable)
	generation.qmp = qemu.NewQMPClient(qmpPath)
	generation.serial = guest.NewSerialConnection(serialPath)
	validator, err := build.NewCandidateBootValidator(build.CandidateBootConfig{
		QEMUExecutable: options.QEMUExecutable, HardwareProfile: options.HardwareProfile,
		ReadyTimeout: options.CandidateReadyTimeout, TemporaryParent: workspace,
		ActivityStream: options.ActivityStream, Generation: &number, ProvidedAssets: r.live.provided,
		SerialEventRecorder: r.liveSerialRecorder(number, "candidate"),
	})
	if err != nil {
		return nil, qemu.HardwareManifest{}, err
	}
	generation.hostServices, err = build.NewCodexOSHostServices(build.HostServicesConfig{
		StagingDirectory: filepath.Join(workspace, "builds"), CandidateValidator: validator,
		BuildConfig: options.BuildConfig, FeatureRequestStore: r.live.featureStore,
		Generation: &number, EventLog: options.EventLog, Metrics: options.Metrics,
		ActivityStream: options.ActivityStream, ProvidedAssets: r.live.provided, Provenance: r.live.provenance,
	})
	if err != nil {
		return nil, qemu.HardwareManifest{}, err
	}
	arguments, err := options.HardwareProfile.QEMUCommandArguments(generation.bootISO, qmpPath, serialPath)
	if err != nil {
		return nil, qemu.HardwareManifest{}, err
	}
	if err := generation.controller.Start(qemu.QEMUStartOptions{
		Arguments: arguments, StdoutPath: generation.stdoutPath, StderrPath: generation.stderrPath,
	}); err != nil {
		return nil, qemu.HardwareManifest{}, err
	}
	if err := generation.qmp.Connect(ctx); err != nil {
		return nil, qemu.HardwareManifest{}, err
	}
	if err := generation.serial.Connect(ctx); err != nil {
		return nil, qemu.HardwareManifest{}, err
	}
	assetHandler := providedAssetsHandler(r.live.provided)
	generation.dispatcher = guest.NewSerialProtocolDispatcher(generation.serial, guest.DispatcherOptions{
		StartupHostServices: assetHandler, BackgroundHostServices: assetHandler,
		ExchangeHostServices: generation.hostServices.HandleRequest,
		EventRecorder:        r.liveSerialRecorder(number, "active"),
	})
	if err := generation.dispatcher.Start(dispatcherContext); err != nil {
		return nil, qemu.HardwareManifest{}, err
	}
	if err := generation.dispatcher.WaitUntilReady(ctx, options.ReadyTimeout); err != nil {
		diagnostic := generation.dispatcher.StartupDiagnostic()
		if len(diagnostic) != 0 {
			err = fmt.Errorf("%w\nGuest serial before failure:\n%s", err, guest.EscapeDiagnosticBytes(diagnostic))
		}
		return nil, qemu.HardwareManifest{}, err
	}
	generation.toolClient = guest.NewToolClient(generation.dispatcher)
	generation.startedAt = time.Now()
	return generation, hardware, nil
}

func (r *CodexOSRun) configureLiveAssets(generation uint64) error {
	if r.live.assetsReady {
		return nil
	}
	provided, err := store.ConfigureProvidedAssets(r.runDirectory, r.live.options.ProvidedAssetsDirectory, generation)
	if err != nil {
		return err
	}
	r.live.provided = provided
	r.live.assetsReady = true
	if provided != nil && provided.Provenance != nil && provided.Provenance.Created {
		r.recordLive("provided_assets_revision_accepted", &generation, map[string]any{
			"revision":        provided.Provenance.Revision,
			"new_asset_count": provided.Provenance.IntroducedAssetCount,
			"asset_count":     len(provided.AssetsCopy()),
		})
	}
	return nil
}

func (r *CodexOSRun) requireRunningLiveGeneration() (*liveGeneration, uint64, error) {
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	if r.state != RuntimeStateRunning || r.generationNumber == nil {
		return nil, 0, &GenerationRuntimeError{Reason: "CodexOS generation is not running"}
	}
	generation := r.liveGeneration()
	if generation == nil || generation.toolClient == nil {
		return nil, 0, &GenerationRuntimeError{Reason: "CodexOS guest tool client is unavailable"}
	}
	return generation, *r.generationNumber, nil
}

func (r *CodexOSRun) installLiveGeneration(generation *liveGeneration) {
	r.live.resourceMu.Lock()
	r.live.generation = generation
	r.live.resourceMu.Unlock()
}

func (r *CodexOSRun) liveGeneration() *liveGeneration {
	if r == nil || r.live == nil {
		return nil
	}
	r.live.resourceMu.RLock()
	defer r.live.resourceMu.RUnlock()
	return r.live.generation
}

func (r *CodexOSRun) detachExpectedLiveGeneration(expected *liveGeneration) bool {
	if r == nil || r.live == nil {
		return false
	}
	r.live.resourceMu.Lock()
	defer r.live.resourceMu.Unlock()
	if r.live.generation != expected {
		return false
	}
	r.live.generation = nil
	return true
}

func closeLiveGeneration(generation *liveGeneration, timeout time.Duration) error {
	if generation == nil {
		return nil
	}
	if timeout <= 0 {
		timeout = defaultGenerationStopTimeout
	}
	errorsSeen := make([]error, 0, 3)
	if generation.cancel != nil {
		generation.cancel()
	}
	if generation.qmp != nil && generation.controller != nil && generation.controller.IsRunning() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		_ = generation.qmp.Quit(ctx)
		cancel()
	}
	if generation.controller != nil && generation.controller.IsRunning() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		if err := generation.controller.Stop(ctx, timeout); err != nil {
			errorsSeen = append(errorsSeen, err)
		}
		cancel()
	}
	if generation.dispatcher != nil {
		if err := generation.dispatcher.Close(); err != nil {
			errorsSeen = append(errorsSeen, err)
		}
	} else if generation.serial != nil {
		if err := generation.serial.Close(); err != nil {
			errorsSeen = append(errorsSeen, err)
		}
	}
	if generation.qmp != nil {
		if err := generation.qmp.Close(); err != nil {
			errorsSeen = append(errorsSeen, err)
		}
	}
	if generation.controller != nil {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		if err := generation.controller.Stop(ctx, timeout); err != nil {
			errorsSeen = append(errorsSeen, err)
		}
		cancel()
	}
	return errors.Join(errorsSeen...)
}

func (r *CodexOSRun) stopLive() error {
	if r == nil || r.live == nil {
		return nil
	}
	r.live.cancel()
	r.live.operationMu.Lock()
	r.live.closed = true
	generation := r.liveGeneration()
	err := closeLiveGeneration(generation, defaultGenerationStopTimeout)
	if generation != nil && err == nil {
		_ = r.detachExpectedLiveGeneration(generation)
		_ = os.RemoveAll(generation.workspace)
	}
	if err == nil && r.live.started {
		r.recordLive("run_stopped", nil, map[string]any{})
		r.live.started = false
	}
	r.live.operationMu.Unlock()
	return err
}

// Close cancels active guest/build work, then joins the live QEMU and serial
// owners. A cleanup error leaves the workspace in place for diagnosis.
func (r *CodexOSRun) Close() error {
	if r == nil {
		return nil
	}
	err := r.stopLive()
	if err == nil {
		r.clearStoppedState()
	}
	return err
}

func (r *CodexOSRun) liveOperationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	operationContext, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.live.context, cancel)
	return operationContext, func() {
		stop()
		cancel()
	}
}

func (r *CodexOSRun) recordLive(event string, generation *uint64, data map[string]any) {
	if r == nil || r.live == nil {
		return
	}
	if r.live.options.EventLog != nil {
		r.live.options.EventLog.Record(event, generation, data)
	}
	if r.live.options.Metrics != nil {
		r.live.options.Metrics.Record(event, data)
	}
}

func (r *CodexOSRun) setObservedLiveState() {
	if r == nil || r.live == nil || r.live.options.Metrics == nil {
		return
	}
	var generation *uint64
	if value, ok := r.GenerationNumber(); ok {
		generation = &value
	}
	r.live.options.Metrics.SetRuntimeState(generation, string(r.State()))
}

func (r *CodexOSRun) recordLiveGenerationStarted(number uint64, parent *uint64, transition string, controller *qemu.QEMUProcessController) {
	pid, _ := controller.PID()
	profile := r.live.options.HardwareProfile
	r.recordLive("generation_started", &number, map[string]any{
		"transition": transition, "parent_generation": parent, "qemu_pid": pid,
		"hardware_profile": profile.Profile, "vcpus": profile.VCPUs, "memory_mib": profile.MemoryMiB,
	})
}

func (r *CodexOSRun) liveSerialRecorder(generation uint64, connection string) guest.SerialEventRecorder {
	return func(event string, data map[string]any) {
		copy := make(map[string]any, len(data)+1)
		for key, value := range data {
			copy[key] = value
		}
		copy["connection"] = connection
		r.recordLive(event, &generation, copy)
	}
}

func providedAssetsHandler(assets *store.ProvidedAssets) guest.HostServiceHandler {
	if assets == nil {
		return nil
	}
	return func(_ context.Context, request guest.HostRequest) (guest.Frame, error) {
		return assets.HandleRequest(request)
	}
}

func copyLiveFile(source, destination string) error {
	input, err := openRegularNoFollow(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.CopyBuffer(output, input, make([]byte, 1024*1024))
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func openRegularNoFollow(path string) (*os.File, error) {
	fileDescriptor, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fileDescriptor), path)
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("source is not a regular file: %s", path)
	}
	return file, nil
}
