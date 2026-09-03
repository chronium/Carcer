package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"codexos/internal/observability"
)

func keyPress(text string, code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Text: text, Code: code})
}

func testApplication(t *testing.T, options ApplicationOptions) *Application {
	t.Helper()
	app, err := NewApplication(options)
	if err != nil {
		t.Fatal(err)
	}
	app.Init()
	return app
}

func TestApplicationViewUsesV2FullScreenAndTypedTranscriptRows(t *testing.T) {
	stream := observability.NewActivityStream()
	app := testApplication(t, ApplicationOptions{
		Activity: stream,
		Status: func() StatusSnapshot {
			return StatusSnapshot{
				RunName: "run-1", Generation: 7, HasGeneration: true,
				RuntimeState: "running", SolState: "working",
				PendingFeatures: 2,
			}
		},
		StartupOutput: "ready",
	})
	generation := uint64(7)
	if _, err := stream.Publish(&generation, observability.ActivityImplementor, observability.ActivityAgentMessage,
		map[string]any{"text": "safe \x1b[2J message"}, "thread", "turn", "message"); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Publish(&generation, observability.ActivityReviewer, observability.ActivityToolFailed,
		map[string]any{"tool": "read", "arguments": map[string]any{"path": "seed/nope"}, "error": "missing"}, "review", "turn", "failed"); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Publish(&generation, observability.ActivityRole("host\x1b[2J"), observability.ActivityKind("lifecycle\x1b[31m"),
		map[string]any{"error": "failed"}, "", "", "hostile-lifecycle"); err != nil {
		t.Fatal(err)
	}
	app.Update(activityPollMsg{})
	view := app.View()
	if !view.AltScreen {
		t.Fatal("TUI view did not request the alternate screen")
	}
	if view.MouseMode != tea.MouseModeCellMotion {
		t.Fatalf("mouse mode = %v, want cell motion", view.MouseMode)
	}
	for _, want := range []string{"run-1", "g7", "p2", "Sol", "Luna", "read", "missing", "details"} {
		if !strings.Contains(view.Content, want) {
			t.Fatalf("view does not contain %q:\n%s", want, view.Content)
		}
	}
	if strings.Contains(view.Content, "\x1b[2J") {
		t.Fatal("view exposed the hostile terminal control")
	}
}

func TestApplicationRendersAgentMessagesAsSafeMarkdown(t *testing.T) {
	app := testApplication(t, ApplicationOptions{})
	generation := uint64(4)
	streaming := observability.ActivityEvent{
		Sequence: 1, Generation: &generation,
		Role: observability.ActivityImplementor, Kind: observability.ActivityAgentTextDelta,
		Data:     map[string]any{"text": "Streaming **plain**", "turn_phase": "planning"},
		ThreadID: "thread", TurnID: "turn", ItemID: "message",
	}
	if !app.model.Consume(streaming) {
		t.Fatal("streaming event did not change the model")
	}
	entries := app.model.Entries()
	messageEntry := entries[len(entries)-1]
	if messageEntry.Kind != ActivityDisplayKindMessage {
		t.Fatalf("last streaming entry = %#v, want message", messageEntry)
	}
	key := messageEntry.Key
	streamed := app.renderEntry(messageEntry)
	if !strings.Contains(ansi.Strip(streamed), "Streaming plain") || strings.Contains(streamed, "**plain**") || strings.Contains(streamed, "\x1b[2J") {
		t.Fatalf("streaming message was not rendered safely: %q", streamed)
	}

	final := streaming
	final.Sequence = 2
	final.Kind = observability.ActivityAgentMessage
	final.Data = map[string]any{
		"text":       "# Result\n\nFinal **Markdown** with [docs](https://example.test), `code`, and ~~old~~.\x1b[2J\n\n- item",
		"turn_phase": "planning",
	}
	if !app.model.Consume(final) {
		t.Fatal("final event did not change the model")
	}
	entries = app.model.Entries()
	messageEntry = entries[len(entries)-1]
	if messageEntry.Key != key {
		t.Fatalf("final message did not update the stable row: %#v", entries)
	}
	rendered := app.renderEntry(messageEntry)
	plain := ansi.Strip(rendered)
	for _, want := range []string{"Sol · planning", "Result", "Final Markdown", "docs (https://example.test)", "code", "old", "• item", `\x1b[2J`} {
		if !strings.Contains(plain, want) {
			t.Fatalf("rendered Markdown missing %q:\n%s", want, plain)
		}
	}
	for _, unwanted := range []string{"**Markdown**", "[docs]", "`code`", "~~old~~", "\x1b[2J", "\x1b]8;"} {
		if strings.Contains(rendered, unwanted) {
			t.Fatalf("rendered Markdown contains %q:\n%q", unwanted, rendered)
		}
	}
	for _, line := range strings.Split(plain, "\n")[1:] {
		if !strings.HasPrefix(line, "  ") {
			t.Fatalf("Markdown body line is not padded: %q\n%s", line, plain)
		}
	}
	if !strings.Contains(rendered, "\x1b[") {
		t.Fatalf("finalized Markdown did not carry trusted terminal styling: %q", rendered)
	}
	if raw := app.model.RenderText(); !strings.Contains(raw, "**Markdown**") {
		t.Fatalf("presentation rendering mutated canonical model text: %q", raw)
	}
}

