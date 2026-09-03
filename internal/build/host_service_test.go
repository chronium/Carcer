package build

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codexos/internal/guest"
	"codexos/internal/observability"
	"codexos/internal/provenance"
	"codexos/internal/qemu"
	"codexos/internal/store"
)

func TestBuildHostServiceValidatesRequestsAndRequiresCandidateProof(t *testing.T) {
	config, files := syntheticBuildFixture(t)
	generation := uint64(8)
	activity := observability.NewActivityStream()
	service, err := NewBuildHostService(BuildHostServiceConfig{
		StagingDirectory: filepath.Join(t.TempDir(), "staging"),
		BuildConfig:      config,
		ActivityStream:   activity,
		Generation:       &generation,
	})
	if err != nil {
		t.Fatal(err)
	}

	invalid, err := service.HandleRequest(context.Background(), guest.HostRequest{
		RequestID: 41, ServiceName: "build", Arguments: [][]byte{{1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, output := decodeHostResponse(t, invalid)
	if status != BuildResponseHarnessFailure || !bytes.Contains(output, []byte("truncated")) {
		t.Fatalf("invalid build response = %d, %q", status, output)
	}
	if service.LatestSuccessfulBuild() != nil {
		t.Fatal("invalid build produced a latest success")
	}

	unknown, err := service.HandleRequest(context.Background(), guest.HostRequest{
		RequestID: 42, ServiceName: "unknown", Arguments: [][]byte{{1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := decodeHostResponse(t, unknown); status != BuildResponseFailure {
		t.Fatalf("unknown service status = %d", status)
	}

	snapshot, err := guest.EncodeSourceSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	built, err := service.HandleRequest(context.Background(), guest.HostRequest{
		RequestID: 43, ServiceName: "build", Arguments: [][]byte{snapshot},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, output = decodeHostResponse(t, built)
	if status != BuildResponseHarnessFailure || !bytes.Contains(output, []byte("candidate validator is not configured")) {
		t.Fatalf("unvalidated build response = %d, %q", status, output)
	}
	if service.LatestSuccessfulBuild() != nil {
		t.Fatal("build without candidate proof became latest success")
	}

	events := activity.Drain()
	wantKinds := []observability.ActivityKind{
		observability.ActivityBuildStarted,
		observability.ActivityBuildCompileCompleted,
		observability.ActivityBuildCompleted,
		observability.ActivityBuildStarted,
		observability.ActivityBuildCompileCompleted,
		observability.ActivityBuildCompleted,
	}
	if len(events) != len(wantKinds) {
		t.Fatalf("activity events = %#v", events)
	}
	for index, event := range events {
		if event.Kind != wantKinds[index] || event.Generation == nil || *event.Generation != generation || event.Role != observability.ActivityHarness {
			t.Fatalf("activity[%d] = %#v", index, event)
		}
	}
}

func TestCodexOSHostServicesFreezesMatchingFinishAndRoutesFeatures(t *testing.T) {
	root := t.TempDir()
	featureStore, err := store.NewFeatureRequestStore(filepath.Join(root, "run"))
	if err != nil {
		t.Fatal(err)
	}
	generation := uint64(7)
	featuresRecorded := 0
	services, err := NewCodexOSHostServices(HostServicesConfig{
		StagingDirectory:    filepath.Join(root, "staging"),
		FeatureRequestStore: featureStore,
		FeatureRecorded:     func() { featuresRecorded++ },
		Generation:          &generation,
	})
	if err != nil {
		t.Fatal(err)
	}

	files := []guest.SnapshotFile{{Path: "seed/kernel.c", Content: []byte("kernel")}}
	snapshot, err := guest.EncodeSourceSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	// The build service is deliberately populated with an already validated
	// candidate here so this test exercises finish state independently of the
	// external QEMU availability required by candidate boot tests.
	kernel := filepath.Join(root, "kernel.elf")
	iso := filepath.Join(root, "codexos.iso")
	if err := os.WriteFile(kernel, []byte("kernel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(iso, []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	services.buildService.latestSuccessful = &StagedBuildArtifacts{
		KernelELF: kernel, ISO: iso, SourceSnapshot: append([]byte(nil), snapshot...),
	}

	feature, err := services.HandleRequest(context.Background(), guest.HostRequest{
		RequestID: 59, ServiceName: "request_feature",
		Arguments: [][]byte{[]byte("Δυνατότητα"), []byte("Needed externally. λ")},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, output := decodeHostResponse(t, feature)
	if status != FeatureResponseRecorded || string(output) != "1" {
		t.Fatalf("feature response = %d, %q", status, output)
	}
	recorded, err := featureStore.Request(1)
	if err != nil || recorded.Generation != generation || recorded.Title != "Δυνατότητα" {
		t.Fatalf("recorded feature = %#v, %v", recorded, err)
	}
	if featuresRecorded != 1 {
		t.Fatalf("feature-recorded callbacks = %d, want 1", featuresRecorded)
	}

	handoff := []byte("Continue from the validated successor. λ")
	finish, err := services.HandleRequest(context.Background(), guest.HostRequest{
		RequestID: 61, ServiceName: "finish_generation", Arguments: [][]byte{handoff, snapshot},
	})
	if err != nil {
		t.Fatal(err)
	}
	if status, output := decodeHostResponse(t, finish); status != FinishResponseAccepted || len(output) != 0 {
		t.Fatalf("finish response = %d, %q", status, output)
	}
	pending := services.PendingGenerationFinish()
	if pending == nil || pending.HandoffMessage != string(handoff) || !bytes.Equal(pending.SourceSnapshot, snapshot) || pending.KernelELF != kernel || pending.ISO != iso {
		t.Fatalf("pending finish = %#v", pending)
	}

	second, err := services.HandleRequest(context.Background(), guest.HostRequest{
		RequestID: 62, ServiceName: "finish_generation", Arguments: [][]byte{[]byte("replacement"), snapshot},
	})
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := decodeHostResponse(t, second); status != FinishResponseHarnessFailure {
		t.Fatalf("second finish status = %d", status)
	}
	laterBuild, err := services.HandleRequest(context.Background(), guest.HostRequest{
		RequestID: 63, ServiceName: "build", Arguments: [][]byte{snapshot},
	})
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := decodeHostResponse(t, laterBuild); status != BuildResponseHarnessFailure {
		t.Fatalf("build after finish status = %d", status)
	}
	frozenFeature, err := services.HandleRequest(context.Background(), guest.HostRequest{
		RequestID: 64, ServiceName: "request_feature", Arguments: [][]byte{[]byte("too late"), []byte("must not persist")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := decodeHostResponse(t, frozenFeature); status != FeatureResponseHarnessFailure {
		t.Fatalf("feature after finish status = %d", status)
	}
	if requests, err := featureStore.Requests(); err != nil || len(requests) != 1 {
		t.Fatalf("feature records after finish = %#v, %v", requests, err)
	}
	if featuresRecorded != 1 {
		t.Fatalf("rejected feature changed callback count to %d", featuresRecorded)
	}
}

func TestBuildHostServiceLeavesFailedCandidateAttemptWithForensicIdentities(t *testing.T) {
	config, files := syntheticBuildFixture(t)
	snapshot, err := guest.EncodeSourceSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	generation := uint64(12)
	service, err := NewBuildHostService(BuildHostServiceConfig{
		StagingDirectory:   filepath.Join(root, "staging"),
		CandidateValidator: nil,
		BuildConfig:        config,
		Generation:         &generation,
		Provenance:         provenance.NewBuildReviewProvenance(filepath.Join(root, "run")),
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.HandleRequest(context.Background(), guest.HostRequest{
		RequestID: 1, ServiceName: "build", Arguments: [][]byte{snapshot},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, output := decodeHostResponse(t, response)
	if status != BuildResponseHarnessFailure || !bytes.Contains(output, []byte("candidate validator")) {
		t.Fatalf("build response = %d, %q", status, output)
	}
	manifestPath := filepath.Join(root, "run", "build-review-provenance", "generation-0012", "build-000001", "manifest.json")
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest["outcome"] != "harness_failure" || manifest["stage"] != "completed" {
		t.Fatalf("attempt manifest = %#v", manifest)
	}
	source, ok := manifest["source_snapshot"].(map[string]any)
	if !ok || source["decoded"] != true || source["file_count"] != float64(len(files)) {
		t.Fatalf("source provenance = %#v", manifest["source_snapshot"])
	}
	if _, ok := manifest["latest_success"]; ok {
		t.Fatal("failed candidate claimed latest success")
	}
}

func TestBuildHostServiceRecordsPresentEmptySourceSnapshot(t *testing.T) {
	root := t.TempDir()
	generation := uint64(13)
	service, err := NewBuildHostService(BuildHostServiceConfig{
		StagingDirectory: filepath.Join(root, "staging"),
		Generation:       &generation,
		Provenance:       provenance.NewBuildReviewProvenance(filepath.Join(root, "run")),
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.HandleRequest(context.Background(), guest.HostRequest{
		RequestID: 2, ServiceName: "build", Arguments: [][]byte{[]byte{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := decodeHostResponse(t, response); status != BuildResponseHarnessFailure {
		t.Fatalf("empty build status = %d, want harness failure", status)
	}
	manifestPath := filepath.Join(root, "run", "build-review-provenance", "generation-0013", "build-000001", "manifest.json")
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	source, ok := manifest["source_snapshot"].(map[string]any)
	if !ok || source["size"] != float64(0) || source["sha256"] == "" {
		t.Fatalf("present empty source provenance = %#v", manifest["source_snapshot"])
	}
}

func TestBuildHostServiceRetainsValidatedSuccessAfterLaterBuildFailure(t *testing.T) {
	config, files := syntheticBuildFixture(t)
	root := candidateRoot(t)
	t.Setenv(candidateHelperEnvironment, "success")
	configureCandidateHelperPID(t, root)
	validator, err := NewCandidateBootValidator(CandidateBootConfig{
		QEMUExecutable:  candidateHelperExecutable(t),
		HardwareProfile: qemu.TestHardwareProfile,
		ReadyTimeout:    time.Second,
		TemporaryParent: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	generation := uint64(8)
	service, err := NewBuildHostService(BuildHostServiceConfig{
		StagingDirectory:   filepath.Join(root, "staging"),
		CandidateValidator: validator,
		BuildConfig:        config,
		Generation:         &generation,
		Provenance:         provenance.NewBuildReviewProvenance(filepath.Join(root, "run")),
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := guest.EncodeSourceSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.HandleRequest(context.Background(), guest.HostRequest{
		RequestID: 90, ServiceName: "build", Arguments: [][]byte{snapshot},
	})
	if err != nil {
		t.Fatal(err)
	}
	if status, output := decodeHostResponse(t, response); status != BuildResponseSuccess {
		t.Fatalf("validated build response = %d, %q", status, output)
	}
	validated := service.LatestSuccessfulBuild()
	if validated == nil || !bytes.Equal(validated.SourceSnapshot, snapshot) || validated.BuildAttemptID != "build-000001" || validated.SourceIdentity.Size != uint64(len(snapshot)) {
		t.Fatalf("latest validated build = %#v", validated)
	}
	manifestPath := filepath.Join(root, "run", "build-review-provenance", "generation-0008", "build-000001", "manifest.json")
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest["outcome"] != "success" || manifest["stage"] != "latest_success" {
		t.Fatalf("successful build provenance = %#v", manifest)
	}
	if info, err := os.Stat(validated.KernelELF); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("validated kernel = %q: %v", validated.KernelELF, err)
	}
	if info, err := os.Stat(validated.ISO); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("validated ISO = %q: %v", validated.ISO, err)
	}
	assertCandidateHelperReaped(t)

	writeExecutable(t, "#!/bin/sh\necho 'kernel.c: error: forced failure' >&2\nexit 1\n", config.Tools.Xorriso)
	failed, err := service.HandleRequest(context.Background(), guest.HostRequest{
		RequestID: 91, ServiceName: "build", Arguments: [][]byte{snapshot},
	})
	if err != nil {
		t.Fatal(err)
	}
	if status, output := decodeHostResponse(t, failed); status != BuildResponseFailure || !bytes.Contains(output, []byte("forced failure")) {
		t.Fatalf("failed build response = %d, %q", status, output)
	}
	retained := service.LatestSuccessfulBuild()
	if retained == nil || retained.KernelELF != validated.KernelELF || retained.ISO != validated.ISO || !bytes.Equal(retained.SourceSnapshot, snapshot) {
		t.Fatalf("latest success changed after failure: %#v", retained)
	}
}

func TestCodexOSHostServicesRoutesFrozenProvidedAssets(t *testing.T) {
	root := t.TempDir()
	assetDirectory := filepath.Join(root, "assets", "alpha")
	if err := os.MkdirAll(assetDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assetDirectory, "payload.bin"), []byte("frozen payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	assets, err := store.LoadProvidedAssets(filepath.Join(root, "assets"))
	if err != nil {
		t.Fatal(err)
	}
	services, err := NewCodexOSHostServices(HostServicesConfig{
		StagingDirectory: filepath.Join(root, "staging"),
		ProvidedAssets:   assets,
	})
	if err != nil {
		t.Fatal(err)
	}
	list, err := services.HandleRequest(context.Background(), guest.HostRequest{
		RequestID: 92, ServiceName: "list_provided_assets",
	})
	if err != nil {
		t.Fatal(err)
	}
	if status, output := decodeHostResponse(t, list); status != 0 || !bytes.Contains(output, []byte("alpha\tpayload.bin\t14\t")) {
		t.Fatalf("provided-assets list = %d, %q", status, output)
	}

	unconfigured, err := NewCodexOSHostServices(HostServicesConfig{StagingDirectory: filepath.Join(root, "other")})
	if err != nil {
		t.Fatal(err)
	}
	response, err := unconfigured.HandleRequest(context.Background(), guest.HostRequest{
		RequestID: 93, ServiceName: "list_provided_assets",
	})
	if err != nil {
		t.Fatal(err)
	}
	if status, output := decodeHostResponse(t, response); status != BuildResponseFailure || string(output) != "provided-assets service is not configured" {
		t.Fatalf("unconfigured provided-assets response = %d, %q", status, output)
	}
}

func TestCodexOSHostServicesRejectsInvalidFinishAndFeatureArguments(t *testing.T) {
	services, err := NewCodexOSHostServices(HostServicesConfig{StagingDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	validSnapshot, err := guest.EncodeSourceSnapshot([]guest.SnapshotFile{{Path: "seed/kernel.c", Content: []byte("kernel")}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		args [][]byte
		want uint32
	}{
		{name: "no build", args: [][]byte{[]byte("handoff"), validSnapshot}, want: FinishResponseRejected},
		{name: "invalid utf8", args: [][]byte{{0xff}, validSnapshot}, want: FinishResponseHarnessFailure},
		{name: "oversized handoff", args: [][]byte{bytes.Repeat([]byte("x"), maxFinishHandoffBytes+1), validSnapshot}, want: FinishResponseHarnessFailure},
		{name: "malformed snapshot", args: [][]byte{nil, []byte{1}}, want: FinishResponseHarnessFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			response, err := services.HandleRequest(context.Background(), guest.HostRequest{RequestID: 70, ServiceName: "finish_generation", Arguments: test.args})
			if err != nil {
				t.Fatal(err)
			}
			if status, _ := decodeHostResponse(t, response); status != test.want {
				t.Fatalf("status = %d, want %d", status, test.want)
			}
		})
	}

	storeRoot := filepath.Join(t.TempDir(), "run")
	featureStore, err := store.NewFeatureRequestStore(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	generation := uint64(4)
	services, err = NewCodexOSHostServices(HostServicesConfig{
		StagingDirectory:    t.TempDir(),
		FeatureRequestStore: featureStore,
		Generation:          &generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	invalidArguments := [][][]byte{
		nil,
		{[]byte("title")},
		{nil, []byte("description")},
		{{0xff}, []byte("description")},
		{[]byte("title"), {0xff}},
		{[]byte(strings.Repeat("x", store.MaxFeatureTitleBytes+1)), []byte("description")},
		{[]byte("title"), []byte(strings.Repeat("x", store.MaxFeatureDescriptionBytes+1))},
	}
	for index, arguments := range invalidArguments {
		response, err := services.HandleRequest(context.Background(), guest.HostRequest{RequestID: uint32(80 + index), ServiceName: "request_feature", Arguments: arguments})
		if err != nil {
			t.Fatal(err)
		}
		if status, _ := decodeHostResponse(t, response); status != FeatureResponseHarnessFailure {
			t.Fatalf("feature[%d] status = %d", index, status)
		}
	}
	if requests, err := featureStore.Requests(); err != nil || len(requests) != 0 {
		t.Fatalf("invalid feature records = %#v, %v", requests, err)
	}
}

func decodeHostResponse(t *testing.T, frame guest.Frame) (uint32, []byte) {
	t.Helper()
	if frame.MessageType != guest.HostServiceResponse {
		t.Fatalf("response message type = 0x%04x", frame.MessageType)
	}
	if len(frame.Payload) < 4 {
		t.Fatalf("response payload has no status: %d", len(frame.Payload))
	}
	return binary.LittleEndian.Uint32(frame.Payload[:4]), frame.Payload[4:]
}
