package tui

// This file contains the Bubble Tea frontend for the activity model.  The
// model remains the authority for event interpretation and bounds; this layer
// only owns terminal interaction, command input, and the current viewport.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"codexos/internal/observability"
)

const (
	defaultActivityPoll = 50 * time.Millisecond
	defaultStatusPoll   = 250 * time.Millisecond
	pauseConfirmation   = 2500 * time.Millisecond
)

// InterviewState is the small amount of interview state needed by the view.
// The command runner remains responsible for deciding whether a question is
// actually admissible.
type InterviewState string

const (
	InterviewUnavailable InterviewState = "unavailable"
	InterviewAvailable   InterviewState = "available"
	InterviewIdle        InterviewState = "idle"
	InterviewAnswering   InterviewState = "answering"
)

// StatusSnapshot is a read-only view of runtime state supplied by the
// operator/runtime integration.  TUI state such as Busy is overwritten by the
// application while a command is in flight.
type StatusSnapshot struct {
	RunName         string
	Generation      uint64
	HasGeneration   bool
	RuntimeState    string
	SolState        string
	PendingFeatures int
	Interview       InterviewState
	Busy            bool
}

// ConfirmationFunc is passed to the command runner for operations which
// need an operator decision.  It blocks the command worker until the TUI
// receives y/yes, or until shutdown (which is always No).
type ConfirmationFunc func(prompt string) bool

// CommandResult is the completed, aggregated result of one operator command.
// Streaming activity is delivered independently through ActivityStream.
type CommandResult struct {
	Output string
	Status *StatusSnapshot
	Exit   bool
}

// CommandHandler is the only command integration point owned by the TUI.
// OperatorConsole/runtime code can keep all command meaning and lifecycle
// authority outside this package.
type CommandHandler func(context.Context, string, ConfirmationFunc) (CommandResult, error)

// StatusProvider supplies a current snapshot for the compact header.  It is
// deliberately a function rather than a runtime interface: the application
// does not own experiment state.
type StatusProvider func() StatusSnapshot

// ApplicationOptions configures one Bubble Tea application instance.
type ApplicationOptions struct {
	Activity      *observability.ActivityStream
	Status        StatusProvider
	Execute       CommandHandler
	OnShutdown    func()
	StartupOutput string
	ModelOptions  ActivityModelOptions
	ActivityPoll  time.Duration
	StatusPoll    time.Duration
}

// Application is a Bubble Tea v2 model and the concrete full-screen operator
// frontend.  It uses Bubble Tea's default cursed renderer; Run delegates
// terminal mode setup and restoration to Bubble Tea's bounded Run/shutdown
// path.
type Application struct {
	activity *observability.ActivityStream
	statusFn StatusProvider
	execute  CommandHandler
	onClose  func()

	model  *OperatorActivityModel
	follow ActivityFollowState

	activityPoll time.Duration
	statusPoll   time.Duration
	startup      string

	status StatusSnapshot
	input  string
	prompt string
	busy   bool

	width  int
	height int
	// scrollTop is a transcript line offset used only while live-follow is
	// released.  Keeping a line offset lets PageUp/PageDown work inside a
	// multiline entry instead of jumping whole entries.
	scrollTop int
	expanded  map[string]bool

	pauseDeadline time.Time
	pauseGen      uint64

	initialized bool
	closed      bool

	// viewMu protects frontend state exposed to callers outside Bubble Tea's
	// event loop.  Bubble Tea itself invokes Init, Update, and View serially,
	// but diagnostics and tests may inspect the application concurrently.
	viewMu sync.RWMutex

	stateMu      sync.Mutex
	program      *tea.Program
	context      context.Context
	cancel       context.CancelFunc
	started      bool
	done         chan struct{}
	confirmation *confirmationRequest
	shutdownOnce sync.Once
	hookOnce     sync.Once
}

type activityPollMsg struct{}
type statusPollMsg struct{}

type commandResultMsg struct {
	result CommandResult
	err    error
}

type confirmationRequest struct {
	prompt string
	reply  chan bool
}

// NewApplication constructs a frontend without opening a terminal or
// starting a goroutine.  It is therefore safe to use in deterministic tests.
func NewApplication(options ApplicationOptions) (*Application, error) {
	model, err := NewOperatorActivityModel(options.ModelOptions)
	if err != nil {
		return nil, err
	}
	activityPoll := options.ActivityPoll
	if activityPoll <= 0 {
		activityPoll = defaultActivityPoll
	}
	statusPoll := options.StatusPoll
	if statusPoll <= 0 {
		statusPoll = defaultStatusPoll
	}
	status := StatusSnapshot{
		RunName:      "CodexOS",
		RuntimeState: "unknown",
		SolState:     "idle",
		Interview:    InterviewUnavailable,
	}
	if options.Status != nil {
		status = options.Status()
	}
	status = normalizeStatus(status)
	return &Application{
		activity:     options.Activity,
		statusFn:     options.Status,
		execute:      options.Execute,
		onClose:      options.OnShutdown,
		model:        model,
		follow:       NewActivityFollowState(),
		activityPoll: activityPoll,
		statusPoll:   statusPoll,
		startup:      options.StartupOutput,
		status:       status,
		prompt:       "codexos>",
		height:       24,
		width:        80,
		expanded:     make(map[string]bool),
		context:      context.Background(),
		done:         make(chan struct{}),
	}, nil
}

