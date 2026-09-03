package tui

// This file contains the Bubble Tea frontend for the activity model.  The
// model remains the authority for event interpretation and bounds; this layer
// only owns terminal interaction, command input, and the current viewport.

import (
	"context"
	"errors"
	"fmt"
	"image/color"
	"io"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"codexos/internal/observability"
)

const (
	defaultActivityPoll           = 50 * time.Millisecond
	defaultStatusPoll             = 250 * time.Millisecond
	defaultCommandShutdownTimeout = 10 * time.Second
	pauseConfirmation             = 2500 * time.Millisecond
	maxComposerRows               = 8
)

var (
	statusBackground    color.Color = lipgloss.Color("#273449")
	statusForeground    color.Color = lipgloss.Color("#E6EDF3")
	separatorBackground color.Color = lipgloss.Color("#38465B")
	separatorForeground color.Color = lipgloss.Color("#D8DEE9")
	composerBackground  color.Color = lipgloss.Color("#17202D")
	composerForeground  color.Color = lipgloss.Color("#F3F4F6")
	cursorColor         color.Color = lipgloss.Color("#7DD3FC")
	solColor            color.Color = lipgloss.Color("#7DD3FC")
	lunaColor           color.Color = lipgloss.Color("#C4B5FD")
	operatorColor       color.Color = lipgloss.Color("#FBBF24")
	planningColor       color.Color = lipgloss.Color("#93C5FD")
	implementationColor color.Color = lipgloss.Color("#67E8F9")
	successColor        color.Color = lipgloss.Color("#86EFAC")
	warningColor        color.Color = lipgloss.Color("#FCD34D")
	failureColor        color.Color = lipgloss.Color("#FCA5A5")
	interruptionColor   color.Color = lipgloss.Color("#FDBA74")
	mutedColor          color.Color = lipgloss.Color("#94A3B8")
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
	ActiveAgent     string
	ActivePhase     string
	PendingFeatures int
	Interview       InterviewState
	Busy            bool
}

// ConfirmationFunc is passed to the command runner for operations which
// need an operator decision.  It blocks the command worker until the TUI
// receives y/Y, or until shutdown (which is always No).
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
	Activity               *observability.ActivityStream
	Status                 StatusProvider
	Execute                CommandHandler
	OnShutdown             func()
	StartupOutput          string
	ModelOptions           ActivityModelOptions
	ActivityPoll           time.Duration
	StatusPoll             time.Duration
	CommandShutdownTimeout time.Duration
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

	model    *OperatorActivityModel
	follow   ActivityFollowState
	markdown *markdownRenderer
	viewport viewport.Model
	composer textarea.Model

	activityPoll           time.Duration
	statusPoll             time.Duration
	commandShutdownTimeout time.Duration
	startup                string

	status StatusSnapshot
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
	commandDone  chan struct{}
	shutdownOnce sync.Once
	hookOnce     sync.Once
}

type activityPollMsg struct{}
type statusPollMsg struct{}
type operatorOutputMsg string

