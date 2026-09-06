package operator

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"codexos/internal/agent"
	"codexos/internal/experiment"
	"codexos/internal/provenance"
	"codexos/internal/qemu"
	"codexos/internal/sourcecapacity"
	"codexos/internal/store"
)

type consoleRuntime interface {
	generationRuntime
	State() experiment.RuntimeState
	ActivePID() (int, bool)
	HardwareProfile() qemu.HardwareProfile
	PendingGenerationFinish() (*experiment.PendingGenerationFinish, bool)
	PreviousHandoff() (string, bool)
	ArchivedGenerations() ([]experiment.ArchivedGeneration, error)
	InspectGeneration(uint64) (experiment.ArchivedGeneration, error)
	FeatureRequests() ([]store.FeatureRequest, error)
	FeatureRequest(uint64) (store.FeatureRequest, error)
	ApproveFeatureRequest(uint64, string) (store.FeatureRequest, error)
	DenyFeatureRequest(uint64, string) (store.FeatureRequest, error)
	PresentationSnapshot() experiment.RunPresentationSnapshot
}

var _ consoleRuntime = (*experiment.CodexOSRun)(nil)

type PlainConsoleOptions struct {
	Input               io.Reader
	Output              io.Writer
	OutputHandler       func(string)
	ConfirmationHandler func(string) bool
	Controller          GenerationControllerOptions
	GitRecorder         *provenance.GenerationGitRecorder
	InterviewStore      *provenance.ExitInterviewArtifactStore
}

type consoleTurn struct {
	done      chan struct{}
	interview bool
	phase     string
}

// PlainConsole owns the line-oriented operator interaction and one
// GenerationController. Command errors are recoverable; shutdown errors are
// returned to the process boundary.
type PlainConsole struct {
	runtime             consoleRuntime
	controller          *GenerationController
	input               *bufio.Reader
	inputCloser         io.Closer
	output              io.Writer
	outputHandler       func(string)
	confirmationHandler func(string) bool
	git                 *provenance.GenerationGitRecorder
	interviews          *provenance.ExitInterviewArtifactStore

	outputMu sync.Mutex
	turnMu   sync.Mutex
	turn     *consoleTurn

	shutdownMu         sync.Mutex
	shutdownStarted    bool
	interviewMu        sync.Mutex
	persistedInterview *uint64
}

func NewPlainConsole(runtime *experiment.CodexOSRun, options PlainConsoleOptions) (*PlainConsole, error) {
	if runtime == nil {
		return nil, errors.New("CodexOS operator runtime is nil")
	}
	return newPlainConsole(runtime, options)
}

func newPlainConsole(runtime consoleRuntime, options PlainConsoleOptions) (*PlainConsole, error) {
	if runtime == nil {
		return nil, errors.New("CodexOS operator runtime is nil")
	}
	if options.Input == nil {
		options.Input = os.Stdin
	}
	if options.Output == nil {
		options.Output = os.Stdout
	}
	controller, err := newGenerationController(runtime, options.Controller)
	if err != nil {
		return nil, err
	}
	console := &PlainConsole{
		runtime: runtime, controller: controller,
		input: bufio.NewReader(options.Input), output: options.Output,
		outputHandler: options.OutputHandler, confirmationHandler: options.ConfirmationHandler,
		git: options.GitRecorder, interviews: options.InterviewStore,
	}
	console.inputCloser, _ = options.Input.(io.Closer)
	return console, nil
}

func (c *PlainConsole) Run(ctx context.Context) (resultErr error) {
	if c == nil {
		return errors.New("CodexOS plain console is nil")
	}
	if ctx == nil {
		return errors.New("CodexOS console context is nil")
	}
	stopInputClose := func() bool { return false }
	if c.inputCloser != nil {
		stopInputClose = context.AfterFunc(ctx, func() {
			_ = c.inputCloser.Close()
		})
	}
	defer stopInputClose()
	defer func() {
		resultErr = errors.Join(resultErr, c.Shutdown())
	}()
	if err := c.ShowStartup(); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			c.printLine("Interrupted; stopping CodexOS run.")
			return nil
		}
		line, err := c.readLine(c.prompt())
		if ctx.Err() != nil {
			c.printLine("Interrupted; stopping CodexOS run.")
			return nil
		}
		if errors.Is(err, io.EOF) && line == "" {
			c.printLine("Input closed; stopping CodexOS run.")
			return nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		quit, err := c.ExecuteLine(ctx, line)
		if err != nil {
			c.printLine("Error: " + err.Error())
			continue
		}
		if quit {
			return nil
		}
	}
}

