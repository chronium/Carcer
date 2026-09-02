package operator

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"codexos/internal/experiment"
	"codexos/internal/observability"
	"codexos/internal/qemu"
	"codexos/internal/store"
)

func TestRunnerStartsPausesAndAbortsDisposableGeneration(t *testing.T) {
	qemuExecutable := buildDisposableRunnerQEMU(t)
	runDirectory, err := os.MkdirTemp("/tmp", "codexos-runner-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDirectory) })
	initialISO := filepath.Join(t.TempDir(), "initial.iso")
	if err := os.WriteFile(initialISO, []byte("disposable initial image"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var output bytes.Buffer
	err = runWithIOConfigured(ctx, Options{
		RunDirectory: runDirectory,
		InitialISO:   initialISO,
	}, strings.NewReader("status\npause\nabort\ny\nquit\n"), &output, runnerConfiguration{
		live: experiment.LiveRunOptions{
			QEMUExecutable:  qemuExecutable,
			HardwareProfile: qemu.TestHardwareProfile,
			ReadyTimeout:    2 * time.Second,
		},
	})
	if err != nil {
		t.Fatalf("run disposable generation: %v\n%s", err, output.String())
	}
	if ctx.Err() != nil {
		t.Fatalf("runner exceeded acceptance deadline: %v", ctx.Err())
	}
	for _, want := range []string{
		"Generation 0: RUNNING",
		"Hardware profile: test-v1",
		"Generation 0 paused.",
		"Generation 0 aborted.",
		"No successor was selected.",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("operator output missing %q:\n%s", want, output.String())
		}
	}
	loaded, err := experiment.NewCodexOSRun(runDirectory)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := loaded.InspectGeneration(0)
	if err != nil {
		t.Fatal(err)
	}
	if archive.Outcome != "aborted" || archive.Transition != "initial" {
		t.Fatalf("archive = %#v", archive)
	}
	workspaces, err := filepath.Glob(filepath.Join(runDirectory, ".generation-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(workspaces) != 0 {
		t.Fatalf("runner left generation workspaces: %v", workspaces)
	}
	events, err := os.ReadFile(filepath.Join(runDirectory, observability.EventLogFilename))
	if err != nil {
		t.Fatal(err)
	}
	previous := -1
	for _, event := range []string{"run_started", "generation_paused", "generation_aborted", "run_stopped"} {
		index := bytes.Index(events, []byte(`"event":"`+event+`"`))
		if index < 0 || index <= previous {
			t.Fatalf("event %q missing or out of order:\n%s", event, events)
		}
		previous = index
	}
}

func buildDisposableRunnerQEMU(t *testing.T) string {
	t.Helper()
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), "fake-qemu")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "build", "-o", executable, "./internal/operator/testdata/fakeqemu")
	command.Dir = repositoryRoot
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build disposable QEMU fixture: %v\n%s", err, output)
	}
	return executable
}

func TestRunnerReopensArchivedGateThroughPlainConsole(t *testing.T) {
	runDirectory := t.TempDir()
	hardware, err := qemu.TestHardwareProfile.Manifest("QEMU emulator version runner-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := experiment.WriteAbortedArchive(runDirectory, experiment.AbortedArchive{
		Generation: 0,
		Transition: "initial",
		Hardware:   hardware,
		BootISO:    []byte("archived boot image"),
	}); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	err = runWithIO(context.Background(), Options{
		RunDirectory: runDirectory,
		ResumeAtGate: true,
	}, strings.NewReader("quit\n"), &output)
	if err != nil {
		t.Fatalf("run archived gate: %v", err)
	}
	for _, want := range []string{
		"CodexOS operator console",
		"Generation 0 aborted.",
		"No successor was selected.",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("operator output missing %q:\n%s", want, output.String())
		}
	}
	events, err := os.ReadFile(filepath.Join(runDirectory, observability.EventLogFilename))
	if err != nil {
		t.Fatal(err)
	}
	reopened := bytes.Index(events, []byte(`"event":"run_reopened_at_gate"`))
	stopped := bytes.Index(events, []byte(`"event":"run_stopped"`))
	if reopened < 0 || stopped < 0 || reopened >= stopped {
		t.Fatalf("startup/shutdown events are out of order:\n%s", events)
	}
}

func TestRunnerWrapsStartupFailuresWithoutStartingProcesses(t *testing.T) {
	tests := []struct {
		name    string
		options func(string) Options
		want    string
	}{
		{
			name: "empty gate",
			options: func(run string) Options {
				return Options{RunDirectory: run, ResumeAtGate: true}
			},
			want: "run has no archived generation gate",
		},
		{
			name: "missing initial ISO",
			options: func(run string) Options {
				return Options{RunDirectory: run, InitialISO: filepath.Join(run, "missing.iso")}
			},
			want: "initial ISO is unavailable",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runDirectory := filepath.Join(t.TempDir(), "run")
			err := runWithIO(context.Background(), test.options(runDirectory), strings.NewReader(""), &bytes.Buffer{})
			var startupError *StartupError
			if !errors.As(err, &startupError) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want startup error containing %q", err, test.want)
			}
			matches, globErr := filepath.Glob(filepath.Join(runDirectory, ".generation-*"))
			if globErr != nil {
				t.Fatal(globErr)
			}
			if len(matches) != 0 {
				t.Fatalf("startup failure left generation workspaces: %v", matches)
			}
		})
	}
}

