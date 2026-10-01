// Terma plugin for DeepSeek Harness (dsh, @deepseek-ai/dsh).
//
// Written into $DSH_HOME/plugins/terma-relay.mjs by `terma relay setup --harness dsh`,
// and inserted by absolute path into $DSH_HOME/cordis.patch.yml, the home layer every
// profile loads; do not edit — the next setup rewrites it. CONFIG below is the only part
// that differs per machine (the raw template ships `CONFIG = null` and is inert).
//
// dsh's own OTLP goes to DeepSeek's collector and carries no usage, so this plugin is its
// exporter, under the GenAI semantic conventions: a `chat {model}` span per model
// response with its usage (from the assistant/message session event), one for each
// auxiliary call too (the session title, seen through the llm/stream waterfall with its
// purpose), an `execute_tool {tool}` span per tool call, and a `dsh.user_prompt` and a
// `dsh.assistant_response` log per turn. Every record names dsh's session
// (`session-<uuid>`) as `session.id`. Session lifecycle, prompts and file edits are handed
// to `terma hook dsh-*`, which claims the session for the local relay and records commit
// attribution. dsh computes no cost; the platform prices the tokens.
//
// It sets no environment: dsh passes its own to every tool, and a tool must never inherit
// terma's endpoint or token. Dependency-free: only Node's fetch, node:crypto and
// node:child_process.

const CONFIG = null /* terma:config */

import { createHash, randomBytes } from "node:crypto"
import { spawn } from "node:child_process"

export const name = "terma"

const MAX_TEXT = 64 * 1024
const MAX_TOOL_TEXT = 16 * 1024

// --- ids and attributes --------------------------------------------------------------

const hex = (n) => randomBytes(n).toString("hex")
const traceId = (session) => createHash("sha256").update("dsh:" + session).digest("hex").slice(0, 32)

function value(v) {
  if (typeof v === "boolean") return { boolValue: v }
  if (typeof v === "number") return Number.isInteger(v) ? { intValue: String(v) } : { doubleValue: v }
  return { stringValue: String(v) }
}

function attrs(m) {
  const out = []
  for (const [key, v] of Object.entries(m)) {
    if (v === undefined || v === null || v === "") continue
    out.push({ key, value: value(v) })
  }
  return out
}

const nanos = (ms) => String(BigInt(Math.round(ms)) * 1000000n)

function truncate(s, max) {
  const t = typeof s === "string" ? s : JSON.stringify(s ?? "")
  return t.length > max ? t.slice(0, max) : t
}

// text is what a message's content says, whatever shape dsh gave it.
function text(content) {
  if (typeof content === "string") return content
  if (!Array.isArray(content)) return ""
  return content.map((c) => (typeof c === "string" ? c : c && c.type === "text" ? c.text : "")).filter(Boolean).join("\n")
}

// --- export ----------------------------------------------------------------------------

const RESOURCE = { attributes: attrs({ "service.name": "dsh", "telemetry.sdk.name": "terma-dsh" }) }
const SCOPE = { name: "terma-dsh", version: "1" }

let spans = []
let logs = []
let timer

async function post(path, body) {
  try {
    await fetch(CONFIG.endpoint.replace(/\/+$/, "") + path, {
      method: "POST",
      headers: { "content-type": "application/json", ...CONFIG.headers },
      body: JSON.stringify(body),
      signal: AbortSignal.timeout(10000),
    })
  } catch {
    // A telemetry failure must never surface in the agent.
  }
}

async function flush() {
  if (timer) clearTimeout(timer)
  timer = undefined
  const s = spans
  const l = logs
  spans = []
  logs = []
  const sends = []
  if (s.length) sends.push(post("/v1/traces", { resourceSpans: [{ resource: RESOURCE, scopeSpans: [{ scope: SCOPE, spans: s }] }] }))
  if (l.length) sends.push(post("/v1/logs", { resourceLogs: [{ resource: RESOURCE, scopeLogs: [{ scope: SCOPE, logRecords: l }] }] }))
  await Promise.all(sends)
}

function schedule() {
  if (!timer) timer = setTimeout(() => void flush(), 1000)
}

function span(session, name, start, end, a, error = false) {
  spans.push({
    traceId: traceId(session), spanId: hex(8), name, kind: 3,
    startTimeUnixNano: nanos(start), endTimeUnixNano: nanos(end),
    attributes: attrs({ "session.id": session, ...a }),
    status: error ? { code: 2 } : {},
  })
  schedule()
}

function record(session, event, body, a) {
  const t = nanos(Date.now())
  logs.push({
    timeUnixNano: t, observedTimeUnixNano: t, severityNumber: 9, severityText: "INFO",
    body: { stringValue: body },
    attributes: attrs({ "event.name": event, "session.id": session, ...a }),
    traceId: traceId(session),
  })
  schedule()
}

function usageAttrs(u) {
  u = u || {}
  return {
    "gen_ai.usage.input_tokens": u.inputTokens,
    "gen_ai.usage.output_tokens": u.outputTokens,
    "gen_ai.usage.cache_read.input_tokens": u.cacheReadTokens,
    "gen_ai.usage.cache_creation.input_tokens": u.cacheWriteTokens,
    "gen_ai.usage.total_tokens": u.totalTokens,
  }
}

// --- terma hook ------------------------------------------------------------------------