func (c *PlainConsole) ExecuteLine(ctx context.Context, line string) (bool, error) {
	if c == nil {
		return false, errors.New("CodexOS plain console is nil")
	}
	if ctx == nil {
		return false, errors.New("CodexOS console context is nil")
	}
	commandLine := strings.TrimRight(line, "\r\n")
	words := strings.Fields(commandLine)
	if len(words) == 0 {
		return false, nil
	}
	if words[0] == "os-request" || words[0] == "os-requests" {
		return false, c.executeOperatorRequest(commandLine, words)
	}
	if c.interviewOpen() {
		switch {
		case len(words) == 1 && (words[0] == "end" || words[0] == "end-interview"):
			return false, c.endInterview()
		case len(words) == 1 && words[0] == "quit":
			return c.quit()
		default:
			question, ok := ExitInterviewQuestion(commandLine)
			if !ok {
				question = commandLine
			}
			return false, c.askInterview(question)
		}
	}

	switch words[0] {
	case "help":
		if !c.requireArity(words, 1, "help") {
			return false, nil
		}
		c.printHelp()
	case "status":
		if !c.requireArity(words, 1, "status") {
			return false, nil
		}
		return false, c.printStatus()
	case "bootstrap":
		runtime, ok := c.runtime.(interface {
			ProvisionBootstrap(context.Context, string) error
			BootstrapStatus() (string, error)
			RecoverBootstrap(context.Context) error
			GarbageCollectBootstrap() error
		})
		if !ok {
			return false, errors.New("bootstrap service is unavailable")
		}
		if len(words) == 1 {
			text, e := runtime.BootstrapStatus()
			if e != nil {
				return false, e
			}
			c.printLine(text)
			return false, nil
		}
		switch words[1] {
		case "provision":
			if len(words) != 3 {
				return false, errors.New("usage: bootstrap provision TCC_ASSET_ID")
			}
			if c.currentTurn() != nil {
				return false, errors.New("previous generation Codex turn is still active")
			}
			if e := runtime.ProvisionBootstrap(ctx, words[2]); e != nil {
				return false, e
			}
			c.recordOperator("bootstrap-provision", "success", map[string]any{"tcc_asset": words[2]})
			c.printLine("Linux bootstrap service provisioned. No generation started; feature request #3 status is unchanged.")
		case "recover":
			if len(words) != 2 {
				return false, errors.New("usage: bootstrap recover")
			}
			if e := runtime.RecoverBootstrap(ctx); e != nil {
				return false, e
			}
			c.printLine("Bootstrap cleanup verified.")
		case "gc":
			if len(words) != 2 {
				return false, errors.New("usage: bootstrap gc")
			}
			if e := runtime.GarbageCollectBootstrap(); e != nil {
				return false, e
			}
			c.printLine("Unreferenced bootstrap jobs reclaimed; archived references retained.")
		default:
			return false, errors.New("usage: bootstrap [provision TCC_ASSET_ID|recover|gc]")
		}
	case "source-capacity":
		if c.currentTurn() != nil {
			return false, errors.New("previous generation Codex turn is still active")
		}
		value, ok := c.unsignedArgument(words, "source-capacity")
		if !ok {
			return false, nil
		}
		if value != sourcecapacity.Default && value != sourcecapacity.Expanded {
			return false, fmt.Errorf("source content capacity must be %d or %d bytes", sourcecapacity.Default, sourcecapacity.Expanded)
		}
		runtime, ok := c.runtime.(interface {
			SetSourceCapacity(sourcecapacity.Budget) error
		})
		if !ok {
			return false, fmt.Errorf("source capacity provisioning is unavailable")
		}
		if err := runtime.SetSourceCapacity(sourcecapacity.Budget(value)); err != nil {
			return false, err
		}
		c.recordOperator("source-capacity", "success", map[string]any{"source_content_bytes": value})
		c.printLine(fmt.Sprintf("Source content capacity provisioned: %d bytes (plus snapshot framing). No generation started.", value))
	case "history":
		if !c.requireArity(words, 1, "history") {
			return false, nil
		}
		return false, c.printHistory()
	case "inspect":
		generation, ok := c.unsignedArgument(words, "inspect")
		if !ok {
			return false, nil
		}
		item, err := c.runtime.InspectGeneration(generation)
		if err != nil {
			return false, err
		}
		c.printInspection(item)
	case "features":
		if !c.requireArity(words, 1, "features") {
			return false, nil
		}
		return false, c.printFeatures()
	case "feature":
		requestID, ok := c.unsignedArgument(words, "feature")
		if !ok {
			return false, nil
		}
		return false, c.printFeature(requestID)
	case "feature-approve":
		requestID, note, ok := c.featureDecisionArguments(commandLine, words)
		if !ok {
			return false, nil
		}
		return false, c.approveFeature(requestID, note)
	case "feature-deny":
		requestID, note, ok := c.featureDecisionArguments(commandLine, words)
		if !ok {
			return false, nil
		}
		return false, c.denyFeature(requestID, note)
	case "agent":
		if !c.requireArity(words, 1, "agent") {
			return false, nil
		}
		if err := c.startAgent(""); err != nil {
			c.recordOperator("agent", "failed", nil)
			return false, err
		}
		c.recordOperator("agent", "success", nil)
	case "interview":
		if !c.requireArity(words, 1, "interview") {
			return false, nil
		}
		return false, c.beginInterview()
	case "ask":
		question, ok := ExitInterviewQuestion(commandLine)
		if !ok {
			c.printLine("Usage: ask <text>")
			return false, nil
		}
		return false, c.askInterview(question)
	case "end-interview":
		if !c.requireArity(words, 1, "end-interview") {
			return false, nil
		}
		return false, c.endInterview()
	case "git-record":
		if !c.requireArity(words, 1, "git-record") {
			return false, nil
		}
		if c.git == nil {
			c.printLine("Git provenance is not configured.")
			return false, nil
		}
		if c.reconcileGit() {
			c.printLine("Git provenance is up to date.")
		}
	case "pause":
		if !c.requireArity(words, 1, "pause") {
			return false, nil
		}
		return false, c.pause(ctx)
	case "resume":
		if !c.requireArity(words, 1, "resume") {
			return false, nil
		}
		return false, c.resume(ctx)
	case "abort":
		reason, ok := AbortReason(commandLine)
		if !ok {
			c.printLine("Usage: abort REASON")
			return false, nil
		}
		if err := experiment.ValidateAbortReason(reason); err != nil {
			return false, err
		}
		return false, c.abort(reason)
	case "continue":
		if !c.requireArity(words, 1, "continue") {
			return false, nil
		}
		return false, c.continueGeneration()
	case "rollback":
		generation, ok := c.unsignedArgument(words, "rollback")
		if !ok {
			return false, nil
		}
		return false, c.rollback(generation)
	case "quit":
		if !c.requireArity(words, 1, "quit") {
			return false, nil
		}
		return c.quit()
	default:
		c.printLine("Unknown command. Type 'help'.")
	}
	return false, nil
}