func TestApplicationCommandsKeepInputIndependentFromActivityAndFinishRows(t *testing.T) {
	stream := observability.NewActivityStream()
	var gotCommand atomic.Value
	app := testApplication(t, ApplicationOptions{
		Activity: stream,
		Execute: func(_ context.Context, command string, _ ConfirmationFunc) (CommandResult, error) {
			gotCommand.Store(command)
			return CommandResult{Output: "State: STOPPED"}, nil
		},
	})
	for _, character := range "status" {
		app.Update(keyPress(string(character), character))
	}
	if app.Input() != "status" {
		t.Fatalf("input before submit = %q", app.Input())
	}
	_, command := app.Update(keyPress("\r", tea.KeyEnter))
	if command == nil {
		t.Fatal("command submission returned no command")
	}
	if app.Input() != "" {
		t.Fatalf("input after submit = %q", app.Input())
	}
	if _, second := app.Update(keyPress("\r", tea.KeyEnter)); second != nil {
		t.Fatal("busy application accepted a second command")
	}
	message := command()
	app.Update(message)
	if gotCommand.Load() != "status" {
		t.Fatalf("command callback received %v", gotCommand.Load())
	}
	if !strings.Contains(app.ActivityModel().RenderText(), "codexos> status") ||
		!strings.Contains(app.ActivityModel().RenderText(), "State: STOPPED") {
		t.Fatalf("command block missing from transcript: %q", app.ActivityModel().RenderText())
	}
	if !app.composer.Focused() || app.View().Cursor == nil {
		t.Fatal("composer did not regain focused cursor after command completion")
	}
}

func TestApplicationCommandErrorsAreRecoverable(t *testing.T) {
	app := testApplication(t, ApplicationOptions{
		Execute: func(context.Context, string, ConfirmationFunc) (CommandResult, error) {
			return CommandResult{}, errors.New("command is not valid")
		},
	})
	app.composer.SetValue("status")
	_, command := app.Update(keyPress("\r", tea.KeyEnter))
	if command == nil {
		t.Fatal("command submission returned no command")
	}
	if _, quit := app.Update(command()); quit != nil {
		t.Fatal("ordinary command error quit the application")
	}
	if app.busy {
		t.Fatal("application remained busy after ordinary command error")
	}
	if transcript := app.ActivityModel().RenderText(); !strings.Contains(transcript, "Error: command is not valid") {
		t.Fatalf("recoverable command error missing from transcript: %q", transcript)
	}

	app.composer.SetValue("status")
	if _, command = app.Update(keyPress("\r", tea.KeyEnter)); command == nil {
		t.Fatal("application did not accept a command after an ordinary error")
	}
}

func TestApplicationPasteMessageAppendsOneSafeCommandLine(t *testing.T) {
	var received atomic.Value
	app := testApplication(t, ApplicationOptions{
		Execute: func(_ context.Context, command string, _ ConfirmationFunc) (CommandResult, error) {
			received.Store(command)
			return CommandResult{}, nil
		},
	})
	app.Update(tea.PasteMsg{Content: "status\r\n"})
	if got := app.Input(); got != "status" {
		t.Fatalf("pasted input = %q, want one flattened command line", got)
	}
	_, command := app.Update(keyPress("\r", tea.KeyEnter))
	if command == nil {
		t.Fatal("pasted command was not submitted")
	}
	app.Update(command())
	if got := received.Load(); got != "status" {
		t.Fatalf("handler received %v for pasted command", got)
	}
}

