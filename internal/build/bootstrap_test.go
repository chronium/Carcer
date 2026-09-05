package build

import (
	"context"
	"testing"

	"codexos/internal/guest"
)

func TestBootstrapServicesAreDisabledByDefaultAndRejectAfterFinish(t *testing.T) {
	config, _ := syntheticBuildFixture(t)
	s, e := NewCodexOSHostServices(HostServicesConfig{StagingDirectory: t.TempDir(), BuildConfig: config})
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"bootstrap_job", "read_bootstrap_artifact"} {
		frame, e := s.HandleRequest(context.Background(), guest.HostRequest{RequestID: 17, ServiceName: name})
		if e != nil {
			t.Fatal(e)
		}
		status, _ := decodeHostResponse(t, frame)
		if status != 2 || frame.RequestID != 17 {
			t.Fatalf("disabled %s status=%d", name, status)
		}
	}
	s.pendingFinish = &PendingGenerationFinish{}
	frame, e := s.HandleRequest(context.Background(), guest.HostRequest{RequestID: 18, ServiceName: "bootstrap_job"})
	if e != nil {
		t.Fatal(e)
	}
	status, body := decodeHostResponse(t, frame)
	if status != 2 || string(body) != "bootstrap job rejected after generation finish" {
		t.Fatalf("after finish %d %q", status, body)
	}
	if s.LatestSuccessfulBuild() != nil {
		t.Fatal("bootstrap affected trusted successor validation")
	}
}