// cloneActivityModel returns a read-only diagnostic snapshot.  Returning the
// live model would let a caller race the Bubble Tea event loop (and would
// expose mutable maps/slices through the model's internal presentation types).
func cloneActivityModel(source *OperatorActivityModel) *OperatorActivityModel {
	if source == nil {
		return nil
	}
	snapshot := &OperatorActivityModel{
		maxEntries:        source.maxEntries,
		maxBytes:          source.maxBytes,
		displayBytes:      source.displayBytes,
		entries:           source.Entries(),
		positions:         make(map[string]int, len(source.positions)),
		messageText:       make(map[string]string, len(source.messageText)),
		reasoningText:     make(map[string]map[int]string, len(source.reasoningText)),
		toolPresentations: make(map[string]ToolPresentation, len(source.toolPresentations)),
		buildNumber:       source.buildNumber,
		activeBuildKey:    source.activeBuildKey,
		builds:            make(map[string]BuildPresentation, len(source.builds)),
		operatorNumber:    source.operatorNumber,
		activeOperatorKey: source.activeOperatorKey,
		discarded:         source.discarded,
		revision:          source.revision,
	}
	for key, position := range source.positions {
		snapshot.positions[key] = position
	}
	for key, text := range source.messageText {
		snapshot.messageText[key] = text
	}
	for key, parts := range source.reasoningText {
		clonedParts := make(map[int]string, len(parts))
		for index, text := range parts {
			clonedParts[index] = text
		}
		snapshot.reasoningText[key] = clonedParts
	}
	for key, presentation := range source.toolPresentations {
		snapshot.toolPresentations[key] = clonePresentation(presentation).(ToolPresentation)
	}
	for key, presentation := range source.builds {
		cloned := clonePresentation(presentation).(BuildPresentation)
		snapshot.builds[key] = cloned
	}
	if source.latestReviewerMessage != nil {
		identity := *source.latestReviewerMessage
		snapshot.latestReviewerMessage = &identity
	}
	return snapshot
}

// ActivityModel exposes the frontend-independent transcript for diagnostics
// and integration tests.  Callers receive model snapshots from Entries.
func (a *Application) ActivityModel() *OperatorActivityModel {
	if a == nil {
		return nil
	}
	a.viewMu.RLock()
	defer a.viewMu.RUnlock()
	return cloneActivityModel(a.model)
}

// FollowState returns a copy of the live-follow state.
func (a *Application) FollowState() ActivityFollowState {
	if a == nil {
		return ActivityFollowState{}
	}
	a.viewMu.RLock()
	defer a.viewMu.RUnlock()
	return a.follow
}

// Input returns the currently typed command text.
func (a *Application) Input() string {
	if a == nil {
		return ""
	}
	a.viewMu.RLock()
	defer a.viewMu.RUnlock()
	return a.input
}

// SetStatus injects a status snapshot for tests and runtime integrations that
// already have an ordered event loop.  Normal applications should prefer a
// StatusProvider so the header refresh remains read-only and current.
func (a *Application) SetStatus(status StatusSnapshot) {
	if a == nil {
		return
	}
	a.viewMu.Lock()
	defer a.viewMu.Unlock()
	a.status = normalizeStatus(status)
	a.status.Busy = a.busy
	a.disarmPauseIfStateChanged()
}

// ConfirmationPrompt reports the visible prompt, if a command is waiting for
// an operator decision.
func (a *Application) ConfirmationPrompt() string {
	if a == nil {
		return ""
	}
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.confirmation == nil {
		return ""
	}
	return a.confirmation.prompt
}

// Shutdown cancels a pending confirmation and invokes the integration hook
// once. When the program is running it also asks Bubble Tea to leave its event
// loop, allowing Bubble Tea to restore terminal state through its normal path.
func (a *Application) Shutdown() {
	if a == nil {
		return
	}
	a.close()
	a.stateMu.Lock()
	p := a.program
	started := a.started
	a.stateMu.Unlock()
	if p == nil || !started {
		// There is no Bubble Tea Run whose terminal restoration we need to
		// await.  Run itself invokes the hook after Bubble Tea has returned.
		a.finishShutdown()
		return
	}
	if p != nil {
		// close canceled the derived context used by command handlers.  Quit
		// lets Bubble Tea own renderer/input restoration; Run's deferred
		// cleanup handles setup failures and calls Kill when necessary.
		p.Quit()
	}
}