func (c *PlainConsole) startAgent(prompt string) error {
	phase := c.controller.NextTurnKind()
	turn, err := c.reserveTurn(false, phase)
	if err != nil {
		return err
	}
	outcomes, err := c.controller.StartTurn(prompt)
	if err != nil {
		c.releaseReservedTurn(turn)
		return err
	}
	c.watchTurn(turn, outcomes)
	generation, _ := c.runtime.GenerationNumber()
	switch phase {
	case "initial":
		c.printLine(fmt.Sprintf("Codex planning and implementation started for generation %d.", generation))
	case "planning":
		c.printLine(fmt.Sprintf("Codex planning resumed for generation %d.", generation))
	default:
		c.printLine(fmt.Sprintf("Codex turn started for generation %d.", generation))
	}
	return nil
}

func (c *PlainConsole) reserveTurn(interview bool, phase string) (*consoleTurn, error) {
	c.turnMu.Lock()
	defer c.turnMu.Unlock()
	if c.turn != nil {
		return nil, errors.New("Codex turn is already active")
	}
	turn := &consoleTurn{done: make(chan struct{}), interview: interview, phase: phase}
	c.turn = turn
	return turn, nil
}

func (c *PlainConsole) releaseReservedTurn(turn *consoleTurn) {
	c.turnMu.Lock()
	if c.turn == turn {
		c.turn = nil
	}
	c.turnMu.Unlock()
	close(turn.done)
}

func (c *PlainConsole) watchTurn(turn *consoleTurn, outcomes <-chan TurnOutcome) {
	go func() {
		outcome, ok := <-outcomes
		if ok {
			if turn.interview {
				c.reportInterviewOutcome(outcome)
			} else {
				c.reportGenerationOutcome(outcome)
			}
		}
		c.turnMu.Lock()
		if c.turn == turn {
			c.turn = nil
		}
		c.turnMu.Unlock()
		close(turn.done)
	}()
}

func (c *PlainConsole) reportGenerationOutcome(outcome TurnOutcome) {
	if outcome.Err != nil {
		if c.controller.SessionOwned() {
			c.printLine("Codex turn failed: " + outcome.Err.Error())
		} else {
			c.printLine("Codex session failed: " + outcome.Err.Error())
		}
		return
	}
	atGate := c.runtime.State() == experiment.RuntimeStateAwaitingNextGeneration
	if atGate {
		c.reconcileGit()
		generation, _ := c.runtime.GenerationNumber()
		c.printLine(fmt.Sprintf("Generation %d completed cooperatively.", generation))
		_ = c.printGate()
	} else if outcome.Result.TurnStatus == "interrupted" {
		c.printLine("Codex turn interrupted.")
	} else if outcome.Result.Summary != "" {
		c.printLine(outcome.Result.Summary)
	}
	if outcome.Result.FinalMessage != "" {
		c.printLine("Codex:")
		c.printIndented(outcome.Result.FinalMessage)
	}
}

func (c *PlainConsole) reportInterviewOutcome(outcome TurnOutcome) {
	if outcome.Err != nil {
		if c.controller.ExitInterviewAvailable() {
			c.printLine("Exit interview turn failed: " + outcome.Err.Error())
		} else {
			c.persistInterview("failed")
			c.printLine("Exit interview session failed: " + outcome.Err.Error())
		}
		return
	}
	if outcome.Result.TurnStatus == "interrupted" {
		c.printLine("Exit interview turn interrupted.")
	} else if outcome.Result.FinalMessage != "" && c.outputHandler == nil {
		c.printLine("Sol:")
		c.printIndented(outcome.Result.FinalMessage)
	}
}

func (c *PlainConsole) pause(ctx context.Context) error {
	turn := c.currentTurn()
	if err := c.controller.Pause(ctx); err != nil {
		c.recordOperator("pause", "failed", nil)
		return err
	}
	if turn != nil {
		if err := waitConsoleTurn(ctx, turn.done); err != nil {
			c.recordOperator("pause", "failed", nil)
			return err
		}
	}
	generation, _ := c.runtime.GenerationNumber()
	c.printLine(fmt.Sprintf("Generation %d paused.", generation))
	c.recordOperator("pause", "success", nil)
	return nil
}

func (c *PlainConsole) resume(ctx context.Context) error {
	turn, reserveErr := c.reserveTurn(false, c.controller.NextTurnKind())
	if reserveErr != nil {
		c.recordOperator("resume", "failed", nil)
		return reserveErr
	}
	outcomes, err := c.controller.Resume(ctx)
	if err != nil {
		c.releaseReservedTurn(turn)
		c.recordOperator("resume", "failed", nil)
		return err
	}
	generation, _ := c.runtime.GenerationNumber()
	if outcomes != nil {
		c.watchTurn(turn, outcomes)
		c.printLine(fmt.Sprintf("Generation %d resumed; Codex continued in the same session.", generation))
	} else {
		c.releaseReservedTurn(turn)
		c.printLine(fmt.Sprintf("Generation %d resumed.", generation))
	}
	c.recordOperator("resume", "success", nil)
	return nil
}

func (c *PlainConsole) continueGeneration() error {
	if c.currentTurn() != nil {
		return errors.New("previous generation Codex turn is still active")
	}
	next := uint64(0)
	if generation, ok := c.runtime.GenerationNumber(); ok && generation != ^uint64(0) {
		next = generation + 1
	}
	if _, ok := c.runtime.PendingGenerationFinish(); ok {
		c.printLine(fmt.Sprintf("Starting generation %d from selected successor...", next))
	}
	if err := c.controller.ContinueGeneration(); err != nil {
		c.recordOperator("continue", "failed", nil)
		return err
	}
	c.resetPersistedInterview()
	c.printRunningSummary()
	c.recordOperator("continue", "success", nil)
	return nil
}

