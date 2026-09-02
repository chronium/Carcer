package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"codexos/internal/guest"
	"codexos/internal/observability"
	"codexos/internal/provenance"
	"codexos/internal/store"
)

const (
	// Build host-service statuses are part of the version 1 service contract.
	BuildResponseSuccess        uint32 = 0
	BuildResponseFailure        uint32 = 1
	BuildResponseHarnessFailure uint32 = 2

	// Finish host-service statuses are part of the version 1 service contract.
	FinishResponseAccepted       uint32 = 0
	FinishResponseRejected       uint32 = 1
	FinishResponseHarnessFailure uint32 = 2

	FeatureResponseRecorded       uint32 = 0
	FeatureResponseHarnessFailure uint32 = 2

	maxFinishHandoffBytes = 16 * 1024
	maxFeatureDiagnostic  = 1024
)

// StagedBuildArtifacts identifies a trusted build that has also passed the
// candidate boot and protocol proof. Paths are host-only and are never sent
// over the guest protocol.
//
// SourceSnapshot is copied when returned by LatestSuccessfulBuild. Callers
// should treat the artifact paths as owned by the host-service session.
type StagedBuildArtifacts struct {
	KernelELF      string
	ISO            string
	SourceSnapshot []byte

	BuildAttemptID string
	SourceIdentity provenance.FileIdentity
	KernelIdentity provenance.FileIdentity
	ISOIdentity    provenance.FileIdentity
	Evidence       *provenance.BuildAttemptEvidence
}

// BuildHostServiceConfig configures one synchronous build host service.
// BuildConfig contains trusted build inputs; no field is populated from a
// guest request.
type BuildHostServiceConfig struct {
	StagingDirectory   string
	CandidateValidator *CandidateBootValidator
	BuildConfig        Config
	ActivityStream     *observability.ActivityStream
	Generation         *uint64
	Provenance         *provenance.BuildReviewProvenance
}

// BuildHostService synchronously handles the guest's build host service.
// Every attempt receives a fresh staging directory, and a failed attempt
// never replaces the latest successful candidate.
type BuildHostService struct {
	stagingDirectory   string
	candidateValidator *CandidateBootValidator
	buildConfig        Config
	activityStream     *observability.ActivityStream
	generation         *uint64
	provenance         *provenance.BuildReviewProvenance

	latestSuccessful *StagedBuildArtifacts
}