// ExpandedTool reports whether a tool's bounded detail is visible.
func (a *Application) ExpandedTool(key string) bool {
	if a == nil {
		return false
	}
	a.viewMu.RLock()
	defer a.viewMu.RUnlock()
	return a.expanded[key]
}

// ToggleTool toggles one existing tool entry.  It returns false for missing
// keys and entries without detail, keeping selection a presentation-only
// operation.
func (a *Application) ToggleTool(key string) bool {
	if a == nil {
		return false
	}
	a.viewMu.Lock()
	defer a.viewMu.Unlock()
	return a.toggleTool(key)
}

func (a *Application) toggleTool(key string) bool {
	entries := a.model.Entries()
	anchorKey, anchorLine := a.scrollAnchor(entries)
	for _, entry := range entries {
		if entry.Key != key {
			continue
		}
		presentation, ok := entry.Presentation.(ToolPresentation)
		if !ok || presentation.Detail == nil {
			return false
		}
		a.expanded[key] = !a.toolExpanded(entry)
		a.restoreScrollAnchor(anchorKey, anchorLine)
		a.clampScroll()
		return true
	}
	return false
}

// RequestConfirmation can be used by a command handler outside the Bubble
// Tea command callback.  It returns false when the app is not running or is
// shutting down.
func (a *Application) RequestConfirmation(prompt string) bool {
	if a == nil {
		return false
	}
	request := &confirmationRequest{prompt: prompt, reply: make(chan bool, 1)}
	a.stateMu.Lock()
	p := a.program
	ctx := a.context
	closed := a.closed
	started := a.started
	a.stateMu.Unlock()
	if p == nil || closed || !started {
		return false
	}
	p.Send(request)
	select {
	case accepted := <-request.reply:
		return accepted
	case <-a.done:
		return false
	case <-ctx.Done():
		return false
	}
}

// Init starts bounded one-shot poll commands.  A tick schedules its own next
// tick, so there is no long-lived TUI goroutine to leak at shutdown.
func (a *Application) Init() tea.Cmd {
	a.viewMu.Lock()
	defer a.viewMu.Unlock()
	if !a.initialized {
		a.initialized = true
		a.refreshStatus()
		a.model.BeginOperatorBlock(nil)
		if a.startup != "" {
			a.model.AppendOperatorOutput(a.startup)
		}
		a.model.FinishOperatorBlock()
	}
	return tea.Batch(a.activityTick(), a.statusTick())
}

// Update consumes Bubble Tea v2 events and returns the next command.  All
// model mutations happen on this event-loop goroutine except the explicit
// synchronized status setter used by tests/integrations.
func (a *Application) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if a == nil {
		return a, nil
	}
	a.viewMu.Lock()
	defer a.viewMu.Unlock()
	switch value := msg.(type) {
	case activityPollMsg:
		a.drainActivity()
		return a, a.activityTick()
	case statusPollMsg:
		a.refreshStatus()
		a.expirePauseArm()
		return a, a.statusTick()
	case commandResultMsg:
		return a, a.applyCommandResult(value)
	case *confirmationRequest:
		a.showConfirmation(value)
		return a, nil
	case confirmationRequest:
		request := value
		a.showConfirmation(&request)
		return a, nil
	case tea.WindowSizeMsg:
		a.width, a.height = value.Width, value.Height
		if a.height < 1 {
			a.height = 1
		}
		a.clampScroll()
		return a, nil
	case tea.KeyPressMsg:
		return a, a.handleKey(value)
	case tea.PasteMsg:
		a.handlePaste(value.Content)
		return a, nil
	case tea.MouseWheelMsg:
		return a, a.handleMouseWheel(value)
	case tea.MouseClickMsg:
		a.handleMouseClick(value)
		return a, nil
	case tea.QuitMsg:
		a.close()
		return a, nil
	default:
		return a, nil
	}
}

// View implements tea.Model using the v2 declarative View.  AltScreen and
// MouseMode are properties of the view in Bubble Tea v2; the cursed renderer
// is selected by NewProgram when no renderer override is supplied.
func (a *Application) View() tea.View {
	if a == nil {
		return tea.NewView("")
	}
	a.viewMu.RLock()
	defer a.viewMu.RUnlock()
	var view tea.View
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	view.SetContent(a.viewText())
	return view
}

