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
	for _, want := range []string{"run-1", "gen 7", "2 pending", "Sol", "Luna", "read", "missing", "details"} {
		if !strings.Contains(view.Content, want) {
			t.Fatalf("view does not contain %q:\n%s", want, view.Content)
		}
	}
	if strings.Contains(view.Content, "\x1b") {
		t.Fatal("view exposed a raw terminal control")
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
}

func TestApplicationCommandErrorsAreRecoverable(t *testing.T) {
	app := testApplication(t, ApplicationOptions{
		Execute: func(context.Context, string, ConfirmationFunc) (CommandResult, error) {
			return CommandResult{}, errors.New("command is not valid")
		},
	})
	app.input = "status"
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

	app.input = "status"
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
	start, _ := app.visibleWindow(entries)
	line := 1
	for index := start; index < toolIndex; index++ {
		line += countLines(app.renderEntry(entries[index])) + 1
	}
	app.Update(tea.MouseClickMsg{X: 0, Y: line, Button: tea.MouseLeft})
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
	app.input = "partially typed"
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
	if view := app.View().Content; strings.Contains(view, "\x1b") || !strings.Contains(view, `Stop the run / without archiving?\x1b[2J [y/N]`) {
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
	yesRequest := &confirmationRequest{prompt: "Confirm?", reply: make(chan bool, 1)}
	app.Update(yesRequest)
	for _, character := range "yes" {
		app.Update(keyPress(string(character), character))
	}
	app.Update(keyPress("\r", tea.KeyEnter))
	if accepted := <-yesRequest.reply; accepted {
		t.Fatal("confirmation accepted 'yes'; reference accepts only y/Y")
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
	if err == nil || !errors.Is(err, tea.ErrProgramKilled) {
		t.Fatalf("Run error = %v, want Bubble Tea killed error", err)
	}
	if shutdowns.Load() != 1 {
		t.Fatalf("shutdown hook count = %d", shutdowns.Load())
	}
	// The cursed renderer is allowed to emit terminal controls for screen
	// management. User payload safety is asserted at the View boundary above.
	_ = output
}

func TestApplicationShutdownCancelsCommandBeforePostRunHook(t *testing.T) {
	handlerStarted := make(chan struct{})
	handlerCanceled := make(chan struct{})
	shutdownHook := make(chan struct{})
	var hookState atomic.Value
	var app *Application
	var err error
	app, err = NewApplication(ApplicationOptions{
		Execute: func(ctx context.Context, _ string, _ ConfirmationFunc) (CommandResult, error) {
			close(handlerStarted)
			<-ctx.Done()
			close(handlerCanceled)
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
	case runErr := <-runDone:
		if runErr == nil || !errors.Is(runErr, tea.ErrProgramKilled) {
			t.Fatalf("Run error = %v, want Bubble Tea killed error", runErr)
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
		map[string]any{"text": strings.Join(lines, "\n")}, "thread", "turn", "long"); err != nil {
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
