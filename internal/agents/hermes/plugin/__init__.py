"""Terma plugin for Hermes (Nous Research).

Written into $HERMES_HOME/plugins/terma/ by `terma relay setup --harness hermes`; do not
edit — the next setup rewrites it. CONFIG_JSON below is the only part that differs per
machine (the raw template ships None and is inert).

Hermes has no OpenTelemetry export terma can use (its NeMo Relay exporter names no
session, drops usage on streamed calls and carries the whole system prompt), so this
plugin is its exporter, following the GenAI semantic conventions: one `chat {model}`
span per provider call with its usage and Hermes's own cost estimate, one
`execute_tool {tool}` span per tool call, and a `hermes.user_prompt` and a
`hermes.assistant_response` log per turn. Every record names its session as
`session.id`, Hermes's own id. Session lifecycle, prompts and file edits are handed to
`terma hook hermes-*`, which claims the session for the local relay and records commit
attribution. The relay decides what leaves the machine and withholds content per
project; this plugin sends prompt and tool content only as CONFIG allows.

Plugin hooks, unlike Hermes's shell hooks, run in every front end (the CLI, the TUI's
backend). They run synchronously in the agent's thread, so nothing here blocks: records
are batched and sent by a background thread, and `terma hook` is started, not waited
for. Standard library only.
"""

from __future__ import annotations

import atexit
import hashlib
import json
import os
import queue
import secrets
import subprocess
import threading
import time
import urllib.request

CONFIG_JSON = None  # terma:config

CONFIG = json.loads(CONFIG_JSON) if CONFIG_JSON else None

MAX_TEXT = 64 * 1024
MAX_TOOL_TEXT = 16 * 1024
FLUSH_SECONDS = 1.0

# --- attributes -------------------------------------------------------------------------


def _value(v):
    if isinstance(v, bool):
        return {"boolValue": v}
    if isinstance(v, int):
        return {"intValue": str(v)}
    if isinstance(v, float):
        return {"doubleValue": v}
    return {"stringValue": str(v)}


def _attrs(m):
    return [{"key": k, "value": _value(v)} for k, v in m.items() if v is not None and v != ""]


def _nanos(seconds):
    return str(int(seconds * 1e9))


def _text(v, limit):
    s = v if isinstance(v, str) else json.dumps(v, default=str)
    return s[:limit]


def _trace_id(session):
    # Stable per session, so a session's spans form one trace.
    return hashlib.sha256(("hermes:" + session).encode()).hexdigest()[:32]


RESOURCE = {"attributes": _attrs({"service.name": "hermes", "telemetry.sdk.name": "terma-hermes"})}
SCOPE = {"name": "terma-hermes", "version": "1"}

# --- export -----------------------------------------------------------------------------

_lock = threading.Lock()
_spans = []
_logs = []
_tainted = set()
_wake = threading.Event()


def _post(path, body):
    try:
        req = urllib.request.Request(
            CONFIG["endpoint"].rstrip("/") + path,
            data=json.dumps(body).encode(),
            headers={"Content-Type": "application/json", **CONFIG.get("headers", {})},
            method="POST",
        )
        urllib.request.urlopen(req, timeout=10).close()
    except Exception:
        pass  # A telemetry failure must never surface in the agent.


def _flush():
    with _lock:
        spans, logs = _spans[:], _logs[:]
        _spans.clear()
        _logs.clear()
    if spans:
        _post("/v1/traces", {"resourceSpans": [{"resource": RESOURCE, "scopeSpans": [{"scope": SCOPE, "spans": spans}]}]})
    if logs:
        _post("/v1/logs", {"resourceLogs": [{"resource": RESOURCE, "scopeLogs": [{"scope": SCOPE, "logRecords": logs}]}]})


def _sender():
    while True:
        _wake.wait(FLUSH_SECONDS)
        _wake.clear()
        _flush()


def _span(session, name, start, end, attrs, error=False):
    with _lock:
        if session and session in _tainted:
            return
        _spans.append({
            "traceId": _trace_id(session),
            "spanId": secrets.token_hex(8),
            "name": name,
            "kind": 3,
            "startTimeUnixNano": _nanos(start),
            "endTimeUnixNano": _nanos(end),
            "attributes": _attrs({"session.id": session, **attrs}),
            "status": {"code": 2} if error else {},
        })


def _log(session, event, body, attrs):
    t = _nanos(time.time())
    with _lock:
        if session and session in _tainted:
            return
        _logs.append({
            "timeUnixNano": t,
            "observedTimeUnixNano": t,
            "severityNumber": 9,
            "severityText": "INFO",
            "body": {"stringValue": body},
            "attributes": _attrs({"event.name": event, "session.id": session, **attrs}),
            "traceId": _trace_id(session),
        })


