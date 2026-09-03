package operator

import (
	"context"
	"strings"
	"testing"
)

func TestCommandPreservesValidatedStartupOptions(t *testing.T) {
	var received Options
	command := NewCommand(true, func(_ context.Context, options Options) error {
		received = options
		return nil
	})
	command.SetArgs([]string{
		"--run-directory", "new-run",
		"--initial-iso", "successor.iso",
		"--git-repository", "repository",
		"--git-base-ref", "old-run/generation-0007",
		"--inherit-from-run", "old-run",
		"--inherit-from-generation", "7",
		"--provided-assets", "assets",
		"--otlp-endpoint", "http://127.0.0.1:4318",
	})
	if err := command.Execute(); err != nil {
		t.Fatalf("execute command: %v", err)
	}
	want := Options{
		RunDirectory:             "new-run",
		InitialISO:               "successor.iso",
		InitialISOConfigured:     true,
		GitRepository:            "repository",
		GitBaseRef:               "old-run/generation-0007",
		GitConfigured:            true,
		InheritFromRun:           "old-run",
		InheritFromGeneration:    7,
		InheritanceRequested:     true,
		ProvidedAssets:           "assets",
		ProvidedAssetsConfigured: true,
		OTLPEndpoint:             "http://127.0.0.1:4318",
		UseTUI:                   true,
	}
	if received != want {
		t.Fatalf("options = %#v, want %#v", received, want)
	}
}

func TestCommandOpeningDisplayAndPairingValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"run directory required", []string{"--resume-at-gate"}, "required flag(s) \"run-directory\" not set"},
		{"opening required", []string{"--run-directory", "run"}, "exactly one of --initial-iso and --resume-at-gate"},
		{"opening exclusive", []string{"--run-directory", "run", "--initial-iso", "seed.iso", "--resume-at-gate"}, "exactly one of --initial-iso and --resume-at-gate"},
		{"git pair", []string{"--run-directory", "run", "--initial-iso", "seed.iso", "--git-repository", "repo"}, "--git-repository and --git-base-ref must be supplied together"},
		{"inheritance pair", []string{"--run-directory", "run", "--initial-iso", "seed.iso", "--inherit-from-run", "old"}, "--inherit-from-run and --inherit-from-generation must be supplied together"},
		{"inheritance opening", []string{"--run-directory", "run", "--resume-at-gate", "--inherit-from-run", "old", "--inherit-from-generation", "1", "--git-repository", "repo", "--git-base-ref", "base"}, "cross-run inheritance is valid only with --initial-iso"},
		{"inheritance provenance", []string{"--run-directory", "run", "--initial-iso", "seed.iso", "--inherit-from-run", "old", "--inherit-from-generation", "1"}, "cross-run inheritance requires Git provenance options"},
		{"negative inherited generation", []string{"--run-directory", "run", "--initial-iso", "seed.iso", "--inherit-from-run", "old", "--inherit-from-generation", "-1", "--git-repository", "repo", "--git-base-ref", "base"}, "--inherit-from-generation must not be negative"},
		{"removed harness acknowledgement", []string{"--run-directory", "run", "--resume-at-gate", "--acknowledge-harness-change"}, "unknown flag: --acknowledge-harness-change"},
		{"display exclusive", []string{"--run-directory", "run", "--initial-iso", "seed.iso", "--plain", "--tui"}, "--plain and --tui are mutually exclusive"},
		{"positional arguments rejected", []string{"--run-directory", "run", "--resume-at-gate", "extra"}, "unknown command \"extra\""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			command := NewCommand(true, func(context.Context, Options) error {
				called = true
				return nil
			})
			command.SetArgs(test.args)
			err := command.Execute()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
			if called {
				t.Fatal("runner was called after invalid arguments")
			}
		})
	}
}

func TestCommandDisplaySelectionMatchesTerminalSupport(t *testing.T) {
	tests := []struct {
		name              string
		terminalSupported bool
		flag              string
		wantTUI           bool
		wantError         string
	}{
		{"automatic tui", true, "", true, ""},
		{"automatic plain", false, "", false, ""},
		{"forced plain", true, "--plain", false, ""},
		{"required tui", true, "--tui", true, ""},
		{"unsupported tui", false, "--tui", false, "--tui requires interactive stdin/stdout and a supported terminal"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			command := NewCommand(test.terminalSupported, func(_ context.Context, options Options) error {
				called = true
				if options.UseTUI != test.wantTUI {
					t.Fatalf("UseTUI = %v, want %v", options.UseTUI, test.wantTUI)
				}
				return nil
			})
			args := []string{"--run-directory", "run", "--resume-at-gate"}
			if test.flag != "" {
				args = append(args, test.flag)
			}
			command.SetArgs(args)
			err := command.Execute()
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want substring %q", err, test.wantError)
				}
				if called {
					t.Fatal("runner was called for an unsupported TUI")
				}
				return
			}
			if err != nil {
				t.Fatalf("execute command: %v", err)
			}
			if !called {
				t.Fatal("runner was not called")
			}
		})
	}
}

func TestCommandPreservesExplicitEmptyPathFlagPresence(t *testing.T) {
	var received Options
	command := NewCommand(false, func(_ context.Context, options Options) error {
		received = options
		return nil
	})
	command.SetArgs([]string{
		"--run-directory", "run",
		"--initial-iso", "image",
		"--git-repository", "",
		"--git-base-ref", "",
		"--inherit-from-run", "",
		"--inherit-from-generation", "0",
		"--provided-assets", "",
	})
	if err := command.Execute(); err != nil {
		t.Fatalf("execute command: %v", err)
	}
	if !received.GitConfigured || !received.InheritanceRequested || !received.ProvidedAssetsConfigured {
		t.Fatalf("explicit flag presence was lost: %#v", received)
	}
}
