package build

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codexos/internal/guest"
	"codexos/internal/observability"
	"codexos/internal/provenance"
	"codexos/internal/qemu"
	"codexos/internal/store"
)

const (
	defaultCandidateReadyTimeout = 10 * time.Second
	candidateQEMUExitTimeout     = 5 * time.Second
	maxCandidateDiagnostics      = 64 * 1024
)

// CandidateBootConfig contains the trusted configuration for one ephemeral
// candidate VM.  All values are harness configuration; no value comes from a
// guest source snapshot.
type CandidateBootConfig struct {
	QEMUExecutable      string
	HardwareProfile     qemu.HardwareProfile
	ReadyTimeout        time.Duration
	TemporaryParent     string
	ActivityStream      *observability.ActivityStream
	Generation          *uint64
	ProvidedAssets      *store.ProvidedAssets
	SerialEventRecorder guest.SerialEventRecorder
}

// CandidateBootResult is the outcome of compiling an exact candidate and
// proving that it boots and speaks the canonical guest protocol.  A
// successful compilation that fails either proof is BuildStatusBuildFailure;
// inability of the trusted harness to perform the proof is
// BuildStatusHarnessFailure.
type CandidateBootResult struct {
	Status      BuildStatus
	Diagnostics string
}

// CandidateBootValidator owns the ephemeral QEMU, QMP, serial, and temporary
// workspace resources used for one candidate validation.
type CandidateBootValidator struct {
	qemuExecutable    string
	hardwareProfile   qemu.HardwareProfile
	readyTimeout      time.Duration
	temporaryParent   string
	activityStream    *observability.ActivityStream
	generation        *uint64
	providedAssets    *store.ProvidedAssets
	serialEventRecord guest.SerialEventRecorder
}

// NewCandidateBootValidator validates the local validator configuration and
// returns an owner for disposable candidate VMs.  A zero readiness timeout
// selects the Python reference default.
func NewCandidateBootValidator(config CandidateBootConfig) (*CandidateBootValidator, error) {
	if config.ReadyTimeout < 0 {
		return nil, errors.New("candidate readiness timeout must not be negative")
	}
	if err := config.HardwareProfile.Validate(); err != nil {
		return nil, err
	}
	if config.ReadyTimeout == 0 {
		config.ReadyTimeout = defaultCandidateReadyTimeout
	}
	return &CandidateBootValidator{
		qemuExecutable:    config.QEMUExecutable,
		hardwareProfile:   config.HardwareProfile,
		readyTimeout:      config.ReadyTimeout,
		temporaryParent:   config.TemporaryParent,
		activityStream:    config.ActivityStream,
		generation:        cloneGeneration(config.Generation),
		providedAssets:    config.ProvidedAssets,
		serialEventRecord: config.SerialEventRecorder,
	}, nil
}

// Validate boots candidateISO under the configured trusted profile, waits for
// CODEXOS-SEED-READY, and completes one canonical list-tools exchange.  The
// caller's context controls the operation, while cleanup uses a fresh bounded
// context so cancellation never abandons the candidate child.
func (v *CandidateBootValidator) Validate(
	ctx context.Context,
	candidateISO string,
	evidence *provenance.BuildAttemptEvidence,
	isoIdentity *provenance.FileIdentity,
) CandidateBootResult {
	if ctx == nil {
		result := harnessCandidateFailure(errors.New("candidate boot context is nil"))
		identity := identityData(evidence, isoIdentity)
		v.publishCandidateFailure(result, identity)
		if evidence != nil {
			_ = evidence.RecordCandidateStage(
				"build_candidate_validation_completed",
				"candidate_completed",
				map[string]any{"outcome": "harness_failure"},
			)
		}
		return result
	}
	identity := identityData(evidence, isoIdentity)
	if evidence != nil {
		if err := evidence.RecordCandidateStage(
			"build_candidate_validation_started",
			"candidate_started",
			map[string]any{
				"expected_iso_sha256": identityValue(identity, "iso_sha256"),
				"expected_iso_bytes":  identityValue(identity, "iso_bytes"),
			},
		); err != nil {
			result := harnessCandidateFailure(err)
			v.publishCandidateFailure(result, identity)
			return result
		}
	}
	v.publish(observability.ActivityBuildCandidateStarted, identity)
	if err := ctx.Err(); err != nil {
		result := harnessCandidateFailure(err)
		v.publishCandidateFailure(result, identity)
		return v.complete(result, evidence)
	}

	workspace, err := os.MkdirTemp(v.temporaryParent, "codexos-candidate-")
	if err != nil {
		result := harnessCandidateFailure(fmt.Errorf("could not create candidate workspace: %w", err))
		v.publishCandidateFailure(result, identity)
		return v.complete(result, evidence)
	}
	defer func() { _ = os.RemoveAll(workspace) }()
	result := v.validateInWorkspace(ctx, workspace, candidateISO, evidence, identity)
	removeErr := os.RemoveAll(workspace)
	if removeErr != nil {
		result = harnessCandidateFailure(fmt.Errorf("could not remove candidate workspace: %w", removeErr))
	}
	if result.Status != BuildStatusSuccess {
		v.publishCandidateFailure(result, identity)
	}
	return v.complete(result, evidence)
}