// Run enters Bubble Tea's cursed renderer and always invokes the integration
// shutdown hook after Bubble Tea restores terminal state.  Passing a nil input
// disables input (useful for bounded non-interactive tests); output defaults
// to stdout when nil.
func (a *Application) Run(ctx context.Context, input io.Reader, output io.Writer) error {
	if a == nil {
		return errors.New("nil TUI application")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runContext, cancel := context.WithCancel(ctx)
	options := []tea.ProgramOption{tea.WithContext(runContext)}
	if input == nil {
		options = append(options, tea.WithInput(nil))
	} else {
		options = append(options, tea.WithInput(input))
	}
	if output != nil {
		options = append(options, tea.WithOutput(output))
	}
	program := tea.NewProgram(a, options...)
	a.stateMu.Lock()
	if a.started || a.program != nil {
		a.stateMu.Unlock()
		cancel()
		return errors.New("TUI application is already running")
	}
	if a.closed {
		a.stateMu.Unlock()
		cancel()
		return errors.New("TUI application is closed")
	}
	a.context = runContext
	a.cancel = cancel
	a.started = true
	a.program = program
	a.stateMu.Unlock()
	defer func() {
		// Bubble Tea normally restores terminal state before Run returns.  It
		// can return early during setup, though, so Kill is the idempotent
		// final cleanup for both paths.
		program.Kill()
		a.close()
		a.stateMu.Lock()
		a.program = nil
		a.cancel = nil
		a.started = false
		a.stateMu.Unlock()
		a.finishShutdown()
	}()
	_, err := program.Run()
	return err
}

func (a *Application) activityTick() tea.Cmd {
	interval := a.activityPoll
	return tea.Tick(interval, func(time.Time) tea.Msg { return activityPollMsg{} })
}

func (a *Application) statusTick() tea.Cmd {
	interval := a.statusPoll
	return tea.Tick(interval, func(time.Time) tea.Msg { return statusPollMsg{} })
}

func (a *Application) drainActivity() {
	if a.activity == nil {
		return
	}
	before := a.model.Entries()
	anchorKey, anchorLine := a.scrollAnchor(before)
	changed := 0
	for _, event := range a.activity.Drain() {
		if a.model.Consume(event) {
			changed++
		}
	}
	a.pruneExpanded()
	if changed == 0 {
		return
	}
	if a.follow.Following {
		a.scrollTop = 0
	} else {
		a.restoreScrollAnchor(anchorKey, anchorLine)
		a.follow.Arrived(changed)
		a.clampScroll()
	}
}

func (a *Application) pruneExpanded() {
	if len(a.expanded) == 0 {
		return
	}
	present := make(map[string]struct{}, len(a.model.Entries()))
	for _, entry := range a.model.Entries() {
		present[entry.Key] = struct{}{}
	}
	for key := range a.expanded {
		if _, ok := present[key]; !ok {
			delete(a.expanded, key)
		}
	}
}

func (a *Application) refreshStatus() {
	if a.statusFn != nil {
		a.status = normalizeStatus(a.statusFn())
	}
	a.status.Busy = a.busy
	a.disarmPauseIfStateChanged()
	if a.status.Interview == InterviewIdle {
		a.prompt = "interview>"
	} else if !a.busy {
		a.prompt = "codexos>"
	}
}

func normalizeStatus(status StatusSnapshot) StatusSnapshot {
	if status.RunName == "" {
		status.RunName = "CodexOS"
	}
	if status.RuntimeState == "" {
		status.RuntimeState = "unknown"
	}
	if status.SolState == "" {
		status.SolState = "idle"
	}
	if status.Interview == "" {
		status.Interview = InterviewUnavailable
	}
	if status.PendingFeatures < 0 {
		status.PendingFeatures = 0
	}
	return status
}

func (a *Application) disarmPauseIfStateChanged() {
	if a.pauseDeadline.IsZero() {
		return
	}
	if !runtimeRunning(a.status.RuntimeState) ||
		(a.status.HasGeneration && a.status.Generation != a.pauseGen) {
		a.pauseDeadline = time.Time{}
		a.pauseGen = 0
	}
}

func (a *Application) expirePauseArm() {
	if !a.pauseDeadline.IsZero() && !time.Now().Before(a.pauseDeadline) {
		a.pauseDeadline = time.Time{}
		a.pauseGen = 0
	}
}

func (a *Application) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.Key()
	if key.Code != tea.KeyEscape {
		a.pauseDeadline = time.Time{}
		a.pauseGen = 0
	}
	if key.Mod&(tea.ModCtrl|tea.ModMeta) != 0 {
		switch msg.String() {
		case "ctrl+c", "ctrl+d", "meta+c", "meta+d":
			return a.safeQuit()
		}
	}

	switch key.Code {
	case tea.KeyPgUp:
		a.scrollBy(-a.pageSize())
		return nil
	case tea.KeyPgDown:
		a.scrollBy(a.pageSize())
		return nil
	case tea.KeyEnd:
		a.follow.ReturnToLive()
		a.scrollTop = 0
		return nil
	case tea.KeyEscape:
		return a.handleEscape()
	case tea.KeyEnter, tea.KeyKpEnter:
		if a.hasConfirmation() {
			a.completeConfirmation(strings.EqualFold(strings.TrimSpace(a.input), "y"))
			return nil
		}
		if a.busy || strings.TrimSpace(a.input) == "" {
			return nil
		}
		command := a.input
		a.input = ""
		return a.submitCommand(command, false)
	case tea.KeyBackspace, tea.KeyDelete:
		if a.hasConfirmation() || a.input == "" {
			return nil
		}
		_, size := lastRune(a.input)
		if size > 0 {
			a.input = a.input[:len(a.input)-size]
		}
		return nil
	}
	if key.Mod&(tea.ModCtrl|tea.ModMeta) != 0 {
		shortcut := strings.ToLower(key.Text)
		if shortcut == "" {
			shortcut = strings.ToLower(string(key.Code))
		}
		switch shortcut {
		case "c", "d":
			return a.safeQuit()
		}
	}
	if key.Text != "" && printableInput(key.Text) {
		a.input += key.Text
	}
	return nil
}

