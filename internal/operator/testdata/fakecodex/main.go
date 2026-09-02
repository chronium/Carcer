// Command fakecodex is a disposable stdio Codex app-server peer for live
// operator acceptance tests. It is built as its own executable so the test
// never re-enters a Go test binary through os.Args[0].
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	model                             = "gpt-5.6-sol"
	serviceTier                       = "priority"
	planningPermissions               = "codexos-planning"
	implementationProfile             = "codexos-implementor"
	ordinaryToolHandoff               = "The disposable generation completed its validated build."
	processRecordDirectoryEnvironment = "CODEXOS_DISPOSABLE_PROCESS_RECORDS"
)

type peer struct {
	decoder *json.Decoder
	encoder *json.Encoder
}

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if err := recordProcess("codex"); err != nil {
		return err
	}
	server := &peer{
		decoder: json.NewDecoder(bufio.NewReader(os.Stdin)),
		encoder: json.NewEncoder(os.Stdout),
	}
	server.decoder.UseNumber()
	server.encoder.SetEscapeHTML(false)

	initialize, err := server.expectRequest("initialize")
	if err != nil {
		return err
	}
	if err := server.respond(initialize, map[string]any{
		"userAgent": "fake-codex",
	}); err != nil {
		return err
	}
	if err := server.expectNotification("initialized"); err != nil {
		return err
	}

	account, err := server.expectRequest("account/read")
	if err != nil {
		return err
	}
	if err := server.respond(account, map[string]any{
		"account": map[string]any{"type": "chatgpt"},
	}); err != nil {
		return err
	}

	models, err := server.expectRequest("model/list")
	if err != nil {
		return err
	}
	if err := server.respond(models, map[string]any{
		"data": []any{map[string]any{
			"model":                     model,
			"supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "high"}},
			"serviceTiers":              []any{map[string]any{"id": serviceTier, "name": "Priority"}},
		}},
		"nextCursor": nil,
	}); err != nil {
		return err
	}

	threadStart, err := server.expectRequest("thread/start")
	if err != nil {
		return err
	}
	if err := validateThreadStart(threadStart); err != nil {
		return err
	}
	threadID := fmt.Sprintf("fakecodex-thread-%d", os.Getpid())
	if err := server.respond(threadStart, map[string]any{
		"thread":                  map[string]any{"id": threadID, "ephemeral": true},
		"model":                   model,
		"serviceTier":             serviceTier,
		"activePermissionProfile": map[string]any{"id": implementationProfile},
		"sandbox":                 map[string]any{"type": "workspace-write", "networkAccess": false},
	}); err != nil {
		return err
	}

	planning, err := server.expectRequest("turn/start")
	if err != nil {
		return err
	}
	if err := validateTurnStart(planning, threadID, planningPermissions, true); err != nil {
		return err
	}
	planningID := fmt.Sprintf("fakecodex-turn-%d-planning", os.Getpid())
	if err := server.respond(planning, map[string]any{"turn": map[string]any{"id": planningID}}); err != nil {
		return err
	}
	if err := server.completeTurn(threadID, planningID, "Planning complete."); err != nil {
		return err
	}

	implementation, err := server.expectRequest("turn/start")
	if err != nil {
		return err
	}
	if err := validateTurnStart(implementation, threadID, implementationProfile, false); err != nil {
		return err
	}
	implementationID := fmt.Sprintf("fakecodex-turn-%d-implementation", os.Getpid())
	if err := server.respond(implementation, map[string]any{"turn": map[string]any{"id": implementationID}}); err != nil {
		return err
	}

	interrupted, err := server.callTool(threadID, implementationID, "read", map[string]any{
		"path": "seed/kernel.c", "offset": 0, "length": 1,
	})
	if err != nil {
		return err
	}
	if interrupted {
		return server.waitForEOF()
	}
	interrupted, err = server.callTool(threadID, implementationID, "build", map[string]any{})
	if err != nil {
		return err
	}
	if interrupted {
		return server.waitForEOF()
	}
	interrupted, err = server.callTool(threadID, implementationID, "finish_generation", map[string]any{
		"handoff": ordinaryToolHandoff,
	})
	if err != nil {
		return err
	}
	if interrupted {
		return server.waitForEOF()
	}
	if err := server.completeTurn(threadID, implementationID, "Generation complete."); err != nil {
		return err
	}

	// Keep the app-server process alive after the terminal notification. A
	// retained generation owns this exact process until the operator closes
	// the session; EOF is the normal disposable shutdown path.
	return server.waitForEOF()
}

func recordProcess(kind string) error {
	directory := os.Getenv(processRecordDirectoryEnvironment)
	if directory == "" {
		return nil
	}
	path := filepath.Join(directory, fmt.Sprintf("%s-%d.pid", kind, os.Getpid()))
	return os.WriteFile(path, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600)
}

func (p *peer) read() (map[string]any, error) {
	var message map[string]any
	if err := p.decoder.Decode(&message); err != nil {
		return nil, err
	}
	if message == nil {
		return nil, errors.New("fake Codex app-server received a null message")
	}
	return message, nil
}

func (p *peer) send(message map[string]any) error {
	return p.encoder.Encode(message)
}

func (p *peer) expectRequest(method string) (map[string]any, error) {
	message, err := p.read()
	if err != nil {
		return nil, err
	}
	if message["method"] != method || !hasRequestID(message) {
		return nil, fmt.Errorf("fake Codex app-server expected %s request, got %#v", method, message)
	}
	return message, nil
}

func (p *peer) expectNotification(method string) error {
	message, err := p.read()
	if err != nil {
		return err
	}
	if message["method"] != method || hasRequestID(message) {
		return fmt.Errorf("fake Codex app-server expected %s notification, got %#v", method, message)
	}
	return nil
}

