package build

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codexos/internal/guest"
	"codexos/internal/qemu"
	"codexos/internal/sourcecapacity"
)

func TestSourceCapacityReviewBuildAndFinishAgreement(t *testing.T) {
	config, files := syntheticBuildFixture(t)
	size := 0
	for _, file := range files {
		size += len(file.Content)
	}
	files = append(files, guest.SnapshotFile{Path: "seed/padding.txt", Content: make([]byte, sourcecapacity.Expanded-size)})
	snapshot, err := guest.NewCanonicalSourceSnapshotWithBudget(files, sourcecapacity.Expanded)
	if err != nil {
		t.Fatal(err)
	}
	root := candidateRoot(t)
	t.Setenv(candidateHelperEnvironment, "success")
	configureCandidateHelperPID(t, root)
	validator, err := NewCandidateBootValidator(CandidateBootConfig{QEMUExecutable: candidateHelperExecutable(t), HardwareProfile: qemu.TestHardwareProfile, ReadyTimeout: 2 * time.Second, TemporaryParent: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, budget := range []sourcecapacity.Budget{0, sourcecapacity.Expanded} {
		config.SourceCapacity = budget
		services, err := NewCodexOSHostServices(HostServicesConfig{StagingDirectory: filepath.Join(t.TempDir(), "builds"), BuildConfig: config, CandidateValidator: validator})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		response, err := services.HandleRequest(ctx, guest.HostRequest{RequestID: 1, ServiceName: "build", Arguments: [][]byte{snapshot.Bytes()}})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		status, output := decodeHostResponse(t, response)
		if budget == 0 {
			if status != BuildResponseHarnessFailure || !strings.Contains(string(output), "64 KiB") {
				t.Fatalf("default build: %d %s", status, output)
			}
			if services.LatestSuccessfulBuild() != nil {
				t.Fatal("rejected build published success")
			}
			continue
		}
		if status != BuildResponseSuccess {
			t.Fatalf("expanded build: %d %s", status, output)
		}
		built := services.LatestSuccessfulBuild()
		if built == nil || !bytes.Equal(built.SourceSnapshot, snapshot.Bytes()) {
			t.Fatal("build differs from review snapshot")
		}
		response, err = services.HandleRequest(context.Background(), guest.HostRequest{RequestID: 2, ServiceName: "finish_generation", Arguments: [][]byte{[]byte("handoff"), snapshot.Bytes()}})
		if err != nil {
			t.Fatal(err)
		}
		status, output = decodeHostResponse(t, response)
		if status != 0 {
			t.Fatalf("expanded finish: %d %s", status, output)
		}
		pending := services.PendingGenerationFinish()
		if pending == nil || !bytes.Equal(pending.SourceSnapshot, snapshot.Bytes()) {
			t.Fatal("finish did not preserve exact reviewed/built snapshot")
		}
	}
	assertCandidateHelperReaped(t)
}