# --- terma hook -------------------------------------------------------------------------


def _cwd():
    # The session's workspace: in the TUI the process's own cwd is where its backend
    # started, not where the session works.
    try:
        from agent.runtime_cwd import resolve_agent_cwd

        return str(resolve_agent_cwd())
    except Exception:
        return os.getcwd()


_hook_queue = queue.Queue(maxsize=128)
_hook_worker_lock = threading.Lock()
_hook_worker_started = False


def _hook_worker():
    while True:
        event, payload, done, result = _hook_queue.get()
        ok = False
        try:
            proc = subprocess.Popen(
                [*CONFIG["hookCommand"], event],
                stdin=subprocess.PIPE,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
            try:
                proc.communicate(json.dumps(payload).encode(), timeout=2.0)
                ok = proc.returncode == 0
            except Exception:
                try:
                    proc.kill()
                    proc.wait(timeout=1.0)
                except Exception:
                    pass
        except Exception:
            pass
        finally:
            result[0] = ok
            done.set()
            _hook_queue.task_done()


def _ensure_hook_worker():
    global _hook_worker_started
    if _hook_worker_started:
        return
    with _hook_worker_lock:
        if not _hook_worker_started:
            threading.Thread(target=_hook_worker, name="terma-hook", daemon=True).start()
            _hook_worker_started = True


def _terma_hook(event, payload, wait=False):
    """Hands an event to `terma hook` in FIFO order so session-start always finishes before
    tool-reach. When wait=True, blocks up to 2s and returns True only if the hook exited 0."""
    try:
        _ensure_hook_worker()
        done = threading.Event()
        result = [False]
        _hook_queue.put_nowait((event, payload, done, result))
        if not wait:
            return True
        return done.wait(timeout=2.0) and result[0]
    except Exception:
        return False


# --- hooks ------------------------------------------------------------------------------

EDIT_TOOLS = {"write_file", "patch"}


def _usage_of(usage):
    if isinstance(usage, str):
        try:
            usage = json.loads(usage)
        except Exception:
            return {}
    if isinstance(usage, dict):
        return usage
    return {k: getattr(usage, k) for k in ("input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens", "request_count") if hasattr(usage, k)}


def _cost(model, usage, provider, base_url):
    """Hermes's own estimate (agent.usage_pricing), as its Langfuse plugin makes it.
    None when the route is included in a subscription, or unpriced."""
    try:
        from agent.usage_pricing import CanonicalUsage, estimate_usage_cost

        canonical = CanonicalUsage(
            input_tokens=usage.get("input_tokens", 0) or 0,
            output_tokens=usage.get("output_tokens", 0) or usage.get("completion_tokens", 0) or 0,
            cache_read_tokens=usage.get("cache_read_tokens", 0) or 0,
            cache_write_tokens=usage.get("cache_write_tokens", 0) or 0,
            reasoning_tokens=usage.get("reasoning_tokens", 0) or 0,
            request_count=usage.get("request_count", 1) or 1,
        )
        cost = estimate_usage_cost(model, canonical, provider=provider, base_url=base_url, api_key="")
        if cost.amount_usd is None or getattr(cost, "status", "") == "included":
            return None
        return float(cost.amount_usd)
    except Exception:
        return None


def _on_session_start(session_id="", model="", **_):
    if session_id:
        _terma_hook("hermes-session-start", {"session_id": session_id, "cwd": _cwd(), "model": model})


def _on_pre_llm_call(session_id="", user_message="", model="", turn_id="", **_):
    if not session_id:
        return
    prompt = user_message if isinstance(user_message, str) else ""
    _log(session_id, "hermes.user_prompt", _text(prompt, MAX_TEXT) if CONFIG["includePrompts"] else "",
         {"prompt_length": len(prompt), "hermes.turn_id": turn_id})
    # Every turn claims too: a session whose start the plugin missed, or a relay that
    # stopped between turns, is covered before the turn exports.
    _terma_hook("hermes-prompt", {"session_id": session_id, "cwd": _cwd(), "model": model})


def _on_post_llm_call(session_id="", assistant_response="", turn_id="", **_):
    if not session_id:
        return
    reply = assistant_response if isinstance(assistant_response, str) else ""
    _log(session_id, "hermes.assistant_response", _text(reply, MAX_TEXT) if CONFIG["includePrompts"] else "",
         {"response_length": len(reply), "hermes.turn_id": turn_id})


def _on_post_api_request(session_id="", model="", response_model="", provider="", base_url="", usage=None,
                         started_at=None, ended_at=None, finish_reason="", api_request_id="", turn_id="", **_):
    if not session_id:
        return
    u = _usage_of(usage)
    end = ended_at or time.time()
    start = started_at or end
    inp, out = u.get("input_tokens"), u.get("output_tokens") or u.get("completion_tokens")
    total = u.get("total_tokens")
    if total is None and isinstance(inp, int) and isinstance(out, int):
        total = inp + out
    _span(session_id, "chat " + (response_model or model or ""), start, end, {
        "gen_ai.operation.name": "chat",
        "gen_ai.provider.name": provider,
        "gen_ai.request.model": model,
        "gen_ai.response.model": response_model or model,
        "gen_ai.response.id": api_request_id,
        "gen_ai.response.finish_reasons": finish_reason,
        "gen_ai.usage.input_tokens": inp,
        "gen_ai.usage.output_tokens": out,
        "gen_ai.usage.cache_read.input_tokens": u.get("cache_read_tokens"),
        "gen_ai.usage.cache_creation.input_tokens": u.get("cache_write_tokens"),
        "gen_ai.usage.reasoning.output_tokens": u.get("reasoning_tokens"),
        "gen_ai.usage.total_tokens": total,
        "gen_ai.usage.total_cost": _cost(model, u, provider, base_url),
        "hermes.turn_id": turn_id,
    }, error=finish_reason == "error")


def _tool_reach(tool_name, args):
    if not isinstance(args, dict):
        return None
    name = str(tool_name or "").lower()
    out = {}

    def first(*keys):
        for k in keys:
            v = args.get(k)
            if isinstance(v, str) and v:
                return v
        return ""

    path = first("path", "file_path", "dir_path")
    if name in ("read_file", "read"):
        if path:
            out["paths"] = [path]
    elif name in ("search_files", "grep"):
        out["paths"] = [path or "."]
        g = first("file_glob", "glob", "include")
        if not g and name == "search_files" and args.get("target") == "files":
            g = first("pattern")
        if g:
            out["globs"] = [g]
    elif name in ("glob", "find_files"):
        out["paths"] = [path or "."]
        g = first("pattern", "glob")
        if g:
            out["globs"] = [g]
    elif name in ("list_directory", "list_files", "ls"):
        if path:
            out["lists"] = [path]
    elif name in ("terminal", "bash", "shell"):
        cmd = first("command", "cmd")
        if cmd:
            out["command"] = cmd
        d = first("workdir", "cwd")
        if d:
            out["dir"] = d
    else:
        return None
    return out or None


def _on_post_tool_call(session_id="", tool_name="", args=None, result=None, tool_call_id="", duration_ms=None,
                       status="", turn_id="", **_):
    if not session_id:
        return
    reach = _tool_reach(tool_name, args)
    if reach and not _terma_hook("hermes-tool-reach", {"session_id": session_id, "cwd": _cwd(), "tool": tool_name, **reach}, wait=True):
        with _lock:
            _tainted.add(session_id)
        return
    end = time.time()
    start = end - (duration_ms or 0) / 1000.0
    content = CONFIG["includeToolContent"]
    _span(session_id, "execute_tool " + str(tool_name), start, end, {
        "gen_ai.operation.name": "execute_tool",
        "gen_ai.tool.name": tool_name,
        "gen_ai.tool.call.id": tool_call_id,
        "gen_ai.tool.call.arguments": _text(args, MAX_TOOL_TEXT) if content and args is not None else None,
        "gen_ai.tool.call.result": _text(result, MAX_TOOL_TEXT) if content and result is not None else None,
        "hermes.turn_id": turn_id,
    }, error=bool(status) and status != "ok")
    path = args.get("path") if isinstance(args, dict) else None
    if tool_name in EDIT_TOOLS and isinstance(path, str) and path:
        _terma_hook("hermes-file-edit", {"session_id": session_id, "cwd": _cwd(), "tool": tool_name, "file": path})


def _on_session_end(**_):
    # Hermes calls this once per run or turn: the turn's records leave now.
    _wake.set()


def _on_session_finalize(session_id="", **_):
    if session_id:
        _terma_hook("hermes-session-end", {"session_id": session_id, "cwd": _cwd()}, wait=True)
    _flush()


def register(ctx):
    if not CONFIG:
        return
    threading.Thread(target=_sender, name="terma-export", daemon=True).start()
    atexit.register(_flush)
    for name, fn in (
        ("on_session_start", _on_session_start),
        ("pre_llm_call", _on_pre_llm_call),
        ("post_llm_call", _on_post_llm_call),
        ("post_api_request", _on_post_api_request),
        ("post_tool_call", _on_post_tool_call),
        ("on_session_end", _on_session_end),
        ("on_session_finalize", _on_session_finalize),
    ):
        ctx.register_hook(name, fn)
