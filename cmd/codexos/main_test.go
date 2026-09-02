package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecuteDistinguishesUsageAndStartupFailures(t *testing.T) {
	t.Run("usage", func(t *testing.T) {
		var output bytes.Buffer
		var errorOutput bytes.Buffer
		code := execute([]string{"--resume-at-gate"}, strings.NewReader(""), &output, &errorOutput)
		if code != 2 {
			t.Fatalf("exit code = %d, want 2", code)
		}
		if output.Len() != 0 || !strings.Contains(errorOutput.String(), "required flag") {
			t.Fatalf("stdout=%q stderr=%q", output.String(), errorOutput.String())
		}
	})

	t.Run("startup", func(t *testing.T) {
		var output bytes.Buffer
		var errorOutput bytes.Buffer
		runDirectory := filepath.Join(t.TempDir(), "empty-run")
		code := execute([]string{
			"--run-directory", runDirectory,
			"--resume-at-gate",
			"--plain",
		}, strings.NewReader(""), &output, &errorOutput)
		if code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		if !strings.Contains(output.String(), "Error: failed to start CodexOS: run has no archived generation gate") {
			t.Fatalf("stdout=%q", output.String())
		}
		if errorOutput.Len() != 0 {
			t.Fatalf("startup failure wrote stderr: %q", errorOutput.String())
		}
	})
}

func TestExecuteHelpSucceedsWithoutStartingRunner(t *testing.T) {
	var output bytes.Buffer
	var errorOutput bytes.Buffer
	if code := execute([]string{"--help"}, strings.NewReader(""), &output, &errorOutput); code != 0 {
		t.Fatalf("exit code = %d, stderr=%q", code, errorOutput.String())
	}
	if !strings.Contains(output.String(), "CodexOS operator console") {
		t.Fatalf("help output = %q", output.String())
	}
}
