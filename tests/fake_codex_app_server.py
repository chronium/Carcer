#!/usr/bin/env python3
"""Tiny stdio peer used to test the concrete Codex app-server client."""

from __future__ import annotations

import json
import os
import sys
import time
from pathlib import Path


def read_message() -> dict[str, object]:
    line = sys.stdin.readline()
    if not line:
        raise SystemExit("client closed stdin")
    value = json.loads(line)
    if not isinstance(value, dict):
        raise SystemExit("client message is not an object")
    messages.append(value)
    return value


def send(message: dict[str, object]) -> None:
    sys.stdout.write(json.dumps(message, separators=(",", ":")) + "\n")
    sys.stdout.flush()


def expect_request(method: str) -> dict[str, object]:
    message = read_message()
    if message.get("method") != method or "id" not in message:
        raise SystemExit(f"expected {method}, got {message}")
    return message


def respond(request: dict[str, object], result: object) -> None:
    send({"id": request["id"], "result": result})


def request_client(method: str, params: object) -> dict[str, object]:
    request_id = f"server-{len(server_responses) + 1}"
    send({"id": request_id, "method": method, "params": params})
    response = read_message()
    if response.get("id") != request_id:
        raise SystemExit(f"wrong client response ID: {response}")
    server_responses.append(response)
    return response


scenario_path = Path(os.environ["CODEXOS_FAKE_SCENARIO"])
record_path = Path(os.environ["CODEXOS_FAKE_RECORD"])
scenario = json.loads(scenario_path.read_text(encoding="utf-8"))
messages: list[dict[str, object]] = []
server_responses: list[dict[str, object]] = []
pid = os.getpid()
record_path.write_text(json.dumps({"pid": pid}), encoding="utf-8")

if scenario.get("failure") == "malformed_json":
    sys.stdout.write("not JSON\n")
    sys.stdout.flush()
    while True:
        time.sleep(1)

initialize = expect_request("initialize")
respond(
    initialize,
    {
        "userAgent": "fake-codex/0.150.1",
        "codexHome": os.environ.get("CODEX_HOME"),
        "platformFamily": "unix",
        "platformOs": "linux",
    },
)
initialized = read_message()
if initialized.get("method") != "initialized" or "id" in initialized:
    raise SystemExit(f"expected initialized notification, got {initialized}")

account = expect_request("account/read")
respond(
    account,
    {
        "account": {
            "type": "chatgpt",
            "email": None,
            "planType": "pro",
        },
        "requiresOpenaiAuth": True,
    },
)
models = expect_request("model/list")
respond(
    models,
    {
        "data": [
            {
                "id": "gpt-5.6-sol",
                "model": "gpt-5.6-sol",
                "displayName": "GPT-5.6-Sol",
                "description": "fake",
                "hidden": False,
                "isDefault": True,
                "defaultReasoningEffort": "low",
                "supportedReasoningEfforts": [
                    {"reasoningEffort": "high", "description": "fake"}
                ],
            }
        ],
        "nextCursor": None,
    },
)

thread_id = f"thread-{pid}"
thread_start = expect_request("thread/start")
thread_params = thread_start["params"]
respond(
    thread_start,
    {
        "thread": {
            "id": thread_id,
            "ephemeral": True,
            "cwd": thread_params["cwd"],
            "turns": [],
        },
        "model": "gpt-5.6-sol",
        "modelProvider": "openai",
        "cwd": thread_params["cwd"],
        "runtimeWorkspaceRoots": thread_params["runtimeWorkspaceRoots"],
        "instructionSources": [],
        "approvalPolicy": "never",
        "approvalsReviewer": "user",
        "sandbox": {"type": "readOnly", "networkAccess": False},
        "activePermissionProfile": {"id": "codexos-implementor"},
        "reasoningEffort": None,
    },
)

turn_id = f"turn-{pid}"
turn_start = expect_request("turn/start")
respond(
    turn_start,
    {"turn": {"id": turn_id, "items": [], "status": "inProgress"}},
)

for method in scenario.get("server_requests", []):
    request_client(method, {})

tool_results: list[dict[str, object]] = []
for index, call in enumerate(scenario.get("tool_calls", []), 1):
    response = request_client(
        "item/tool/call",
        {
            "threadId": thread_id,
            "turnId": turn_id,
            "callId": f"call-{index}",
            "namespace": call.get("namespace", "codexos"),
            "tool": call["tool"],
            "arguments": call["arguments"],
        },
    )
    tool_results.append(response)

record_path.write_text(
    json.dumps(
        {
            "pid": pid,
            "thread_id": thread_id,
            "codex_home": os.environ["CODEX_HOME"],
            "config": (
                Path(os.environ["CODEX_HOME"]) / "config.toml"
            ).read_text(encoding="utf-8"),
            "messages": messages,
            "server_responses": server_responses,
            "tool_results": tool_results,
        }
    ),
    encoding="utf-8",
)

final_message = scenario.get("final_message", "Fake Codex turn complete.")
item = {"id": "message-1", "type": "agentMessage", "text": final_message}
completed_turn = {
    "id": turn_id,
    "items": [item],
    "status": scenario.get("turn_status", "completed"),
}
if "turn_error" in scenario:
    completed_turn["error"] = scenario["turn_error"]
send(
    {
        "method": "item/completed",
        "params": {"threadId": thread_id, "turnId": turn_id, "item": item},
    }
)
send(
    {
        "method": "turn/completed",
        "params": {
            "threadId": thread_id,
            "turn": completed_turn,
        },
    }
)

# The worker must terminate its fresh app-server after the turn completes.
while True:
    time.sleep(1)
