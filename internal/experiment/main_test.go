package experiment

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv(liveQEMUHelperEnvironment) != "" {
		os.Exit(runLiveQEMUHelper())
	}
	os.Exit(m.Run())
}
