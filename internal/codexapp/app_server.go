package codexapp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	appServerStopTimeout      = 2 * time.Second
	maxAppServerQueueMessages = 256
	maxAppServerQueueBytes    = 32 * 1024 * 1024
)

// CodexAppServerOptions describes one isolated app-server process.
//
// AuthFile is copied into the isolated Codex home as a symlink, matching the
// Python harness. An empty AuthFile uses CODEX_HOME/auth.json when CODEX_HOME
// is set, or ~/.codex/auth.json otherwise.
type CodexAppServerOptions struct {
	Executable            string
	AuthFile              string
	TemporaryPrefix       string
	ConfigText            string
	ServerRequestObserver func(map[string]any)
	StopTimeout           time.Duration
}

// StartThreadOptions contains the exact policy values sent to thread/start.
type StartThreadOptions struct {
	Model             string
	Effort            string
	ServiceTier       string
	PermissionProfile string
	DynamicTools      []map[string]any
	RequireReadOnly   bool
}

// StartTurnOptions contains the exact values sent to turn/start.
type StartTurnOptions struct {
	ThreadID              string
	Prompt                string
	Model                 string
	Effort                string
	ReasoningSummary      string
	ServiceTier           string
	PermissionProfile     string
	RuntimeWorkspaceRoots []string
}

// CodexAppServer owns one app-server child, its JSONL stdout reader, stderr
// capture, and all protocol queues. Requests may be made concurrently; one
// reader goroutine owns stdout and one writer lock owns stdin ordering.
type CodexAppServer struct {
	options CodexAppServerOptions

	lifecycleMu sync.Mutex
	stateMu     sync.Mutex
	writeMu     sync.Mutex
	requestMu   sync.Mutex
	pendingMu   sync.Mutex
	handlerMu   sync.Mutex

	process       *exec.Cmd
	stdin         io.WriteCloser
	stdout        io.ReadCloser
	processDone   chan struct{}
	readerDone    chan struct{}
	serverDone    chan struct{}
	processErr    error
	closing       bool
	started       bool
	readerError   *Error
	callbackDepth atomic.Int32

	nextRequestID uint64
	pending       map[uint64]chan any

	notifications  *appMessageQueue
	serverRequests *appMessageQueue
	serverHandler  func(map[string]any)

	stderrCapture boundedOutput
	temporaryRoot string
	workspacePath string
}

// NewCodexAppServer constructs a server. It does not start a subprocess.
func NewCodexAppServer(options CodexAppServerOptions) *CodexAppServer {
	if options.Executable == "" {
		options.Executable = "codex"
	}
	if options.TemporaryPrefix == "" {
		options.TemporaryPrefix = "codexos-app-server-"
	}
	if options.StopTimeout == 0 {
		options.StopTimeout = appServerStopTimeout
	}
	return &CodexAppServer{
		options:        options,
		nextRequestID:  1,
		pending:        make(map[uint64]chan any),
		notifications:  newAppMessageQueue(),
		serverRequests: newAppMessageQueue(),
	}
}