func TestApplicationFollowUnreadEndAndMouseToolSelection(t *testing.T) {
	stream := observability.NewActivityStream()
	app := testApplication(t, ApplicationOptions{Activity: stream})
	generation := uint64(1)
	for index := 0; index < 20; index++ {
		if _, err := stream.Publish(&generation, observability.ActivityImplementor, observability.ActivityAgentMessage,
			map[string]any{"text": "history " + string(rune('a'+index)) + strings.Repeat("x", 30)}, "thread", "turn", string(rune('a'+index))); err != nil {
			t.Fatal(err)
		}
	}
	app.Update(activityPollMsg{})
	app.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	app.Update(keyPress("", tea.KeyPgUp))
	if app.FollowState().Following {
		t.Fatal("PageUp did not release live follow")
	}
	if _, err := stream.Publish(&generation, observability.ActivityReviewer, observability.ActivityAgentMessage,
		map[string]any{"text": "new reviewer"}, "review", "turn", "new"); err != nil {
		t.Fatal(err)
	}
	app.Update(activityPollMsg{})
	if app.FollowState().NewEvents == 0 {
		t.Fatal("activity arriving off-tail did not increment unread count")
	}
	app.Update(keyPress("", tea.KeyEnd))
	state := app.FollowState()
	if !state.Following || state.NewEvents != 0 {
		t.Fatalf("End state = %#v", state)
	}

	if _, err := stream.Publish(&generation, observability.ActivityImplementor, observability.ActivityToolCompleted,
		map[string]any{"tool": "read", "arguments": map[string]any{"path": "seed/a"}, "result": map[string]any{"status": 0, "output": "source"}}, "thread", "turn", "tool"); err != nil {
		t.Fatal(err)
	}
	app.Update(activityPollMsg{})
	entries := app.ActivityModel().Entries()
	toolIndex := -1
	for index, entry := range entries {
		if entry.Kind == ActivityDisplayKindTool {
			toolIndex = index
			break
		}
	}
	if toolIndex < 0 {
		t.Fatal("tool row was not created")
	}
	rows, totalLines := app.transcriptLayout(entries)
	line := rows[toolIndex].start - app.viewportOffset(totalLines)
	app.Update(tea.MouseClickMsg{X: 0, Y: app.regionLayout().header + line, Button: tea.MouseLeft})
	if !app.ExpandedTool(entries[toolIndex].Key) {
		t.Fatal("mouse click did not expand tool detail")
	}
}

func TestApplicationPauseUsesTwoEscapesAndPreservesInput(t *testing.T) {
	var commands []string
	app := testApplication(t, ApplicationOptions{
		Status: func() StatusSnapshot {
			return StatusSnapshot{RuntimeState: "running", Generation: 4, HasGeneration: true}
		},
		Execute: func(_ context.Context, command string, _ ConfirmationFunc) (CommandResult, error) {
			commands = append(commands, command)
			return CommandResult{}, nil
		},
	})
	app.composer.SetValue("partially typed")
	_, command := app.Update(keyPress("", tea.KeyEscape))
	if command != nil || len(commands) != 0 || !strings.Contains(app.View().Content, "Esc again") {
		t.Fatalf("first escape state: cmd=%v commands=%v view=%s", command != nil, commands, app.View().Content)
	}
	_, command = app.Update(keyPress("", tea.KeyEscape))
	if command == nil {
		t.Fatal("second escape did not submit pause")
	}
	if app.Input() != "partially typed" {
		t.Fatalf("pause changed input to %q", app.Input())
	}
	app.Update(command())
	if len(commands) != 1 || commands[0] != "pause" {
		t.Fatalf("pause commands = %#v", commands)
	}
}

func TestApplicationConfirmationDefaultsToNoAndShutdownIsIdempotent(t *testing.T) {
	var shutdowns atomic.Int32
	app := testApplication(t, ApplicationOptions{OnShutdown: func() { shutdowns.Add(1) }})
	request := &confirmationRequest{prompt: "Stop the run\nwithout archiving?\x1b[2J", reply: make(chan bool, 1)}
	app.Update(request)
	if got := app.ConfirmationPrompt(); got == "" || !strings.Contains(got, "Stop the run") {
		t.Fatalf("confirmation prompt = %q", got)
	}
	if view := app.View().Content; strings.Contains(view, "\x1b[2J") || strings.Count(ansi.Strip(view), "[y/N]") != 1 || !strings.Contains(ansi.Strip(view), `Stop the run / without archiving?\x1b[2J [y/N]`) {
		t.Fatalf("unsafe confirmation view = %q", view)
	}
	app.Update(keyPress("", tea.KeyEscape))
	select {
	case accepted := <-request.reply:
		if accepted {
			t.Fatal("Esc accepted confirmation")
		}
	case <-time.After(time.Second):
		t.Fatal("confirmation was not completed")
	}
	yesRequest := &confirmationRequest{prompt: "Confirm? [y/N] ", reply: make(chan bool, 1)}
	app.Update(yesRequest)
	if got := strings.Count(ansi.Strip(app.View().Content), "[y/N]"); got != 1 {
		t.Fatalf("confirmation indicators = %d, want one", got)
	}
	app.Update(keyPress("y", 'y'))
	if accepted := <-yesRequest.reply; !accepted {
		t.Fatal("confirmation rejected y")
	}
	app.Shutdown()
	app.Shutdown()
	if got := shutdowns.Load(); got != 1 {
		t.Fatalf("shutdown hook called %d times", got)
	}
}