func (c *PlainConsole) rollback(parent uint64) error {
	if c.currentTurn() != nil {
		return errors.New("previous generation Codex turn is still active")
	}
	current, _ := c.runtime.GenerationNumber()
	next := current + 1
	if !c.confirm(fmt.Sprintf("Fork generation %d from generation %d's selected successor?\nThis preserves all later archives unchanged. [y/N] ", next, parent)) {
		c.printLine("Rollback cancelled.")
		c.recordOperator("rollback", "cancelled", map[string]any{"parent_generation": parent})
		return nil
	}
	if err := c.controller.Rollback(parent); err != nil {
		c.recordOperator("rollback", "failed", map[string]any{"parent_generation": parent})
		return err
	}
	c.resetPersistedInterview()
	generation, _ := c.runtime.GenerationNumber()
	c.printLine(fmt.Sprintf("Generation %d started from generation %d.", generation, parent))
	c.printLine("State: " + runtimeStateName(c.runtime.State()))
	c.printLine(fmt.Sprintf("Source content capacity: %d bytes (snapshot maximum: %d bytes)", c.runtime.PresentationSnapshot().SourceCapacity.Bytes(), c.runtime.PresentationSnapshot().SourceCapacity.SnapshotLimit()))
	if pid, ok := c.runtime.ActivePID(); ok {
		c.printLine(fmt.Sprintf("QEMU PID: %d", pid))
	} else {
		c.printLine("QEMU PID: none")
	}
	c.recordOperator("rollback", "success", map[string]any{"parent_generation": parent})
	return nil
}

func (c *PlainConsole) abort(reason string) error {
	generation, _ := c.runtime.GenerationNumber()
	data := map[string]any{"reason": reason}
	if !c.confirm(fmt.Sprintf("Abort generation %d permanently?\nReason:\n%s\n[y/N] ", generation, EscapeTerminalText(reason, true))) {
		c.printLine("Abort cancelled.")
		c.recordOperator("abort", "cancelled", data)
		return nil
	}
	turn := c.currentTurn()
	if err := c.controller.Abort(reason); err != nil {
		c.recordOperator("abort", "failed", data)
		return err
	}
	if err := c.waitForConsoleTurn(turn); err != nil {
		c.recordOperator("abort", "failed", data)
		return err
	}
	c.resetPersistedInterview()
	c.reconcileGit()
	if err := c.printGate(); err != nil {
		return err
	}
	c.recordOperator("abort", "success", data)
	return nil
}

func (c *PlainConsole) beginInterview() error {
	if c.runtime.State() != experiment.RuntimeStateAwaitingNextGeneration {
		return errors.New("exit interview is available only after cooperative completion")
	}
	if _, ok := c.runtime.PendingGenerationFinish(); !ok {
		return errors.New("exit interview is available only after cooperative completion")
	}
	if !c.controller.ExitInterviewAvailable() {
		c.printLine("No live generation session is available for an exit interview.")
		c.printLine("Only generations completed after exit-interview support was active can retain their original Sol thread.")
		return nil
	}
	if c.interviews == nil {
		return errors.New("exit interview persistence requires --git-repository")
	}
	if err := c.controller.BeginExitInterview(); err != nil {
		return err
	}
	generation, _ := c.runtime.GenerationNumber()
	c.printLine(fmt.Sprintf("Exit interview started for generation %d.", generation))
	c.printLine("The generation, selected successor, and handoff are frozen. Interview turns are read-only.")
	return nil
}

func (c *PlainConsole) askInterview(question string) error {
	turn, err := c.reserveTurn(true, "interview")
	if err != nil {
		return err
	}
	outcomes, err := c.controller.StartExitInterviewTurn(question)
	if err != nil {
		c.releaseReservedTurn(turn)
		return err
	}
	c.watchTurn(turn, outcomes)
	c.printLine("Exit interview question sent.")
	return nil
}

func (c *PlainConsole) endInterview() error {
	turn := c.currentTurn()
	outcome := "completed"
	if turn != nil && turn.interview {
		outcome = "interrupted"
	}
	controllerErr := c.controller.EndExitInterview()
	waitErr := c.waitForConsoleTurn(turn)
	c.persistInterview(outcome)
	if err := errors.Join(controllerErr, waitErr); err != nil {
		return err
	}
	c.printLine("Exit interview ended.")
	return nil
}

func (c *PlainConsole) quit() (bool, error) {
	state := c.runtime.State()
	if state == experiment.RuntimeStateRunning || state == experiment.RuntimeStatePaused {
		generation, _ := c.runtime.GenerationNumber()
		if !c.confirm(fmt.Sprintf("Stop the run without archiving generation %d? [y/N] ", generation)) {
			c.printLine("Quit cancelled.")
			c.recordOperator("quit", "cancelled", nil)
			return false, nil
		}
	}
	if err := c.Shutdown(); err != nil {
		c.recordOperator("quit", "failed", nil)
		return false, err
	}
	c.recordOperator("quit", "success", nil)
	return true, nil
}

func (c *PlainConsole) Shutdown() error {
	if c == nil {
		return nil
	}
	c.shutdownMu.Lock()
	first := !c.shutdownStarted
	c.shutdownStarted = true
	c.shutdownMu.Unlock()
	turn := c.currentTurn()
	outcome := "incomplete"
	if turn != nil && turn.interview {
		outcome = "interrupted"
	}
	controllerErr := c.controller.Close()
	waitErr := c.waitForConsoleTurn(turn)
	if first {
		c.persistInterview(outcome)
	}
	return errors.Join(controllerErr, waitErr)
}

// ShowStartup reports the opened run and validates the gate state needed to
// render it. TUI and plain frontends share this one authoritative path.
func (c *PlainConsole) ShowStartup() error {
	if c == nil {
		return errors.New("CodexOS plain console is nil")
	}
	c.printLine("CodexOS operator console")
	c.printLine("")
	c.printLine("Run directory: " + c.runtime.RunDirectory())
	c.reconcileGit()
	if c.runtime.State() == experiment.RuntimeStateAwaitingNextGeneration {
		c.printHarnessGateTransition(c.runtime.PresentationSnapshot().HarnessTransition)
		if err := c.printGate(); err != nil {
			return err
		}
	} else {
		c.printRunningSummary()
	}
	c.printLine("")
	c.printLine("Type 'help' for commands.")
	return nil
}