// DefaultAuthFile returns the file-based ChatGPT login location used by the
// Python harness.
func DefaultAuthFile() string {
	if configured := os.Getenv("CODEX_HOME"); configured != "" {
		return filepath.Join(configured, "auth.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".codex", "auth.json")
	}
	return filepath.Join(home, ".codex", "auth.json")
}

// Start starts the isolated process and performs initialize and the ChatGPT
// account check, matching CodexAppServer.__enter__ in the Python harness.
func (s *CodexAppServer) Start(ctx context.Context) error {
	if err := s.startProcess(ctx); err != nil {
		return err
	}
	if err := s.Initialize(ctx); err != nil {
		_ = s.Close()
		return err
	}
	if err := s.ValidateChatGPTLogin(ctx); err != nil {
		_ = s.Close()
		return err
	}
	return nil
}

// startProcess starts the child and protocol reader without performing the
// protocol handshake. Production callers use Start.
func (s *CodexAppServer) startProcess(ctx context.Context) error {
	if ctx == nil {
		return &Error{Reason: "Codex app-server start context is nil"}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.stateMu.Lock()
	if s.process != nil {
		s.stateMu.Unlock()
		return &Error{Reason: "Codex app-server is already running"}
	}
	if s.started {
		s.stateMu.Unlock()
		return &Error{Reason: "Codex app-server cannot be restarted"}
	}
	s.closing = false
	s.readerError = nil
	s.processErr = nil
	s.stateMu.Unlock()

	authFile := s.options.AuthFile
	if authFile == "" {
		authFile = DefaultAuthFile()
	}
	info, err := os.Stat(authFile)
	if err != nil || !info.Mode().IsRegular() {
		return &Error{Reason: "Codex is not authenticated with a file-based ChatGPT login"}
	}

	temporaryRoot, err := os.MkdirTemp("/tmp", s.options.TemporaryPrefix)
	if err != nil {
		return &Error{Reason: "failed to prepare isolated Codex app-server state", Err: err}
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(temporaryRoot)
		}
	}()

	codexHome := filepath.Join(temporaryRoot, "codex-home")
	workspace := filepath.Join(temporaryRoot, "workspace")
	processTmp := filepath.Join(temporaryRoot, "tmp")
	for _, directory := range []string{codexHome, workspace, processTmp} {
		if err := os.Mkdir(directory, 0o777); err != nil {
			return &Error{Reason: "failed to prepare isolated Codex app-server state", Err: err}
		}
	}
	if err := os.Symlink(authFile, filepath.Join(codexHome, "auth.json")); err != nil {
		return &Error{Reason: "failed to prepare isolated Codex app-server state", Err: err}
	}
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(s.options.ConfigText), 0o666); err != nil {
		return &Error{Reason: "failed to prepare isolated Codex app-server state", Err: err}
	}

	executable, err := exec.LookPath(s.options.Executable)
	if err != nil {
		return &Error{Reason: "Codex executable not found: " + s.options.Executable}
	}
	environment := os.Environ()
	environment = setEnvironment(environment, "CODEX_HOME", codexHome)
	environment = setEnvironment(environment, "CODEX_SQLITE_HOME", codexHome)
	environment = setEnvironment(environment, "TMPDIR", processTmp)
	environment = setEnvironment(environment, "CODEX_NON_INTERACTIVE", "1")
	for _, name := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
		environment = removeEnvironment(environment, name)
	}

	command := exec.Command(executable, "app-server", "--listen", "stdio://")
	command.Dir = workspace
	command.Env = environment
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdin, err := command.StdinPipe()
	if err != nil {
		return &Error{Reason: "failed to start Codex app-server", Err: err}
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return &Error{Reason: "failed to start Codex app-server", Err: err}
	}
	// Assigning the bounded writer directly lets os/exec drain stderr as part
	// of Cmd.Wait, so a noisy child cannot block and diagnostics are complete
	// before processDone is closed.
	s.stderrCapture.reset()
	command.Stderr = &s.stderrCapture
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return &Error{Reason: "failed to start Codex app-server", Err: err}
	}

	processDone := make(chan struct{})
	readerDone := make(chan struct{})
	serverDone := make(chan struct{})
	s.stateMu.Lock()
	s.process = command
	s.started = true
	s.stdin = stdin
	s.stdout = stdout
	s.processDone = processDone
	s.readerDone = readerDone
	s.serverDone = serverDone
	s.processErr = nil
	s.temporaryRoot = temporaryRoot
	s.workspacePath = workspace
	s.notifications = newAppMessageQueue()
	s.serverRequests = newAppMessageQueue()
	s.stateMu.Unlock()
	s.handlerMu.Lock()
	s.serverHandler = nil
	s.handlerMu.Unlock()

	// processDone is closed by the sole owner of Cmd.Wait.
	go s.waitProcess(command, processDone)
	go s.readLoop()
	go s.serverRequestLoop()
	cleanup = false
	return nil
}

// Initialize performs the protocol handshake's initialize request and
// initialized notification.
func (s *CodexAppServer) Initialize(ctx context.Context) error {
	if _, err := s.Request(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "codexos-harness",
			"title":   "CodexOS harness",
			"version": "0.1.0",
		},
		"capabilities": map[string]any{"experimentalApi": true},
	}); err != nil {
		return err
	}
	return s.Notify(ctx, "initialized", nil)
}

// ValidateChatGPTLogin verifies the account/read response selected by the
// app-server.
func (s *CodexAppServer) ValidateChatGPTLogin(ctx context.Context) error {
	response, err := s.Request(ctx, "account/read", map[string]any{"refreshToken": false})
	if err != nil {
		return err
	}
	values, err := ObjectValue(response, "account/read response")
	if err != nil {
		return err
	}
	account, err := ObjectValue(values["account"], "account")
	if err != nil || account["type"] != "chatgpt" {
		return &Error{Reason: "Codex is not authenticated using ChatGPT"}
	}
	return nil
}

