package operator

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"

	"codexos/internal/agent"
	"codexos/internal/experiment"
	"codexos/internal/observability"
	"codexos/internal/provenance"
	"codexos/internal/store"
	"codexos/internal/tui"
)

// StartupError identifies a failure before the selected operator frontend was
// ready. The process entry point uses it to preserve Python's startup message
// and exit behavior.
type StartupError struct {
	Err error
}

func (e *StartupError) Error() string {
	if e == nil || e.Err == nil {
		return "failed to start CodexOS"
	}
	return "failed to start CodexOS: " + e.Err.Error()
}

func (e *StartupError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// ExecutionError identifies a frontend or cleanup failure after startup.
// Parser errors remain unwrapped so the process entry point can return the
// reference CLI's usage-error status.
type ExecutionError struct {
	Err error
}

func (e *ExecutionError) Error() string {
	if e == nil || e.Err == nil {
		return "CodexOS operator failed"
	}
	return e.Err.Error()
}

func (e *ExecutionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Run opens and supervises one operator process using the real standard
// streams. The Python entry point remains unchanged and operational.
func Run(ctx context.Context, options Options) error {
	return runWithIO(ctx, options, os.Stdin, os.Stdout)
}

// RunWithIO is the process-boundary variant used by the thin command entry
// point and deterministic non-interactive tests.
func RunWithIO(ctx context.Context, options Options, input io.Reader, output io.Writer) error {
	return runWithIO(ctx, options, input, output)
}

// runnerConfiguration contains trusted concrete process inputs used by the
// disposable acceptance boundary. The public runner supplies the zero value;
// no test setting is exposed through the operator CLI or guest state.
type runnerConfiguration struct {
	live    experiment.LiveRunOptions
	session agent.GenerationSessionOptions
}

func runWithIO(ctx context.Context, options Options, input io.Reader, output io.Writer) (resultErr error) {
	return runWithIOConfigured(ctx, options, input, output, runnerConfiguration{})
}

func runWithIOConfigured(
	ctx context.Context,
	options Options,
	input io.Reader,
	output io.Writer,
	configuration runnerConfiguration,
) (resultErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if input == nil {
		input = os.Stdin
	}
	if output == nil {
		output = os.Stdout
	}

	startupComplete := false
	var runtime *experiment.CodexOSRun
	var eventLog *observability.EventLog
	var metrics *observability.Metrics
	defer func() {
		var closeErr error
		if runtime != nil {
			closeErr = runtime.Close()
		}
		if eventLog != nil {
			eventLog.Close()
		}
		if metrics != nil {
			metrics.Close()
		}
		resultErr = errors.Join(resultErr, closeErr)
		if resultErr == nil {
			return
		}
		if startupComplete {
			var executionError *ExecutionError
			if !errors.As(resultErr, &executionError) {
				resultErr = &ExecutionError{Err: resultErr}
			}
			return
		}
		var startupError *StartupError
		if !errors.As(resultErr, &startupError) {
			resultErr = &StartupError{Err: resultErr}
		}
	}()

	if options.RunDirectory == "" {
		return errors.New("run directory is required")
	}
	initialISOConfigured := options.InitialISOConfigured || options.InitialISO != ""
	if initialISOConfigured == options.ResumeAtGate {
		return errors.New("exactly one of --initial-iso and --resume-at-gate must be supplied")
	}
	if (options.GitRepository == "") != (options.GitBaseRef == "") {
		return errors.New("--git-repository and --git-base-ref must be supplied together")
	}
	gitConfigured := options.GitConfigured || options.GitRepository != "" || options.GitBaseRef != ""
	inheritanceRequested := options.InheritanceRequested || options.InheritFromRun != "" || options.InheritFromGeneration != 0
	if inheritanceRequested && options.InheritFromGeneration < 0 {
		return errors.New("--inherit-from-generation must not be negative")
	}
	if inheritanceRequested && (!initialISOConfigured || options.ResumeAtGate) {
		return errors.New("cross-run inheritance is valid only with --initial-iso")
	}
	if inheritanceRequested && !gitConfigured {
		return errors.New("cross-run inheritance requires Git provenance options")
	}
	if inheritanceRequested {
		if _, err := store.InitializeCrossRunBootstrap(
			options.RunDirectory,
			options.InitialISO,
			options.InheritFromRun,
			uint64(options.InheritFromGeneration),
			options.GitRepository,
			options.GitBaseRef,
		); err != nil {
			return err
		}
	}

	bootstrap, err := store.LoadCrossRunBootstrap(options.RunDirectory)
	if err != nil {
		return err
	}
	if bootstrap != nil && !gitConfigured {
		return errors.New("cross-run continuation requires its recorded Git provenance options")
	}

	activity := (*observability.ActivityStream)(nil)
	if options.UseTUI {
		activity = observability.NewActivityStream()
	}
	eventLog, err = observability.OpenEventLog(options.RunDirectory)
	if err != nil {
		return err
	}
	metrics, err = observability.NewMetrics(options.RunDirectory, observability.MetricsOptions{
		OTLPEndpoint: options.OTLPEndpoint,
	})
	if err != nil {
		return err
	}

	providedAssetsConfigured := options.ProvidedAssetsConfigured || options.ProvidedAssets != ""
	var providedAssets *string
	if providedAssetsConfigured {
		providedAssets = &options.ProvidedAssets
	}
	liveOptions := configuration.live
	liveOptions.ProvidedAssetsDirectory = providedAssets
	liveOptions.EventLog = eventLog
	liveOptions.Metrics = metrics
	liveOptions.ActivityStream = activity
	runtime, err = experiment.NewLiveCodexOSRun(options.RunDirectory, liveOptions)
	if err != nil {
		return err
	}

	var gitRecorder *provenance.GenerationGitRecorder
	if gitConfigured {
		gitRecorder, err = provenance.NewGenerationGitRecorder(
			options.GitRepository,
			options.RunDirectory,
			options.GitBaseRef,
		)
		if err != nil {
			return err
		}
		if bootstrap != nil && (options.GitBaseRef != bootstrap.GitBaseRef || gitRecorder.BaseCommit() != bootstrap.GitBaseCommit) {
			return errors.New("configured Git base does not match cross-run bootstrap provenance")
		}
	}

	if options.ResumeAtGate {
		err = runtime.ReopenAtGate()
	} else {
		err = runtime.Start(ctx, options.InitialISO)
	}
	if err != nil {
		return err
	}
	startupComplete = true

	var interviewStore *provenance.ExitInterviewArtifactStore
	if gitConfigured {
		interviewStore, err = provenance.NewExitInterviewArtifactStore(options.GitRepository, options.RunDirectory)
		if err != nil {
			return err
		}
	}
	sessionOptions := configuration.session
	sessionOptions.ActivityStream = activity
	consoleOptions := PlainConsoleOptions{
		Input:          input,
		Output:         output,
		Controller:     GenerationControllerOptions{Session: sessionOptions},
		GitRecorder:    gitRecorder,
		InterviewStore: interviewStore,
	}
	if options.UseTUI {
		return runTUI(ctx, runtime, activity, consoleOptions, input, output)
	}
	console, err := NewPlainConsole(runtime, consoleOptions)
	if err != nil {
		return err
	}
	return console.Run(ctx)
}

func runTUI(
	ctx context.Context,
	runtime *experiment.CodexOSRun,
	activity *observability.ActivityStream,
	options PlainConsoleOptions,
	input io.Reader,
	output io.Writer,
) (resultErr error) {
	var outputMu sync.Mutex
	var startupLines []string
	var application *tui.Application
	options.OutputHandler = func(line string) {
		outputMu.Lock()
		current := application
		if current == nil {
			startupLines = append(startupLines, line)
			outputMu.Unlock()
			return
		}
		outputMu.Unlock()
		current.PostOperatorOutput(line)
	}
	options.ConfirmationHandler = func(prompt string) bool {
		outputMu.Lock()
		current := application
		outputMu.Unlock()
		return current != nil && current.RequestConfirmation(prompt)
	}
	console, err := NewPlainConsole(runtime, options)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, console.Shutdown())
	}()
	if err := console.ShowStartup(); err != nil {
		return err
	}
	outputMu.Lock()
	startupOutput := strings.Join(startupLines, "\n")
	outputMu.Unlock()

	app, err := tui.NewApplication(tui.ApplicationOptions{
		Activity:      activity,
		Status:        func() tui.StatusSnapshot { return tuiStatus(console) },
		StartupOutput: startupOutput,
		Execute: func(commandContext context.Context, command string, _ tui.ConfirmationFunc) (tui.CommandResult, error) {
			exit, commandErr := console.ExecuteLine(commandContext, command)
			status := tuiStatus(console)
			return tui.CommandResult{Status: &status, Exit: exit}, commandErr
		},
	})
	if err != nil {
		return err
	}
	outputMu.Lock()
	application = app
	outputMu.Unlock()
	return normalizeTUIRunError(ctx, app.Run(ctx, input, output))
}