func (c *PlainConsole) printHarnessGateTransition(transition *provenance.HarnessGateTransition) {
	if transition == nil || !transition.RequiresRecord {
		return
	}
	c.printLine("")
	c.printLine("Harness identity changed at this validated generation gate.")
	c.printLine("Previous harness identity:")
	c.printHarnessIdentity(transition.Previous)
	c.printLine("Current harness identity:")
	c.printHarnessIdentity(&transition.Current)
	c.printLine("The transition is recorded; continue or rollback authorizes the current harness to start the next generation.")
}

func (c *PlainConsole) printHarnessIdentity(identity *provenance.HarnessIdentity) {
	if identity == nil {
		c.printLine("  unavailable (legacy run without harness identity provenance)")
		return
	}
	dirty := "clean"
	if identity.RepositoryDirty {
		dirty = "dirty; tree SHA-256 " + *identity.DirtyTreeSHA256
	}
	build := identity.Build
	c.printLine("  Repository commit: " + identity.RepositoryCommit)
	c.printLine("  Repository state: " + dirty)
	c.printLine(fmt.Sprintf("  Executable: SHA-256 %s; %d bytes", identity.Executable.SHA256, identity.Executable.Size))
	c.printLine("  Build module: " + EscapeTerminalText(build.ModulePath+" "+build.ModuleVersion, false))
	c.printLine("  Build module sum: " + EscapeTerminalText(build.ModuleSum, false))
	c.printLine("  Go version: " + EscapeTerminalText(build.GoVersion, false))
	c.printLine("  Build settings SHA-256: " + build.SettingsSHA256)
	c.printLine(fmt.Sprintf("  Embedded VCS: %s; revision %s; time %s; modified %t",
		EscapeTerminalText(build.VCS, false), EscapeTerminalText(build.VCSRevision, false),
		EscapeTerminalText(build.VCSTime, false), build.VCSModified))
}

func (c *PlainConsole) printHelp() {
	for _, line := range []string{
		"help        show these commands",
		"bootstrap [provision TCC_ASSET_ID|recover|gc]  inspect/provision the optional Linux job service",
		"source-capacity BYTES  provision 65536 or 1048576 content bytes at an inactive gate",
		"status      show current runtime state",
		"history     show archived generation lineage",
		"inspect N   show archived generation N",
		"os-requests  list advisory operator OS requests",
		"os-request N  inspect an OS request and its attributed history",
		"os-request create TITLE | DESCRIPTION  record desired OS behavior",
		"os-request withdraw N REASON  withdraw an OS request",
		"os-request verify N REPORT_REVISION NOTE  attest an applicable completion report",
		"features    list external feature requests",
		"feature N   show external feature request N",
		"feature-approve N [NOTE]  approve a pending request at the gate",
		"feature-deny N [NOTE]     deny a pending request at the gate",
		"agent       start or continue the generation's Codex session",
		"interview   enter a retained post-generation exit interview",
		"ask TEXT    ask one retrospective exit-interview question",
		"end-interview  end the interview and close retained Sol",
		"git-record  reconcile local generation Git provenance",
		"pause       pause the running generation",
		"resume      resume the paused generation",
		"abort REASON  permanently abort the running/paused generation",
		"continue    start the cooperatively selected successor",
		"rollback N  fork from completed generation N",
		"quit        end the run",
	} {
		c.printLine(line)
	}
}

func (c *PlainConsole) printStatus() error {
	if generation, ok := c.runtime.GenerationNumber(); ok {
		c.printLine(fmt.Sprintf("Generation: %d", generation))
	} else {
		c.printLine("Generation: none")
	}
	c.printLine("State: " + runtimeStateName(c.runtime.State()))
	c.printLine(fmt.Sprintf("Source content capacity: %d bytes (snapshot maximum: %d bytes)", c.runtime.PresentationSnapshot().SourceCapacity.Bytes(), c.runtime.PresentationSnapshot().SourceCapacity.SnapshotLimit()))
	if pid, ok := c.runtime.ActivePID(); ok {
		c.printLine(fmt.Sprintf("QEMU PID: %d", pid))
	} else {
		c.printLine("QEMU PID: none")
	}
	state := c.runtime.State()
	if state == experiment.RuntimeStateRunning || state == experiment.RuntimeStatePaused {
		c.printLine("Hardware profile: " + c.runtime.HardwareProfile().Profile)
	}
	_, selected := c.runtime.PendingGenerationFinish()
	c.printLine("Selected successor: " + yesNo(selected))
	requests, err := c.runtime.FeatureRequests()
	if err != nil {
		return err
	}
	pending := 0
	for _, request := range requests {
		if request.Status == store.FeaturePending {
			pending++
		}
	}
	c.printLine(fmt.Sprintf("Pending feature requests: %d", pending))
	c.printObservability()
	sessionOwned := c.controller.SessionOwned()
	if sessionOwned {
		c.printLine("Codex session: active")
	} else {
		c.printLine("Codex session: none")
	}
	if c.currentTurn() != nil {
		c.printLine("Codex turn: running")
	} else if sessionOwned {
		c.printLine("Codex turn: idle")
	} else {
		c.printLine("Codex turn: none")
	}
	c.printLine("Exit interview: " + c.exitInterviewState())
	if handoff, ok := c.runtime.PreviousHandoff(); ok {
		label := "Previous handoff"
		if state == experiment.RuntimeStateAwaitingNextGeneration {
			label = "Handoff"
		}
		c.printLine(label + ":")
		c.printIndented(handoff)
	} else {
		c.printLine("Previous handoff: none")
	}
	return nil
}