func (v *CandidateBootValidator) validateInWorkspace(
	ctx context.Context,
	workspace string,
	candidateISO string,
	evidence *provenance.BuildAttemptEvidence,
	identity map[string]any,
) (result CandidateBootResult) {
	qmpPath := filepath.Join(workspace, "qmp.sock")
	serialPath := filepath.Join(workspace, "serial.sock")
	qmpClient := qemu.NewQMPClient(qmpPath)
	serial := guest.NewSerialConnection(serialPath)
	controller := qemu.NewQEMUProcessController(v.qemuExecutable)
	var dispatcher *guest.SerialProtocolDispatcher
	qmpConnected := false

	defer func() {
		cleanupErrors := make([]string, 0, 2)
		if dispatcher != nil {
			if err := dispatcher.Close(); err != nil {
				cleanupErrors = append(cleanupErrors, "serial close failed: "+err.Error())
			}
		} else if err := serial.Close(); err != nil {
			cleanupErrors = append(cleanupErrors, "serial close failed: "+err.Error())
		}
		if qmpConnected {
			cleanupContext, cancel := context.WithTimeout(context.Background(), candidateQEMUExitTimeout)
			_ = qmpClient.Quit(cleanupContext)
			cancel()
		}
		_ = qmpClient.Close()
		cleanupContext, cancel := context.WithTimeout(context.Background(), candidateQEMUExitTimeout)
		if err := controller.Stop(cleanupContext, candidateQEMUExitTimeout); err != nil {
			cleanupErrors = append(cleanupErrors, "QEMU termination failed: "+err.Error())
		}
		cancel()
		if controller.IsRunning() {
			cleanupErrors = append(cleanupErrors, "candidate QEMU remained running")
		}
		if len(cleanupErrors) != 0 {
			result = harnessCandidateFailure(errors.New(strings.Join(cleanupErrors, "; ")))
		}
	}()

	if err := v.hardwareProfile.RequireAvailable(); err != nil {
		return harnessCandidateFailure(fmt.Errorf("could not start candidate QEMU: %w", err))
	}
	arguments, err := v.hardwareProfile.QEMUCommandArguments(candidateISO, qmpPath, serialPath)
	if err != nil {
		return harnessCandidateFailure(fmt.Errorf("could not start candidate QEMU: %w", err))
	}
	// Keep the VM paused until both trusted controls have been established.
	arguments = append(arguments, "-S")
	if err := controller.Start(qemu.QEMUStartOptions{
		Arguments:  arguments,
		StdoutPath: filepath.Join(workspace, "qemu.stdout"),
		StderrPath: filepath.Join(workspace, "qemu.stderr"),
	}); err != nil {
		return harnessCandidateFailure(fmt.Errorf("could not start candidate QEMU: %w", err))
	}
	if evidence != nil {
		if err := evidence.RecordCandidateStage("build_candidate_qemu_started", "candidate_qemu_started"); err != nil {
			return harnessCandidateFailure(err)
		}
	}

	if err := qmpClient.Connect(ctx); err != nil {
		return harnessCandidateFailure(fmt.Errorf("could not establish candidate QEMU control: %w", err))
	}
	qmpConnected = true
	if err := serial.Connect(ctx); err != nil {
		return harnessCandidateFailure(fmt.Errorf("could not establish candidate QEMU control: %w", err))
	}
	if err := qmpClient.Continue(ctx); err != nil {
		if controller.IsRunning() {
			return harnessCandidateFailure(fmt.Errorf("could not start candidate execution: %w", err))
		}
		return guestCandidateFailure("candidate QEMU exited before CODEXOS-SEED-READY")
	}

	hostHandler := candidateAssetHandler(v.providedAssets)
	dispatcher = guest.NewSerialProtocolDispatcher(serial, guest.DispatcherOptions{
		StartupHostServices:    hostHandler,
		BackgroundHostServices: hostHandler,
		ExchangeHostServices:   hostHandler,
		EventRecorder:          v.serialEventRecord,
	})
	if err := dispatcher.Start(ctx); err != nil {
		return harnessCandidateFailure(fmt.Errorf("could not start candidate serial protocol: %w", err))
	}
	if err := dispatcher.WaitUntilReady(ctx, v.readyTimeout); err != nil {
		if ctx.Err() != nil {
			return harnessCandidateFailure(ctx.Err())
		}
		return guestCandidateFailure(candidateReadyDiagnostic(err, dispatcher.StartupDiagnostic()))
	}
	if evidence != nil {
		if err := evidence.RecordCandidateStage(
			"build_candidate_ready_observed",
			"ready_observed",
			map[string]any{"ready": true},
		); err != nil {
			return harnessCandidateFailure(err)
		}
	}
	v.publish(observability.ActivityBuildCandidateReady, identity)

	if evidence != nil {
		if err := evidence.RecordCandidateStage("build_protocol_validation_started", "protocol_validation_started"); err != nil {
			return harnessCandidateFailure(err)
		}
	}
	client := guest.NewToolClient(dispatcher)
	if _, err := client.ListTools(ctx); err != nil {
		if ctx.Err() != nil {
			if evidence != nil {
				if evidenceErr := evidence.RecordCandidateStage(
					"build_protocol_validation_completed",
					"protocol_validation_failed",
					map[string]any{"outcome": "harness_failure"},
				); evidenceErr != nil {
					return harnessCandidateFailure(evidenceErr)
				}
			}
			return harnessCandidateFailure(errors.New("canonical list-tools exchange failed: " + err.Error()))
		}
		if evidence != nil {
			if evidenceErr := evidence.RecordCandidateStage(
				"build_protocol_validation_completed",
				"protocol_validation_failed",
				map[string]any{"outcome": "build_failure"},
			); evidenceErr != nil {
				return harnessCandidateFailure(evidenceErr)
			}
		}
		return guestCandidateFailure("canonical list-tools exchange failed: " + err.Error())
	}
	if evidence != nil {
		if err := evidence.RecordCandidateStage(
			"build_protocol_validation_completed",
			"protocol_validated",
			map[string]any{"outcome": "success", "protocol_validated": true},
		); err != nil {
			return harnessCandidateFailure(err)
		}
	}
	v.publish(observability.ActivityBuildProtocolValidated, identity)
	return CandidateBootResult{Status: BuildStatusSuccess}
}