func (p *peer) respond(request map[string]any, result any) error {
	return p.send(map[string]any{"id": request["id"], "result": result})
}

func (p *peer) completeTurn(threadID, turnID, text string) error {
	item := map[string]any{
		"id": fmt.Sprintf("%s-message", turnID), "type": "agentMessage", "text": text,
	}
	if err := p.send(map[string]any{
		"method": "item/completed",
		"params": map[string]any{"threadId": threadID, "turnId": turnID, "item": item},
	}); err != nil {
		return err
	}
	return p.send(map[string]any{
		"method": "turn/completed",
		"params": map[string]any{
			"threadId": threadID,
			"turn":     map[string]any{"id": turnID, "items": []any{item}, "status": "completed"},
		},
	})
}

// callTool emits one app-server initiated dynamic-tool request and does not
// issue the next request until the matching response has arrived. An
// interrupt is handled in the same way as the real app-server: acknowledge
// it, publish the terminal interrupted notification, then drain the in-flight
// tool response before returning to the caller.
func (p *peer) callTool(threadID, turnID, tool string, arguments map[string]any) (bool, error) {
	callID := "fakecodex-call-" + tool
	if err := p.send(map[string]any{
		"id":     callID,
		"method": "item/tool/call",
		"params": map[string]any{
			"callId": callID, "threadId": threadID, "turnId": turnID,
			"namespace": "codexos", "tool": tool, "arguments": arguments,
		},
	}); err != nil {
		return false, err
	}
	interrupted := false
	for {
		message, err := p.read()
		if err != nil {
			return interrupted, err
		}
		if message["method"] == "turn/interrupt" && hasRequestID(message) {
			if err := p.respond(message, map[string]any{}); err != nil {
				return interrupted, err
			}
			if !interrupted {
				if err := p.send(map[string]any{
					"method": "turn/completed",
					"params": map[string]any{
						"threadId": threadID,
						"turn":     map[string]any{"id": turnID, "items": []any{}, "status": "interrupted"},
					},
				}); err != nil {
					return interrupted, err
				}
				interrupted = true
			}
			continue
		}
		if message["id"] != callID {
			return interrupted, fmt.Errorf("fake Codex app-server expected %s tool response, got %#v", callID, message)
		}
		result, ok := message["result"].(map[string]any)
		if !ok || result["success"] != true {
			return interrupted, fmt.Errorf("fake Codex app-server tool response %s failed: %#v", callID, message)
		}
		items, ok := result["contentItems"].([]any)
		if !ok || len(items) != 1 {
			return interrupted, fmt.Errorf("fake Codex app-server tool response %s has malformed content: %#v", callID, message)
		}
		item, ok := items[0].(map[string]any)
		text, textOK := item["text"].(string)
		if !ok || !textOK {
			return interrupted, fmt.Errorf("fake Codex app-server tool response %s has no text result: %#v", callID, message)
		}
		var toolResult struct {
			Output string `json:"output"`
			Status uint32 `json:"status"`
		}
		if err := json.Unmarshal([]byte(text), &toolResult); err != nil {
			return interrupted, fmt.Errorf("fake Codex app-server tool %s returned malformed result %q: %w", tool, text, err)
		}
		if toolResult.Status != 0 {
			return interrupted, fmt.Errorf("fake Codex app-server tool %s returned status %d: %s", tool, toolResult.Status, toolResult.Output)
		}
		return interrupted, nil
	}
}

func (p *peer) waitForEOF() error {
	for {
		if _, err := p.read(); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func validateThreadStart(request map[string]any) error {
	params, ok := request["params"].(map[string]any)
	if !ok {
		return errors.New("fake Codex app-server thread/start params are not an object")
	}
	if params["allowProviderModelFallback"] != false ||
		params["approvalPolicy"] != "never" ||
		params["approvalsReviewer"] != "user" ||
		params["ephemeral"] != true ||
		params["model"] != model ||
		params["permissions"] != implementationProfile ||
		params["serviceTier"] != serviceTier {
		return fmt.Errorf("fake Codex app-server received unexpected thread/start policy: %#v", params)
	}
	if cwd, ok := params["cwd"].(string); !ok || cwd == "" {
		return errors.New("fake Codex app-server thread/start has no workspace")
	}
	return nil
}

func validateTurnStart(request map[string]any, threadID, permissions string, planning bool) error {
	params, ok := request["params"].(map[string]any)
	if !ok {
		return errors.New("fake Codex app-server turn/start params are not an object")
	}
	if params["threadId"] != threadID ||
		params["approvalPolicy"] != "never" ||
		params["approvalsReviewer"] != "user" ||
		params["effort"] != "high" ||
		params["model"] != model ||
		params["permissions"] != permissions ||
		params["serviceTier"] != serviceTier ||
		params["summary"] != "auto" {
		return fmt.Errorf("fake Codex app-server received unexpected turn/start policy: %#v", params)
	}
	if planning {
		roots, ok := params["runtimeWorkspaceRoots"].([]any)
		if !ok || len(roots) != 0 {
			return fmt.Errorf("fake Codex app-server planning turn has unexpected workspace roots: %#v", params["runtimeWorkspaceRoots"])
		}
	} else if _, present := params["runtimeWorkspaceRoots"]; present {
		return fmt.Errorf("fake Codex app-server implementation turn unexpectedly has workspace roots: %#v", params["runtimeWorkspaceRoots"])
	}
	return nil
}

func hasRequestID(message map[string]any) bool {
	value, present := message["id"]
	return present && value != nil
}