// handlePaste keeps bracketed-paste input in the same single-command line as
// ordinary key input.  Newlines are separators in the line-oriented console,
// not a request to submit multiple commands, so they become spaces; other
// non-printable controls are ignored.  A confirmation still requires the
// existing explicit y/Y check when Enter is pressed.
func (a *Application) handlePaste(content string) {
	var pasted strings.Builder
	lastSpace := false
	for _, character := range normalizeUTF8(content) {
		switch character {
		case '\n', '\r', '\t':
			if !lastSpace {
				pasted.WriteByte(' ')
				lastSpace = true
			}
		default:
			if !unicode.IsPrint(character) {
				continue
			}
			pasted.WriteRune(character)
			lastSpace = false
		}
	}
	value := strings.TrimRight(pasted.String(), " ")
	if value == "" {
		return
	}
	a.pauseDeadline = time.Time{}
	a.pauseGen = 0
	a.input += value
}

func (a *Application) handleEscape() tea.Cmd {
	if a.hasConfirmation() {
		a.completeConfirmation(false)
		return nil
	}
	if a.busy || !runtimeRunning(a.status.RuntimeState) {
		a.pauseDeadline = time.Time{}
		a.pauseGen = 0
		return nil
	}
	now := time.Now()
	if !a.pauseDeadline.IsZero() && now.Before(a.pauseDeadline) &&
		(!a.status.HasGeneration || a.pauseGen == a.status.Generation) {
		a.pauseDeadline = time.Time{}
		a.pauseGen = 0
		return a.submitCommand("pause", true)
	}
	a.pauseDeadline = now.Add(pauseConfirmation)
	if a.status.HasGeneration {
		a.pauseGen = a.status.Generation
	}
	return nil
}

func (a *Application) safeQuit() tea.Cmd {
	a.pauseDeadline = time.Time{}
	a.pauseGen = 0
	if a.busy {
		return nil
	}
	a.input = ""
	return a.submitCommand("quit", false)
}

func (a *Application) submitCommand(command string, preserveInput bool) tea.Cmd {
	if a.busy {
		return nil
	}
	interviewQuestion := a.status.Interview == InterviewIdle && !isInterviewEndCommand(command)
	if interviewQuestion {
		a.model.BeginOperatorBlock(nil)
	} else {
		value := command
		a.model.BeginOperatorBlock(&value)
	}
	a.busy = true
	a.status.Busy = true
	if !preserveInput {
		a.input = ""
	}
	handler := a.execute
	a.stateMu.Lock()
	ctx := a.context
	a.stateMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	return func() tea.Msg {
		if handler == nil {
			return commandResultMsg{err: errors.New("operator command handler is unavailable")}
		}
		result, err := handler(ctx, command, a.RequestConfirmation)
		return commandResultMsg{result: result, err: err}
	}
}

func (a *Application) applyCommandResult(message commandResultMsg) tea.Cmd {
	if message.result.Output != "" {
		a.model.AppendOperatorOutput(message.result.Output)
	}
	if message.err != nil {
		// The Python console reports ordinary command exceptions and returns
		// to its prompt.  A returned Go error has the same recoverable meaning;
		// only an explicit CommandResult.Exit (or a Bubble Tea panic) ends the
		// application.
		a.model.AppendOperatorOutput("Error: " + SafeDisplayText(message.err.Error(), DefaultDisplayBytes))
		a.model.FinishOperatorBlock()
		a.busy = false
		a.status.Busy = false
		a.refreshStatus()
		return nil
	}
	a.model.FinishOperatorBlock()
	if message.result.Status != nil {
		a.status = normalizeStatus(*message.result.Status)
	}
	a.busy = false
	a.status.Busy = false
	if message.result.Exit {
		return tea.Quit
	}
	a.refreshStatus()
	return nil
}

func (a *Application) showConfirmation(request *confirmationRequest) {
	if request == nil {
		return
	}
	a.stateMu.Lock()
	if a.closed || a.confirmation != nil {
		a.stateMu.Unlock()
		request.reply <- false
		return
	}
	a.confirmation = request
	a.stateMu.Unlock()
	safePrompt := strings.TrimSpace(strings.ReplaceAll(
		SafeDisplayText(request.prompt, SummaryDisplayBytes), "\n", " / ",
	))
	a.prompt = safePrompt + " [y/N]"
	a.input = ""
}