// ValidateModel validates one exact model/reasoning/service-tier selection
// and returns the server's display name for the selected tier.
func (s *CodexAppServer) ValidateModel(ctx context.Context, model, effort, serviceTier, reasoningSummary string) (string, error) {
	switch reasoningSummary {
	case "auto", "concise", "detailed", "none":
	default:
		return "", &Error{Reason: "unsupported reasoning summary setting: " + strconv.Quote(reasoningSummary)}
	}

	var cursor any
	for {
		response, err := s.Request(ctx, "model/list", map[string]any{"cursor": cursor})
		if err != nil {
			return "", err
		}
		values, err := ObjectValue(response, "model/list response")
		if err != nil {
			return "", err
		}
		data, ok := values["data"].([]any)
		if !ok {
			return "", &Error{Reason: "model/list response is missing its model catalog"}
		}
		for _, entryValue := range data {
			entry, ok := entryValue.(map[string]any)
			if !ok || entry["model"] != model {
				continue
			}
			supported, ok := entry["supportedReasoningEfforts"].([]any)
			if !ok {
				return "", &Error{Reason: "model " + strconv.Quote(model) + " has malformed reasoning capabilities"}
			}
			effortSupported := false
			for _, optionValue := range supported {
				option, ok := optionValue.(map[string]any)
				if ok && option["reasoningEffort"] == effort {
					effortSupported = true
					break
				}
			}
			if !effortSupported {
				return "", &Error{Reason: "model " + strconv.Quote(model) + " does not support reasoning effort " + strconv.Quote(effort)}
			}

			var tiers []any
			if value, exists := entry["serviceTiers"]; exists {
				var tiersOK bool
				tiers, tiersOK = value.([]any)
				if !tiersOK {
					return "", &Error{Reason: "model " + strconv.Quote(model) + " has malformed service-tier capabilities"}
				}
			}
			for _, tierValue := range tiers {
				tier, ok := tierValue.(map[string]any)
				if !ok || tier["id"] != serviceTier {
					continue
				}
				name, ok := tier["name"].(string)
				if !ok || name == "" {
					return "", &Error{Reason: "model " + strconv.Quote(model) + " has malformed service-tier capabilities for " + strconv.Quote(serviceTier)}
				}
				return name, nil
			}
			return "", &Error{Reason: "model " + strconv.Quote(model) + " does not support service tier " + strconv.Quote(serviceTier)}
		}

		cursorValue, exists := values["nextCursor"]
		if !exists || cursorValue == nil {
			break
		}
		cursorString, ok := cursorValue.(string)
		if !ok {
			return "", &Error{Reason: "model/list returned an invalid cursor"}
		}
		cursor = cursorString
	}
	return "", &Error{Reason: "requested Codex model is unavailable: " + model}
}

// StartThread starts one ephemeral thread with the supplied policy.
func (s *CodexAppServer) StartThread(ctx context.Context, options StartThreadOptions) (string, error) {
	workspace := s.WorkspacePath()
	if workspace == "" {
		return "", &Error{Reason: "Codex app-server is not running"}
	}
	response, err := s.Request(ctx, "thread/start", map[string]any{
		"allowProviderModelFallback": false,
		"approvalPolicy":             "never",
		"approvalsReviewer":          "user",
		"config":                     map[string]any{"model_reasoning_effort": options.Effort},
		"cwd":                        workspace,
		"dynamicTools":               options.DynamicTools,
		"environments":               []any{},
		"ephemeral":                  true,
		"model":                      options.Model,
		"permissions":                options.PermissionProfile,
		"runtimeWorkspaceRoots":      []string{workspace},
		"serviceTier":                options.ServiceTier,
	})
	if err != nil {
		return "", err
	}
	values, err := ObjectValue(response, "thread/start response")
	if err != nil {
		return "", err
	}
	thread, err := ObjectValue(values["thread"], "thread/start thread")
	if err != nil {
		return "", err
	}
	threadID, ok := thread["id"].(string)
	if !ok || threadID == "" {
		return "", &Error{Reason: "thread/start response is missing a thread ID"}
	}
	if thread["ephemeral"] != true {
		return "", &Error{Reason: "Codex app-server did not create an ephemeral thread"}
	}
	if values["model"] != options.Model {
		return "", &Error{Reason: "Codex app-server did not select the requested model"}
	}
	if values["reasoningEffort"] != options.Effort {
		return "", &Error{Reason: "Codex app-server did not select the requested reasoning effort"}
	}
	if values["serviceTier"] != options.ServiceTier {
		return "", &Error{Reason: "Codex app-server did not select the requested service tier"}
	}
	profile, err := ObjectValue(values["activePermissionProfile"], "active permission profile")
	if err != nil || profile["id"] != options.PermissionProfile {
		return "", &Error{Reason: "Codex app-server did not activate the isolated permission profile"}
	}
	sandbox, err := ObjectValue(values["sandbox"], "sandbox")
	if err != nil || sandbox["networkAccess"] != false {
		return "", &Error{Reason: "Codex app-server did not disable command network access"}
	}
	if sandbox["type"] == "dangerFullAccess" {
		return "", &Error{Reason: "Codex app-server selected an unsafe filesystem sandbox"}
	}
	if options.RequireReadOnly && sandbox["type"] != "readOnly" {
		return "", &Error{Reason: "Codex app-server did not activate a read-only filesystem sandbox"}
	}
	return threadID, nil
}