func normalizeTUIRunError(ctx context.Context, err error) error {
	if err == nil || ctx == nil || ctx.Err() == nil {
		return err
	}
	if errors.Is(err, ctx.Err()) || errors.Is(err, tea.ErrInterrupted) || errors.Is(err, tea.ErrProgramKilled) {
		return nil
	}
	return err
}

func tuiStatus(console *PlainConsole) tui.StatusSnapshot {
	agentName, phase := console.CodexActivity()
	status := tui.StatusSnapshot{
		RunName:      filepath.Base(console.runtime.RunDirectory()),
		RuntimeState: runtimeStateName(console.runtime.State()),
		SolState:     console.CodexTurnState(),
		ActiveAgent:  agentName,
		ActivePhase:  phase,
	}
	status.Generation, status.HasGeneration = console.runtime.GenerationNumber()
	requests, err := console.runtime.FeatureRequests()
	if err == nil {
		for _, request := range requests {
			if request.Status == store.FeaturePending {
				status.PendingFeatures++
			}
		}
	}
	switch console.ExitInterviewState() {
	case "available":
		status.Interview = tui.InterviewAvailable
	case "idle":
		status.Interview = tui.InterviewIdle
	case "answering":
		status.Interview = tui.InterviewAnswering
	default:
		status.Interview = tui.InterviewUnavailable
	}
	return status
}
