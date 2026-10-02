"""Real programming-task probe for enabled OpenCodeFree models — runs ON hangzhou2-2.

The admin password arrives on stdin (one line) and never touches argv, disk or
output. A temporary access key is created for /v1 calls and always deleted.

Unlike the dimension matrix in ``capability_eval_remote.py`` this script does
not score one-shot answers: it drives a genuine coding-agent loop (list/read/
write/run tools, multi-turn tool results) against a seeded failing test suite
and verifies the final workspace with ``python3 -m unittest``. Success means
the model actually changed the code and made the tests pass, not that it
answered plausibly.

Modes (passed by ``remote_exec.py --arg MODE=...``):
    state      snapshot of catalog, candidates, pool gauge and gateway
    reprobe    force the detailed capability probe on enabled OCF models
    chat       non-streaming /v1/chat/completions tool loop
    stream     streaming /v1/chat/completions tool loop (agent clients stream)
    responses  /v1/responses tool loop (Codex path)
    all        chat + stream + responses for every enabled OCF model

TOOLSET is a second placeholder (--arg TOOLSET=standard) that adds the
bash/glob/grep/read quartet the OpenCode fingerprint shim injects on its own.
"""

from __future__ import annotations

import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

BASE = "http://127.0.0.1:3756"
MODE = "__MODE__"
RUN_TAG = "ocf-programming-probe"
MAX_TURNS = 8
REQUEST_TIMEOUT = 150
MAX_TOOL_OUTPUT = 4000
SEED_TASK = (
    "The Python project in the current working directory has a failing test suite. "
    "Find the bug in calc.py, fix it so that `python3 -m unittest` passes, and stop once the tests pass."
)

CALC_SOURCE = '''"""Small statistics helpers used by the sample project."""


def moving_average(values, window):
    """Return the moving average of the trailing `window` values.

    Element i averages values[0..i] when fewer than `window` samples exist and
    the last `window` values once the window is full.
    """
    result = []
    for index in range(len(values)):
        chunk = values[max(0, index - window):index + 1]
        result.append(sum(chunk) / len(chunk))
    return result
'''

TEST_SOURCE = '''import unittest

from calc import moving_average


class MovingAverageTest(unittest.TestCase):
    def test_full_window(self):
        self.assertEqual(moving_average([1, 2, 3, 4], 2), [1.0, 1.5, 2.5, 3.5])

    def test_partial_window(self):
        self.assertEqual(moving_average([2, 4], 3), [2.0, 3.0])

    def test_window_of_one(self):
        self.assertEqual(moving_average([5, 7, 9], 1), [5.0, 7.0, 9.0])

    def test_empty_input(self):
        self.assertEqual(moving_average([], 3), [])


if __name__ == "__main__":
    unittest.main()
'''