type commandResultMsg struct {
	result CommandResult
	err    error
	done   chan struct{}
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
	commandShutdownTimeout := options.CommandShutdownTimeout
	if commandShutdownTimeout <= 0 {
		commandShutdownTimeout = defaultCommandShutdownTimeout
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
	transcript := viewport.New(viewport.WithWidth(80), viewport.WithHeight(19))
	transcript.FillHeight = true
	transcript.MouseWheelEnabled = false
	transcript.SoftWrap = false
	composer := textarea.New()
	composer.Prompt = "codexos> "
	composer.ShowLineNumbers = false
	composer.DynamicHeight = true
	composer.MinHeight = 1
	composer.MaxHeight = maxComposerRows
	composer.SetVirtualCursor(false)
	styles := textarea.DefaultDarkStyles()
	styles.Focused.Base = styles.Focused.Base.Background(composerBackground)
	styles.Focused.Text = styles.Focused.Text.Foreground(composerForeground).Background(composerBackground)
	styles.Focused.CursorLine = styles.Focused.CursorLine.Foreground(composerForeground).Background(composerBackground)
	styles.Focused.Prompt = styles.Focused.Prompt.Foreground(cursorColor).Background(composerBackground).Bold(true)
	styles.Focused.Placeholder = styles.Focused.Placeholder.Background(composerBackground)
	styles.Focused.EndOfBuffer = styles.Focused.EndOfBuffer.Background(composerBackground)
	styles.Blurred.Base = styles.Blurred.Base.Background(composerBackground)
	styles.Blurred.Text = styles.Blurred.Text.Foreground(composerForeground).Background(composerBackground)
	styles.Blurred.CursorLine = styles.Blurred.CursorLine.Foreground(composerForeground).Background(composerBackground)
	styles.Blurred.Prompt = styles.Blurred.Prompt.Foreground(separatorForeground).Background(composerBackground)
	styles.Blurred.Placeholder = styles.Blurred.Placeholder.Background(composerBackground)
	styles.Blurred.EndOfBuffer = styles.Blurred.EndOfBuffer.Background(composerBackground)
	styles.Cursor.Color = cursorColor
	styles.Cursor.Shape = tea.CursorBar
	styles.Cursor.Blink = true
	composer.SetStyles(styles)
	composer.SetWidth(80)
	composer.Focus()
	return &Application{
		activity:               options.Activity,
		statusFn:               options.Status,
		execute:                options.Execute,
		onClose:                options.OnShutdown,
		model:                  model,
		follow:                 NewActivityFollowState(),
		markdown:               newMarkdownRenderer(model.maxEntries),
		viewport:               transcript,
		composer:               composer,
		activityPoll:           activityPoll,
		statusPoll:             statusPoll,
		commandShutdownTimeout: commandShutdownTimeout,
		startup:                options.StartupOutput,
		status:                 status,
		prompt:                 "codexos>",
		height:                 24,
		width:                  80,
		expanded:               make(map[string]bool),
		context:                context.Background(),
		done:                   make(chan struct{}),
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
	return a.composer.Value()
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
	a.updatePrompt()
	a.resizeRegions()
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

// PostOperatorOutput safely delivers asynchronous console output to the
// Bubble Tea event loop. Output produced before Run starts or after shutdown
// is rejected so a console watcher can never block on an absent frontend.
func (a *Application) PostOperatorOutput(output string) bool {
	if a == nil || output == "" {
		return false
	}
	a.stateMu.Lock()
	program := a.program
	started := a.started
	closed := a.closed
	a.stateMu.Unlock()
	if program == nil || !started || closed {
		return false
	}
	program.Send(operatorOutputMsg(output))
	return true
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
		a.resizeRegions()
	}
	return tea.Batch(a.composer.Focus(), a.activityTick(), a.statusTick())
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
	case operatorOutputMsg:
		a.appendOperatorOutput(string(value))
		return a, nil
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
		if a.width < 1 {
			a.width = 1
		}
		if a.height < 1 {
			a.height = 1
		}
		a.resizeRegions()
		a.clampScroll()
		return a, nil
	case tea.KeyPressMsg:
		return a, a.handleKey(value)
	case tea.PasteMsg:
		return a, a.handlePaste(value.Content)
	case tea.MouseWheelMsg:
		return a, a.handleMouseWheel(value)
	case tea.MouseClickMsg:
		a.handleMouseClick(value)
		return a, nil
	case tea.QuitMsg:
		a.close()
		return a, nil
	case tea.ResumeMsg:
		if !a.busy && !a.hasConfirmation() {
			return a, a.composer.Focus()
		}
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
	// textarea.View refreshes an internal viewport cache, so rendering is a
	// mutation even though View is logically read-only. Serialize diagnostic
	// callers with Bubble Tea's renderer rather than allowing concurrent views.
	a.viewMu.Lock()
	defer a.viewMu.Unlock()
	var view tea.View
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	layout := a.regionLayout()
	view.SetContent(a.viewText(layout))
	if layout.composerText > 0 && !a.busy && !a.hasConfirmation() && a.composer.Focused() {
		if cursor := a.composer.Cursor(); cursor != nil {
			composerStart := layout.header + layout.transcript + layout.separator + layout.composerTopPadding
			cursor.Position.X = min(max(0, cursor.Position.X), max(0, a.width-1))
			cursor.Position.Y = min(max(composerStart, cursor.Position.Y+composerStart), composerStart+layout.composerText-1)
			view.Cursor = cursor
		}
	}
	return view
}

// Run enters Bubble Tea's cursed renderer and always invokes the integration
// shutdown hook after Bubble Tea restores terminal state.  Passing a nil input
// disables input (useful for bounded non-interactive tests); output defaults
// to stdout when nil.
func (a *Application) Run(ctx context.Context, input io.Reader, output io.Writer) (resultErr error) {
	if a == nil {
		return errors.New("nil TUI application")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Bubble Tea v2.0.9's Linux input shutdown skips joining its cancel reader
	// when its context is cancelled.  Keep command cancellation under our
	// ownership, but let Bubble Tea leave through a Quit message so its graceful
	// path joins the reader before restoring and closing the terminal.
	runContext, cancel := context.WithCancel(ctx)
	options := []tea.ProgramOption{
		tea.WithContext(context.WithoutCancel(runContext)),
		tea.WithoutSignalHandler(),
	}
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
	contextShutdownDone := make(chan struct{})
	stopContextShutdown := context.AfterFunc(ctx, func() {
		defer close(contextShutdownDone)
		a.Shutdown()
	})
	defer func() {
		contextShutdownStopped := stopContextShutdown()
		// Bubble Tea normally restores terminal state before Run returns.  It
		// can return early during setup, though, so Kill is the idempotent
		// final cleanup for both paths.
		program.Kill()
		if !contextShutdownStopped {
			// Kill also unblocks a cancellation callback that raced with setup
			// before Bubble Tea began receiving messages.
			<-contextShutdownDone
		}
		a.close()
		resultErr = errors.Join(resultErr, a.waitForCommand())
		a.stateMu.Lock()
		a.program = nil
		a.cancel = nil
		a.started = false
		a.stateMu.Unlock()
		a.finishShutdown()
	}()
	_, resultErr = program.Run()
	return resultErr
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
	a.updatePrompt()
	a.resizeRegions()
}

func (a *Application) updatePrompt() {
	if a.status.Interview == InterviewIdle {
		a.prompt = "interview>"
	} else if !a.busy {
		a.prompt = "codexos>"
	}
	a.composer.Prompt = a.prompt + " "
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
	if a.hasConfirmation() {
		switch {
		case key.Code == tea.KeyEscape || key.Code == tea.KeyEnter || key.Code == tea.KeyKpEnter:
			a.completeConfirmation(false)
		case key.Text == "y" || key.Text == "Y":
			a.completeConfirmation(true)
		case key.Text == "n" || key.Text == "N":
			a.completeConfirmation(false)
		}
		return nil
	}
	if key.Code != tea.KeyEscape {
		a.pauseDeadline = time.Time{}
		a.pauseGen = 0
	}
	if key.Mod&(tea.ModCtrl|tea.ModMeta) != 0 {
		switch msg.String() {
		case "ctrl+c", "ctrl+d", "meta+c", "meta+d":
			return a.safeQuit()
		case "ctrl+z":
			return tea.Suspend
		}
	}

	switch key.Code {
	case tea.KeyPgUp:
		a.scrollBy(-a.pageSize())
		return nil
	case tea.KeyPgDown:
		a.scrollBy(a.pageSize())
		return nil
	case tea.KeyHome:
		a.follow.Scrolled(0)
		a.scrollTop = 0
		a.clampScroll()
		return nil
	case tea.KeyEnd:
		a.follow.ReturnToLive()
		a.scrollTop = 0
		a.clampScroll()
		return nil
	case tea.KeyEscape:
		return a.handleEscape()
	case tea.KeyEnter, tea.KeyKpEnter:
		if key.Mod&(tea.ModShift|tea.ModAlt) != 0 {
			a.composer.InsertString("\n")
			a.resizeRegions()
			return nil
		}
		if a.busy || strings.TrimSpace(a.composer.Value()) == "" {
			return nil
		}
		command := a.composer.Value()
		a.composer.Reset()
		a.resizeRegions()
		return a.submitCommand(command, false)
	case tea.KeyUp, tea.KeyDown:
		if a.composer.Value() == "" {
			if key.Code == tea.KeyUp {
				a.scrollBy(-1)
			} else {
				a.scrollBy(1)
			}
			return nil
		}
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
	var command tea.Cmd
	a.composer, command = a.composer.Update(msg)
	a.resizeRegions()
	a.clampScroll()
	return command
}

// handlePaste delegates sanitization, cursor placement, and multiline state
// to the retained textarea component.
func (a *Application) handlePaste(content string) tea.Cmd {
	if a.hasConfirmation() || a.busy {
		return nil
	}
	a.pauseDeadline = time.Time{}
	a.pauseGen = 0
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	content = strings.TrimRight(content, "\n")
	if content == "" {
		return nil
	}
	var command tea.Cmd
	a.composer, command = a.composer.Update(tea.PasteMsg{Content: normalizeUTF8(content)})
	a.resizeRegions()
	a.clampScroll()
	return command
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
	a.composer.Reset()
	a.resizeRegions()
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
	a.composer.Blur()
	if !preserveInput {
		a.composer.Reset()
	}
	a.resizeRegions()
	handler := a.execute
	done := make(chan struct{})
	a.stateMu.Lock()
	ctx := a.context
	a.commandDone = done
	a.stateMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	return func() tea.Msg {
		defer close(done)
		if handler == nil {
			return commandResultMsg{err: errors.New("operator command handler is unavailable"), done: done}
		}
		result, err := handler(ctx, command, a.RequestConfirmation)
		return commandResultMsg{result: result, err: err, done: done}
	}
}

func (a *Application) applyCommandResult(message commandResultMsg) tea.Cmd {
	a.stateMu.Lock()
	if a.commandDone == message.done {
		a.commandDone = nil
	}
	a.stateMu.Unlock()
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
		return a.composer.Focus()
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
	return a.composer.Focus()
}

func (a *Application) appendOperatorOutput(output string) {
	if output == "" {
		return
	}
	before := a.model.Entries()
	anchorKey, anchorLine := a.scrollAnchor(before)
	if !a.model.AppendOperatorOutput(output) {
		return
	}
	if a.follow.Following {
		a.scrollTop = 0
	} else {
		a.restoreScrollAnchor(anchorKey, anchorLine)
		a.follow.Arrived(1)
		a.clampScroll()
	}
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
	a.composer.Blur()
	a.resizeRegions()
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
	a.composer.Prompt = a.prompt + " "
	if !a.busy {
		a.composer.Focus()
	}
	a.resizeRegions()
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

func (a *Application) waitForCommand() error {
	a.stateMu.Lock()
	done := a.commandDone
	timeout := a.commandShutdownTimeout
	a.stateMu.Unlock()
	if done == nil {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		return errors.New("operator command did not stop before the TUI shutdown deadline")
	}
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

type applicationRegionLayout struct {
	header, transcript, separator                           int
	composerTopPadding, composerText, composerBottomPadding int
}

func (a *Application) regionLayout() applicationRegionLayout {
	height := max(1, a.height)
	if height == 1 {
		return applicationRegionLayout{composerText: 1}
	}
	layout := applicationRegionLayout{header: 1, composerText: 1}
	if height == 2 {
		return layout
	}
	layout.separator = 1
	if height == 3 {
		return layout
	}
	layout.composerBottomPadding = 1
	if height == 4 {
		return layout
	}
	layout.composerTopPadding = 1
	desired := a.desiredComposerRows()
	available := height - layout.header - layout.separator - layout.composerTopPadding - layout.composerBottomPadding
	layout.composerText = min(desired, max(1, available-1))
	layout.transcript = max(0, available-layout.composerText)
	return layout
}

func (a *Application) desiredComposerRows() int {
	width := max(1, a.width)
	if prompt := a.confirmationText(); prompt != "" {
		if ansi.StringWidth(prompt) <= width {
			return 1
		}
		question := confirmationQuestion(prompt)
		return min(maxComposerRows, countWrappedLines(question, width)+1)
	}
	contentWidth := max(1, width-ansi.StringWidth(a.composer.Prompt))
	return min(maxComposerRows, countWrappedLines(a.composer.Value(), contentWidth))
}

func countWrappedLines(text string, width int) int {
	if text == "" {
		return 1
	}
	return max(1, countLines(ansi.Hardwrap(text, max(1, width), true)))
}

func (a *Application) resizeRegions() {
	a.composer.Prompt = a.composerPrompt()
	a.composer.SetWidth(max(1, a.width))
	layout := a.regionLayout()
	a.composer.SetHeight(max(1, layout.composerText))
	a.viewport.SetWidth(max(1, a.width))
	a.viewport.SetHeight(max(0, layout.transcript))
	a.viewport.SetContent(a.transcriptContent(a.model.Entries()))
	if a.follow.Following {
		a.viewport.GotoBottom()
		a.scrollTop = a.viewport.YOffset()
	} else {
		a.viewport.SetYOffset(a.scrollTop)
		a.scrollTop = a.viewport.YOffset()
	}
}

func (a *Application) composerPrompt() string {
	prompt := a.prompt + " "
	width := max(1, a.width)
	if ansi.StringWidth(prompt) < width {
		return prompt
	}
	if width == 1 {
		return ""
	}
	if width == 2 {
		return ">"
	}
	return "> "
}

func (a *Application) viewText(layout applicationRegionLayout) string {
	rows := make([]string, 0, a.height)
	if layout.header > 0 {
		rows = append(rows, paintRow(a.headerText(), a.width, statusForeground, statusBackground))
	}
	if layout.transcript > 0 {
		viewportCopy := a.viewport
		viewportCopy.SetWidth(max(1, a.width))
		viewportCopy.SetHeight(layout.transcript)
		viewportCopy.SetContent(a.transcriptContent(a.model.Entries()))
		viewportCopy.SetYOffset(a.viewportOffset(viewportCopy.TotalLineCount()))
		rows = append(rows, fixedRows(viewportCopy.View(), a.width, layout.transcript, nil, nil)...)
	}
	if layout.separator > 0 {
		rows = append(rows, paintRow(a.separatorText(), a.width, separatorForeground, separatorBackground))
	}
	for range layout.composerTopPadding {
		rows = append(rows, paintRow("", a.width, composerForeground, composerBackground))
	}
	composerText := a.composer.View()
	if confirmation := a.confirmationText(); confirmation != "" {
		rows = append(rows, renderConfirmationRows(confirmation, a.width, layout.composerText)...)
	} else {
		rows = append(rows, fixedRows(composerText, a.width, layout.composerText, composerForeground, composerBackground)...)
	}
	for range layout.composerBottomPadding {
		rows = append(rows, paintRow("", a.width, composerForeground, composerBackground))
	}
	return strings.Join(rows, "\n")
}

func renderConfirmationRows(value string, width, height int) []string {
	if height <= 0 {
		return nil
	}
	width = max(1, width)
	const indicator = "[y/N]"
	question := confirmationQuestion(value)
	if height == 1 {
		if width <= ansi.StringWidth(indicator) {
			return []string{paintRow(indicator, width, composerForeground, composerBackground)}
		}
		available := width - ansi.StringWidth(indicator) - 1
		question = ansi.Truncate(question, available, "…")
		return []string{paintRow(question+" "+indicator, width, composerForeground, composerBackground)}
	}

	questionLines := strings.Split(ansi.Hardwrap(question, width, true), "\n")
	questionHeight := height - 1
	truncated := len(questionLines) > questionHeight
	if truncated {
		questionLines = questionLines[:questionHeight]
		last := len(questionLines) - 1
		if width == 1 {
			questionLines[last] = "…"
		} else {
			questionLines[last] = ansi.Truncate(questionLines[last], width-1, "") + "…"
		}
	}
	rows := make([]string, 0, height)
	for _, line := range questionLines {
		rows = append(rows, paintRow(line, width, composerForeground, composerBackground))
	}
	for len(rows) < questionHeight {
		rows = append(rows, paintRow("", width, composerForeground, composerBackground))
	}
	rows = append(rows, paintRow(indicator, width, composerForeground, composerBackground))
	return rows
}

func confirmationQuestion(value string) string {
	value = strings.TrimSpace(value)
	return strings.TrimSpace(strings.TrimSuffix(value, "[y/N]"))
}

type transcriptRowLayout struct {
	start int
	end   int
}

func (a *Application) transcriptHeight() int {
	return a.regionLayout().transcript
}

func (a *Application) transcriptLayout(entries []ActivityDisplayEntry) ([]transcriptRowLayout, int) {
	layout := make([]transcriptRowLayout, 0, len(entries))
	totalLines := 0
	for index, entry := range entries {
		if index != 0 {
			totalLines++ // one blank line between logical transcript rows
		}
		start := totalLines
		totalLines += countLines(a.renderTranscriptEntry(entry))
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
		lines = append(lines, strings.Split(a.renderTranscriptEntry(entry), "\n")...)
	}
	if offset >= len(lines) {
		return ""
	}
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[offset:end], "\n")
}

func (a *Application) transcriptContent(entries []ActivityDisplayEntry) string {
	rows := make([]string, 0, len(entries))
	for _, entry := range entries {
		rows = append(rows, a.renderTranscriptEntry(entry))
	}
	return strings.Join(rows, "\n\n")
}

func (a *Application) renderTranscriptEntry(entry ActivityDisplayEntry) string {
	return ansi.Hardwrap(a.renderEntry(entry), max(1, a.width), true)
}

func fixedRows(value string, width, height int, foreground, background color.Color) []string {
	if height <= 0 {
		return nil
	}
	lines := strings.Split(value, "\n")
	if len(lines) > height {
		lines = lines[len(lines)-height:]
	}
	rows := make([]string, height)
	start := height - len(lines)
	for index := range rows {
		if index >= start {
			rows[index] = paintRow(lines[index-start], width, foreground, background)
		} else {
			rows[index] = paintRow("", width, foreground, background)
		}
	}
	return rows
}

func paintRow(value string, width int, foreground, background color.Color) string {
	width = max(1, width)
	value = ansi.Truncate(value, width, "")
	padding := max(0, width-ansi.StringWidth(value))
	style := lipgloss.NewStyle()
	if foreground != nil {
		style = style.Foreground(foreground)
	}
	if background != nil {
		style = style.Background(background)
	}
	return style.Render(value + strings.Repeat(" ", padding))
}

func (a *Application) confirmationText() string {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.confirmation == nil {
		return ""
	}
	prompt := strings.TrimSpace(strings.ReplaceAll(
		SafeDisplayText(a.confirmation.prompt, SummaryDisplayBytes), "\n", " / ",
	))
	if strings.HasSuffix(prompt, "[y/N]") {
		prompt = strings.TrimSpace(strings.TrimSuffix(prompt, "[y/N]"))
	}
	return prompt + " [y/N]"
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
	run := oneLine(SafeDisplayText(a.status.RunName, SummaryDisplayBytes))
	state := oneLine(SafeDisplayText(a.status.RuntimeState, SummaryDisplayBytes))
	agent := oneLine(SafeDisplayText(a.status.ActiveAgent, SummaryDisplayBytes))
	phase := oneLine(SafeDisplayText(a.status.ActivePhase, SummaryDisplayBytes))
	if agent == "" && a.status.SolState != "idle" && a.status.SolState != "stopped" {
		agent = "Sol"
	}
	if phase == "" {
		phase = oneLine(SafeDisplayText(a.status.SolState, SummaryDisplayBytes))
	}
	operatorState := "operator input"
	if a.hasConfirmation() {
		operatorState = "operator confirm"
	} else if a.busy {
		operatorState = "operator busy"
	} else if !a.composer.Focused() {
		operatorState = "operator unfocused"
	}
	generation := "-"
	if a.status.HasGeneration {
		generation = fmt.Sprintf("%d", a.status.Generation)
	}
	activity := "agent idle"
	if agent != "" {
		activity = agent
		if phase != "" {
			activity += " " + phase
		}
	}
	switch a.status.Interview {
	case InterviewAnswering:
		activity = "Sol interview"
	case InterviewIdle:
		activity = "interview ready"
	case InterviewAvailable:
		activity = "exit interview available"
	}
	full := fmt.Sprintf(" CodexOS  run %s · gen %s · %s · %s · pending %d · %s ", run, generation, state, activity, a.status.PendingFeatures, operatorState)
	if ansi.StringWidth(full) <= a.width {
		return full
	}
	compact := fmt.Sprintf(" %s · g%s · %s · %s · p%d · %s ", run, generation, state, activity, a.status.PendingFeatures, strings.TrimPrefix(operatorState, "operator "))
	return ansi.Truncate(compact, max(1, a.width), "…")
}

func (a *Application) separatorText() string {
	left := " PgUp/PgDn · mouse wheel · Home/End"
	right := "LIVE"
	if !a.follow.Following {
		right = fmt.Sprintf("SCROLLED · %d new · End: live", a.follow.NewEvents)
	}
	if a.hasConfirmation() {
		right = "CONFIRM · y: yes · n/Enter/Esc: no"
	} else if hint := a.pauseHint(); hint != "" {
		right += " · " + hint
	} else if a.busy {
		right += " · working"
	} else {
		right += " · Enter: send · Shift+Enter: newline"
	}
	return joinRowSides(left, right+" ", a.width)
}

func joinRowSides(left, right string, width int) string {
	width = max(1, width)
	if ansi.StringWidth(left)+ansi.StringWidth(right) <= width {
		return left + strings.Repeat(" ", width-ansi.StringWidth(left)-ansi.StringWidth(right)) + right
	}
	if ansi.StringWidth(right) >= width {
		return ansi.Truncate(right, width, "")
	}
	left = ansi.Truncate(left, width-ansi.StringWidth(right), "…")
	return left + strings.Repeat(" ", max(0, width-ansi.StringWidth(left)-ansi.StringWidth(right))) + right
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
		phase := phaseSuffix(presentation.TurnPhase)
		body := a.markdown.render(entry.Key, presentation.Text, a.width-2)
		heading := styled(SafeDisplayText(roleName(presentation.Role), SummaryDisplayBytes), roleColor(presentation.Role), true)
		if phase != "" {
			heading += styled(phase, phaseColor(presentation.TurnPhase), false)
		}
		if body == "" {
			return heading
		}
		return heading + "\n" + indentBlock(body, "  ")
	case ReasoningPresentation:
		phase := phaseSuffix(presentation.TurnPhase)
		body := a.markdown.render(entry.Key, presentation.Text, a.width-2)
		return styled("  ◇ "+SafeDisplayText(roleName(presentation.Role), SummaryDisplayBytes), roleColor(presentation.Role), false) + styled(phase+" · reasoning summary", mutedColor, false) + "\n" + indentBlock(body, "  ")
	case ToolPresentation:
		marker := styled(toolMarker(presentation.State), stateColor(presentation.State), true)
		text := "  " + styled("● "+SafeDisplayText(roleName(presentation.Role), SummaryDisplayBytes), roleColor(presentation.Role), false)
		if phase := phaseSuffix(presentation.TurnPhase); phase != "" {
			text += styled(phase, phaseColor(presentation.TurnPhase), false)
		}
		text += " · " + styled(SafeDisplayText(presentation.Tool, SummaryDisplayBytes), mutedColor, true)
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
		lines := []string{styled("── Trusted build", implementationColor, true)}
		for _, phase := range presentation.Phases {
			lines = append(lines, fmt.Sprintf("   %-18s %s", phase.Name, styled(stateMarker(phase.State), stateColor(phase.State), true)))
		}
		if presentation.Diagnostic != "" {
			lines = append(lines, presentation.Diagnostic)
		}
		return strings.Join(lines, "\n")
	case OperatorPresentation:
		lines := []string{styled("Operator", operatorColor, true)}
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
		return styled("Operator", operatorColor, true) + "\n  " + presentation.Text
	case LifecyclePresentation:
		role := styled(SafeDisplayText(roleName(presentation.Role), SummaryDisplayBytes), roleColor(presentation.Role), true)
		title := SafeDisplayText(presentation.Title, SummaryDisplayBytes)
		if presentation.Detail == "" {
			return styled(stateMarker(presentation.State), stateColor(presentation.State), true) + " " + role + " · " + title
		}
		return styled(stateMarker(presentation.State), stateColor(presentation.State), true) + " " + role + " · " + title + "\n  " + strings.ReplaceAll(presentation.Detail, "\n", "\n  ")
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
	layout := a.regionLayout()
	composerStart := layout.header + layout.transcript + layout.separator
	if msg.Mouse().Y >= composerStart && msg.Mouse().Y < a.height {
		if !a.busy && !a.hasConfirmation() {
			a.composer.Focus()
		}
		return
	}
	entries := a.model.Entries()
	line := msg.Mouse().Y - layout.header
	if line < 0 || line >= layout.transcript {
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

func styled(text string, foreground color.Color, bold bool) string {
	style := lipgloss.NewStyle().Foreground(foreground)
	if bold {
		style = style.Bold(true)
	}
	return style.Render(text)
}

func roleColor(role observability.ActivityRole) color.Color {
	switch role {
	case observability.ActivityReviewer:
		return lunaColor
	case observability.ActivityHarness:
		return mutedColor
	default:
		return solColor
	}
}

func phaseColor(phase string) color.Color {
	switch phase {
	case "planning":
		return planningColor
	case "implementation":
		return implementationColor
	case "review":
		return lunaColor
	case "interview":
		return operatorColor
	default:
		return mutedColor
	}
}

func phaseSuffix(phase string) string {
	switch phase {
	case "planning", "implementation", "review", "interview":
		return " · " + phase
	default:
		return ""
	}
}

func stateColor(state ActivityDisplayState) color.Color {
	switch state {
	case ActivityDisplayStateCompleted:
		return successColor
	case ActivityDisplayStatePending, ActivityDisplayStateRunning:
		return warningColor
	case ActivityDisplayStateFailed:
		return failureColor
	case ActivityDisplayStateInterrupted, ActivityDisplayStateCancelled:
		return interruptionColor
	default:
		return mutedColor
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

func countLines(value string) int {
	if value == "" {
		return 1
	}
	return strings.Count(value, "\n") + 1
}