func TestApplicationCannotRunAfterShutdown(t *testing.T) {
	app, err := NewApplication(ApplicationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	app.Shutdown()
	var output bytes.Buffer
	if err := app.Run(context.Background(), nil, &output); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Run after shutdown error = %v", err)
	}
}

func TestApplicationRunRestoresThroughBubbleTeaOnNonInteractiveContextCancel(t *testing.T) {
	var shutdowns atomic.Int32
	app, err := NewApplication(ApplicationOptions{OnShutdown: func() { shutdowns.Add(1) }, ActivityPoll: time.Millisecond, StatusPoll: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var output bytes.Buffer
	err = app.Run(ctx, nil, &output)
	if err != nil {
		t.Fatalf("Run after context cancellation: %v", err)
	}
	if shutdowns.Load() != 1 {
		t.Fatalf("shutdown hook count = %d", shutdowns.Load())
	}
	// The cursed renderer is allowed to emit terminal controls for screen
	// management. User payload safety is asserted at the View boundary above.
	_ = output
}

func TestApplicationRunWithAlreadyCanceledContextIsBounded(t *testing.T) {
	app, err := NewApplication(ApplicationOptions{ActivityPoll: time.Millisecond, StatusPoll: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runDone := make(chan error, 1)
	go func() {
		var output bytes.Buffer
		runDone <- app.Run(ctx, nil, &output)
	}()
	select {
	case runErr := <-runDone:
		if runErr != nil {
			t.Fatalf("Run with already canceled context: %v", runErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Run blocked while relaying an already canceled context")
	}
}

func TestApplicationShutdownCancelsCommandBeforePostRunHook(t *testing.T) {
	handlerStarted := make(chan struct{})
	handlerCanceled := make(chan struct{})
	releaseHandler := make(chan struct{})
	handlerExited := make(chan struct{})
	shutdownHook := make(chan struct{})
	var hookState atomic.Value
	var app *Application
	var err error
	app, err = NewApplication(ApplicationOptions{
		Execute: func(ctx context.Context, _ string, _ ConfirmationFunc) (CommandResult, error) {
			close(handlerStarted)
			<-ctx.Done()
			close(handlerCanceled)
			<-releaseHandler
			close(handlerExited)
			return CommandResult{}, ctx.Err()
		},
		OnShutdown: func() {
			appState := "clean"
			// The hook must observe the wrapper's post-Bubble-Tea cleanup.
			// The application pointer is filled below before Run starts.
			if app != nil {
				app.stateMu.Lock()
				if app.started || app.program != nil {
					appState = "running"
				}
				app.stateMu.Unlock()
			}
			hookState.Store(appState)
			close(shutdownHook)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	runDone := make(chan error, 1)
	go func() {
		runDone <- app.Run(context.Background(), nil, &output)
	}()
	var program *tea.Program
	deadline := time.Now().Add(time.Second)
	for program == nil && time.Now().Before(deadline) {
		app.stateMu.Lock()
		if app.started {
			program = app.program
		}
		app.stateMu.Unlock()
		if program == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if program == nil {
		t.Fatal("Bubble Tea program did not start")
	}
	program.Send(keyPress("status", 's'))
	program.Send(keyPress("\r", tea.KeyEnter))
	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("command handler did not start")
	}

	app.Shutdown()
	select {
	case <-shutdownHook:
		t.Fatal("shutdown hook ran before Run cleanup completed")
	default:
	}
	select {
	case <-handlerCanceled:
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not cancel the command handler context")
	}
	select {
	case <-shutdownHook:
		t.Fatal("shutdown hook ran before the command handler exited")
	default:
	}
	close(releaseHandler)
	select {
	case <-handlerExited:
	case <-time.After(time.Second):
		t.Fatal("command handler did not exit")
	}
	select {
	case runErr := <-runDone:
		if runErr != nil {
			t.Fatalf("Run after Shutdown: %v", runErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after Shutdown")
	}
	select {
	case <-shutdownHook:
	case <-time.After(time.Second):
		t.Fatal("shutdown hook did not run")
	}
	if got := hookState.Load(); got != "clean" {
		t.Fatalf("shutdown hook observed application state %v", got)
	}
	app.Shutdown()
}

func TestApplicationPostsAsynchronousOperatorOutputOnlyWhileRunning(t *testing.T) {
	app := testApplication(t, ApplicationOptions{})
	if app.PostOperatorOutput("before") {
		t.Fatal("operator output was accepted before Run")
	}
	var output bytes.Buffer
	runDone := make(chan error, 1)
	go func() {
		runDone <- app.Run(context.Background(), nil, &output)
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		app.stateMu.Lock()
		started := app.started && app.program != nil
		app.stateMu.Unlock()
		if started {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !app.PostOperatorOutput("asynchronous completion") {
		t.Fatal("operator output was not accepted while Run was active")
	}
	deadline = time.Now().Add(time.Second)
	for !strings.Contains(app.ActivityModel().RenderText(), "asynchronous completion") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(app.ActivityModel().RenderText(), "asynchronous completion") {
		t.Fatal("asynchronous operator output did not reach the model")
	}
	app.Shutdown()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run after Shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop")
	}
	if app.PostOperatorOutput("after") {
		t.Fatal("operator output was accepted after shutdown")
	}
}

func TestApplicationPublicStateAccessIsRaceSafe(t *testing.T) {
	app := testApplication(t, ApplicationOptions{})
	stop := make(chan struct{})
	var group sync.WaitGroup
	group.Add(4)
	go func() {
		defer group.Done()
		for {
			select {
			case <-stop:
				return
			default:
				app.SetStatus(StatusSnapshot{RunName: "run", RuntimeState: "running"})
			}
		}
	}()
	go func() {
		defer group.Done()
		for {
			select {
			case <-stop:
				return
			default:
				app.Update(statusPollMsg{})
			}
		}
	}()
	go func() {
		defer group.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = app.View()
				_ = app.ActivityModel().Entries()
				_ = app.FollowState()
				_ = app.Input()
			}
		}
	}()
	go func() {
		defer group.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = app.ExpandedTool("missing")
				_ = app.ToggleTool("missing")
			}
		}
	}()
	time.Sleep(25 * time.Millisecond)
	close(stop)
	group.Wait()
}

func TestApplicationMultilineViewportScrollsByLinesAndClampsAfterTrim(t *testing.T) {
	stream := observability.NewActivityStream()
	app := testApplication(t, ApplicationOptions{
		Activity: stream,
		ModelOptions: ActivityModelOptions{
			MaxEntries: 4,
			MaxBytes:   4096,
		},
	})
	app.Update(tea.WindowSizeMsg{Width: 80, Height: 8})
	generation := uint64(1)
	lines := make([]string, 20)
	for index := range lines {
		lines[index] = fmt.Sprintf("line-%02d", index)
	}
	if _, err := stream.Publish(&generation, observability.ActivityImplementor, observability.ActivityAgentMessage,
		map[string]any{"text": "```text\n" + strings.Join(lines, "\n") + "\n```"}, "thread", "turn", "long"); err != nil {
		t.Fatal(err)
	}
	app.Update(activityPollMsg{})
	entries := app.ActivityModel().Entries()
	if got := countLines(app.visibleTranscript(entries)); got > app.transcriptHeight() {
		t.Fatalf("live transcript occupies %d lines, want at most %d", got, app.transcriptHeight())
	}
	if !strings.Contains(app.View().Content, "line-19") {
		t.Fatalf("live tail omitted from view: %s", app.View().Content)
	}

	app.Update(keyPress("", tea.KeyPgUp))
	if app.FollowState().Following {
		t.Fatal("PageUp did not release live follow")
	}
	firstOffset := app.scrollTop
	app.Update(keyPress("", tea.KeyPgUp))
	if app.scrollTop >= firstOffset {
		t.Fatalf("PageUp did not move within multiline entry: %d -> %d", firstOffset, app.scrollTop)
	}
	if got := countLines(app.visibleTranscript(app.ActivityModel().Entries())); got > app.transcriptHeight() {
		t.Fatalf("historical transcript occupies %d lines, want at most %d", got, app.transcriptHeight())
	}

	for index := 0; index < 10; index++ {
		if _, err := stream.Publish(&generation, observability.ActivityImplementor, observability.ActivityAgentMessage,
			map[string]any{"text": fmt.Sprintf("trim-%d", index)}, "thread", "turn", fmt.Sprintf("trim-%d", index)); err != nil {
			t.Fatal(err)
		}
	}
	app.Update(activityPollMsg{})
	trimmed := app.ActivityModel().Entries()
	_, totalLines := app.transcriptLayout(trimmed)
	maxOffset := totalLines - app.transcriptHeight()
	if maxOffset < 0 {
		maxOffset = 0
	}
	if app.scrollTop < 0 || app.scrollTop > maxOffset {
		t.Fatalf("scroll offset after trim = %d, want [0,%d]", app.scrollTop, maxOffset)
	}
}

func TestApplicationPreservesSeparatorScrollAnchorAcrossRowGrowth(t *testing.T) {
	stream := observability.NewActivityStream()
	app := testApplication(t, ApplicationOptions{Activity: stream})
	app.Update(tea.WindowSizeMsg{Width: 80, Height: 5})
	generation := uint64(1)
	publish := func(item, text string) {
		t.Helper()
		if _, err := stream.Publish(&generation, observability.ActivityImplementor, observability.ActivityAgentMessage,
			map[string]any{"text": text}, "thread", "turn", item); err != nil {
			t.Fatal(err)
		}
	}
	publish("first", "a\nb\nc")
	publish("second", "d\ne\nf")
	app.Update(activityPollMsg{})
	entries := app.ActivityModel().Entries()
	layout, _ := app.transcriptLayout(entries)
	if len(layout) < 2 {
		t.Fatalf("initial layout rows = %d", len(layout))
	}
	second := len(layout) - 1
	app.follow.Scrolled(float64(layout[second].start - 1))
	app.scrollTop = layout[second].start - 1
	key, line := app.scrollAnchor(entries)
	if key != entries[second].Key || line != -1 {
		t.Fatalf("separator anchor = %q line %d, want %q line -1", key, line, entries[second].Key)
	}
	publish("first", "a\nb\nc\ngrown-1\ngrown-2\ngrown-3")
	app.Update(activityPollMsg{})
	entries = app.ActivityModel().Entries()
	layout, _ = app.transcriptLayout(entries)
	second = len(layout) - 1
	if got, want := app.scrollTop, layout[second].start-1; got != want {
		t.Fatalf("separator scroll offset after row growth = %d, want %d", got, want)
	}
}

func TestApplicationRetainedRegionsStayPinnedDuringHighVolumeOutput(t *testing.T) {
	stream := observability.NewActivityStream()
	app := testApplication(t, ApplicationOptions{
		Activity: stream,
		Status: func() StatusSnapshot {
			return StatusSnapshot{RunName: "run-fixed", Generation: 8, HasGeneration: true, RuntimeState: "running", ActiveAgent: "Sol", ActivePhase: "implementation"}
		},
	})
	app.Update(tea.WindowSizeMsg{Width: 48, Height: 14})
	beforeLayout := app.regionLayout()
	generation := uint64(8)
	for index := 0; index < 200; index++ {
		if _, err := stream.Publish(&generation, observability.ActivityImplementor, observability.ActivityAgentMessage,
			map[string]any{"text": fmt.Sprintf("message %03d %s", index, strings.Repeat("x", 80)), "turn_phase": "implementation"}, "thread", "turn", fmt.Sprintf("message-%03d", index)); err != nil {
			t.Fatal(err)
		}
	}
	app.Update(activityPollMsg{})
	afterLayout := app.regionLayout()
	if beforeLayout != afterLayout {
		t.Fatalf("fixed region geometry changed with output: before=%#v after=%#v", beforeLayout, afterLayout)
	}
	view := app.View()
	assertFixedFrame(t, view.Content, 48, 14)
	rows := strings.Split(ansi.Strip(view.Content), "\n")
	if !strings.Contains(rows[0], "run-fixed") || !strings.Contains(rows[0], "Sol implementation") {
		t.Fatalf("top row = %q", rows[0])
	}
	separator := beforeLayout.header + beforeLayout.transcript
	if !strings.Contains(rows[separator], "LIVE") {
		t.Fatalf("separator row %d = %q", separator, rows[separator])
	}
	if strings.TrimSpace(rows[len(rows)-1]) != "" {
		t.Fatalf("bottom composer padding = %q", rows[len(rows)-1])
	}
}

func TestApplicationKeyboardAndMouseScrollingControlLiveTail(t *testing.T) {
	stream := observability.NewActivityStream()
	app := testApplication(t, ApplicationOptions{Activity: stream})
	app.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	generation := uint64(1)
	for index := 0; index < 30; index++ {
		if _, err := stream.Publish(&generation, observability.ActivityImplementor, observability.ActivityAgentMessage,
			map[string]any{"text": fmt.Sprintf("history-%02d", index)}, "thread", "turn", fmt.Sprintf("item-%02d", index)); err != nil {
			t.Fatal(err)
		}
	}
	app.Update(activityPollMsg{})
	app.Update(tea.MouseWheelMsg{X: 2, Y: 3, Button: tea.MouseWheelUp})
	if app.FollowState().Following {
		t.Fatal("mouse wheel did not leave live-tail mode")
	}
	beforePageDown := app.scrollTop
	app.Update(keyPress("", tea.KeyPgDown))
	if app.scrollTop <= beforePageDown || app.FollowState().Following {
		t.Fatalf("PageDown state: offset %d -> %d follow=%t", beforePageDown, app.scrollTop, app.FollowState().Following)
	}
	app.Update(keyPress("", tea.KeyPgUp))
	mouseOffset := app.scrollTop
	app.Update(keyPress("", tea.KeyUp))
	if app.scrollTop >= mouseOffset {
		t.Fatalf("keyboard up did not scroll: %d -> %d", mouseOffset, app.scrollTop)
	}
	app.Update(keyPress("", tea.KeyHome))
	if app.scrollTop != 0 || app.FollowState().Following {
		t.Fatalf("Home state: offset=%d follow=%t", app.scrollTop, app.FollowState().Following)
	}
	app.Update(keyPress("", tea.KeyEnd))
	if !app.FollowState().Following {
		t.Fatal("End did not return to live tail")
	}
}

func TestApplicationMultilineComposerGrowthCursorAndClickFocus(t *testing.T) {
	stream := observability.NewActivityStream()
	app := testApplication(t, ApplicationOptions{Activity: stream})
	app.Update(tea.WindowSizeMsg{Width: 28, Height: 12})
	app.Update(tea.PasteMsg{Content: "first line that wraps here\nsecond line"})
	layout := app.regionLayout()
	if layout.composerText < 3 {
		t.Fatalf("composer did not grow for wrapped multiline input: %#v", layout)
	}
	view := app.View()
	assertFixedFrame(t, view.Content, 28, 12)
	if view.Cursor == nil {
		t.Fatal("focused composer did not expose a real cursor")
	}
	composerStart := layout.header + layout.transcript + layout.separator
	if view.Cursor.Position.Y < composerStart || view.Cursor.Position.Y >= 12 {
		t.Fatalf("cursor position = %#v, composer starts at %d", view.Cursor.Position, composerStart)
	}
	input, cursor := app.Input(), view.Cursor.Position
	generation := uint64(1)
	if _, err := stream.Publish(&generation, observability.ActivityReviewer, observability.ActivityAgentMessage,
		map[string]any{"text": "incoming **review**"}, "review", "turn", "message"); err != nil {
		t.Fatal(err)
	}
	app.Update(activityPollMsg{})
	redrawn := app.View()
	if app.Input() != input || redrawn.Cursor == nil || redrawn.Cursor.Position != cursor {
		t.Fatalf("incoming output changed editor state: input=%q cursor=%#v", app.Input(), redrawn.Cursor)
	}
	for y := composerStart; y < 12; y++ {
		app.composer.Blur()
		app.Update(tea.MouseClickMsg{X: 27, Y: y, Button: tea.MouseLeft})
		if !app.composer.Focused() {
			t.Fatalf("composer row %d did not focus on click", y)
		}
	}
}

func TestApplicationSmallTerminalLayoutNeverOverlapsOrClips(t *testing.T) {
	app := testApplication(t, ApplicationOptions{})
	app.composer.SetValue("unicode λ界 " + strings.Repeat("long ", 20))
	for height := 1; height <= 8; height++ {
		for _, width := range []int{1, 2, 8, 20} {
			app.Update(tea.WindowSizeMsg{Width: width, Height: height})
			view := app.View()
			assertFixedFrame(t, view.Content, width, height)
			if view.Cursor == nil || view.Cursor.Position.X < 0 || view.Cursor.Position.X >= width || view.Cursor.Position.Y < 0 || view.Cursor.Position.Y >= height {
				t.Fatalf("focused cursor at %dx%d = %#v", width, height, view.Cursor)
			}
			layout := app.regionLayout()
			if layout.header < 0 || layout.transcript < 0 || layout.separator < 0 || layout.composerText < 0 {
				t.Fatalf("negative layout at %dx%d: %#v", width, height, layout)
			}
		}
	}
}

func TestApplicationComposerPreservesLongMultilineDraft(t *testing.T) {
	app := testApplication(t, ApplicationOptions{})
	lines := make([]string, 300)
	for index := range lines {
		lines[index] = fmt.Sprintf("draft line %03d", index)
	}
	draft := strings.Join(lines, "\n")
	app.Update(tea.PasteMsg{Content: draft})
	if got := app.Input(); got != draft {
		t.Fatalf("long multiline draft bytes = %d, want %d", len(got), len(draft))
	}
}

func TestApplicationHeaderUsesLiveImplementationPhaseOverStaleState(t *testing.T) {
	app := testApplication(t, ApplicationOptions{Status: func() StatusSnapshot {
		return StatusSnapshot{RunName: "phase", RuntimeState: "running", SolState: "planning", ActiveAgent: "Sol", ActivePhase: "implementation"}
	}})
	app.Update(tea.WindowSizeMsg{Width: 100, Height: 8})
	header := strings.Split(ansi.Strip(app.View().Content), "\n")[0]
	if !strings.Contains(header, "Sol implementation") || strings.Contains(header, "Sol planning") {
		t.Fatalf("implementation header = %q", header)
	}
}

func TestApplicationHeaderTracksPlanningReviewAndImplementationTransitions(t *testing.T) {
	app := testApplication(t, ApplicationOptions{})
	app.Update(tea.WindowSizeMsg{Width: 100, Height: 8})
	for _, transition := range []struct {
		agent, phase string
	}{
		{agent: "Sol", phase: "planning"},
		{agent: "Luna", phase: "review"},
		{agent: "Sol", phase: "implementation"},
	} {
		app.SetStatus(StatusSnapshot{RunName: "phase", RuntimeState: "running", ActiveAgent: transition.agent, ActivePhase: transition.phase})
		header := strings.Split(ansi.Strip(app.View().Content), "\n")[0]
		if !strings.Contains(header, transition.agent+" "+transition.phase) {
			t.Fatalf("%s/%s header = %q", transition.agent, transition.phase, header)
		}
	}
}

func TestApplicationExpandedUnicodeToolDetailsRemainInViewport(t *testing.T) {
	stream := observability.NewActivityStream()
	app := testApplication(t, ApplicationOptions{Activity: stream})
	app.Update(tea.WindowSizeMsg{Width: 32, Height: 10})
	generation := uint64(3)
	if _, err := stream.Publish(&generation, observability.ActivityImplementor, observability.ActivityToolCompleted,
		map[string]any{
			"tool":       "write",
			"turn_phase": "implementation",
			"arguments":  map[string]any{"path": "seed/界.c", "offset": 0, "data": strings.Repeat("λ界", 80)},
			"result":     map[string]any{"status": 0, "output": ""},
		}, "thread", "turn", "tool"); err != nil {
		t.Fatal(err)
	}
	app.Update(activityPollMsg{})
	entries := app.ActivityModel().Entries()
	var tool ActivityDisplayEntry
	for _, entry := range entries {
		if entry.Kind == ActivityDisplayKindTool {
			tool = entry
			break
		}
	}
	if tool.Key == "" || !app.ToggleTool(tool.Key) {
		t.Fatalf("expandable tool row not found: %#v", entries)
	}
	app.Update(keyPress("", tea.KeyHome))
	view := app.View()
	assertFixedFrame(t, view.Content, 32, 10)
	plain := ansi.Strip(view.Content)
	if !strings.Contains(plain, "implementation") || !strings.Contains(plain, "details") {
		t.Fatalf("expanded tool presentation missing summary:\n%s", plain)
	}
}

func TestApplicationConfirmationKeyboardResponsesAreSingleState(t *testing.T) {
	for _, test := range []struct {
		name string
		key  tea.KeyPressMsg
		want bool
	}{
		{name: "yes", key: keyPress("Y", 'Y'), want: true},
		{name: "no", key: keyPress("n", 'n')},
		{name: "default", key: keyPress("\r", tea.KeyEnter)},
		{name: "escape", key: keyPress("", tea.KeyEscape)},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := testApplication(t, ApplicationOptions{})
			request := &confirmationRequest{prompt: "Proceed? [y/N] ", reply: make(chan bool, 1)}
			app.Update(request)
			plain := ansi.Strip(app.View().Content)
			if strings.Count(plain, "Proceed?") != 1 || strings.Count(plain, "[y/N]") != 1 {
				t.Fatalf("duplicated confirmation:\n%s", plain)
			}
			app.Update(test.key)
			if got := <-request.reply; got != test.want {
				t.Fatalf("confirmation response = %t, want %t", got, test.want)
			}
		})
	}
}

func TestApplicationSuspendAndResumeRestoreComposerFocus(t *testing.T) {
	app := testApplication(t, ApplicationOptions{})
	message := tea.KeyPressMsg(tea.Key{Code: 'z', Mod: tea.ModCtrl})
	_, suspend := app.Update(message)
	if suspend == nil {
		t.Fatalf("%q did not request Bubble Tea suspension", message.String())
	}
	if _, ok := suspend().(tea.SuspendMsg); !ok {
		t.Fatalf("Ctrl+Z command returned %T", suspend())
	}
	app.composer.Blur()
	app.Update(tea.ResumeMsg{})
	if !app.composer.Focused() || app.View().Cursor == nil {
		t.Fatalf("resume did not restore focused real cursor: focused=%t cursor=%#v", app.composer.Focused(), app.View().Cursor)
	}
}

func assertFixedFrame(t *testing.T, content string, width, height int) {
	t.Helper()
	rows := strings.Split(content, "\n")
	if len(rows) != height {
		t.Fatalf("frame row count = %d, want %d\n%s", len(rows), height, ansi.Strip(content))
	}
	for index, row := range rows {
		if got := ansi.StringWidth(row); got != width {
			t.Fatalf("frame row %d width = %d, want %d: %q", index, got, width, ansi.Strip(row))
		}
	}
}