TOOLS = [
    {
        "type": "function",
        "function": {
            "name": "list_files",
            "description": "List the files in the current working directory.",
            "parameters": {"type": "object", "properties": {}, "additionalProperties": False},
        },
    },
    {
        "type": "function",
        "function": {
            "name": "read_file",
            "description": "Read a UTF-8 text file from the working directory.",
            "parameters": {
                "type": "object",
                "properties": {"path": {"type": "string", "description": "Relative file path."}},
                "required": ["path"],
                "additionalProperties": False,
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "write_file",
            "description": "Create or overwrite a UTF-8 text file in the working directory.",
            "parameters": {
                "type": "object",
                "properties": {
                    "path": {"type": "string"},
                    "content": {"type": "string"},
                },
                "required": ["path", "content"],
                "additionalProperties": False,
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "run_tests",
            "description": "Run `python3 -m unittest` in the working directory and return its output.",
            "parameters": {"type": "object", "properties": {}, "additionalProperties": False},
        },
    },
]

SYSTEM_PROMPT = (
    "You are a coding agent working in a small Python project. Use the provided tools to inspect "
    "files, edit code and run the tests. Do not guess file contents: read them first. Fix the bug "
    "in the source file rather than editing the tests. Reply with one short sentence when the tests pass."
)

# The OpenCodeFree gateway satisfies the upstream fingerprint check by appending
# no-op bash/glob/grep/read tools whenever the caller does not declare them
# (/opt/opencode-free-proxy/src/upstream.js ensureFingerprintTools). Real coding
# agents declare that quartet, so the injection is invisible to them; a caller
# with a custom tool set sees the model call tools it was never offered. TOOLSET
# =standard reproduces the real-agent shape.
TOOLSET = "__TOOLSET__"
FINGERPRINT_TOOLS = [
    {
        "type": "function",
        "function": {
            "name": name,
            "description": "Built-in %s tool provided by the OpenCode client." % name,
            "parameters": {"type": "object", "properties": {}},
        },
    }
    for name in ("bash", "glob", "grep", "read")
]
if TOOLSET == "standard":
    TOOLS.extend(FINGERPRINT_TOOLS)

admin_opener = None
access_key = {"id": None, "key": None}


def log(kind, **fields):
    fields["kind"] = kind
    print("R|" + json.dumps(fields, sort_keys=True, separators=(",", ":")), flush=True)


def call_admin(method, path, payload=None, timeout=60):
    data = None if payload is None else json.dumps(payload, separators=(",", ":")).encode()
    headers = {"Origin": BASE}
    if data is not None:
        headers["Content-Type"] = "application/json"
    request = urllib.request.Request(BASE + path, data=data, headers=headers, method=method)
    try:
        with admin_opener.open(request, timeout=timeout) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as error:
        return error.code, error.read()


def unwrap(body):
    try:
        value = json.loads(body)
    except Exception:  # noqa: BLE001
        return None
    if isinstance(value, dict) and "data" in value:
        return value["data"]
    return value


# --- workspace -------------------------------------------------------------


def seed_workspace():
    root = tempfile.mkdtemp(prefix="ocf-prog-")
    with open(os.path.join(root, "calc.py"), "w", encoding="utf-8") as handle:
        handle.write(CALC_SOURCE)
    with open(os.path.join(root, "test_calc.py"), "w", encoding="utf-8") as handle:
        handle.write(TEST_SOURCE)
    return root


def run_tests(root):
    try:
        completed = subprocess.run(
            [sys.executable, "-m", "unittest", "-v"],
            cwd=root,
            capture_output=True,
            text=True,
            timeout=60,
        )
    except subprocess.TimeoutExpired:
        return False, "timeout"
    tail = (completed.stdout + completed.stderr).strip().splitlines()[-12:]
    return completed.returncode == 0, "\n".join(tail)


def call_tool(root, name, arguments):
    try:
        args = arguments if isinstance(arguments, dict) else json.loads(arguments or "{}")
    except Exception:  # noqa: BLE001
        return "ERROR: arguments are not valid JSON."
    try:
        if name == "list_files":
            return "\n".join(sorted(os.listdir(root)))
        if name == "read_file":
            path = _resolve(root, str(args.get("path", "")))
            if not path:
                return "ERROR: path escapes the working directory."
            with open(path, encoding="utf-8") as handle:
                return handle.read()[:MAX_TOOL_OUTPUT]
        if name == "write_file":
            path = _resolve(root, str(args.get("path", "")))
            if not path:
                return "ERROR: path escapes the working directory."
            with open(path, "w", encoding="utf-8") as handle:
                handle.write(str(args.get("content", "")))
            return "OK wrote %s" % os.path.basename(path)
        if name == "run_tests":
            passed, output = run_tests(root)
            return ("PASS" if passed else "FAIL") + "\n" + output
    except Exception as error:  # noqa: BLE001
        return "ERROR: %s" % type(error).__name__
    return "ERROR: unknown tool %r" % name


def _resolve(root, path):
    candidate = os.path.normpath(os.path.join(root, path))
    if candidate != root and not candidate.startswith(root + os.sep):
        return None
    return candidate


# --- transport -------------------------------------------------------------


def post_json(path, payload, timeout=REQUEST_TIMEOUT, stream=False):
    request = urllib.request.Request(
        BASE + path,
        data=json.dumps(payload, separators=(",", ":")).encode(),
        headers={"Authorization": "Bearer " + access_key["key"], "Content-Type": "application/json"},
        method="POST",
    )
    record = {"stream": stream}
    started = time.monotonic()
    try:
        response = urllib.request.urlopen(request, timeout=timeout)
    except urllib.error.HTTPError as error:
        record["status"] = error.code
        record["total"] = round(time.monotonic() - started, 2)
        try:
            payload = json.loads(error.read(1 << 20))
            err = payload.get("error", {}) if isinstance(payload, dict) else {}
            record["error_code"] = err.get("code") or err.get("type")
            # The upstream body is the only place that says why a provider
            # refused; collapse whitespace and cap it so it stays readable.
            message = re.sub(r"\s+", " ", str(err.get("message") or "")).strip()
            if message:
                record["error_message"] = message[:200]
        except Exception:  # noqa: BLE001
            record["error_code"] = "unparseable"
        return record, None
    except Exception as error:  # noqa: BLE001
        record["status"] = 0
        record["total"] = round(time.monotonic() - started, 2)
        record["error_code"] = type(error).__name__
        return record, None
    record["status"] = response.status
    if not stream:
        body = json.loads(response.read(1 << 24))
        response.close()
        record["total"] = round(time.monotonic() - started, 2)
        return record, body
    return record, response


def iter_sse(response):
    for raw in response:
        line = raw.decode("utf-8", "replace").strip()
        if not line.startswith("data:"):
            continue
        data = line[5:].strip()
        if data == "[DONE]":
            yield {"done": True}
            return
        try:
            yield json.loads(data)
        except Exception:  # noqa: BLE001
            yield {"malformed": True}


# --- chat loop -------------------------------------------------------------


def chat_turn(model, messages, stream=False):
    payload = {
        "model": model,
        "messages": messages,
        "tools": TOOLS,
        "tool_choice": "auto",
        "max_tokens": 2048,
        "stream": stream,
    }
    record, response = post_json("/v1/chat/completions", payload, stream=stream)
    if response is None or record["status"] != 200:
        return record, None
    if not stream:
        choice = (response.get("choices") or [{}])[0] or {}
        message = choice.get("message") or {}
        usage = response.get("usage") or {}
        record["finish_reason"] = choice.get("finish_reason")
        record["content_chars"] = len(str(message.get("content") or ""))
        record["reasoning_chars"] = len(str(message.get("reasoning_content") or ""))
        record["completion_tokens"] = usage.get("completion_tokens")
        calls = []
        for call in message.get("tool_calls") or []:
            function = call.get("function") or {}
            calls.append({"name": function.get("name"), "arguments": function.get("arguments") or ""})
        record["tool_calls"] = calls
        return record, {"content": message.get("content") or "", "tool_calls": calls}

    content, calls, chunks, done, malformed, first_byte = [], [], 0, False, 0, None
    started = time.monotonic()
    for event in iter_sse(response):
        if "done" in event:
            done = True
            continue
        if "malformed" in event:
            malformed += 1
            continue
        chunks += 1
        if first_byte is None:
            first_byte = round(time.monotonic() - started, 2)
        for choice in event.get("choices") or []:
            delta = choice.get("delta") or {}
            if delta.get("content"):
                content.append(str(delta["content"]))
            for call in delta.get("tool_calls") or []:
                index = int(call.get("index") or 0)
                while len(calls) <= index:
                    calls.append({"name": "", "arguments": ""})
                function = call.get("function") or {}
                if function.get("name"):
                    calls[index]["name"] = str(function["name"])
                if function.get("arguments"):
                    calls[index]["arguments"] += str(function["arguments"])
        if event.get("usage"):
            record["completion_tokens"] = (event.get("usage") or {}).get("completion_tokens")
    response.close()
    record["total"] = round(time.monotonic() - started, 2)
    record["ttft"] = first_byte
    record["chunks"] = chunks
    record["done"] = done
    record["malformed_events"] = malformed
    record["content_chars"] = len("".join(content))
    record["tool_calls"] = calls
    return record, {"content": "".join(content), "tool_calls": calls}


def run_chat_loop(model, stream=False):
    root = seed_workspace()
    try:
        passed_before, _ = run_tests(root)
        messages = [
            {"role": "system", "content": SYSTEM_PROMPT},
            {"role": "user", "content": SEED_TASK},
        ]
        turns, tool_calls_total, invalid_args, finish = 0, 0, 0, "turn_budget"
        for turn in range(MAX_TURNS):
            record, message = chat_turn(model, messages, stream=stream)
            record.pop("stream", None)
            record["turn"] = turn + 1
            turns += 1
            calls = (message or {}).get("tool_calls") or []
            record["tool_call_count"] = len(calls)
            log("TURN", model=model, stream=stream, **record)
            if message is None:
                finish = "request_failed"
                break
            for call in calls:
                if not _is_json_object(call.get("arguments")):
                    invalid_args += 1
            tool_calls_total += len(calls)
            assistant = {"role": "assistant", "content": message["content"] or None}
            if calls:
                assistant["tool_calls"] = [
                    {
                        "id": "call_%d_%d" % (turn, index),
                        "type": "function",
                        "function": {"name": call["name"], "arguments": call["arguments"] or "{}"},
                    }
                    for index, call in enumerate(calls)
                ]
            messages.append(assistant)
            if not calls:
                finish = "final_answer"
                break
            for index, call in enumerate(calls):
                output = call_tool(root, call.get("name"), call.get("arguments"))
                messages.append(
                    {
                        "role": "tool",
                        "tool_call_id": "call_%d_%d" % (turn, index),
                        "content": output[:MAX_TOOL_OUTPUT],
                    }
                )
        passed_after, output = run_tests(root)
        with open(os.path.join(root, "calc.py"), encoding="utf-8") as handle:
            final_source = handle.read()
        return {
            "model": model,
            "transport": "stream" if stream else "chat",
            "turns": turns,
            "finish": finish,
            "tool_calls": tool_calls_total,
            "invalid_tool_args": invalid_args,
            "tests_passed_before": passed_before,
            "tests_passed_after": passed_after,
            "source_changed": final_source != CALC_SOURCE,
            "source_sha256": hashlib.sha256(final_source.encode()).hexdigest()[:16],
            "test_tail": output.splitlines()[-1][:160] if output else "",
            "solved": bool(passed_before is False and passed_after),
        }
    finally:
        shutil.rmtree(root, ignore_errors=True)


def _is_json_object(raw):
    try:
        return isinstance(json.loads(raw or ""), dict)
    except Exception:  # noqa: BLE001
        return False


# --- responses loop --------------------------------------------------------


def responses_turn(model, items, stream=False):
    payload = {
        "model": model,
        "input": items,
        "instructions": SYSTEM_PROMPT,
        "tools": [
            {
                "type": "function",
                "name": tool["function"]["name"],
                "description": tool["function"]["description"],
                "parameters": tool["function"]["parameters"],
            }
            for tool in TOOLS
        ],
        "tool_choice": "auto",
        "max_output_tokens": 2048,
    }
    record, body = post_json("/v1/responses", payload, timeout=REQUEST_TIMEOUT)
    if body is None or record["status"] != 200:
        return record, None
    output = body.get("output") or []
    calls, text = [], []
    for item in output:
        if item.get("type") == "function_call":
            calls.append({"name": item.get("name"), "call_id": item.get("call_id"), "arguments": item.get("arguments") or ""})
        if item.get("type") == "message":
            for part in item.get("content") or []:
                if part.get("type") in ("output_text", "text") and part.get("text"):
                    text.append(str(part["text"]))
    record["output_types"] = sorted({str(item.get("type")) for item in output})
    record["content_chars"] = len("".join(text))
    record["completion_tokens"] = (body.get("usage") or {}).get("output_tokens")
    record["tool_calls"] = calls
    return record, {"text": "".join(text), "calls": calls}


def run_responses_loop(model):
    root = seed_workspace()
    try:
        passed_before, _ = run_tests(root)
        items = [{"role": "user", "content": [{"type": "input_text", "text": SEED_TASK}]}]
        finish, turns, tool_calls_total, invalid_args = "turn_budget", 0, 0, 0
        for turn in range(MAX_TURNS):
            record, message = responses_turn(model, items)
            record["turn"] = turn + 1
            turns += 1
            calls = (message or {}).get("calls") or []
            record["tool_call_count"] = len(calls)
            log("TURN", model=model, transport="responses", **record)
            if message is None:
                finish = "request_failed"
                break
            for call in calls:
                if not _is_json_object(call.get("arguments")):
                    invalid_args += 1
            tool_calls_total += len(calls)
            for call in calls:
                if not call.get("call_id"):
                    call["call_id"] = "call_%d_%d" % (turn, len(items))
                items.append(
                    {
                        "type": "function_call",
                        "call_id": call["call_id"],
                        "name": call.get("name") or "",
                        "arguments": call.get("arguments") or "{}",
                    }
                )
            if not calls:
                finish = "final_answer"
                break
            for index, call in enumerate(calls):
                items.append(
                    {
                        "type": "function_call_output",
                        "call_id": call["call_id"],
                        "output": call_tool(root, call.get("name"), call.get("arguments"))[:MAX_TOOL_OUTPUT],
                    }
                )
        passed_after, output = run_tests(root)
        with open(os.path.join(root, "calc.py"), encoding="utf-8") as handle:
            final_source = handle.read()
        return {
            "model": model,
            "transport": "responses",
            "turns": turns,
            "finish": finish,
            "tool_calls": tool_calls_total,
            "invalid_tool_args": invalid_args,
            "tests_passed_before": passed_before,
            "tests_passed_after": passed_after,
            "source_changed": final_source != CALC_SOURCE,
            "source_sha256": hashlib.sha256(final_source.encode()).hexdigest()[:16],
            "test_tail": output.splitlines()[-1][:160] if output else "",
            "solved": bool(passed_before is False and passed_after),
        }
    finally:
        shutil.rmtree(root, ignore_errors=True)


# --- modes -----------------------------------------------------------------


def enabled_models(provider=None):
    status, body = call_admin("GET", "/admin/api/models")
    catalog = unwrap(body) if status == 200 else []
    models = [item for item in catalog if item.get("enabled") and item.get("kind") == "chat"]
    if provider:
        models = [item for item in models if item.get("provider") == provider]
    return sorted(models, key=lambda item: item.get("public_id") or "")


def report_state():
    status, body = call_admin("GET", "/admin/api/models")
    catalog = unwrap(body) if status == 200 else []
    for item in sorted(catalog, key=lambda row: str(row.get("public_id"))):
        log(
            "MODEL",
            provider=item.get("provider"),
            public_id=item.get("public_id"),
            upstream_id=item.get("upstream_id"),
            enabled=item.get("enabled"),
            tools_status=item.get("tools_status"),
            tools_verified_at=item.get("tools_verified_at"),
            supports_tools=item.get("supports_tools"),
            supports_reasoning=item.get("supports_reasoning"),
            reasoning_status=item.get("reasoning_status"),
            context_length=item.get("context_length"),
        )
    status, body = call_admin("GET", "/admin/api/models/candidates?provider=opencodefree")
    candidates = unwrap(body) if status == 200 else []
    if isinstance(candidates, dict):
        candidates = candidates.get("candidates") or []
    log("CANDIDATES", status=status, count=len(candidates or []),
        ids=sorted(str(item.get("public_id")) for item in (candidates or []))[:20])
    status, metrics = call_admin("GET", "/metrics", timeout=20)
    healthy = None
    if status == 200:
        text = metrics.decode("utf-8", "replace")
        for line in text.splitlines():
            if line.startswith("nvidia_router_proxy_pool_healthy "):
                healthy = int(line.rsplit(" ", 1)[1])
    log("POOL", status=status, healthy=healthy)


def reprobe_models():
    """Run the detailed capability probe now instead of waiting for the cycle."""
    models = enabled_models()
    if not models:
        log("REPROBE", error="no enabled opencodefree models")
        return
    status, body = call_admin("POST", "/admin/api/model-test-jobs", {
        "model_ids": [item["id"] for item in models],
        "mode": "sequential",
    })
    job = unwrap(body) if status in (200, 201, 202) else {}
    log("REPROBE", step="create", status=status, job_id=bool(job.get("id")))
    if not job.get("id"):
        return
    job_id = job["id"]
    for _ in range(180):
        status, body = call_admin("GET", "/admin/api/model-test-jobs/" + job_id)
        job = unwrap(body) if status == 200 else {}
        if job.get("status") in ("completed", "failed", "cancelled"):
            break
        time.sleep(5)
    for result in job.get("results") or []:
        probe = result.get("probe") or {}
        log("REPROBE_RESULT", public_id=result.get("public_id"), status=result.get("status"),
            error=result.get("error"), base=probe.get("base"),
            reasoning=probe.get("reasoning"), tools=probe.get("tools"),
            duration_ms=result.get("duration_ms"))
    report_state()


def main():
    global admin_opener
    admin_opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor())
    password = sys.stdin.readline().rstrip("\r\n")
    status, _ = call_admin("POST", "/admin/api/auth/login", {"username": "admin", "password": password})
    log("AUTH", step="login", status=status)
    if status != 200:
        return 2
    try:
        if MODE == "state":
            report_state()
            return 0
        if MODE == "reprobe":
            reprobe_models()
            return 0
        status, body = call_admin("POST", "/admin/api/access-keys", {"name": RUN_TAG})
        created = unwrap(body) if status == 201 else {}
        access_key["id"] = created.get("id")
        access_key["key"] = created.get("key")
        log("AUTH", step="access_key", status=status)
        if not access_key["key"]:
            return 3
        models = enabled_models("opencodefree" if MODE != "all" else None)
        targets = [item["public_id"] for item in models]
        log("TARGETS", models=targets)
        if MODE == "all":
            modes = ["chat", "stream", "responses"]
        else:
            modes = [MODE]
        for model in targets:
            for mode in modes:
                runner = run_responses_loop if mode == "responses" else (lambda m, s=mode == "stream": run_chat_loop(m, stream=s))
                started = time.monotonic()
                try:
                    summary = runner(model)
                except Exception as error:  # noqa: BLE001
                    summary = {"model": model, "transport": mode, "probe_error": type(error).__name__}
                summary["wall_seconds"] = round(time.monotonic() - started, 1)
                log("SOLVE", **summary)
    finally:
        if access_key["id"] is not None:
            call_admin("DELETE", "/admin/api/access-keys/%s" % access_key["id"])
            log("AUTH", step="cleanup_access", done=True)
        call_admin("POST", "/admin/api/auth/logout")
    return 0


raise SystemExit(main())
