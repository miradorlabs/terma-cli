// Terma extension for Pi (@earendil-works/pi-coding-agent) and OMP (Pi's fork,
// with the same extension events).
//
// Written by `terma relay setup --harness pi|omp` into ~/.pi/agent/extensions/terma.ts or
// ~/.omp/agent/extensions/terma-omp.ts; do not edit — the next setup rewrites it.
// CONFIG below is the only part that differs per machine (the raw template ships
// `CONFIG = null` and is inert). CONFIG.agent names the agent on every record and hook.
//
// Pi has no OpenTelemetry of its own, so for Pi this extension is its exporter, following
// the GenAI semantic conventions: one `chat {model}` span per model response carrying its
// usage and cost, one `execute_tool {tool}` span per tool call, and a `pi.user_prompt`
// log per prompt — every record naming its session as `session.id`, Pi's own id
// (ctx.sessionManager.getSessionId()). The relay decides what leaves the machine and
// withholds content per project; this extension sends prompt and tool content only as
// CONFIG allows.
//
// omp exports natively, from the OTEL_* settings terma writes into its .env, so for omp
// CONFIG has no endpoint and this extension exports nothing: it only hands session
// lifecycle, prompts and file edits to `terma hook omp-*`, naming each edit's tool call
// as omp's own spans do, and a subagent's session names the session that opened it.
//
// Session lifecycle, prompts and file edits are handed to `terma hook <agent>-*`, which
// claims the session for the local relay and records commit attribution.
//
// Dependency-free: Pi loads extensions with jiti, and this file uses only Node's
// fetch, node:crypto, node:fs, node:path and node:child_process.

const CONFIG: TermaConfig | null = {"version":1,"agent":"pi","lifecycle":true,"endpoint":"http://127.0.0.1:43180","headers":{"Authorization":"Bearer relay-token"},"includePrompts":true,"includeToolContent":true,"hookCommand":["/usr/local/bin/terma","hook"]} /* terma:config */

import { createHash, randomBytes } from "node:crypto"
import { spawn } from "node:child_process"
import { closeSync, openSync, readSync } from "node:fs"
import { basename, dirname } from "node:path"

type TermaConfig = {
  version: 1
  agent?: string
  lifecycle?: boolean
  endpoint: string
  headers: Record<string, string>
  includePrompts: boolean
  includeToolContent: boolean
  hookCommand: string[]
}

const MAX_TEXT = 64 * 1024

// --- ids and attributes --------------------------------------------------------------

function hex(n: number): string {
  return randomBytes(n).toString("hex")
}

// traceId is stable per session, so a session's spans form one trace.
function traceId(session: string): string {
  return createHash("sha256").update(AGENT + ":" + session).digest("hex").slice(0, 32)
}

type AnyValue = { stringValue?: string; intValue?: string; doubleValue?: number; boolValue?: boolean }

function value(v: unknown): AnyValue {
  if (typeof v === "boolean") return { boolValue: v }
  if (typeof v === "number") return Number.isInteger(v) ? { intValue: String(v) } : { doubleValue: v }
  return { stringValue: String(v) }
}

function attrs(m: Record<string, unknown>) {
  const out: { key: string; value: AnyValue }[] = []
  for (const [key, v] of Object.entries(m)) {
    if (v === undefined || v === null || v === "") continue
    out.push({ key, value: value(v) })
  }
  return out
}

function nanos(ms: number): string {
  return String(BigInt(Math.round(ms)) * 1000000n)
}

function truncate(s: unknown, max: number): string {
  const t = typeof s === "string" ? s : JSON.stringify(s ?? "")
  return t.length > max ? t.slice(0, max) : t
}

// --- export ----------------------------------------------------------------------------

const AGENT = (CONFIG && CONFIG.agent) || "pi"
const LIFECYCLE = !CONFIG || CONFIG.lifecycle !== false
// EXPORT is false for an agent that exports natively: CONFIG names no endpoint.
const EXPORT = !!CONFIG && !!CONFIG.endpoint
const RESOURCE = { attributes: attrs({ "service.name": AGENT, "telemetry.sdk.name": "terma-" + AGENT }) }
const SCOPE = { name: "terma-" + AGENT, version: "1" }

let spans: unknown[] = []
let logs: unknown[] = []
let timer: ReturnType<typeof setTimeout> | undefined

