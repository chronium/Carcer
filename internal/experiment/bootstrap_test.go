package experiment

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codexos/internal/bootstrap"
)

func TestBootstrapGateAndArchiveReferences(t *testing.T) {
	runDir := t.TempDir()
	legacy := capacityArchive(t, runDir, 0, 0, 1)
	before := archiveBytes(t, legacy.ArchivePath)
	r, e := NewCodexOSRun(runDir)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.requireBootstrapGate(); e == nil {
		t.Fatal("unvalidated gate accepted")
	}
	if e = r.ReopenAtGate(); e != nil {
		t.Fatal(e)
	}
	if e = r.requireBootstrapGate(); e != nil {
		t.Fatal(e)
	}
	if !r.RetainGenerationFinish(0) {
		t.Fatal("retain failed")
	}
	if e = r.requireBootstrapGate(); e == nil {
		t.Fatal("retained interview admitted provisioning")
	}
	r.ReleaseGenerationFinish(0)
	if e = bootstrap.Provision(runDir, filepath.Join(t.TempDir(), "artifacts"), "tcc"); e != nil {
		t.Fatal(e)
	}
	zero := uint64(0)
	svc, e := bootstrap.NewService(runDir, 1, &zero, []bootstrap.Asset{{ID: "tcc", SHA256: bootstrap.TCCSHA256}}, nil)
	if e != nil || svc == nil {
		t.Fatal(e)
	}
	archive := capacityArchive(t, runDir, 1, 0, 1)
	if archive.Bootstrap == nil || archive.Bootstrap.Generation != 1 || archive.Bootstrap.Limits != bootstrap.Baseline() {
		t.Fatalf("missing archive limits: %+v", archive)
	}
	reopened := capacityGate(t, runDir)
	if _, e = reopened.InspectGeneration(1); e != nil {
		t.Fatal(e)
	}
	if !sameArchiveBytes(before, archiveBytes(t, legacy.ArchivePath)) {
		t.Fatal("legacy archive changed")
	}
	// Historical references are validated before reopening, even when no job is
	// about to run. A forged owner cannot authorize another run's artifacts.
	path := filepath.Join(archive.ArchivePath, bootstrap.ReferencesFilename)
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	b = append(b, []byte("{}")...)
	if e = os.WriteFile(path, b, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = InspectGeneration(runDir, 1); e == nil {
		t.Fatal("malformed artifact references accepted")
	}
}
func TestBootstrapProvisioningRejectsActiveOrMissingPinsWithoutWorker(t *testing.T) {
	runDir := t.TempDir()
	capacityArchive(t, runDir, 0, 0, 1)
	r, e := NewLiveCodexOSRun(runDir, LiveRunOptions{})
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	if e = r.ProvisionBootstrap(context.Background(), "tcc"); e == nil {
		t.Fatal("provisioned without gate")
	}
	if e = r.ReopenAtGate(); e != nil {
		t.Fatal(e)
	}
	if e = r.ProvisionBootstrap(context.Background(), "tcc"); e == nil {
		t.Fatal("provisioned missing pinned asset")
	}
	if c, e := bootstrap.LoadConfig(runDir); e != nil || c != nil {
		t.Fatalf("failed provisioning published config %+v %v", c, e)
	}
}

func TestBootstrapStatusSeparatesExecutionFromZeroRetainedJobs(t *testing.T) {
	runDir := t.TempDir()
	r, err := NewCodexOSRun(runDir)
	if err != nil {
		t.Fatal(err)
	}
	status, err := r.BootstrapStatus()
	if err != nil || status != "Bootstrap service: not provisioned." {
		t.Fatalf("status=%q err=%v", status, err)
	}
	if err := bootstrap.Provision(runDir, filepath.Join(t.TempDir(), "artifacts"), "tcc"); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{true, false} {
		config, err := bootstrap.LoadConfig(runDir)
		if err != nil {
			t.Fatal(err)
		}
		config.Enabled = enabled
		encoded, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runDir, "bootstrap-service.json"), encoded, 0600); err != nil {
			t.Fatal(err)
		}
		status, err := r.BootstrapStatus()
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"retained job references=0", "limits=", "does not fulfill feature request #3"} {
			if !strings.Contains(status, want) {
				t.Fatalf("status missing %q: %s", want, status)
			}
		}
		if enabled {
			for _, want := range []string{"execution is enabled", "permits new jobs", "without a separate batch grant", "no previous jobs exist"} {
				if !strings.Contains(status, want) {
					t.Fatalf("status missing %q: %s", want, status)
				}
			}
		} else if !strings.Contains(status, "execution is disabled; new jobs are not permitted") || strings.Contains(status, "permits new jobs") {
			t.Fatalf("disabled status: %s", status)
		}
	}
}