// StartTurn starts one turn and returns its server-assigned ID.
func (s *CodexAppServer) StartTurn(ctx context.Context, options StartTurnOptions) (string, error) {
	params := map[string]any{
		"approvalPolicy":    "never",
		"approvalsReviewer": "user",
		"effort":            options.Effort,
		"environments":      []any{},
		"input":             []map[string]any{{"type": "text", "text": options.Prompt}},
		"model":             options.Model,
		"permissions":       options.PermissionProfile,
		"serviceTier":       options.ServiceTier,
		"summary":           options.ReasoningSummary,
		"threadId":          options.ThreadID,
	}
	if options.RuntimeWorkspaceRoots != nil {
		params["runtimeWorkspaceRoots"] = options.RuntimeWorkspaceRoots
	}
	response, err := s.Request(ctx, "turn/start", params)
	if err != nil {
		return "", err
	}
	values, err := ObjectValue(response, "turn/start response")
	if err != nil {
		return "", err
	}
	turn, err := ObjectValue(values["turn"], "turn/start turn")
	if err != nil {
		return "", err
	}
	turnID, ok := turn["id"].(string)
	if !ok || turnID == "" {
		return "", &Error{Reason: "turn/start response is missing a turn ID"}
	}
	return turnID, nil
}

// InterruptTurn sends the app-server cancellation request. The caller can
// consume NextNotification and wait for turn/completed with status
// "interrupted", just as the Python worker does.
func (s *CodexAppServer) InterruptTurn(ctx context.Context, threadID, turnID string) error {
	_, err := s.Request(ctx, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID})
	return err
}