func (a *Application) completeConfirmation(accepted bool) {
	a.stateMu.Lock()
	request := a.confirmation
	a.confirmation = nil
	a.stateMu.Unlock()
	if request != nil {
		request.reply <- accepted
	}
	a.prompt = a.commandPrompt()
	a.input = ""
}

func (a *Application) commandPrompt() string {
	if a.status.Interview == InterviewIdle {
		return "interview>"
	}
	return "codexos>"
}

func (a *Application) close() {
	a.shutdownOnce.Do(func() {
		a.stateMu.Lock()
		a.closed = true
		request := a.confirmation
		a.confirmation = nil
		cancel := a.cancel
		a.stateMu.Unlock()
		if cancel != nil {
			cancel()
		}
		close(a.done)
		if request != nil {
			request.reply <- false
		}
	})
}

// finishShutdown invokes the integration hook once, after Bubble Tea has
// returned (and therefore after its terminal restoration).  Calls made before
// Run are completed immediately because there is no terminal session to wait
// for; a running session always reaches this through Run's defer.
func (a *Application) finishShutdown() {
	a.hookOnce.Do(func() {
		if a.onClose != nil {
			a.onClose()
		}
	})
}

func (a *Application) pageSize() int {
	lines := a.transcriptHeight()
	if lines < 1 {
		return 1
	}
	return lines
}

func (a *Application) scrollBy(delta int) {
	entries := a.model.Entries()
	if len(entries) == 0 {
		return
	}
	_, totalLines := a.transcriptLayout(entries)
	maxOffset := totalLines - a.transcriptHeight()
	if maxOffset < 0 {
		maxOffset = 0
	}
	if a.follow.Following {
		a.follow.Scrolled(float64(maxOffset))
		a.scrollTop = maxOffset
	}
	a.scrollTop += delta
	a.clampScroll()
	a.follow.ScrollY = float64(a.scrollTop)
}

func (a *Application) visibleWindow(entries []ActivityDisplayEntry) (int, int) {
	if len(entries) == 0 {
		return 0, 0
	}
	layout, totalLines := a.transcriptLayout(entries)
	maxLines := a.transcriptHeight()
	offset := a.viewportOffset(totalLines)
	endLine := offset + maxLines
	start := 0
	for start < len(layout) && layout[start].end <= offset {
		start++
	}
	end := start
	for end < len(layout) && layout[end].start < endLine {
		end++
	}
	return start, end
}

func (a *Application) viewText() string {
	entries := a.model.Entries()
	transcript := a.visibleTranscript(entries)
	header := a.headerText()
	follow := "live"
	if !a.follow.Following {
		follow = fmt.Sprintf("↓ %d new   End: return to live", a.follow.NewEvents)
	}
	if hint := a.pauseHint(); hint != "" {
		follow += "   " + hint
	}
	prompt := a.prompt + " " + SafeDisplayText(a.input, DefaultDisplayBytes)
	parts := []string{header}
	if transcript != "" {
		parts = append(parts, transcript)
	}
	parts = append(parts, follow, prompt)
	return strings.Join(parts, "\n")
}

type transcriptRowLayout struct {
	start int
	end   int
}

func (a *Application) transcriptHeight() int {
	lines := a.height - 3 // header, follow indicator, and command prompt
	if lines < 1 {
		return 1
	}
	return lines
}

func (a *Application) transcriptLayout(entries []ActivityDisplayEntry) ([]transcriptRowLayout, int) {
	layout := make([]transcriptRowLayout, 0, len(entries))
	totalLines := 0
	for index, entry := range entries {
		if index != 0 {
			totalLines++ // one blank line between logical transcript rows
		}
		start := totalLines
		totalLines += countLines(a.renderEntry(entry))
		layout = append(layout, transcriptRowLayout{start: start, end: totalLines})
	}
	return layout, totalLines
}

func (a *Application) viewportOffset(totalLines int) int {
	maxOffset := totalLines - a.transcriptHeight()
	if maxOffset < 0 {
		maxOffset = 0
	}
	if a.follow.Following {
		return maxOffset
	}
	if a.scrollTop < 0 {
		return 0
	}
	if a.scrollTop > maxOffset {
		return maxOffset
	}
	return a.scrollTop
}

func (a *Application) clampScroll() {
	if a.follow.Following {
		a.scrollTop = 0
		return
	}
	entries := a.model.Entries()
	_, totalLines := a.transcriptLayout(entries)
	a.scrollTop = a.viewportOffset(totalLines)
	a.follow.ScrollY = float64(a.scrollTop)
}

func (a *Application) visibleTranscript(entries []ActivityDisplayEntry) string {
	if len(entries) == 0 {
		return ""
	}
	_, totalLines := a.transcriptLayout(entries)
	offset := a.viewportOffset(totalLines)
	end := offset + a.transcriptHeight()
	if end > totalLines {
		end = totalLines
	}
	lines := make([]string, 0, totalLines)
	for index, entry := range entries {
		if index != 0 {
			lines = append(lines, "")
		}
		lines = append(lines, strings.Split(a.renderEntry(entry), "\n")...)
	}
	if offset >= len(lines) {
		return ""
	}
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[offset:end], "\n")
}

