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
    while True:
        response = read_message()
        if response.get("method") == "turn/interrupt" and "id" in response:
            respond(response, {})
            interrupted_turns.add(current_turn_id)
            send(
                {
                    "method": "turn/completed",
                    "params": {
                        "threadId": thread_id,
                        "turn": {
                            "id": current_turn_id,
                            "items": [],
                            "status": "interrupted",
                        },
                    },
                }
            )
            continue
        if response.get("id") != request_id:
            raise SystemExit(f"wrong client response ID: {response}")
        break
    server_responses.append(response)
    return response


executable_root = Path(sys.argv[0]).resolve().parent
scenario_path = Path(
    os.environ.get(
        "CODEXOS_FAKE_SCENARIO",
        executable_root / "scenario.json",
    )
)
record_path = Path(
    os.environ.get(
        "CODEXOS_FAKE_RECORD",
        executable_root / "record.json",
    )
)
scenario = json.loads(scenario_path.read_text(encoding="utf-8"))
messages: list[dict[str, object]] = []
server_responses: list[dict[str, object]] = []
interrupted_turns: set[str] = set()
current_turn_id = ""
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
model = scenario.get("model", "gpt-5.6-sol")
permission_profile = scenario.get(
    "permission_profile",
    "codexos-implementor",
)
models = expect_request("model/list")
respond(
    models,
    {
        "data": [
            {
                "id": model,
                "model": model,
                "displayName": model,
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
        "model": model,
        "modelProvider": "openai",
        "cwd": thread_params["cwd"],
        "runtimeWorkspaceRoots": thread_params["runtimeWorkspaceRoots"],
        "instructionSources": [],
        "approvalPolicy": "never",
        "approvalsReviewer": "user",
        "sandbox": {"type": "readOnly", "networkAccess": False},
        "activePermissionProfile": {"id": permission_profile},
        "reasoningEffort": None,
    },
)

tool_results: list[dict[str, object]] = []
dead_process_checks: list[list[dict[str, object]]] = []
turn_ids: list[str] = []


def save_record() -> None:
    value = json.dumps(
        {
            "pid": pid,
            "thread_id": thread_id,
            "turn_ids": turn_ids,
            "codex_home": os.environ["CODEX_HOME"],
            "config": (
                Path(os.environ["CODEX_HOME"]) / "config.toml"
            ).read_text(encoding="utf-8"),
            "messages": messages,
            "server_responses": server_responses,
            "tool_results": tool_results,
            "dead_process_checks": dead_process_checks,
        }
    )
    temporary_record = record_path.with_suffix(".tmp")
    temporary_record.write_text(value, encoding="utf-8")
    temporary_record.replace(record_path)
    record_path.with_name(f"record-{pid}.json").write_text(
        value,
        encoding="utf-8",
    )


turns = scenario.get("turns")
if not isinstance(turns, list):
    turns = [scenario]

for turn_index, turn_scenario in enumerate(turns, 1):
    turn_id = f"turn-{pid}-{turn_index}"
    current_turn_id = turn_id
    turn_ids.append(turn_id)
    turn_start = expect_request("turn/start")
    respond(
        turn_start,
        {"turn": {"id": turn_id, "items": [], "status": "inProgress"}},
    )
    save_record()

    for method in turn_scenario.get("server_requests", []):
        request_client(method, {})

    for index, call in enumerate(turn_scenario.get("tool_calls", []), 1):
        call_params = {
            "threadId": call.get("thread_id", thread_id),
            "turnId": call.get("turn_id", turn_id),
            "callId": call.get("call_id", f"call-{turn_index}-{index}"),
            "namespace": call.get("namespace", "codexos"),
            "tool": call["tool"],
            "arguments": call["arguments"],
        }
        if call.get("omit_call_id"):
            del call_params["callId"]
        response = request_client("item/tool/call", call_params)
        tool_results.append(response)
        check_root = turn_scenario.get(
            "assert_dead_processes_in",
            scenario.get("assert_dead_processes_in"),
        )
        if isinstance(check_root, str):
            checks: list[dict[str, object]] = []
            for path in sorted(Path(check_root).glob("record-*.json")):
                checked_pid = json.loads(path.read_text(encoding="utf-8"))["pid"]
                try:
                    os.kill(checked_pid, 0)
                    dead = False
                except ProcessLookupError:
                    dead = True
                checks.append({"pid": checked_pid, "dead": dead})
            dead_process_checks.append(checks)

    token_usage = turn_scenario.get("token_usage")
    if isinstance(token_usage, dict):
        send(
            {
                "method": "thread/tokenUsage/updated",
                "params": {
                    "threadId": thread_id,
                    "turnId": turn_id,
                    "tokenUsage": {
                        "last": token_usage,
                        "total": token_usage,
                    },
                },
            }
        )

    if turn_id in interrupted_turns:
        save_record()
        continue
    if turn_scenario.get("hold_for_interrupt"):
        save_record()
        interrupt = expect_request("turn/interrupt")
        if turn_scenario.get("interrupt_response", True):
            respond(interrupt, {})
        save_record()
        if not turn_scenario.get("interrupt_terminal", True):
            while True:
                time.sleep(1)
        status = "interrupted"
    else:
        status = turn_scenario.get("turn_status", "completed")

    final_message = turn_scenario.get(
        "final_message",
        "Fake Codex turn complete.",
    )
    item = {
        "id": f"message-{turn_index}",
        "type": "agentMessage",
        "text": final_message,
    }
    completed_turn = {
        "id": turn_id,
        "items": [item],
        "status": status,
    }
    if "turn_error" in turn_scenario:
        completed_turn["error"] = turn_scenario["turn_error"]
    save_record()
    send(
        {
            "method": "item/completed",
            "params": {"threadId": thread_id, "turnId": turn_id, "item": item},
        }
    )
    send(
        {
            "method": "turn/completed",
            "params": {"threadId": thread_id, "turn": completed_turn},
        }
    )

# The worker must terminate its fresh app-server after the turn completes.
while True:
    time.sleep(1)
