package codexapp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	appServerHelperEnvironment = "CODEXOS_GO_APP_SERVER_HELPER"
	appServerHelperMode        = "CODEXOS_GO_APP_SERVER_MODE"
)

func TestCodexAppServerRoutesConcurrentResponsesNotificationsAndApproval(t *testing.T) {
	if os.Getenv(appServerHelperEnvironment) == "1" {
		return
	}
	root := t.TempDir()
	authFile := filepath.Join(root, "auth.json")
	if err := os.WriteFile(authFile, []byte("fake login"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(appServerHelperEnvironment, "1")
	t.Setenv(appServerHelperMode, "route")
	server := NewCodexAppServer(CodexAppServerOptions{
		Executable:      os.Args[0],
		AuthFile:        authFile,
		TemporaryPrefix: "codexos-go-test-",
		StopTimeout:     time.Second,
	})
	if err := server.startProcess(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if pid, ok := server.PID(); !ok || pid <= 0 {
		t.Fatalf("PID = %d, %v", pid, ok)
	}
	workspace := server.WorkspacePath()
	if workspace == "" {
		t.Fatal("server has no isolated workspace")
	}
	if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
		t.Fatalf("workspace stat = %v, %v", info, err)
	}
	configPath := filepath.Join(filepath.Dir(workspace), "codex-home", "config.toml")
	if err := os.WriteFile(configPath, []byte("test = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	type response struct {
		value any
		err   error
	}
	responses := make(chan response, 2)
	var waitGroup sync.WaitGroup
	for _, method := range []string{"first", "second"} {
		waitGroup.Add(1)
		go func(method string) {
			defer waitGroup.Done()
			value, err := server.Request(context.Background(), method, map[string]any{"method": method})
			responses <- response{value: value, err: err}
		}(method)
	}
	waitGroup.Wait()
	close(responses)
	seen := make(map[string]bool)
	for result := range responses {
		if result.err != nil {
			t.Fatalf("concurrent request failed: %v", result.err)
		}
		value, ok := result.value.(map[string]any)
		if !ok {
			t.Fatalf("response result = %#v", result.value)
		}
		method, ok := value["method"].(string)
		if !ok || seen[method] {
			t.Fatalf("response result = %#v", value)
		}
		seen[method] = true
	}
	if !seen["first"] || !seen["second"] {
		t.Fatalf("response methods = %#v", seen)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	notification, err := server.NextNotification(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if notification["method"] != "test/notification" {
		t.Fatalf("notification = %#v", notification)
	}
}

func TestCodexAppServerCancellationRemovesPendingRequest(t *testing.T) {
	if os.Getenv(appServerHelperEnvironment) == "1" {
		return
	}
	authFile := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authFile, []byte("fake login"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(appServerHelperEnvironment, "1")
	t.Setenv(appServerHelperMode, "hold")
	server := NewCodexAppServer(CodexAppServerOptions{
		Executable:  os.Args[0],
		AuthFile:    authFile,
		StopTimeout: time.Second,
	})
	if err := server.startProcess(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := server.Request(ctx, "hold", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Request error = %v, want deadline exceeded", err)
	}
	server.pendingMu.Lock()
	pending := len(server.pending)
	server.pendingMu.Unlock()
	if pending != 0 {
		t.Fatalf("pending requests = %d, want zero", pending)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCodexAppServerShutdownKillsChildAfterBoundedTimeout(t *testing.T) {
	if os.Getenv(appServerHelperEnvironment) == "1" {
		return
	}
	authFile := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authFile, []byte("fake login"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(appServerHelperEnvironment, "1")
	t.Setenv(appServerHelperMode, "ignore-term")
	server := NewCodexAppServer(CodexAppServerOptions{
		Executable:  os.Args[0],
		AuthFile:    authFile,
		StopTimeout: 30 * time.Millisecond,
	})
	if err := server.startProcess(context.Background()); err != nil {
		t.Fatal(err)
	}
	pid, ok := server.PID()
	if !ok {
		t.Fatal("server did not expose child PID")
	}
	started := time.Now()
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("shutdown took %s", elapsed)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child PID %d still exists: %v", pid, err)
	}
}

func TestCodexAppServerShutdownUnblocksStalledWrite(t *testing.T) {
	if os.Getenv(appServerHelperEnvironment) == "1" {
		return
	}
	authFile := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authFile, []byte("fake login"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(appServerHelperEnvironment, "1")
	t.Setenv(appServerHelperMode, "no-read")
	server := NewCodexAppServer(CodexAppServerOptions{
		Executable:  os.Args[0],
		AuthFile:    authFile,
		StopTimeout: 100 * time.Millisecond,
	})
	if err := server.startProcess(context.Background()); err != nil {
		t.Fatal(err)
	}
	requestDone := make(chan error, 1)
	go func() {
		_, err := server.Request(context.Background(), "large", map[string]any{
			"text": strings.Repeat("x", 8*1024*1024),
		})
		requestDone <- err
	}()
	time.Sleep(20 * time.Millisecond)
	started := time.Now()
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("shutdown took %s", elapsed)
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("stalled request did not unblock after shutdown")
	}
}

func TestCodexAppServerReportsMalformedOutputAndBoundedStderr(t *testing.T) {
	if os.Getenv(appServerHelperEnvironment) == "1" {
		return
	}
	authFile := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authFile, []byte("fake login"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(appServerHelperEnvironment, "1")
	t.Setenv(appServerHelperMode, "exit")
	server := NewCodexAppServer(CodexAppServerOptions{
		Executable:  os.Args[0],
		AuthFile:    authFile,
		StopTimeout: time.Second,
	})
	if err := server.startProcess(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := server.NextNotification(ctx)
	if err == nil || !strings.Contains(err.Error(), "exited unexpectedly with status 7") {
		t.Fatalf("notification error = %v", err)
	}
	if !strings.Contains(err.Error(), "diagnostic-prefix") {
		t.Fatalf("notification error omitted stderr diagnostic: %v", err)
	}
	if captured := server.stderrCapture.text(); len(captured) > MaxErrorOutput {
		t.Fatalf("captured stderr bytes = %d", len(captured))
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCodexAppServerBoundsNotificationBacklog(t *testing.T) {
	if os.Getenv(appServerHelperEnvironment) == "1" {
		return
	}
	authFile := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authFile, []byte("fake login"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(appServerHelperEnvironment, "1")
	t.Setenv(appServerHelperMode, "burst")
	server := NewCodexAppServer(CodexAppServerOptions{
		Executable:  os.Args[0],
		AuthFile:    authFile,
		StopTimeout: time.Second,
	})
	if err := server.startProcess(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	deadline := time.Now().Add(time.Second)
	for {
		server.stateMu.Lock()
		readerError := server.readerError
		server.stateMu.Unlock()
		if readerError != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("notification overflow was not detected")
		}
		time.Sleep(time.Millisecond)
	}
	_, err := server.NextNotification(ctx)
	if err == nil || !strings.Contains(err.Error(), "notification queue exceeds bounds") {
		t.Fatalf("notification overflow error = %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCodexAppServerHandshakeCatalogThreadTurnAndInterrupt(t *testing.T) {
	if os.Getenv(appServerHelperEnvironment) == "1" {
		return
	}
	authFile := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authFile, []byte("fake login"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(appServerHelperEnvironment, "1")
	t.Setenv(appServerHelperMode, "high-level")
	server := NewCodexAppServer(CodexAppServerOptions{
		Executable:  os.Args[0],
		AuthFile:    authFile,
		ConfigText:  "sandbox = true\n",
		StopTimeout: time.Second,
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	tierName, err := server.ValidateModel(ctx, "model-x", "high", "priority", "auto")
	if err != nil || tierName != "Fast" {
		t.Fatalf("ValidateModel = %q, %v", tierName, err)
	}
	threadID, err := server.StartThread(ctx, StartThreadOptions{
		Model:             "model-x",
		ServiceTier:       "priority",
		PermissionProfile: "codexos-implementor",
		DynamicTools:      []map[string]any{},
	})
	if err != nil || threadID != "thread-high-level" {
		t.Fatalf("StartThread = %q, %v", threadID, err)
	}
	turnID, err := server.StartTurn(ctx, StartTurnOptions{
		ThreadID:          threadID,
		Prompt:            "Continue.",
		Model:             "model-x",
		Effort:            "high",
		ReasoningSummary:  "auto",
		ServiceTier:       "priority",
		PermissionProfile: "codexos-implementor",
	})
	if err != nil || turnID != "turn-high-level" {
		t.Fatalf("StartTurn = %q, %v", turnID, err)
	}
	if err := server.InterruptTurn(ctx, threadID, turnID); err != nil {
		t.Fatal(err)
	}
}

func TestCodexAppServerHelper(t *testing.T) {
	if os.Getenv(appServerHelperEnvironment) != "1" {
		return
	}
	mode := os.Getenv(appServerHelperMode)
	if mode == "ignore-term" {
		signalIgnoreTerm()
		for {
			time.Sleep(time.Hour)
		}
	}
	if mode == "exit" {
		_, _ = fmt.Fprintln(os.Stderr, "diagnostic-prefix "+strings.Repeat("x", MaxErrorOutput*2))
		os.Exit(7)
	}
	decoder := json.NewDecoder(bufio.NewReader(os.Stdin))
	decoder.UseNumber()
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	send := func(message map[string]any) {
		if err := encoder.Encode(message); err != nil {
			os.Exit(8)
		}
	}
	read := func() map[string]any {
		var message map[string]any
		if err := decoder.Decode(&message); err != nil {
			os.Exit(9)
		}
		return message
	}
	if mode != "route" && mode != "hold" && mode != "burst" && mode != "high-level" && mode != "no-read" {
		os.Exit(10)
	}
	if mode == "no-read" {
		for {
			time.Sleep(time.Hour)
		}
	}
	if mode == "burst" {
		for index := 0; index < maxAppServerQueueMessages+1; index++ {
			send(map[string]any{"method": "test/burst", "params": map[string]any{"index": index}})
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	if mode == "hold" {
		for {
			_ = read()
			time.Sleep(time.Hour)
		}
	}
	if mode == "high-level" {
		runHighLevelHelper(read, send)
	}
	requests := []map[string]any{read(), read()}
	send(map[string]any{"method": "test/notification", "params": map[string]any{"value": "ok"}})
	send(map[string]any{
		"id":     "approval-1",
		"method": "item/commandExecution/requestApproval",
		"params": map[string]any{},
	})
	approval := read()
	result, resultOK := approval["result"].(map[string]any)
	if approval["id"] != "approval-1" || !resultOK || result["decision"] != "decline" {
		os.Exit(11)
	}
	for index := len(requests) - 1; index >= 0; index-- {
		request := requests[index]
		send(map[string]any{"id": request["id"], "result": map[string]any{"method": request["method"]}})
	}
	for {
		time.Sleep(time.Hour)
	}
}

func runHighLevelHelper(read func() map[string]any, send func(map[string]any)) {
	modelPage := 0
	for {
		request := read()
		method, _ := request["method"].(string)
		requestID := request["id"]
		switch method {
		case "initialized":
			continue
		case "initialize":
			send(map[string]any{"id": requestID, "result": map[string]any{"userAgent": "fake"}})
		case "account/read":
			send(map[string]any{"id": requestID, "result": map[string]any{"account": map[string]any{"type": "chatgpt"}}})
		case "model/list":
			if modelPage == 0 {
				modelPage++
				send(map[string]any{"id": requestID, "result": map[string]any{"data": []any{}, "nextCursor": "page-2"}})
				continue
			}
			send(map[string]any{"id": requestID, "result": map[string]any{
				"data": []any{map[string]any{
					"model":                     "model-x",
					"supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "high"}},
					"serviceTiers":              []any{map[string]any{"id": "priority", "name": "Fast"}},
				}}, "nextCursor": nil,
			}})
		case "thread/start":
			params, ok := request["params"].(map[string]any)
			if !ok || params["model"] != "model-x" || params["serviceTier"] != "priority" || params["permissions"] != "codexos-implementor" || params["ephemeral"] != true {
				os.Exit(12)
			}
			send(map[string]any{"id": requestID, "result": map[string]any{
				"thread": map[string]any{"id": "thread-high-level", "ephemeral": true},
				"model":  "model-x", "serviceTier": "priority",
				"activePermissionProfile": map[string]any{"id": "codexos-implementor"},
				"sandbox":                 map[string]any{"type": "workspaceWrite", "networkAccess": false},
			}})
		case "turn/start":
			params, ok := request["params"].(map[string]any)
			if !ok || params["threadId"] != "thread-high-level" || params["model"] != "model-x" || params["effort"] != "high" || params["summary"] != "auto" {
				os.Exit(13)
			}
			send(map[string]any{"id": "approval-high-level", "method": "item/commandExecution/requestApproval", "params": map[string]any{}})
			approval := read()
			approvalResult, ok := approval["result"].(map[string]any)
			if !ok || approval["id"] != "approval-high-level" || approvalResult["decision"] != "decline" {
				os.Exit(14)
			}
			send(map[string]any{"id": requestID, "result": map[string]any{"turn": map[string]any{"id": "turn-high-level"}}})
		case "turn/interrupt":
			if params, ok := request["params"].(map[string]any); !ok || params["threadId"] != "thread-high-level" || params["turnId"] != "turn-high-level" {
				os.Exit(15)
			}
			send(map[string]any{"id": requestID, "result": map[string]any{}})
		default:
			os.Exit(16)
		}
	}
}

func signalIgnoreTerm() {
	signal.Ignore(syscall.SIGTERM)
}