func (a *Application) scrollAnchor(entries []ActivityDisplayEntry) (string, int) {
	if a.follow.Following || len(entries) == 0 {
		return "", 0
	}
	layout, totalLines := a.transcriptLayout(entries)
	offset := a.viewportOffset(totalLines)
	for index, row := range layout {
		if offset >= row.start && offset < row.end {
			return entries[index].Key, offset - row.start
		}
		// A blank separator belongs to the entry immediately below it for
		// anchoring purposes. Preserve it as line -1 so growth or trimming
		// above the viewport cannot make historical content jump.
		if offset < row.start {
			return entries[index].Key, offset - row.start
		}
	}
	return "", 0
}

func (a *Application) restoreScrollAnchor(key string, line int) {
	if key == "" {
		return
	}
	entries := a.model.Entries()
	layout, _ := a.transcriptLayout(entries)
	for index, entry := range entries {
		if entry.Key != key {
			continue
		}
		rowLines := layout[index].end - layout[index].start
		if line >= rowLines {
			line = rowLines - 1
		}
		if line < -1 {
			line = -1
		}
		a.scrollTop = layout[index].start + line
		if a.scrollTop < 0 {
			a.scrollTop = 0
		}
		return
	}
}

func (a *Application) headerText() string {
	run := SafeDisplayText(a.status.RunName, SummaryDisplayBytes)
	state := SafeDisplayText(a.status.RuntimeState, SummaryDisplayBytes)
	sol := SafeDisplayText(a.status.SolState, SummaryDisplayBytes)
	busy := "operator idle"
	if a.busy {
		busy = "operator busy"
	}
	generation := "-"
	if a.status.HasGeneration {
		generation = fmt.Sprintf("%d", a.status.Generation)
	}
	runtimeStatus := state + " · Sol " + sol
	switch a.status.Interview {
	case InterviewAnswering:
		runtimeStatus = "EXIT INTERVIEW · Sol answering"
	case InterviewIdle:
		runtimeStatus = "EXIT INTERVIEW · Sol idle"
	case InterviewAvailable:
		runtimeStatus = state + " · exit interview available"
	}
	return fmt.Sprintf("CodexOS   %s · gen %s · %s · %d pending · %s", run, generation, runtimeStatus, a.status.PendingFeatures, busy)
}

func (a *Application) pauseHint() string {
	if a.busy || a.hasConfirmation() || !runtimeRunning(a.status.RuntimeState) {
		return ""
	}
	if !a.pauseDeadline.IsZero() && time.Now().Before(a.pauseDeadline) {
		return "Esc again to confirm pause"
	}
	return "Esc: pause"
}

func (a *Application) hasConfirmation() bool {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	return a.confirmation != nil
}

func (a *Application) renderEntry(entry ActivityDisplayEntry) string {
	switch presentation := entry.Presentation.(type) {
	case AgentMessagePresentation:
		phase := ""
		if presentation.TurnPhase == "planning" {
			phase = " · planning"
		}
		return SafeDisplayText(roleName(presentation.Role)+phase, SummaryDisplayBytes) + "\n" + presentation.Text
	case ReasoningPresentation:
		phase := ""
		if presentation.TurnPhase == "planning" {
			phase = " · planning"
		}
		return "  ◇ " + SafeDisplayText(roleName(presentation.Role)+phase, SummaryDisplayBytes) + " · reasoning summary\n" + presentation.Text
	case ToolPresentation:
		marker := toolMarker(presentation.State)
		text := fmt.Sprintf("  ● %s · %s", SafeDisplayText(roleName(presentation.Role), SummaryDisplayBytes), SafeDisplayText(presentation.Tool, SummaryDisplayBytes))
		if presentation.Summary != "" {
			text += "  " + presentation.Summary
		}
		text += "  " + marker
		if presentation.Detail != nil {
			affordance := "▸ details"
			if a.toolExpanded(entry) {
				affordance = "▾ details"
			}
			text += "  " + affordance + " (" + toolDetailSummary(presentation.Detail) + ")"
			if a.toolExpanded(entry) {
				text += "\n" + presentation.Detail.Text
			}
		}
		if presentation.ResultNote != "" {
			text += "\n" + SafeDisplayText(presentation.ResultNote, SummaryDisplayBytes)
		}
		return text
	case FeatureRequestPresentation:
		lines := []string{"Feature request", presentation.Title, presentation.Description, string(presentation.RecordingState)}
		if presentation.RequestID != "" {
			lines = append(lines, "request "+presentation.RequestID)
		}
		if presentation.InitialStatus != nil {
			lines = append(lines, "initial status: "+string(*presentation.InitialStatus), "recording did not provision the capability")
		}
		if presentation.Error != "" {
			lines = append(lines, presentation.Error)
		}
		return nonEmptyLines(lines)
	case BuildPresentation:
		lines := []string{"── Trusted build"}
		for _, phase := range presentation.Phases {
			lines = append(lines, fmt.Sprintf("   %-18s %s", phase.Name, stateMarker(phase.State)))
		}
		if presentation.Diagnostic != "" {
			lines = append(lines, presentation.Diagnostic)
		}
		return strings.Join(lines, "\n")
	case OperatorPresentation:
		lines := []string{"Operator"}
		if presentation.Command != nil {
			lines = append(lines, "  codexos> "+*presentation.Command)
		}
		if presentation.Output != "" {
			for _, line := range strings.Split(presentation.Output, "\n") {
				lines = append(lines, "  "+line)
			}
		}
		return strings.Join(lines, "\n")
	case InterviewQuestionPresentation:
		return "You\n  " + presentation.Text
	case LifecyclePresentation:
		role := SafeDisplayText(roleName(presentation.Role), SummaryDisplayBytes)
		title := SafeDisplayText(presentation.Title, SummaryDisplayBytes)
		if presentation.Detail == "" {
			return stateMarker(presentation.State) + " " + role + " · " + title
		}
		return stateMarker(presentation.State) + " " + role + " · " + title + "\n  " + strings.ReplaceAll(presentation.Detail, "\n", "\n  ")
	case NoticePresentation:
		if presentation.Text == "" {
			return presentation.Title
		}
		return presentation.Title + "\n" + presentation.Text
	default:
		return entryText(entry)
	}
}