async function post(path: string, body: unknown) {
  if (!CONFIG) return
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
  const sends: Promise<void>[] = []
  if (s.length) sends.push(post("/v1/traces", { resourceSpans: [{ resource: RESOURCE, scopeSpans: [{ scope: SCOPE, spans: s }] }] }))
  if (l.length) sends.push(post("/v1/logs", { resourceLogs: [{ resource: RESOURCE, scopeLogs: [{ scope: SCOPE, logRecords: l }] }] }))
  await Promise.all(sends)
}

function schedule() {
  if (!timer) timer = setTimeout(() => void flush(), 1000)
}

function span(session: string, name: string, start: number, end: number, a: Record<string, unknown>, error = false) {
  if (!EXPORT) return
  spans.push({
    traceId: traceId(session),
    spanId: hex(8),
    name,
    kind: 3,
    startTimeUnixNano: nanos(start),
    endTimeUnixNano: nanos(end),
    attributes: attrs({ "session.id": session, ...a }),
    status: error ? { code: 2 } : {},
  })
  schedule()
}

function record(session: string, event: string, body: string, a: Record<string, unknown>) {
  if (!EXPORT) return
  const t = nanos(Date.now())
  logs.push({
    timeUnixNano: t,
    observedTimeUnixNano: t,
    severityNumber: 9,
    severityText: "INFO",
    body: { stringValue: body },
    attributes: attrs({ "event.name": event, "session.id": session, ...a }),
    traceId: traceId(session),
  })
  schedule()
}

// --- terma hook ----------------------------------------------------------------------

// termaHook hands an event to `terma hook pi-*`, detached: attribution is best-effort
// and never delays the agent. It settles once the payload is written, which a handler that
// runs as the agent exits awaits: the agent's exit would otherwise cut the payload off.
function termaHook(event: string, payload: Record<string, unknown>): Promise<void> {
  if (!CONFIG) return Promise.resolve()
  return new Promise((resolve) => {
    try {
      const [cmd, ...args] = CONFIG!.hookCommand
      const child = spawn(cmd, [...args, event], { stdio: ["pipe", "ignore", "ignore"], detached: false })
      child.on("error", () => resolve())
      child.stdin.on("error", () => resolve())
      child.stdin.end(JSON.stringify(payload), () => resolve())
    } catch {
      resolve()
    }
  })
}

// --- the extension -------------------------------------------------------------------

let session = ""
let turnStart = Date.now()
const toolStarts = new Map<string, number>()

function sessionOf(ctx: any): string {
  const own = ctx && ctx.sessionManager && typeof ctx.sessionManager.getSessionId === "function" ? ctx.sessionManager.getSessionId() : ""
  return own ? String(own) : session
}

// parentOf is the session that opened this one for a subagent: its header's parentSession
// is the parent's transcript, and a subagent's own transcript sits in the folder of that
// name. A top-level parent's is <dir>/<time>_<id>.jsonl; a subagent's, opening one of its
// own, is <agent>.jsonl (an agent id may hold "_"), whose header names its id. A fork or a continuation names a parent
// too, but elsewhere.
function parentOf(ctx: any): string {
  try {
    const sm = ctx && ctx.sessionManager
    const parent = sm && typeof sm.getHeader === "function" ? sm.getHeader()?.parentSession : undefined
    const file = sm && typeof sm.getSessionFile === "function" ? sm.getSessionFile() : undefined
    if (typeof parent !== "string" || typeof file !== "string" || dirname(file) + ".jsonl" !== parent) return ""
    const m = /^\d{4}-\d\d-\d\dT\d\d-\d\d-\d\d-\d{3}Z_(.+)\.jsonl$/.exec(basename(parent))
    if (m) return m[1]
    const head = Buffer.alloc(16 * 1024)
    const fd = openSync(parent, "r")
    const n = readSync(fd, head, 0, head.length, 0)
    closeSync(fd)
    for (const line of head.subarray(0, n).toString("utf8").split("\n")) {
      if (!line.includes('"type":"session"')) continue
      const id = JSON.parse(line).id
      return typeof id === "string" ? id : ""
    }
    return ""
  } catch {
    return ""
  }
}

// editedFiles are the files a tool call changed. omp's write names its file in its input,
// and its edit in its details, one file or several, each with its own outcome; Pi's write
// and edit name theirs in their input.
function editedFiles(event: any): string[] {
  const name = String(event?.toolName ?? "")
  const input = event?.input
  if (AGENT !== "omp") {
    return (name === "write" || name === "edit") && input && typeof input.path === "string" && input.path ? [input.path] : []
  }
  if (event?.isError === true) return []
  const files: string[] = []
  const add = (p: unknown) => {
    if (typeof p === "string" && p && !files.includes(p)) files.push(p)
  }
  if (name === "write") add(input?.path)
  if (name === "edit") {
    const d = event?.details
    add(d?.path)
    add(d?.move)
    if (Array.isArray(d?.perFileResults)) {
      for (const r of d.perFileResults) {
        if (r && r.isError !== true) {
          add(r.path)
          add(r.move)
        }
      }
    }
  }
  return files
}