// NewBuildHostService creates a build host-service owner. It does not start
// a compiler or candidate VM.
func NewBuildHostService(config BuildHostServiceConfig) (*BuildHostService, error) {
	staging := config.StagingDirectory
	if staging == "" {
		staging = "."
	}
	absolute, err := filepath.Abs(staging)
	if err != nil {
		return nil, fmt.Errorf("could not resolve build staging directory: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o777); err != nil {
		return nil, fmt.Errorf("could not create build staging directory: %w", err)
	}
	if config.Provenance != nil && config.Generation == nil {
		return nil, errors.New("build provenance requires a generation")
	}
	return &BuildHostService{
		stagingDirectory:   absolute,
		candidateValidator: config.CandidateValidator,
		buildConfig:        config.BuildConfig,
		activityStream:     config.ActivityStream,
		generation:         cloneGeneration(config.Generation),
		provenance:         config.Provenance,
	}, nil
}

// LatestSuccessfulBuild returns the exact candidate retained by this
// session, or nil when no candidate has passed all proofs. The source bytes
// are copied so callers cannot mutate the comparison value used by finish.
func (s *BuildHostService) LatestSuccessfulBuild() *StagedBuildArtifacts {
	if s == nil || s.latestSuccessful == nil {
		return nil
	}
	copy := *s.latestSuccessful
	copy.SourceSnapshot = append([]byte(nil), s.latestSuccessful.SourceSnapshot...)
	return &copy
}

// HandleRequest implements guest.HostServiceHandler for the build service.
// The context controls compilation and candidate validation.
func (s *BuildHostService) HandleRequest(ctx context.Context, request guest.HostRequest) (guest.Frame, error) {
	if s == nil {
		return guest.CreateHostServiceResponse(request.RequestID, BuildResponseHarnessFailure, []byte("build host service is nil"))
	}
	if request.ServiceName != "build" {
		return guest.CreateHostServiceResponse(request.RequestID, BuildResponseFailure, []byte("unknown host service: "+request.ServiceName))
	}

	var evidence *provenance.BuildAttemptEvidence
	if s.provenance != nil {
		generation := *s.generation
		var snapshot []byte
		if len(request.Arguments) == 1 {
			snapshot = request.Arguments[0]
		}
		var err error
		evidence, err = s.provenance.BeginBuild(generation, snapshot)
		if err != nil {
			return guest.CreateHostServiceResponse(request.RequestID, BuildResponseHarnessFailure, []byte("cannot record trusted build provenance"))
		}
	}
	activityData := buildAttemptActivityData(evidence)
	s.publish(observability.ActivityBuildStarted, activityData)

	if len(request.Arguments) != 1 {
		if evidence != nil {
			_ = evidence.RecordFinal("harness_failure")
		}
		return s.completedResponse(request, BuildResponseHarnessFailure, []byte("build expects exactly one source snapshot argument"), activityData)
	}

	attempt, err := os.MkdirTemp(s.stagingDirectory, "build-attempt-")
	if err != nil {
		if evidence != nil {
			_ = evidence.RecordFinal("harness_failure")
		}
		return s.completedResponse(request, BuildResponseHarnessFailure, []byte("cannot create build attempt storage"), activityData)
	}

	snapshot := append([]byte(nil), request.Arguments[0]...)
	if evidence != nil {
		if files, decodeErr := guest.DecodeSourceSnapshot(snapshot); decodeErr == nil {
			contentSize := uint64(0)
			for _, file := range files {
				contentSize += uint64(len(file.Content))
			}
			if recordErr := evidence.RecordDecoded(uint64(len(files)), contentSize); recordErr != nil {
				return s.provenanceFailure(request, activityData)
			}
		}
	}

	result := BuildSourceSnapshot(ctx, snapshot, attempt, s.buildConfig)
	s.publish(observability.ActivityBuildCompileCompleted, mapWithResult(activityData, string(result.Status)))

	status, diagnostics := BuildResponseHarnessFailure, []byte(result.Diagnostics)
	switch result.Status {
	case BuildStatusSuccess:
		status, diagnostics = s.handleBuiltCandidate(ctx, snapshot, result, evidence, activityData)
	case BuildStatusBuildFailure:
		status = BuildResponseFailure
		if evidence != nil {
			if recordErr := evidence.RecordCompileFailure("build_failure"); recordErr != nil {
				return s.provenanceFailure(request, activityData)
			}
		}
	case BuildStatusHarnessFailure:
		status = BuildResponseHarnessFailure
		if evidence != nil {
			if recordErr := evidence.RecordCompileFailure("harness_failure"); recordErr != nil {
				return s.provenanceFailure(request, activityData)
			}
		}
	default:
		status = BuildResponseHarnessFailure
		if evidence != nil {
			if recordErr := evidence.RecordFinal("harness_failure"); recordErr != nil {
				return s.provenanceFailure(request, activityData)
			}
		}
	}

	return s.completedResponse(request, status, diagnostics, activityData)
}

func (s *BuildHostService) handleBuiltCandidate(
	ctx context.Context,
	snapshot []byte,
	result BuildResult,
	evidence *provenance.BuildAttemptEvidence,
	activityData map[string]any,
) (uint32, []byte) {
	if result.KernelELF == "" || result.ISO == "" {
		if evidence != nil {
			_ = evidence.RecordFinal("harness_failure")
		}
		return BuildResponseHarnessFailure, []byte("trusted build returned no artifacts")
	}
	kernelIdentity, err := provenance.FileIdentityFromPath(result.KernelELF)
	if err != nil {
		return s.artifactProvenanceFailure()
	}
	isoIdentity, err := provenance.FileIdentityFromPath(result.ISO)
	if err != nil {
		return s.artifactProvenanceFailure()
	}
	if evidence != nil {
		if err := evidence.RecordArtifacts(kernelIdentity, isoIdentity); err != nil {
			return s.artifactProvenanceFailure()
		}
	}
	if s.candidateValidator == nil {
		if evidence != nil {
			_ = evidence.RecordFinal("harness_failure")
		}
		return BuildResponseHarnessFailure, []byte("candidate validator is not configured")
	}
	validation := s.candidateValidator.Validate(ctx, result.ISO, evidence, &isoIdentity)
	if validation.provenanceFailure {
		// Candidate evidence failures are not ordinary candidate outcomes. The
		// Python reference leaves the attempt incomplete so an untrusted or
		// partially recorded proof can never be mistaken for a completed failure.
		return s.artifactProvenanceFailure()
	}
	status := BuildResponseHarnessFailure
	switch validation.Status {
	case BuildStatusSuccess:
		if evidence != nil {
			if err := evidence.RecordLatestSuccess(snapshot); err != nil {
				return s.provenanceFailureResult()
			}
		}
		staged := &StagedBuildArtifacts{
			KernelELF:      result.KernelELF,
			ISO:            result.ISO,
			SourceSnapshot: append([]byte(nil), snapshot...),
			KernelIdentity: kernelIdentity,
			ISOIdentity:    isoIdentity,
		}
		if evidence != nil {
			staged.BuildAttemptID = evidence.AttemptID()
			var sourceErr error
			staged.SourceIdentity, sourceErr = evidence.SourceIdentity()
			if sourceErr != nil {
				return s.provenanceFailureResult()
			}
			staged.Evidence = evidence
			_ = evidence.RecordLatestSuccessUpdate()
		}
		s.latestSuccessful = staged
		return BuildResponseSuccess, []byte(validation.Diagnostics)
	case BuildStatusBuildFailure:
		status = BuildResponseFailure
	default:
		status = BuildResponseHarnessFailure
	}
	if evidence != nil {
		outcome := "harness_failure"
		if status == BuildResponseFailure {
			outcome = "build_failure"
		}
		if err := evidence.RecordFinal(outcome); err != nil {
			return s.provenanceFailureResult()
		}
	}
	return status, []byte(validation.Diagnostics)
}

func (s *BuildHostService) artifactProvenanceFailure() (uint32, []byte) {
	// The Python reference leaves the attempt incomplete when artifact
	// identities or artifact provenance cannot be recorded.
	return BuildResponseHarnessFailure, []byte("cannot record trusted build artifact provenance")
}

func (s *BuildHostService) provenanceFailureResult() (uint32, []byte) {
	return BuildResponseHarnessFailure, []byte("cannot record trusted build provenance")
}

func (s *BuildHostService) provenanceFailure(request guest.HostRequest, activityData map[string]any) (guest.Frame, error) {
	return s.completedResponse(request, BuildResponseHarnessFailure, []byte("cannot record trusted build provenance"), activityData)
}

func (s *BuildHostService) completedResponse(request guest.HostRequest, status uint32, diagnostics []byte, activityData map[string]any) (guest.Frame, error) {
	s.publish(observability.ActivityBuildCompleted, mapWithStatus(activityData, status))
	return guest.CreateHostServiceResponse(request.RequestID, status, diagnostics)
}

func (s *BuildHostService) publish(kind observability.ActivityKind, data map[string]any) {
	observability.PublishActivity(s.activityStream, s.generation, observability.ActivityHarness, kind, data, "", "", "")
}

func buildAttemptActivityData(evidence *provenance.BuildAttemptEvidence) map[string]any {
	if evidence == nil {
		return nil
	}
	return map[string]any{"build_attempt_id": evidence.AttemptID()}
}

func mapWithResult(data map[string]any, result string) map[string]any {
	output := cloneMap(data)
	output["result"] = result
	return output
}

func mapWithStatus(data map[string]any, status uint32) map[string]any {
	output := cloneMap(data)
	output["status"] = status
	return output
}

func cloneMap(data map[string]any) map[string]any {
	if len(data) == 0 {
		return map[string]any{}
	}
	output := make(map[string]any, len(data))
	for key, value := range data {
		output[key] = value
	}
	return output
}

// PendingGenerationFinish is the immutable successor selection made by an
// accepted finish request. Acceptance itself does not stop the guest or
// archive the generation.
type PendingGenerationFinish struct {
	HandoffMessage string
	SourceSnapshot []byte
	KernelELF      string
	ISO            string
}

// HostServicesConfig configures the build, finish, feature-request, and
// provided-assets services exposed by one guest session.
type HostServicesConfig struct {
	StagingDirectory    string
	CandidateValidator  *CandidateBootValidator
	BuildConfig         Config
	FeatureRequestStore *store.FeatureRequestStore
	Generation          *uint64
	EventLog            *observability.EventLog
	Metrics             *observability.Metrics
	ActivityStream      *observability.ActivityStream
	ProvidedAssets      *store.ProvidedAssets
	Provenance          *provenance.BuildReviewProvenance
}

// CodexOSHostServices dispatches the concrete host services used by the seed
// guest. Its finish state is session-local and is never inferred from files.
type CodexOSHostServices struct {
	buildService   *BuildHostService
	pendingFinish  *PendingGenerationFinish
	featureStore   *store.FeatureRequestStore
	generation     *uint64
	eventLog       *observability.EventLog
	metrics        *observability.Metrics
	providedAssets *store.ProvidedAssets
}

// NewCodexOSHostServices creates the synchronous host-service dispatcher.
func NewCodexOSHostServices(config HostServicesConfig) (*CodexOSHostServices, error) {
	if (config.FeatureRequestStore == nil) != (config.Generation == nil) {
		return nil, errors.New("feature-request store and generation must be supplied together")
	}
	buildService, err := NewBuildHostService(BuildHostServiceConfig{
		StagingDirectory:   config.StagingDirectory,
		CandidateValidator: config.CandidateValidator,
		BuildConfig:        config.BuildConfig,
		ActivityStream:     config.ActivityStream,
		Generation:         config.Generation,
		Provenance:         config.Provenance,
	})
	if err != nil {
		return nil, err
	}
	return &CodexOSHostServices{
		buildService:   buildService,
		featureStore:   config.FeatureRequestStore,
		generation:     cloneGeneration(config.Generation),
		eventLog:       config.EventLog,
		metrics:        config.Metrics,
		providedAssets: config.ProvidedAssets,
	}, nil
}

// LatestSuccessfulBuild returns the latest validated candidate retained by
// this service session.
func (s *CodexOSHostServices) LatestSuccessfulBuild() *StagedBuildArtifacts {
	if s == nil {
		return nil
	}
	return s.buildService.LatestSuccessfulBuild()
}

// PendingGenerationFinish returns a private copy of the accepted finish, if
// any.
func (s *CodexOSHostServices) PendingGenerationFinish() *PendingGenerationFinish {
	if s == nil || s.pendingFinish == nil {
		return nil
	}
	pending := *s.pendingFinish
	pending.SourceSnapshot = append([]byte(nil), s.pendingFinish.SourceSnapshot...)
	return &pending
}

// HandleRequest implements guest.HostServiceHandler for all concrete seed
// host services.
func (s *CodexOSHostServices) HandleRequest(ctx context.Context, request guest.HostRequest) (guest.Frame, error) {
	if s == nil {
		return guest.CreateHostServiceResponse(request.RequestID, BuildResponseHarnessFailure, []byte("host services are nil"))
	}
	switch request.ServiceName {
	case "build":
		if s.pendingFinish != nil {
			return guest.CreateHostServiceResponse(request.RequestID, BuildResponseHarnessFailure, []byte("build rejected after generation finish was accepted"))
		}
		return s.buildService.HandleRequest(ctx, request)
	case "finish_generation":
		return s.finishGeneration(request)
	case "request_feature":
		return s.requestFeature(request)
	case "list_provided_assets", "read_provided_asset":
		if s.providedAssets == nil {
			return guest.CreateHostServiceResponse(request.RequestID, BuildResponseFailure, []byte("provided-assets service is not configured"))
		}
		return s.providedAssets.HandleRequest(request)
	default:
		return guest.CreateHostServiceResponse(request.RequestID, BuildResponseFailure, []byte("unknown host service: "+request.ServiceName))
	}
}

func (s *CodexOSHostServices) finishGeneration(request guest.HostRequest) (guest.Frame, error) {
	if s.pendingFinish != nil {
		return finishResponse(request, FinishResponseHarnessFailure, []byte("generation finish has already been accepted"))
	}
	if len(request.Arguments) != 2 {
		return finishResponse(request, FinishResponseHarnessFailure, []byte("finish_generation expects a handoff and source snapshot"))
	}

	encodedHandoff, sourceSnapshot := request.Arguments[0], request.Arguments[1]
	if len(encodedHandoff) > maxFinishHandoffBytes {
		return finishResponse(request, FinishResponseHarnessFailure, []byte("handoff message exceeds 16 KiB"))
	}
	if !utf8.Valid(encodedHandoff) {
		return finishResponse(request, FinishResponseHarnessFailure, []byte("handoff message is not valid UTF-8"))
	}
	if _, err := guest.DecodeSourceSnapshot(sourceSnapshot); err != nil {
		return finishResponse(request, FinishResponseHarnessFailure, []byte(err.Error()))
	}

	successfulBuild := s.LatestSuccessfulBuild()
	if successfulBuild == nil {
		return finishResponse(request, FinishResponseRejected, []byte("no successful build is available"))
	}
	if !bytes.Equal(sourceSnapshot, successfulBuild.SourceSnapshot) {
		return finishResponse(request, FinishResponseRejected, []byte("current source differs from the latest successful build"))
	}
	s.pendingFinish = &PendingGenerationFinish{
		HandoffMessage: string(encodedHandoff),
		SourceSnapshot: append([]byte(nil), sourceSnapshot...),
		KernelELF:      successfulBuild.KernelELF,
		ISO:            successfulBuild.ISO,
	}
	return finishResponse(request, FinishResponseAccepted, nil)
}

func (s *CodexOSHostServices) requestFeature(request guest.HostRequest) (guest.Frame, error) {
	if s.pendingFinish != nil {
		return finishResponse(request, FeatureResponseHarnessFailure, []byte("feature request rejected after generation finish was accepted"))
	}
	if len(request.Arguments) != 2 {
		return finishResponse(request, FeatureResponseHarnessFailure, []byte("request_feature expects a title and description"))
	}
	titleBytes, descriptionBytes := request.Arguments[0], request.Arguments[1]
	if len(titleBytes) == 0 {
		return finishResponse(request, FeatureResponseHarnessFailure, []byte("feature-request title must not be empty"))
	}
	if len(titleBytes) > store.MaxFeatureTitleBytes {
		return finishResponse(request, FeatureResponseHarnessFailure, []byte("feature-request title exceeds 256 bytes"))
	}
	if len(descriptionBytes) > store.MaxFeatureDescriptionBytes {
		return finishResponse(request, FeatureResponseHarnessFailure, []byte("feature-request description exceeds 16 KiB"))
	}
	if !utf8.Valid(titleBytes) || !utf8.Valid(descriptionBytes) {
		return finishResponse(request, FeatureResponseHarnessFailure, []byte("feature-request text is not valid UTF-8"))
	}
	if s.featureStore == nil || s.generation == nil {
		return finishResponse(request, FeatureResponseHarnessFailure, []byte("feature-request service is not configured"))
	}
	recorded, err := s.featureStore.Create(*s.generation, string(titleBytes), string(descriptionBytes))
	if err != nil {
		return finishResponse(request, FeatureResponseHarnessFailure, boundedFeatureDiagnostic(err.Error()))
	}
	data := map[string]any{
		"request_id":         recorded.ID,
		"request_generation": recorded.Generation,
		"title":              recorded.Title,
	}
	if s.eventLog != nil {
		s.eventLog.Record("feature_requested", s.generation, data)
	}
	if s.metrics != nil {
		s.metrics.Record("feature_requested", data)
		if requests, requestsErr := s.featureStore.Requests(); requestsErr == nil {
			pending := 0
			for _, item := range requests {
				if item.Status == store.FeaturePending {
					pending++
				}
			}
			s.metrics.SetFeatureRequestsPending(pending)
		}
	}
	return finishResponse(request, FeatureResponseRecorded, []byte(fmt.Sprintf("%d", recorded.ID)))
}

func finishResponse(request guest.HostRequest, status uint32, output []byte) (guest.Frame, error) {
	return guest.CreateHostServiceResponse(request.RequestID, status, output)
}

func boundedFeatureDiagnostic(value string) []byte {
	encoded := []byte(value)
	if len(encoded) <= maxFeatureDiagnostic {
		return encoded
	}
	return []byte(strings.ToValidUTF8(string(encoded[:maxFeatureDiagnostic]), ""))
}