func (a *Application) toolExpanded(entry ActivityDisplayEntry) bool {
	presentation, ok := entry.Presentation.(ToolPresentation)
	if !ok || presentation.Detail == nil {
		return false
	}
	if expanded, exists := a.expanded[entry.Key]; exists {
		return expanded
	}
	return presentation.State == ActivityDisplayStateFailed
}

func (a *Application) handleMouseWheel(msg tea.MouseWheelMsg) tea.Cmd {
	switch msg.Mouse().Button {
	case tea.MouseWheelUp:
		a.scrollBy(-3)
	case tea.MouseWheelDown:
		a.scrollBy(3)
	}
	return nil
}

func (a *Application) handleMouseClick(msg tea.MouseClickMsg) {
	if msg.Mouse().Button != tea.MouseLeft {
		return
	}
	entries := a.model.Entries()
	line := msg.Mouse().Y - 1 // header occupies the first line
	if line < 0 {
		return
	}
	index := a.entryAtVisibleLine(entries, line)
	if index >= 0 && entries[index].Kind == ActivityDisplayKindTool {
		a.toggleTool(entries[index].Key)
	}
}

func (a *Application) entryAtVisibleLine(entries []ActivityDisplayEntry, line int) int {
	if line < 0 || len(entries) == 0 {
		return -1
	}
	layout, totalLines := a.transcriptLayout(entries)
	globalLine := a.viewportOffset(totalLines) + line
	for index, row := range layout {
		if globalLine >= row.start && globalLine < row.end {
			return index
		}
	}
	return -1
}

func toolMarker(state ActivityDisplayState) string {
	switch state {
	case ActivityDisplayStatePending:
		return "·"
	case ActivityDisplayStateRunning:
		return "…"
	case ActivityDisplayStateCompleted:
		return "✓"
	case ActivityDisplayStateFailed:
		return "✗"
	case ActivityDisplayStateInterrupted, ActivityDisplayStateCancelled:
		return "!"
	default:
		return "?"
	}
}

func stateMarker(state ActivityDisplayState) string { return toolMarker(state) }

func toolDetailSummary(detail *ToolDetailPresentation) string {
	if detail == nil {
		return ""
	}
	size := formatBytes(detail.ByteCount)
	if detail.LineCount == nil {
		return size
	}
	unit := "lines"
	if *detail.LineCount == 1 {
		unit = "line"
	}
	return fmt.Sprintf("%s · %d %s", size, *detail.LineCount, unit)
}

func formatBytes(size int) string {
	if size < 1024 {
		return fmt.Sprintf("%d B", size)
	}
	if size < 1024*1024 {
		return fmt.Sprintf("%.1f KiB", float64(size)/1024)
	}
	return fmt.Sprintf("%.1f MiB", float64(size)/(1024*1024))
}

func runtimeRunning(state string) bool { return strings.EqualFold(state, "running") }

func isInterviewEndCommand(command string) bool {
	switch strings.TrimSpace(command) {
	case "end", "end-interview", "quit":
		return true
	default:
		return false
	}
}

func printableInput(value string) bool {
	for _, r := range value {
		if !unicode.IsPrint(r) && r != '\t' {
			return false
		}
	}
	return true
}

func lastRune(value string) (rune, int) {
	r, size := utf8.DecodeLastRuneInString(value)
	if r == utf8.RuneError && size == 0 {
		return 0, 0
	}
	return r, size
}

func countLines(value string) int {
	if value == "" {
		return 1
	}
	return strings.Count(value, "\n") + 1
}
