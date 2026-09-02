package experiment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
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

	options      LiveRunOptions
	featureStore *store.FeatureRequestStore
	provenance   *provenance.BuildReviewProvenance
	provided     *store.ProvidedAssets
	assetsReady  bool
	generation   *liveGeneration
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
	forensics := provenance.NewBuildReviewProvenance(run.runDirectory, func(event string, generation uint64, data map[string]any) {
		run.recordLive(event, &generation, data)
	})
	run.live = &liveRun{
		options: options, featureStore: features, provenance: forensics,
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
	if !isRegularWithoutSymlink(initialISO) {
		return &GenerationRuntimeError{Reason: "initial ISO is unavailable: " + initialISO}
	}
	if err := r.configureLiveAssets(0); err != nil {
		return err
	}
	generation, hardware, err := r.bootLiveGeneration(ctx, 0, initialISO)
	if err != nil {
		return err
	}
	r.installLiveGeneration(generation)
	zero := uint64(0)
	r.gateMu.Lock()
	r.generationNumber = &zero
	r.currentTransition = "initial"
	r.currentHardware = hardware
	r.currentBootImage = generation.bootISO
	r.state = RuntimeStateRunning
	r.gateMu.Unlock()
	r.setObservedLiveState()
	r.recordLive("run_started", nil, map[string]any{})
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
	return r.live.featureStore.Requests()
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
	tools, err := generation.toolClient.ListTools(ctx)
	if err != nil {
		r.recordLive("tool_guest_invocation_failed", &number, map[string]any{"tool": "list_tools"})
		return nil, err
	}
	r.recordLive("tool_guest_invocation_completed", &number, map[string]any{"tool": "list_tools"})
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
	result, err := generation.toolClient.InvokeTool(ctx, name, arguments)
	if err != nil {
		r.recordLive("tool_guest_invocation_failed", &number, map[string]any{"tool": name})
		return guest.ToolResult{}, err
	}
	r.recordLive("tool_guest_invocation_completed", &number, map[string]any{
		"tool": name, "status": result.Status, "output_bytes": len(result.Output),
	})
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
	generation, number, err := r.requireRunningLiveGeneration()
	if err != nil {
		return err
	}
	if err := generation.qmp.Stop(ctx); err != nil {
		return err
	}
	status, err := generation.qmp.QueryStatus(ctx)
	if err != nil {
		return err
	}
	if status != "paused" {
		return &GenerationRuntimeError{Reason: fmt.Sprintf("QEMU did not pause; status is %q", status)}
	}
	r.gateMu.Lock()
	r.state = RuntimeStatePaused
	r.gateMu.Unlock()
	r.setObservedLiveState()
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
	if err := generation.qmp.Continue(ctx); err != nil {
		return err
	}
	status, err := generation.qmp.QueryStatus(ctx)
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
	dispatcherContext, cancelDispatcher := context.WithCancel(context.Background())
	generation.cancel = cancelDispatcher
	defer func() {
		if resultErr != nil {
			_ = closeLiveGeneration(generation, defaultGenerationStopTimeout)
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

func (r *CodexOSRun) detachLiveGeneration() *liveGeneration {
	if r == nil || r.live == nil {
		return nil
	}
	r.live.resourceMu.Lock()
	generation := r.live.generation
	r.live.generation = nil
	r.live.resourceMu.Unlock()
	return generation
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

func (r *CodexOSRun) stopLive() {
	if r == nil || r.live == nil {
		return
	}
	r.live.operationMu.Lock()
	generation := r.detachLiveGeneration()
	_ = closeLiveGeneration(generation, defaultGenerationStopTimeout)
	if generation != nil {
		_ = os.RemoveAll(generation.workspace)
	}
	r.live.operationMu.Unlock()
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
	input, err := os.Open(source)
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
