// Package operator owns the trusted operator-facing command surface.
package operator

import (
	"context"
	"errors"

	"codexos/internal/sourcecapacity"

	"github.com/spf13/cobra"
)

// Options is the validated startup configuration, including explicit Go
// provisioning extensions to the reference operator console. Paths remain
// uninterpreted until the concrete runtime owns their validation.
type Options struct {
	RunDirectory             string
	InitialISO               string
	InitialISOConfigured     bool
	ResumeAtGate             bool
	GitRepository            string
	GitBaseRef               string
	GitConfigured            bool
	InheritFromRun           string
	InheritFromGeneration    int64
	InheritanceRequested     bool
	InheritSourceCapacity    sourcecapacity.Budget
	ProvidedAssets           string
	ProvidedAssetsConfigured bool
	OTLPEndpoint             string
	UseTUI                   bool
}

// NewCommand constructs the compatible Cobra command and invokes run only
// after all parser-level invariants have been checked. terminalSupported is the
// result of inspecting stdin, stdout, and TERM at the process boundary.
func NewCommand(terminalSupported bool, run func(context.Context, Options) error) *cobra.Command {
	var options Options
	var resumeAtGate bool
	var inheritSourceCapacity int
	var plain bool
	var tui bool

	command := &cobra.Command{
		Use:           "codexos",
		Short:         "CodexOS operator console",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(command *cobra.Command, _ []string) error {
			flags := command.Flags()
			initialSet := flags.Changed("initial-iso")
			resumeSet := flags.Changed("resume-at-gate") && resumeAtGate
			if initialSet == resumeSet {
				return errors.New("exactly one of --initial-iso and --resume-at-gate must be supplied")
			}

			gitRepositorySet := flags.Changed("git-repository")
			gitBaseSet := flags.Changed("git-base-ref")
			if gitRepositorySet != gitBaseSet {
				return errors.New("--git-repository and --git-base-ref must be supplied together")
			}

			inheritRunSet := flags.Changed("inherit-from-run")
			inheritGenerationSet := flags.Changed("inherit-from-generation")
			if inheritRunSet != inheritGenerationSet {
				return errors.New("--inherit-from-run and --inherit-from-generation must be supplied together")
			}
			inheritanceRequested := inheritRunSet
			if inheritanceRequested && resumeSet {
				return errors.New("cross-run inheritance is valid only with --initial-iso")
			}
			if inheritanceRequested && !gitRepositorySet {
				return errors.New("cross-run inheritance requires Git provenance options")
			}
			if inheritanceRequested && options.InheritFromGeneration < 0 {
				return errors.New("--inherit-from-generation must not be negative")
			}
			if flags.Changed("inherit-source-capacity") {
				if !inheritanceRequested {
					return errors.New("--inherit-source-capacity requires cross-run inheritance")
				}
				if inheritSourceCapacity != sourcecapacity.Default && inheritSourceCapacity != sourcecapacity.Expanded {
					return errors.New("--inherit-source-capacity must be 65536 or 1048576 content bytes")
				}
				options.InheritSourceCapacity = sourcecapacity.Budget(inheritSourceCapacity)
			}
			if plain && tui {
				return errors.New("--plain and --tui are mutually exclusive")
			}
			if tui && !terminalSupported {
				return errors.New("--tui requires interactive stdin/stdout and a supported terminal")
			}
			options.ResumeAtGate = resumeSet
			options.InitialISOConfigured = initialSet
			options.GitConfigured = gitRepositorySet
			options.InheritanceRequested = inheritanceRequested
			options.ProvidedAssetsConfigured = flags.Changed("provided-assets")
			options.UseTUI = tui || (terminalSupported && !plain)
			if run == nil {
				return errors.New("CodexOS operator runner is unavailable")
			}
			return run(command.Context(), options)
		},
	}

	flags := command.Flags()
	flags.StringVar(&options.RunDirectory, "run-directory", "", "run directory")
	flags.StringVar(&options.InitialISO, "initial-iso", "", "initial guest ISO")
	flags.BoolVar(&resumeAtGate, "resume-at-gate", false, "reopen an archived generation gate")
	flags.StringVar(&options.GitRepository, "git-repository", "", "trusted Git repository")
	flags.StringVar(&options.GitBaseRef, "git-base-ref", "", "trusted Git base reference")
	flags.StringVar(&options.InheritFromRun, "inherit-from-run", "", "bootstrap a fresh run from one validated source run")
	flags.Int64Var(&options.InheritFromGeneration, "inherit-from-generation", 0, "completed source generation whose selected successor is inherited")
	flags.IntVar(&inheritSourceCapacity, "inherit-source-capacity", sourcecapacity.Default, "explicit destination content-byte budget for cross-run bootstrap (65536 or 1048576)")
	flags.StringVar(&options.ProvidedAssets, "provided-assets", "", "freeze and expose assets from this explicit external directory")
	flags.StringVar(&options.OTLPEndpoint, "otlp-endpoint", "", "OTLP/HTTP metrics endpoint")
	flags.BoolVar(&plain, "plain", false, "force the line-oriented console even on an interactive terminal")
	flags.BoolVar(&tui, "tui", false, "require the full-screen interactive terminal interface")
	_ = command.MarkFlagRequired("run-directory")
	return command
}