func (c *PlainConsole) printHistory() error {
	items, err := c.runtime.ArchivedGenerations()
	if err != nil {
		return err
	}
	if len(items) == 0 {
		c.printLine("No archived generations.")
		return nil
	}
	c.printLine("GEN   PARENT   TRANSITION   OUTCOME")
	for _, item := range items {
		parent := "-"
		if item.ParentGeneration != nil {
			parent = strconv.FormatUint(*item.ParentGeneration, 10)
		}
		c.printLine(fmt.Sprintf("%-5d %-8s %-12s %s", item.Generation, parent, item.Transition, item.Outcome))
		if item.AbortReason != nil {
			c.printLine("      Abort reason: " + EscapeTerminalText(*item.AbortReason, false))
		}
	}
	return nil
}

func (c *PlainConsole) printInspection(item experiment.ArchivedGeneration) {
	parent := "-"
	if item.ParentGeneration != nil {
		parent = strconv.FormatUint(*item.ParentGeneration, 10)
	}
	c.printLine(fmt.Sprintf("Generation: %d", item.Generation))
	c.printLine("Parent: " + parent)
	c.printLine("Transition: " + item.Transition)
	c.printLine("Outcome: " + item.Outcome)
	c.printLine("Archive: " + item.ArchivePath)
	c.printLine(fmt.Sprintf("Source content capacity: %d bytes (snapshot maximum: %d bytes)", item.SourceCapacity.Bytes(), item.SourceCapacity.SnapshotLimit()))
	if item.Bootstrap != nil {
		c.printLine(fmt.Sprintf("Bootstrap artifacts: %d retained job references; per-job memory=%d bytes; retained run budget=%d bytes", len(item.Bootstrap.Jobs), item.Bootstrap.Limits.Memory, item.Bootstrap.Limits.RunBytes))
	}
	c.printLine("Hardware:")
	c.printLine("  Profile: " + item.Hardware.Profile)
	c.printLine("  Machine: " + item.Hardware.Machine)
	c.printLine(fmt.Sprintf("  CPU: %s x %d", item.Hardware.CPUModel, item.Hardware.VCPUs))
	c.printLine(fmt.Sprintf("  RAM: %d MiB", item.Hardware.MemoryMiB))
	c.printLine("  Graphics: " + item.Hardware.Graphics)
	c.printLine("  Network: " + item.Hardware.Network)
	writable := strings.Join(item.Hardware.WritableBlockDevices, ", ")
	if writable == "" {
		writable = "none"
	}
	c.printLine("  Writable disks: " + writable)
	if item.Outcome == "completed" {
		c.printLine("Handoff:")
		if item.Handoff != nil {
			c.printIndented(*item.Handoff)
		} else {
			c.printIndented("")
		}
		c.printLine("Artifacts:")
		for _, artifact := range []string{"boot ISO", "hardware manifest", "source snapshot", "materialized source", "successor kernel", "successor ISO", "QEMU stdout", "QEMU stderr"} {
			c.printLine("  " + artifact)
		}
	} else {
		c.printLine("Generation aborted by operator.")
		if item.AbortReason != nil {
			c.printLine("Abort reason:")
			c.printIndented(EscapeTerminalText(*item.AbortReason, true))
		} else {
			c.printLine("Abort reason: unavailable (legacy archive)")
		}
		c.printLine("Artifacts:")
		for _, artifact := range []string{"boot ISO", "hardware manifest", "QEMU stdout", "QEMU stderr"} {
			c.printLine("  " + artifact)
		}
	}
}

func (c *PlainConsole) printFeatures() error {
	requests, err := c.runtime.FeatureRequests()
	if err != nil {
		return err
	}
	if len(requests) == 0 {
		c.printLine("No feature requests.")
		return nil
	}
	c.printLine("ID   GEN   STATUS     TITLE")
	for _, request := range requests {
		c.printLine(fmt.Sprintf("%-4d %-5d %-10s %s", request.ID, request.Generation, request.Status, EscapeTerminalText(request.Title, false)))
		if request.DecisionNote != "" {
			c.printLine("     Operator decision note: " + EscapeTerminalText(request.DecisionNote, false))
		}
	}
	return nil
}

func (c *PlainConsole) printFeature(requestID uint64) error {
	request, err := c.runtime.FeatureRequest(requestID)
	if err != nil {
		return err
	}
	c.printLine(fmt.Sprintf("Feature request: #%d", request.ID))
	c.printLine(fmt.Sprintf("Generation: %d", request.Generation))
	c.printLine("Status: " + string(request.Status))
	c.printLine("Title: " + EscapeTerminalText(request.Title, false))
	c.printLine("")
	c.printLine("Description:")
	c.printIndented(EscapeTerminalText(request.Description, true))
	if request.DecisionNote != "" {
		c.printLine("Operator decision note:")
		c.printIndented(EscapeTerminalText(request.DecisionNote, true))
	}
	return nil
}

// The note is the trimmed remainder of the command; inner whitespace and quotes
// are literal text, matching other free-text operator commands.
func (c *PlainConsole) featureDecisionArguments(line string, words []string) (uint64, string, bool) {
	if len(words) < 2 {
		c.printLine("Usage: " + words[0] + " N [NOTE]")
		return 0, "", false
	}
	id, ok := c.unsignedArgument(words[:2], words[0])
	if !ok {
		return 0, "", false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), words[0]))
	note := strings.TrimSpace(strings.TrimPrefix(rest, words[1]))
	if err := store.ValidateFeatureDecisionNote(note); err != nil {
		c.printLine(err.Error())
		return 0, "", false
	}
	if note != "" {
		c.printLine("Operator decision note: " + EscapeTerminalText(note, false))
	}
	return id, note, true
}

func (c *PlainConsole) approveFeature(requestID uint64, note string) error {
	if !c.confirm(fmt.Sprintf("Mark feature request #%d approved?\nOnly do this after the trusted external capability has been provisioned. [y/N] ", requestID)) {
		c.printLine("Feature approval cancelled.")
		return nil
	}
	if _, err := c.runtime.ApproveFeatureRequest(requestID, note); err != nil {
		return err
	}
	c.printLine(fmt.Sprintf("Feature request #%d approved.", requestID))
	return nil
}