// termaHook hands an event to `terma hook dsh-*`, as this process's child — which is
// what lets its claim name this process. Best-effort; it never delays the agent.
function termaHook(event, payload) {
  try {
    const [cmd, ...args] = CONFIG.hookCommand
    const child = spawn(cmd, [...args, event], { stdio: ["pipe", "ignore", "ignore"] })
    child.on("error", () => {})
    child.stdin.end(JSON.stringify(payload))
  } catch {}
}

// --- the plugin -------------------------------------------------------------------------

const EDIT_TOOLS = new Set(["write", "edit", "multi_edit"])

const cwds = new Map() // session → workspace
const stepStart = new Map() // session → when its model step began
const calls = new Map() // callId → { name, args, start }
const ended = new Set()

function sessionOf(session, event) {
  return String((session && (session.id || session.sessionId)) || (event && event.data && event.data.sessionId) || "")
}

function cwdOf(agent) {
  const candidates = [agent && agent.cwd, agent && agent.workspace && agent.workspace.root, agent && agent.options && agent.options.cwd,
    agent && agent.runtimeContext && agent.runtimeContext.cwd, agent && agent.session && agent.session.cwd]
  for (const c of candidates) if (typeof c === "string" && c) return c
  return process.cwd()
}

function args(raw) {
  if (raw && typeof raw === "object") return raw
  try {
    return JSON.parse(raw)
  } catch {
    return {}
  }
}

export function apply(ctx) {
  if (!CONFIG) return

  ctx.on("agent/created", (created) => {
    const agent = created && created.agent
    const s = String((agent && (agent.sessionId || agent.id)) || "")
    if (!s) return
    const cwd = cwdOf(agent)
    cwds.set(s, cwd)
    termaHook("dsh-session-start", { session_id: s, cwd })
  })

  ctx.on("session/event", (session, event) => {
    const s = sessionOf(session, event)
    if (!s || !event) return
    const d = event.data || {}
    const at = typeof event.time === "number" ? event.time : Date.now()
    const cwd = cwds.get(s) || process.cwd()
    switch (event.type) {
      case "step/start":
        stepStart.set(s, at)
        break
      case "user/message": {
        const prompt = text(d.content)
        record(s, "dsh.user_prompt", CONFIG.includePrompts ? truncate(prompt, MAX_TEXT) : "", { prompt_length: prompt.length })
        // Every prompt claims too: a session whose start the plugin missed, or a relay
        // that stopped between turns, is covered before the turn exports.
        termaHook("dsh-prompt", { session_id: s, cwd })
        break
      }
      case "assistant/message": {
        const m = d.message || {}
        const src = m.source || {}
        span(s, "chat " + (src.model || ""), stepStart.get(s) || at, at, {
          "gen_ai.operation.name": "chat",
          "gen_ai.provider.name": src.provider,
          "gen_ai.request.model": src.model,
          "gen_ai.response.model": src.model,
          "gen_ai.response.id": m.id,
          "dsh.turn": d.turn,
          ...usageAttrs(d.usage),
        })
        const reply = text(m.content)
        if (reply) record(s, "dsh.assistant_response", CONFIG.includePrompts ? truncate(reply, MAX_TEXT) : "", { response_length: reply.length })
        break
      }
      case "tool/call":
        if (d.callId) calls.set(d.callId, { name: String(d.name || ""), args: d.arguments, start: at })
        break
      case "tool/result": {
        const id = d.message && d.message.source && d.message.source.callId
        const call = calls.get(id)
        if (!call) break
        calls.delete(id)
        span(s, "execute_tool " + call.name, call.start, at, {
          "gen_ai.operation.name": "execute_tool",
          "gen_ai.tool.name": call.name,
          "gen_ai.tool.call.id": id,
          "gen_ai.tool.call.arguments": CONFIG.includeToolContent ? truncate(call.args, MAX_TOOL_TEXT) : undefined,
          "gen_ai.tool.call.result": CONFIG.includeToolContent ? truncate(text(d.message.content), MAX_TOOL_TEXT) : undefined,
        }, d.isError === true || (d.message && d.message.isError === true))
        const file = args(call.args).file_path
        if (EDIT_TOOLS.has(call.name) && typeof file === "string" && file) {
          termaHook("dsh-file-edit", { session_id: s, cwd, tool: call.name, file })
        }
        break
      }
      case "turn/end":
        void flush()
        break
    }
  })

  // Every model call passes through here, auxiliary ones included: those (the session
  // title) have a purpose and no assistant/message event, so their usage is spanned here.
  ctx.on("llm/stream", async function* (options, next) {
    const start = Date.now()
    let usage
    for await (const chunk of next()) {
      if (chunk && chunk.type === "usage") usage = chunk.usage
      yield chunk
    }
    const s = String((options && options.sessionId) || "")
    if (s && options.purpose && usage) {
      span(s, "chat " + (options.model || ""), start, Date.now(), {
        "gen_ai.operation.name": "chat",
        "gen_ai.provider.name": options.provider,
        "gen_ai.request.model": options.model,
        "dsh.purpose": options.purpose,
        ...usageAttrs(usage),
      })
    }
  })

  // dsh names no session end: the process's is the session's.
  process.once("beforeExit", async () => {
    for (const [s, cwd] of cwds) {
      if (ended.has(s)) continue
      ended.add(s)
      termaHook("dsh-session-end", { session_id: s, cwd })
    }
    await flush()
  })
}