export default function (pi: any) {
  if (!CONFIG) return

  pi.on("session_start", (event: any, ctx: any) => {
    session = sessionOf(ctx)
    if (LIFECYCLE) termaHook(AGENT + "-session-start", { session_id: session, cwd: ctx?.cwd, model: ctx?.model?.id, reason: event?.reason, parent_session_id: AGENT === "omp" ? parentOf(ctx) || undefined : undefined })
  })

  pi.on("session_shutdown", async (_event: any, ctx: any) => {
    const s = sessionOf(ctx)
    const ended = s && LIFECYCLE ? termaHook(AGENT + "-session-end", { session_id: s, cwd: ctx?.cwd }) : undefined
    await Promise.all([ended, flush()])
  })

  pi.on("before_agent_start", (event: any, ctx: any) => {
    const s = sessionOf(ctx)
    turnStart = Date.now()
    const prompt = typeof event?.prompt === "string" ? event.prompt : ""
    record(s, AGENT + ".user_prompt", CONFIG!.includePrompts ? truncate(prompt, MAX_TEXT) : "", { prompt_length: prompt.length })
    // Every prompt is a claim too: a session whose start the extension missed, or a
    // relay that stopped between turns, is covered before the turn exports anything.
    termaHook(AGENT + "-prompt", { session_id: s, cwd: ctx?.cwd })
  })

  pi.on("message_end", (event: any, ctx: any) => {
    const m = event?.message
    if (!m || m.role !== "assistant") return
    const u = m.usage || {}
    const end = Date.now()
    const text = Array.isArray(m.content) ? m.content.filter((c: any) => c && c.type === "text").map((c: any) => c.text).join("\n") : ""
    span(sessionOf(ctx), "chat " + (m.model || ""), turnStart, end, {
      "gen_ai.operation.name": "chat",
      "gen_ai.provider.name": m.provider,
      "gen_ai.request.model": m.model,
      "gen_ai.response.model": m.responseModel || m.model,
      "gen_ai.response.id": m.responseId,
      "gen_ai.response.finish_reasons": m.stopReason,
      "gen_ai.usage.input_tokens": u.input,
      "gen_ai.usage.output_tokens": u.output,
      "gen_ai.usage.cache_read.input_tokens": u.cacheRead,
      "gen_ai.usage.cache_creation.input_tokens": u.cacheWrite,
      "gen_ai.usage.reasoning.output_tokens": u.reasoning,
      "gen_ai.usage.total_tokens": u.totalTokens,
      "gen_ai.usage.total_cost": u.cost && typeof u.cost.total === "number" ? u.cost.total : undefined,
      "gen_ai.completion": CONFIG!.includePrompts ? truncate(text, MAX_TEXT) : undefined,
    }, m.stopReason === "error")
    turnStart = end
  })

  pi.on("tool_call", (event: any) => {
    if (event?.toolCallId) toolStarts.set(event.toolCallId, Date.now())
  })

  pi.on("tool_result", (event: any, ctx: any) => {
    const s = sessionOf(ctx)
    const start = toolStarts.get(event?.toolCallId) ?? Date.now()
    toolStarts.delete(event?.toolCallId)
    const name = String(event?.toolName ?? "")
    span(s, "execute_tool " + name, start, Date.now(), {
      "gen_ai.operation.name": "execute_tool",
      "gen_ai.tool.name": name,
      "gen_ai.tool.call.id": event?.toolCallId,
      "gen_ai.tool.call.arguments": CONFIG!.includeToolContent ? truncate(event?.input, 16 * 1024) : undefined,
      "gen_ai.tool.call.result": CONFIG!.includeToolContent ? truncate(event?.content, 16 * 1024) : undefined,
    }, event?.isError === true)
    const files = editedFiles(event)
    if (LIFECYCLE && files.length > 0) {
      termaHook(AGENT + "-file-edit", { session_id: s, cwd: ctx?.cwd, tool: name, files, tool_call_id: AGENT === "omp" ? event?.toolCallId : undefined })
    }
  })

  pi.on("agent_end", () => void flush())
}