func TestRunnerRejectsInvalidInheritanceBeforeCreatingRunDirectory(t *testing.T) {
	tests := []struct {
		name    string
		options Options
		want    string
	}{
		{
			name: "resume",
			options: Options{
				ResumeAtGate:         true,
				InheritanceRequested: true,
				GitConfigured:        true,
			},
			want: "cross-run inheritance is valid only with --initial-iso",
		},
		{
			name: "missing Git provenance",
			options: Options{
				InitialISO:           "seed.iso",
				InheritanceRequested: true,
			},
			want: "cross-run inheritance requires Git provenance options",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runDirectory := filepath.Join(t.TempDir(), "run")
			test.options.RunDirectory = runDirectory
			err := runWithIO(context.Background(), test.options, strings.NewReader(""), &bytes.Buffer{})
			var startupError *StartupError
			if !errors.As(err, &startupError) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want startup error containing %q", err, test.want)
			}
			if _, statErr := os.Stat(runDirectory); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("invalid inheritance mutated run directory: %v", statErr)
			}
		})
	}
}

func TestRunnerRejectsOtherInvalidOptionsBeforeCreatingRunDirectory(t *testing.T) {
	tests := []struct {
		name    string
		options Options
		want    string
	}{
		{name: "missing opening", options: Options{}, want: "exactly one of --initial-iso and --resume-at-gate"},
		{
			name:    "two openings",
			options: Options{InitialISO: "seed.iso", ResumeAtGate: true},
			want:    "exactly one of --initial-iso and --resume-at-gate",
		},
		{
			name:    "one-sided Git",
			options: Options{ResumeAtGate: true, GitRepository: "repo"},
			want:    "--git-repository and --git-base-ref must be supplied together",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runDirectory := filepath.Join(t.TempDir(), "run")
			test.options.RunDirectory = runDirectory
			err := runWithIO(context.Background(), test.options, strings.NewReader(""), &bytes.Buffer{})
			var startupError *StartupError
			if !errors.As(err, &startupError) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want startup error containing %q", err, test.want)
			}
			if _, statErr := os.Stat(runDirectory); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("invalid startup options mutated run directory: %v", statErr)
			}
		})
	}
}

func TestNormalizeTUIRunErrorSuppressesOnlyCancellationResults(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, err := range []error{
		context.Canceled,
		tea.ErrInterrupted,
		tea.ErrProgramKilled,
		errors.Join(tea.ErrProgramKilled, tea.ErrInterrupted),
	} {
		if normalized := normalizeTUIRunError(ctx, err); normalized != nil {
			t.Fatalf("normalize %v = %v, want nil", err, normalized)
		}
	}
	ordinary := errors.New("terminal restoration failed")
	if normalized := normalizeTUIRunError(ctx, ordinary); !errors.Is(normalized, ordinary) {
		t.Fatalf("ordinary error = %v, want %v", normalized, ordinary)
	}
	if normalized := normalizeTUIRunError(context.Background(), tea.ErrInterrupted); !errors.Is(normalized, tea.ErrInterrupted) {
		t.Fatalf("uncancelled interrupt = %v, want %v", normalized, tea.ErrInterrupted)
	}
}

func TestRunnerRejectsMalformedBootstrapBeforeObservabilityMutation(t *testing.T) {
	runDirectory := t.TempDir()
	manifest := filepath.Join(runDirectory, store.CrossRunBootstrapManifest)
	original := []byte("not JSON\n")
	if err := os.WriteFile(manifest, original, 0o600); err != nil {
		t.Fatal(err)
	}
	err := runWithIO(context.Background(), Options{
		RunDirectory: runDirectory,
		ResumeAtGate: true,
	}, strings.NewReader(""), &bytes.Buffer{})
	var startupError *StartupError
	if !errors.As(err, &startupError) {
		t.Fatalf("error = %v, want startup error", err)
	}
	after, readErr := os.ReadFile(manifest)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("malformed bootstrap was modified")
	}
	if _, statErr := os.Stat(filepath.Join(runDirectory, observability.EventLogFilename)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("observability was opened before bootstrap validation: %v", statErr)
	}
}

func TestRunnerPreservesMalformedEventLog(t *testing.T) {
	runDirectory := t.TempDir()
	eventPath := filepath.Join(runDirectory, observability.EventLogFilename)
	original := []byte("malformed event\n")
	if err := os.WriteFile(eventPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	err := runWithIO(context.Background(), Options{
		RunDirectory: runDirectory,
		ResumeAtGate: true,
	}, strings.NewReader(""), &bytes.Buffer{})
	var startupError *StartupError
	if !errors.As(err, &startupError) {
		t.Fatalf("error = %v, want startup error", err)
	}
	after, readErr := os.ReadFile(eventPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("malformed event log was modified")
	}
}
