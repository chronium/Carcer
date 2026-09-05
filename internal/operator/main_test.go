package operator

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

const (
	operatorHelperEnvironment = "CODEXOS_GO_OPERATOR_HELPER"
	operatorHelperMode        = "CODEXOS_GO_OPERATOR_HELPER_MODE"
	operatorHelperReady       = "CODEXOS_GO_OPERATOR_HELPER_READY"
	operatorHelperRecord      = "CODEXOS_GO_OPERATOR_HELPER_RECORD"
	operatorHelperSentinel    = "CODEXOS_GO_OPERATOR_SUITE_SENTINEL"
)

func TestMain(tests *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "--bootstrap-operator-worker" {
		os.Exit(bootstrapOperatorWorker(os.Args[2]))
	}
	if os.Getenv(operatorHelperEnvironment) == "1" {
		runOperatorFakeAppServer()
		os.Exit(0)
	}
	os.Exit(tests.Run())
}

func runOperatorFakeAppServer() {
	mode := os.Getenv(operatorHelperMode)
	if mode == "probe" {
		writeOperatorRecord(map[string]any{"mode": mode, "pid": os.Getpid()})
		return
	}
	if mode != "pause" && mode != "admission-pause" && mode != "stuck-interrupt" && mode != "finish" && mode != "interview" && mode != "interview-hold" {
		os.Exit(30)
	}
	decoder := json.NewDecoder(bufio.NewReader(os.Stdin))
	decoder.UseNumber()
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	messages := make([]map[string]any, 0, 16)
	read := func() map[string]any {
		var message map[string]any
		if err := decoder.Decode(&message); err != nil {
			os.Exit(31)
		}
		messages = append(messages, message)
		return message
	}
	send := func(message map[string]any) {
		if err := encoder.Encode(message); err != nil {
			os.Exit(32)
		}
	}
	expect := func(method string) map[string]any {
		message := read()
		if message["method"] != method || message["id"] == nil {
			os.Exit(33)
		}
		return message
	}
	respond := func(request map[string]any, result any) {
		send(map[string]any{"id": request["id"], "result": result})
	}

	initialize := expect("initialize")
	respond(initialize, map[string]any{"userAgent": "fake-operator"})
	initialized := read()
	if initialized["method"] != "initialized" || initialized["id"] != nil {
		os.Exit(34)
	}
	account := expect("account/read")
	respond(account, map[string]any{"account": map[string]any{"type": "chatgpt"}})
	models := expect("model/list")
	respond(models, map[string]any{"data": []any{map[string]any{
		"model":                     "gpt-6-astra",
		"supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "high"}},
		"serviceTiers":              []any{map[string]any{"id": "priority", "name": "Priority"}},
	}}, "nextCursor": nil})
	thread := expect("thread/start")
	threadID := fmt.Sprintf("operator-thread-%d", os.Getpid())
	respond(thread, map[string]any{
		"thread": map[string]any{"id": threadID, "ephemeral": true},
		"model":  "gpt-6-astra", "reasoningEffort": "high", "serviceTier": "priority",
		"activePermissionProfile": map[string]any{"id": "codexos-implementor"},
		"sandbox":                 map[string]any{"type": "workspace-write", "networkAccess": false},
	})

	completeTurn := func(index int, text string) {
		turn := expect("turn/start")
		turnID := fmt.Sprintf("operator-turn-%d-%d", os.Getpid(), index)
		respond(turn, map[string]any{"turn": map[string]any{"id": turnID}})
		item := map[string]any{"id": fmt.Sprintf("message-%d", index), "type": "agentMessage", "text": text}
		send(map[string]any{"method": "item/completed", "params": map[string]any{
			"threadId": threadID, "turnId": turnID, "item": item,
		}})
		send(map[string]any{"method": "turn/completed", "params": map[string]any{
			"threadId": threadID,
			"turn":     map[string]any{"id": turnID, "items": []any{item}, "status": "completed"},
		}})
	}

	if mode == "admission-pause" {
		planning := expect("turn/start")
		planningID := fmt.Sprintf("operator-turn-%d-0", os.Getpid())
		respond(planning, map[string]any{"turn": map[string]any{"id": planningID}})
		writeOperatorReady()
		interrupt := expect("turn/interrupt")
		respond(interrupt, map[string]any{})
		send(map[string]any{"method": "turn/completed", "params": map[string]any{
			"threadId": threadID,
			"turn":     map[string]any{"id": planningID, "items": []any{}, "status": "interrupted"},
		}})
		writeOperatorRecord(map[string]any{
			"mode": mode, "pid": os.Getpid(), "thread_id": threadID, "messages": messages,
		})
		select {}
	}

	completeTurn(0, "Planning complete.")
	implementation := expect("turn/start")
	implementationID := fmt.Sprintf("operator-turn-%d-1", os.Getpid())
	respond(implementation, map[string]any{"turn": map[string]any{"id": implementationID}})

	if mode == "pause" || mode == "stuck-interrupt" {
		writeOperatorReady()
		interrupt := expect("turn/interrupt")
		if mode == "stuck-interrupt" {
			select {}
		}
		respond(interrupt, map[string]any{})
		send(map[string]any{"method": "turn/completed", "params": map[string]any{
			"threadId": threadID,
			"turn":     map[string]any{"id": implementationID, "items": []any{}, "status": "interrupted"},
		}})
		completeTurn(2, "Continuation complete.")
		writeOperatorRecord(map[string]any{
			"mode": mode, "pid": os.Getpid(), "thread_id": threadID, "messages": messages,
		})
		select {}
	}

	callID := "finish-generation"
	send(map[string]any{"id": callID, "method": "item/tool/call", "params": map[string]any{
		"callId": callID, "threadId": threadID, "turnId": implementationID,
		"namespace": "codexos", "tool": "finish_generation",
		"arguments": map[string]any{"handoff": "next"},
	}})
	for {
		response := read()
		if response["id"] == callID {
			break
		}
	}
	item := map[string]any{"id": "message-1", "type": "agentMessage", "text": "Generation complete."}
	send(map[string]any{"method": "item/completed", "params": map[string]any{
		"threadId": threadID, "turnId": implementationID, "item": item,
	}})
	send(map[string]any{"method": "turn/completed", "params": map[string]any{
		"threadId": threadID,
		"turn":     map[string]any{"id": implementationID, "items": []any{item}, "status": "completed"},
	}})
	writeOperatorReady()
	writeOperatorRecord(map[string]any{
		"mode": mode, "pid": os.Getpid(), "thread_id": threadID, "messages": messages,
	})
	if mode == "interview" {
		completeTurn(2, "Retrospective answer.")
		writeOperatorRecord(map[string]any{
			"mode": mode, "pid": os.Getpid(), "thread_id": threadID, "messages": messages,
		})
	} else if mode == "interview-hold" {
		interview := expect("turn/start")
		interviewID := fmt.Sprintf("operator-turn-%d-2", os.Getpid())
		respond(interview, map[string]any{"turn": map[string]any{"id": interviewID}})
		writeOperatorReady()
		send(map[string]any{"method": "item/reasoning/summaryTextDelta", "params": map[string]any{
			"threadId": threadID, "turnId": interviewID, "itemId": "interview-reasoning", "summaryIndex": 0, "delta": "Partial retrospective.",
		}})
		interrupt := expect("turn/interrupt")
		respond(interrupt, map[string]any{})
		send(map[string]any{"method": "turn/completed", "params": map[string]any{
			"threadId": threadID,
			"turn":     map[string]any{"id": interviewID, "items": []any{}, "status": "interrupted"},
		}})
		writeOperatorRecord(map[string]any{
			"mode": mode, "pid": os.Getpid(), "thread_id": threadID, "messages": messages,
		})
	}
	select {}
}

func writeOperatorReady() {
	if path := os.Getenv(operatorHelperReady); path != "" {
		if err := os.WriteFile(path, []byte("ready"), 0o600); err != nil {
			os.Exit(35)
		}
	}
}

func writeOperatorRecord(value map[string]any) {
	path := os.Getenv(operatorHelperRecord)
	if path == "" {
		return
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		os.Exit(36)
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
		os.Exit(37)
	}
	if err := os.Rename(temporary, path); err != nil {
		os.Exit(38)
	}
}