func (c *PlainConsole) denyFeature(requestID uint64, note string) error {
	if !c.confirm(fmt.Sprintf("Deny feature request #%d? [y/N] ", requestID)) {
		c.printLine("Feature denial cancelled.")
		return nil
	}
	if _, err := c.runtime.DenyFeatureRequest(requestID, note); err != nil {
		return err
	}
	c.printLine(fmt.Sprintf("Feature request #%d denied.", requestID))
	return nil
}

func (c *PlainConsole) printGate() error {
	generation, _ := c.runtime.GenerationNumber()
	pending, selected := c.runtime.PendingGenerationFinish()
	c.printLine("")
	if selected {
		c.printLine(fmt.Sprintf("Generation %d complete.", generation))
		c.printLine("")
		c.printLine("Handoff:")
		c.printIndented(pending.HandoffMessage)
		c.printLine("")
		c.printLine("A successor is selected.")
		c.printLine("")
		c.printLine("Use:")
		c.printLine("  continue")
		if c.controller.ExitInterviewAvailable() {
			c.printLine("  interview")
		}
	} else {
		c.printLine(fmt.Sprintf("Generation %d aborted.", generation))
		c.printLine("")
		c.printLine("No successor was selected.")
		c.printLine("")
		c.printLine("Use:")
	}
	c.printLine("  rollback N")
	c.printLine(fmt.Sprintf("  inspect %d", generation))
	c.printLine("  history")
	requests, err := c.runtime.FeatureRequests()
	if err != nil {
		return err
	}
	pendingRequests := make([]store.FeatureRequest, 0)
	for _, request := range requests {
		if request.Status == store.FeaturePending {
			pendingRequests = append(pendingRequests, request)
		}
	}
	if len(pendingRequests) > 0 {
		c.printLine("")
		c.printLine("Pending feature requests:")
		c.printLine("")
		for _, request := range pendingRequests {
			c.printLine(fmt.Sprintf("#%d  %s", request.ID, EscapeTerminalText(request.Title, false)))
		}
		c.printLine("")
		c.printLine("Use:")
		c.printLine("  features")
		c.printLine("  feature N")
		c.printLine("  feature-approve N")
		c.printLine("  feature-deny N")
	}
	c.printLine("  quit")
	return nil
}

func (c *PlainConsole) printRunningSummary() {
	generation, _ := c.runtime.GenerationNumber()
	c.printLine(fmt.Sprintf("Generation %d: %s", generation, runtimeStateName(c.runtime.State())))
	if pid, ok := c.runtime.ActivePID(); ok {
		c.printLine(fmt.Sprintf("QEMU PID: %d", pid))
	} else {
		c.printLine("QEMU PID: none")
	}
}

func (c *PlainConsole) reconcileGit() bool {
	if c.git == nil {
		return true
	}
	records, err := c.git.Reconcile()
	if err != nil {
		c.record("git_reconciliation_failed", map[string]any{"error": boundedConsoleError(err)})
		c.printLine("Git provenance error: " + err.Error())
		return false
	}
	for _, record := range records {
		generation := record.Generation
		data := map[string]any{
			"generation": record.Generation, "tag": record.Tag, "commit": record.Commit,
			"already_recorded": record.AlreadyRecorded,
		}
		c.recordAt("git_generation_recorded", &generation, data)
	}
	return true
}

func (c *PlainConsole) persistInterview(outcome string) {
	if c.interviews == nil {
		return
	}
	c.interviewMu.Lock()
	defer c.interviewMu.Unlock()
	transcript, ok := c.controller.ExitInterviewTranscript()
	if !ok || len(transcript.Turns) == 0 {
		return
	}
	generation := transcript.Metadata.Generation
	if c.persistedInterview != nil && *c.persistedInterview == generation {
		return
	}
	artifact, err := c.interviews.Finalize(transcript, outcome)
	if err != nil {
		c.printLine("Exit interview transcript error: " + err.Error())
		return
	}
	if artifact == nil {
		return
	}
	c.persistedInterview = &generation
	c.printLine("Exit interview saved:")
	c.printLine("  " + artifact.RelativePath)
}

func (c *PlainConsole) resetPersistedInterview() {
	c.interviewMu.Lock()
	c.persistedInterview = nil
	c.interviewMu.Unlock()
}

func (c *PlainConsole) recordOperator(action, result string, extra map[string]any) {
	data := map[string]any{"result": result}
	for key, value := range extra {
		data[key] = value
	}
	c.record("operator_"+action, data)
}

func (c *PlainConsole) record(event string, data map[string]any) {
	var generation *uint64
	if value, ok := c.runtime.GenerationNumber(); ok {
		generation = &value
	}
	c.recordAt(event, generation, data)
}

func (c *PlainConsole) recordAt(event string, generation *uint64, data map[string]any) {
	if provider, ok := any(c.runtime).(interface {
		HarnessIdentity() *provenance.HarnessIdentity
	}); ok {
		if identity := provider.HarnessIdentity(); identity != nil {
			copy := make(map[string]any, len(data)+1)
			for key, value := range data {
				copy[key] = value
			}
			copy["harness_identity"] = identity.AsJSON()
			data = copy
		}
	}
	if log := c.runtime.EventLog(); log != nil {
		log.Record(event, generation, data)
	}
	if metrics := c.runtime.Metrics(); metrics != nil {
		metrics.Record(event, data)
	}
}

func (c *PlainConsole) printObservability() {
	if c.runtime.EventLog() == nil && c.runtime.Metrics() == nil {
		return
	}
	reasons := make([]string, 0, 2)
	if log := c.runtime.EventLog(); log != nil && !log.Healthy() {
		reasons = append(reasons, log.DegradedReason())
	}
	if metrics := c.runtime.Metrics(); metrics != nil && !metrics.Healthy() {
		reasons = append(reasons, metrics.DegradedReason())
	}
	if len(reasons) == 0 {
		c.printLine("Observability: healthy")
	} else {
		c.printLine("Observability: degraded - " + strings.Join(reasons, "; "))
	}
}