// Request sends one JSON-RPC request and waits for its matching response.
// Context cancellation removes the request from the pending table; a late
// response is consequently treated as a protocol error by the sole reader.
func (s *CodexAppServer) Request(ctx context.Context, method string, params any) (any, error) {
	if ctx == nil {
		return nil, &Error{Reason: "Codex app-server request context is nil"}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.requestMu.Lock()
	requestID := s.nextRequestID
	if requestID == 0 {
		s.requestMu.Unlock()
		return nil, &Error{Reason: "Codex app-server request ID space is exhausted"}
	}
	s.nextRequestID++
	s.requestMu.Unlock()

	response := make(chan any, 1)
	s.pendingMu.Lock()
	s.stateMu.Lock()
	readerError := s.readerError
	closing := s.closing
	processRunning := s.process != nil
	s.stateMu.Unlock()
	if readerError != nil {
		s.pendingMu.Unlock()
		return nil, readerError
	}
	if closing || !processRunning {
		s.pendingMu.Unlock()
		return nil, &Error{Reason: "Codex app-server is not running"}
	}
	s.pending[requestID] = response
	s.pendingMu.Unlock()

	defer func() {
		s.pendingMu.Lock()
		delete(s.pending, requestID)
		s.pendingMu.Unlock()
	}()

	if err := s.writeMessage(map[string]any{"id": requestID, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case value := <-response:
		if transportError, ok := value.(*Error); ok {
			return nil, transportError
		}
		message, err := ObjectValue(value, method+" response")
		if err != nil {
			return nil, err
		}
		if errorValue, exists := message["error"]; exists {
			description, encodeErr := ShortJSON(errorValue)
			if encodeErr != nil {
				return nil, encodeErr
			}
			return nil, &Error{Reason: "Codex app-server " + method + " failed: " + description}
		}
		result, exists := message["result"]
		if !exists {
			return nil, &Error{Reason: "Codex app-server " + method + " response has no result"}
		}
		return result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Notify sends a JSON-RPC notification. A nil params value is omitted, as in
// the Python implementation.
func (s *CodexAppServer) Notify(ctx context.Context, method string, params any) error {
	if ctx == nil {
		return &Error{Reason: "Codex app-server notification context is nil"}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	message := map[string]any{"method": method}
	if params != nil {
		message["params"] = params
	}
	return s.writeMessage(message)
}

// NextNotification returns the next unsolicited notification, or a reader
// failure/closed error. Request responses never appear here.
func (s *CodexAppServer) NextNotification(ctx context.Context) (map[string]any, error) {
	if ctx == nil {
		return nil, &Error{Reason: "Codex app-server notification context is nil"}
	}
	value, err := s.notifications.get(ctx)
	if err != nil {
		return nil, err
	}
	if value == appQueueClosed {
		return nil, &Error{Reason: "Codex app-server is closed"}
	}
	if transportError, ok := value.(*Error); ok {
		return nil, transportError
	}
	return ObjectValue(value, "Codex app-server notification")
}

// SetServerRequestHandler installs the sequential server-request callback.
// The callback runs on the app-server request worker and may call
// WriteResult, Request, or RejectServerRequest.
func (s *CodexAppServer) SetServerRequestHandler(handler func(map[string]any)) {
	s.handlerMu.Lock()
	s.serverHandler = handler
	s.handlerMu.Unlock()
}

// RejectServerRequest returns the least-privilege response used by the
// Python harness for known approval requests and an unsupported-method error
// for every other server request.
func (s *CodexAppServer) RejectServerRequest(message map[string]any) error {
	requestID := message["id"]
	method, _ := message["method"].(string)
	var result any
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		result = map[string]any{"decision": "decline"}
	case "item/permissions/requestApproval":
		result = map[string]any{
			"permissions": map[string]any{
				"fileSystem": map[string]any{"entries": []any{}},
				"network":    map[string]any{"enabled": false},
			},
			"scope":            "turn",
			"strictAutoReview": false,
		}
	case "item/tool/requestUserInput":
		result = map[string]any{"answers": map[string]any{}}
	default:
		return s.writeMessage(map[string]any{
			"id": requestID,
			"error": map[string]any{
				"code":    -32601,
				"message": "unsupported server request: " + method,
			},
		})
	}
	return s.writeMessage(map[string]any{"id": requestID, "result": result})
}

// WriteResult sends a response to an app-server initiated request.
func (s *CodexAppServer) WriteResult(requestID any, result any) error {
	return s.writeMessage(map[string]any{"id": requestID, "result": result})
}

// PID returns the running child PID. It returns false after process exit or
// explicit shutdown.
func (s *CodexAppServer) PID() (int, bool) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.process == nil || s.process.Process == nil || s.processDone == nil {
		return 0, false
	}
	select {
	case <-s.processDone:
		return 0, false
	default:
		return s.process.Process.Pid, true
	}
}

// WorkspacePath returns the isolated workspace while the process exists.
func (s *CodexAppServer) WorkspacePath() string {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.process == nil || s.closing {
		return ""
	}
	return s.workspacePath
}

// Close terminates the child with SIGTERM and then SIGKILL after the bounded
// configured timeout. It is idempotent.
func (s *CodexAppServer) Close() error {
	return s.Shutdown(context.Background(), s.options.StopTimeout)
}

// Shutdown is the context-aware bounded shutdown operation. Cancellation
// still reaps the child before returning, matching the QEMU lifecycle owner.
func (s *CodexAppServer) Shutdown(ctx context.Context, timeout time.Duration) error {
	if ctx == nil {
		return &Error{Reason: "Codex app-server shutdown context is nil"}
	}
	if timeout < 0 {
		return &Error{Reason: "Codex app-server shutdown timeout must not be negative"}
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	s.stateMu.Lock()
	process := s.process
	stdin := s.stdin
	stdout := s.stdout
	processDone := s.processDone
	readerDone := s.readerDone
	serverDone := s.serverDone
	temporaryRoot := s.temporaryRoot
	if process == nil {
		s.stateMu.Unlock()
		return nil
	}
	s.closing = true
	s.process = nil
	s.stdin = nil
	s.stdout = nil
	s.stateMu.Unlock()

	// Fail waiters before closing stdin so a caller blocked in Request cannot
	// outlive shutdown merely because the child ignores the pipe close.
	closedError := &Error{Reason: "Codex app-server is closed"}
	s.pendingMu.Lock()
	for _, response := range s.pending {
		nonBlockingSend(response, closedError)
	}
	s.pendingMu.Unlock()
	s.notifications.forcePut(appQueueClosed, 0)
	s.serverRequests.forcePut(appQueueClosed, 0)

	// Closing an os/exec stdin pipe is safe while another goroutine is writing
	// it and is necessary to unblock a stalled large write. Waiting for
	// writeMu here would make shutdown depend on a child that stopped reading.
	if stdin != nil {
		_ = stdin.Close()
	}

	deadline := time.Now().Add(timeout)
	termErr := signalProcessGroup(process, syscall.SIGTERM)
	exited := waitProcessUntil(ctx, processDone, deadline)
	if !exited {
		killErr := signalProcessGroup(process, syscall.SIGKILL)
		if termErr == nil {
			termErr = killErr
		}
		// A TERM timeout and the post-KILL reap each get their own bounded
		// interval. In particular, a child ignoring TERM must still be
		// reaped by the Cmd.Wait owner before temporary state is removed.
		exited = waitChannelUntil(processDone, time.Now().Add(timeout))
	}
	if !exited {
		if termErr == nil {
			termErr = &Error{Reason: "Codex app-server did not exit before shutdown deadline"}
		}
	}

	// Closing the parent pipe copies unblocks a reader that is still waiting
	// after a malformed or partially written child output. The goroutines are
	// owned by this object; they are given the same bounded deadline but are
	// deliberately not allowed to make Close unbounded.
	if stdout != nil {
		_ = stdout.Close()
	}
	// A callback may deliberately close its owning server. Such a callback
	// cannot wait for its own reader/request goroutine; it will observe the
	// closed process and return after Shutdown releases the process boundary.
	if s.callbackDepth.Load() == 0 {
		waitChannelUntil(readerDone, time.Now().Add(timeout))
		waitChannelUntil(serverDone, time.Now().Add(timeout))
	}
	if processDone != nil {
		// Cmd.Wait has already been called by waitProcess; this is only a final
		// bounded observation, never a second Wait.
		waitChannelUntil(processDone, deadline)
	}
	if temporaryRoot != "" {
		if err := os.RemoveAll(temporaryRoot); err != nil && termErr == nil {
			termErr = &Error{Reason: "failed to clean up isolated Codex app-server state", Err: err}
		}
	}
	s.stateMu.Lock()
	s.temporaryRoot = ""
	s.workspacePath = ""
	s.stateMu.Unlock()
	if termErr != nil && !errors.Is(termErr, os.ErrProcessDone) && !errors.Is(termErr, syscall.ESRCH) {
		if _, ok := termErr.(*Error); ok {
			return termErr
		}
		return &Error{Reason: "failed to stop Codex app-server", Err: termErr}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func (s *CodexAppServer) writeMessage(message map[string]any) error {
	encoded, err := EncodeMessage(message)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.stateMu.Lock()
	stdin := s.stdin
	closing := s.closing
	process := s.process
	s.stateMu.Unlock()
	if stdin == nil || process == nil || closing {
		return &Error{Reason: "Codex app-server is not running"}
	}
	if _, err := writeAll(stdin, encoded); err != nil {
		return &Error{Reason: "Codex app-server closed its input unexpectedly", Err: err}
	}
	return nil
}

func (s *CodexAppServer) waitProcess(process *exec.Cmd, processDone chan struct{}) {
	err := process.Wait()
	s.stateMu.Lock()
	s.processErr = err
	s.stateMu.Unlock()
	close(processDone)
}

func (s *CodexAppServer) readLoop() {
	s.stateMu.Lock()
	stdout := s.stdout
	readerDone := s.readerDone
	processDone := s.processDone
	s.stateMu.Unlock()
	defer func() {
		if readerDone != nil {
			close(readerDone)
		}
	}()
	if stdout == nil {
		return
	}
	reader := bufio.NewReader(stdout)
	for {
		line, err := readJSONLine(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Cmd.Wait and the stdout pipe can observe process termination in
				// either order. Give the Wait owner a short bounded opportunity to
				// publish the real exit status before constructing diagnostics.
				waitChannelUntil(processDone, time.Now().Add(100*time.Millisecond))
				s.stateMu.Lock()
				closing := s.closing
				s.stateMu.Unlock()
				if !closing {
					s.failReader(s.unexpectedExitError())
				}
				return
			}
			s.stateMu.Lock()
			closing := s.closing
			s.stateMu.Unlock()
			if !closing {
				s.failReader(asAppServerError(err))
			}
			return
		}
		message, decodeErr := DecodeMessage(line)
		if decodeErr != nil {
			s.stateMu.Lock()
			closing := s.closing
			s.stateMu.Unlock()
			if !closing {
				s.failReader(asAppServerError(decodeErr))
			}
			return
		}
		if !s.routeMessage(message, len(line)) {
			return
		}
	}
}

func (s *CodexAppServer) routeMessage(message map[string]any, encodedSize int) bool {
	requestID, hasID := message["id"]
	_, hasMethod := message["method"]
	if hasID && requestID != nil && hasMethod {
		if observer := s.options.ServerRequestObserver; observer != nil {
			s.callbackDepth.Add(1)
			func() {
				defer func() {
					s.callbackDepth.Add(-1)
					_ = recover()
				}()
				observer(message)
			}()
		}
		if !s.serverRequests.put(message, encodedSize) {
			s.failReader(&Error{Reason: "Codex app-server server-request queue exceeds bounds"})
			return false
		}
		return true
	}
	if hasID && requestID != nil {
		identifier, ok := responseID(requestID)
		if !ok {
			s.failReader(&Error{Reason: "Codex app-server response ID is not an integer"})
			return false
		}
		s.pendingMu.Lock()
		response := s.pending[identifier]
		s.pendingMu.Unlock()
		if response == nil {
			s.failReader(&Error{Reason: "Codex app-server response ID does not match a request"})
			return false
		}
		if !nonBlockingSend(response, message) {
			s.failReader(&Error{Reason: "Codex app-server sent a duplicate response"})
			return false
		}
		return true
	}
	if !s.notifications.put(message, encodedSize) {
		s.failReader(&Error{Reason: "Codex app-server notification queue exceeds bounds"})
		return false
	}
	return true
}

func (s *CodexAppServer) serverRequestLoop() {
	s.stateMu.Lock()
	serverDone := s.serverDone
	s.stateMu.Unlock()
	defer func() {
		if serverDone != nil {
			close(serverDone)
		}
	}()
	for {
		value, err := s.serverRequests.get(context.Background())
		if err != nil || value == appQueueClosed {
			return
		}
		message, ok := value.(map[string]any)
		if !ok {
			s.failReader(&Error{Reason: "Codex app-server server request is not an object"})
			return
		}
		s.handlerMu.Lock()
		handler := s.serverHandler
		s.handlerMu.Unlock()
		if handler == nil {
			if err := s.RejectServerRequest(message); err != nil {
				s.stateMu.Lock()
				closing := s.closing
				s.stateMu.Unlock()
				if !closing {
					s.failReader(asAppServerError(err))
				}
				return
			}
			continue
		}
		func() {
			s.callbackDepth.Add(1)
			defer func() {
				s.callbackDepth.Add(-1)
				_ = recover()
			}()
			handler(message)
		}()
	}
}

func (s *CodexAppServer) failReader(err *Error) {
	if err == nil {
		return
	}
	s.stateMu.Lock()
	if s.readerError != nil || s.closing {
		s.stateMu.Unlock()
		return
	}
	s.readerError = err
	s.stateMu.Unlock()

	s.pendingMu.Lock()
	for _, response := range s.pending {
		nonBlockingSend(response, err)
	}
	s.pendingMu.Unlock()
	s.notifications.replace(err, 0)
}

func (s *CodexAppServer) unexpectedExitError() *Error {
	s.stateMu.Lock()
	processErr := s.processErr
	s.stateMu.Unlock()
	status := "unknown"
	if processErr == nil {
		status = "0"
	} else {
		var exitError *exec.ExitError
		if errors.As(processErr, &exitError) && exitError.ExitCode() >= 0 {
			status = strconv.Itoa(exitError.ExitCode())
		} else {
			status = processErr.Error()
		}
	}
	diagnostics := s.stderrCapture.text()
	reason := "Codex app-server exited unexpectedly with status " + status
	if diagnostics != "" {
		reason += ": " + diagnostics
	}
	return &Error{Reason: reason}
}

func readJSONLine(reader *bufio.Reader) ([]byte, error) {
	line := make([]byte, 0, 4096)
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > maxAppServerMessageSize-len(line) {
			return nil, &Error{Reason: "Codex app-server message exceeds size limit"}
		}
		line = append(line, fragment...)
		if err == nil {
			return line, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(line) != 0 {
			return line, nil
		}
		return nil, err
	}
}

func responseID(value any) (uint64, bool) {
	switch value := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseUint(value.String(), 10, 64)
		return parsed, err == nil
	case uint64:
		return value, true
	case uint:
		return uint64(value), true
	case int:
		if value < 0 {
			return 0, true
		}
		return uint64(value), true
	case int64:
		if value < 0 {
			return 0, true
		}
		return uint64(value), true
	default:
		return 0, false
	}
}

func asAppServerError(err error) *Error {
	if appError, ok := err.(*Error); ok {
		return appError
	}
	return &Error{Reason: "failed to read Codex app-server output", Err: err}
}

// ObjectValue validates an app-server JSON value as an object.
func ObjectValue(value any, description string) (map[string]any, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, &Error{Reason: description + " is not an object"}
	}
	return object, nil
}

func writeAll(writer io.Writer, encoded []byte) (int, error) {
	total := 0
	for total < len(encoded) {
		count, err := writer.Write(encoded[total:])
		total += count
		if err != nil {
			return total, err
		}
		if count == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

func signalProcessGroup(process *exec.Cmd, signalValue syscall.Signal) error {
	if process == nil || process.Process == nil {
		return nil
	}
	if err := syscall.Kill(-process.Process.Pid, signalValue); err == nil {
		return nil
	} else if !errors.Is(err, syscall.ESRCH) {
		if processErr := process.Process.Signal(signalValue); processErr != nil && !errors.Is(processErr, os.ErrProcessDone) {
			return processErr
		}
		return nil
	}
	return nil
}

func waitChannelUntil(done <-chan struct{}, deadline time.Time) bool {
	if done == nil {
		return true
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func waitProcessUntil(ctx context.Context, done <-chan struct{}, deadline time.Time) bool {
	if done == nil {
		return true
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		select {
		case <-done:
			return true
		case <-ctx.Done():
			return false
		default:
			return false
		}
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

func setEnvironment(environment []string, name, value string) []string {
	return append(removeEnvironment(environment, name), name+"="+value)
}

func removeEnvironment(environment []string, name string) []string {
	prefix := name + "="
	result := environment[:0]
	for _, value := range environment {
		if !strings.HasPrefix(value, prefix) {
			result = append(result, value)
		}
	}
	return result
}

type boundedOutput struct {
	mu   sync.Mutex
	data []byte
}

func (output *boundedOutput) reset() {
	output.mu.Lock()
	output.data = nil
	output.mu.Unlock()
}

func (output *boundedOutput) Write(data []byte) (int, error) {
	output.mu.Lock()
	remaining := MaxErrorOutput - len(output.data)
	if remaining > 0 {
		if remaining > len(data) {
			remaining = len(data)
		}
		output.data = append(output.data, data[:remaining]...)
	}
	output.mu.Unlock()
	return len(data), nil
}

func (output *boundedOutput) text() string {
	output.mu.Lock()
	defer output.mu.Unlock()
	return strings.TrimSpace(string(output.data))
}

const appQueueClosed = "__codexapp_closed__"

// appMessageQueue preserves message order while bounding both the number of
// queued values and their encoded byte size. A one-slot wake channel avoids a
// goroutine per waiter.
type appMessageQueue struct {
	mu          sync.Mutex
	values      []queuedMessage
	queuedBytes int
	wake        chan struct{}
}

type queuedMessage struct {
	value any
	bytes int
}

func newAppMessageQueue() *appMessageQueue {
	return &appMessageQueue{wake: make(chan struct{}, 1)}
}

func (queue *appMessageQueue) put(value any, encodedSize int) bool {
	if encodedSize < 0 || encodedSize > maxAppServerQueueBytes {
		return false
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if len(queue.values) >= maxAppServerQueueMessages || queue.queuedBytes > maxAppServerQueueBytes-encodedSize {
		return false
	}
	queue.values = append(queue.values, queuedMessage{value: value, bytes: encodedSize})
	queue.queuedBytes += encodedSize
	queue.signal()
	return true

}

// replace discards queued values so a terminal reader error can always be
// observed without violating the queue bound.
func (queue *appMessageQueue) replace(value any, encodedSize int) {
	queue.mu.Lock()
	queue.values = []queuedMessage{{value: value, bytes: encodedSize}}
	queue.queuedBytes = encodedSize
	queue.mu.Unlock()
	queue.signal()
}

// forcePut reserves a terminal marker, dropping the oldest values if the
// bounded queue is full.
func (queue *appMessageQueue) forcePut(value any, encodedSize int) {
	if encodedSize < 0 || encodedSize > maxAppServerQueueBytes {
		return
	}
	queue.mu.Lock()
	for len(queue.values) >= maxAppServerQueueMessages || queue.queuedBytes > maxAppServerQueueBytes-encodedSize {
		if len(queue.values) == 0 {
			break
		}
		queue.queuedBytes -= queue.values[0].bytes
		queue.values[0] = queuedMessage{}
		queue.values = queue.values[1:]
	}
	queue.values = append(queue.values, queuedMessage{value: value, bytes: encodedSize})
	queue.queuedBytes += encodedSize
	queue.mu.Unlock()
	queue.signal()
}

func (queue *appMessageQueue) signal() {
	select {
	case queue.wake <- struct{}{}:
	default:
	}
}

func (queue *appMessageQueue) get(ctx context.Context) (any, error) {
	for {
		queue.mu.Lock()
		if len(queue.values) != 0 {
			value := queue.values[0].value
			queue.queuedBytes -= queue.values[0].bytes
			queue.values[0] = queuedMessage{}
			queue.values = queue.values[1:]
			queue.mu.Unlock()
			return value, nil
		}
		queue.mu.Unlock()
		select {
		case <-queue.wake:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func nonBlockingSend(channel chan any, value any) bool {
	select {
	case channel <- value:
		return true
	default:
		return false
	}
}
