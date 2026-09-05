package experiment

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "--bootstrap-runtime-worker" {
		os.Exit(bootstrapRuntimeWorker(os.Args[2]))
	}
	if os.Getenv(liveQEMUHelperEnvironment) != "" {
		os.Exit(runLiveQEMUHelper())
	}
	os.Exit(m.Run())
}