func candidateAssetHandler(assets *store.ProvidedAssets) guest.HostServiceHandler {
	if assets == nil {
		return nil
	}
	return func(_ context.Context, request guest.HostRequest) (guest.Frame, error) {
		return assets.HandleRequest(request)
	}
}

func candidateReadyDiagnostic(err error, serialOutput []byte) string {
	reason := err.Error()
	var serialErr *guest.SerialError
	if errors.As(err, &serialErr) {
		reason = "serial connection closed before CODEXOS-SEED-READY"
	} else {
		var framingErr *guest.FramingError
		if errors.As(err, &framingErr) {
			reason = "invalid provided-asset request before CODEXOS-SEED-READY: " + err.Error()
		}
	}
	if len(serialOutput) != 0 {
		reason += "\nCandidate serial before failure:\n" + guest.EscapeDiagnosticBytes(serialOutput)
	}
	return reason
}

func (v *CandidateBootValidator) complete(
	result CandidateBootResult,
	evidence *provenance.BuildAttemptEvidence,
) CandidateBootResult {
	if evidence != nil {
		outcome := result.Status
		if err := evidence.RecordCandidateStage(
			"build_candidate_validation_completed",
			"candidate_completed",
			map[string]any{"outcome": string(outcome)},
		); err != nil {
			result = harnessCandidateFailure(err)
		}
	}
	return result
}

func (v *CandidateBootValidator) publishCandidateFailure(result CandidateBootResult, identity map[string]any) {
	data := map[string]any{"result": string(result.Status)}
	for key, value := range identity {
		data[key] = value
	}
	v.publish(observability.ActivityBuildCandidateFailed, data)
}

func (v *CandidateBootValidator) publish(kind observability.ActivityKind, data map[string]any) {
	observability.PublishActivity(v.activityStream, v.generation, observability.ActivityHarness, kind, data, "", "", "")
}

func guestCandidateFailure(detail string) CandidateBootResult {
	return CandidateBootResult{
		Status:      BuildStatusBuildFailure,
		Diagnostics: boundCandidateDiagnostics("Trusted compilation succeeded.\nCandidate boot validation failed:\n" + detail),
	}
}

func harnessCandidateFailure(detail error) CandidateBootResult {
	message := ""
	if detail != nil {
		message = detail.Error()
	}
	return CandidateBootResult{
		Status:      BuildStatusHarnessFailure,
		Diagnostics: boundCandidateDiagnostics("Trusted compilation succeeded.\nCandidate boot validation could not run safely:\n" + message),
	}
}

func boundCandidateDiagnostics(value string) string {
	encoded := []byte(value)
	if len(encoded) <= maxCandidateDiagnostics {
		return value
	}
	return strings.ToValidUTF8(string(encoded[:maxCandidateDiagnostics]), "")
}

func identityData(evidence *provenance.BuildAttemptEvidence, isoIdentity *provenance.FileIdentity) map[string]any {
	data := make(map[string]any, 3)
	if evidence != nil {
		data["build_attempt_id"] = evidence.AttemptID()
	}
	if isoIdentity != nil {
		data["iso_sha256"] = isoIdentity.SHA256
		data["iso_bytes"] = isoIdentity.Size
	}
	return data
}

func identityValue(data map[string]any, key string) any {
	if value, ok := data[key]; ok {
		return value
	}
	return nil
}

func cloneGeneration(generation *uint64) *uint64 {
	if generation == nil {
		return nil
	}
	value := *generation
	return &value
}