func (c *PlainConsole) requireArity(words []string, count int, command string) bool {
	if len(words) == count {
		return true
	}
	c.printLine("Usage: " + command)
	return false
}

func (c *PlainConsole) unsignedArgument(words []string, command string) (uint64, bool) {
	if len(words) != 2 || words[1] == "" {
		c.printLine("Usage: " + command + " N")
		return 0, false
	}
	for index := range len(words[1]) {
		if words[1][index] < '0' || words[1][index] > '9' {
			c.printLine("Usage: " + command + " N")
			return 0, false
		}
	}
	value, err := strconv.ParseUint(words[1], 10, 64)
	if err != nil {
		c.printLine("Usage: " + command + " N")
		return 0, false
	}
	return value, true
}

func (c *PlainConsole) confirm(prompt string) bool {
	if c.confirmationHandler != nil {
		return c.confirmationHandler(prompt)
	}
	line, err := c.readLine(prompt)
	return err == nil && (strings.TrimSpace(line) == "y" || strings.TrimSpace(line) == "Y")
}

func (c *PlainConsole) prompt() string {
	if c.interviewOpen() {
		return "interview> "
	}
	return "codexos> "
}

func (c *PlainConsole) readLine(prompt string) (string, error) {
	c.outputMu.Lock()
	_, writeErr := io.WriteString(c.output, prompt)
	if writeErr == nil {
		if flusher, ok := c.output.(interface{ Flush() error }); ok {
			writeErr = flusher.Flush()
		}
	}
	c.outputMu.Unlock()
	if writeErr != nil {
		return "", writeErr
	}
	return c.input.ReadString('\n')
}

func (c *PlainConsole) printLine(text string) {
	if c.outputHandler != nil {
		callConsoleOutputHandler(c.outputHandler, text)
		return
	}
	c.outputMu.Lock()
	_, _ = fmt.Fprintln(c.output, text)
	c.outputMu.Unlock()
}

func callConsoleOutputHandler(handler func(string), text string) {
	defer func() {
		_ = recover()
	}()
	handler(text)
}

func (c *PlainConsole) printIndented(text string) {
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 {
		lines = []string{""}
	}
	for _, line := range lines {
		c.printLine("  " + line)
	}
}

func (c *PlainConsole) waitForConsoleTurn(turn *consoleTurn) error {
	if turn == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.controller.options.InterruptTimeout)
	defer cancel()
	return waitConsoleTurn(ctx, turn.done)
}

func (c *PlainConsole) currentTurn() *consoleTurn {
	c.turnMu.Lock()
	defer c.turnMu.Unlock()
	return c.turn
}

func (c *PlainConsole) interviewOpen() bool {
	return c.controller.InterviewOpen()
}

func (c *PlainConsole) exitInterviewState() string {
	turn := c.currentTurn()
	if c.interviewOpen() {
		if turn != nil && turn.interview {
			return "answering"
		}
		return "idle"
	}
	if c.controller.ExitInterviewAvailable() {
		return "available"
	}
	return "unavailable"
}

// CodexTurnState is the compact operator presentation state used by the TUI.
func (c *PlainConsole) CodexTurnState() string {
	if c == nil {
		return "stopped"
	}
	switch c.controller.ReviewYieldState() {
	case agent.ReviewYieldStoppingOrigin:
		return "yielding for review"
	case agent.ReviewYieldAwaitingReview:
		return "awaiting review"
	case agent.ReviewYieldReviewing:
		return "reviewing"
	case agent.ReviewYieldAwaitingContinuation:
		return "review ready"
	case agent.ReviewYieldFailed:
		return "review failed"
	case agent.ReviewYieldResuming:
		return "resuming review"
	}
	if phase := c.controller.ActiveTurnPhase(); phase != "" {
		switch phase {
		case "implementation", "continuation":
			return "implementation"
		default:
			return phase
		}
	}
	if turn := c.currentTurn(); turn != nil {
		return "starting"
	}
	if c.controller.SessionOwned() {
		return "idle"
	}
	return "stopped"
}

// CodexActivity identifies the agent and phase currently represented by the
// live session. It is presentation state only and grants no lifecycle control.
func (c *PlainConsole) CodexActivity() (string, string) {
	if c == nil {
		return "", ""
	}
	switch c.controller.ReviewYieldState() {
	case agent.ReviewYieldStoppingOrigin:
		phase := c.controller.ActiveTurnPhase()
		if phase == "" {
			phase = "review handoff"
		}
		return "Sol", phase
	case agent.ReviewYieldAwaitingReview, agent.ReviewYieldReviewing:
		return "Luna", "review"
	case agent.ReviewYieldFailed:
		return "Luna", "review failed"
	case agent.ReviewYieldAwaitingContinuation, agent.ReviewYieldResuming:
		return "Sol", "review continuation"
	}
	if c.exitInterviewState() == "answering" {
		return "Sol", "interview"
	}
	phase := c.controller.ActiveTurnPhase()
	if phase == "continuation" {
		phase = "implementation"
	}
	if phase != "" {
		return "Sol", phase
	}
	if turn := c.currentTurn(); turn != nil && !turn.interview {
		switch turn.phase {
		case "initial", "planning":
			return "Sol", "planning"
		case "continuation":
			return "Sol", "implementation"
		default:
			return "Sol", "starting"
		}
	}
	return "", ""
}

// ExitInterviewState reports the compact interview presentation state used by
// the TUI without exposing mutable session ownership.
func (c *PlainConsole) ExitInterviewState() string {
	if c == nil {
		return "unavailable"
	}
	return c.exitInterviewState()
}

func waitConsoleTurn(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func runtimeStateName(state experiment.RuntimeState) string {
	return strings.ToUpper(string(state))
}

func boundedConsoleError(err error) string {
	text := err.Error()
	if len(text) > 1024 {
		return text[:1024]
	}
	return text
}
