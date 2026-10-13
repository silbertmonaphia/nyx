#!/usr/bin/env python3
"""OpenAI-compatible stub provider for the planner-retry rollout gate (§2.2).

Simulates "a provider that ignores `response_format`" — the failure mode the
gate needs. Requests carrying a `response_format` key are treated as PLANNER
calls; everything else is treated as an executor `llm` step.

Behaviour:
  - MODE=flaky (default): planner call #1 -> malformed content ("not json at
    all", i.e. what a response_format-ignoring provider emits), call #2+ ->
    valid Plan JSON. Proves the retry path recovers.
  - MODE=bad: every planner call -> malformed. Proves exhaustion terminates
    with a typed `error{code:"plan_invalid"}` frame, never a crashed stream.
  - Non-planner calls -> plain text "Stubbed answer."

Control (POST, JSON body):
  - /mode  {"mode": "bad"|"flaky"}  — switch mode at runtime
  - /reset                          — clear the per-process planner-call counter

Every request is appended to $STUB_LOG (default /tmp/nyx-stub-requests.jsonl)
with a `has_prev_failed` flag — whether the request body carried the
PREVIOUS_ATTEMPT_FAILED retry marker.

Run: STUB_PORT=8002 MODE=flaky python3 stub_provider.py
Used by: openspec/changes/agent-planner-structured-rollout/tasks.md §2.2
"""

import json
import os
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MODE = os.environ.get("STUB_MODE", "flaky")
LOG = os.environ.get("STUB_LOG", "/tmp/nyx-stub-requests.jsonl")
lock = threading.Lock()
plan_calls = 0

VALID_PLAN = {
    "steps": [{"id": "stub_step", "kind": "llm", "prompt": "Answer from the stub."}]
}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):  # silence per-request access logging
        pass

    def _send(self, obj, code=200):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        # The agent's /healthz pings `{llm_base_url}/models` and treats
        # >=500 as `llm_unavailable` — serve the OpenAI model-list shape
        # so a stub-pointed agent instance reports ready.
        if self.path in ("/v1/models", "/models"):
            return self._send(
                {
                    "object": "list",
                    "data": [
                        {"id": "stub-model", "object": "model", "owned_by": "stub"}
                    ],
                }
            )
        return self._send({"error": "not found"}, 404)

    def do_POST(self):
        global MODE, plan_calls
        length = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(length)
        try:
            body = json.loads(raw)
        except Exception:
            body = {}

        if self.path == "/mode":
            MODE = body.get("mode", MODE)
            return self._send({"mode": MODE})
        if self.path == "/reset":
            with lock:
                plan_calls = 0
            return self._send({"reset": True})
        if self.path != "/v1/chat/completions":
            return self._send({"error": "not found"}, 404)

        is_planner = "response_format" in body
        if is_planner:
            with lock:
                plan_calls += 1
                call_no = plan_calls
            if MODE == "bad" or call_no == 1:
                content = "not json at all"
            else:
                content = json.dumps(VALID_PLAN)
        else:
            content = "Stubbed answer."

        with open(LOG, "a") as f:
            f.write(
                json.dumps(
                    {
                        "planner": is_planner,
                        "plan_call": plan_calls if is_planner else None,
                        "mode": MODE,
                        "has_prev_failed": "PREVIOUS_ATTEMPT_FAILED"
                        in raw.decode("utf-8", "replace"),
                    }
                )
                + "\n"
            )

        self._send(
            {
                "id": "stub",
                "object": "chat.completion",
                "created": 0,
                "model": body.get("model", "stub"),
                "choices": [
                    {
                        "index": 0,
                        "message": {"role": "assistant", "content": content},
                        "finish_reason": "stop",
                    }
                ],
                "usage": {
                    "prompt_tokens": 1,
                    "completion_tokens": 1,
                    "total_tokens": 2,
                },
            }
        )


if __name__ == "__main__":
    port = int(os.environ.get("STUB_PORT", "8002"))
    print(f"stub provider on :{port} mode={MODE} log={LOG}", flush=True)
    ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
