// Terma telemetry extension for omp.
//
// Installed by `terma connect omp` into omp's user hooks directory, where it is loaded
// at startup. Managed by `terma disconnect omp`; do not edit — the next connect
// rewrites it. CONFIG below is the only part that differs per machine.
//
// What it does is *not* telemetry export: omp has a native OTLP exporter (spans per
// model call carrying tokens, effort, service tier and latency, gated on
// OTEL_EXPORTER_OTLP_ENDPOINT), and this file sets it up by exporting the OTEL_*
// variables before `initTelemetryExport` reads them. This file's other job is commit
// attribution: session lifecycle and file edits are handed to `terma hook omp-*`,
// where the logic lives. Telemetry from this file is limited to the one thing the
// native exporter cannot produce: the estimated cost, which the CLI wires only when a
// caller passes a costEstimator — something config cannot express.
//
// Dependency-free on purpose: omp loads hooks without installing anything for them, so
// this file uses only what Node ships.

const CONFIG = null /* terma:config */

import { randomUUID } from "node:crypto"
import { readFileSync } from "node:fs"
import { join, resolve as resolvePath } from "node:path"
import { execFileSync, spawn } from "node:child_process"

// --- limits -------------------------------------------------------------------------

// The exporter batches: this many items, or this long after the first, whichever first.
const BATCH_SIZE = 32
const BATCH_DELAY_MS = 2000
const MAX_RETRIES = 4
// A `terma hook` call that outlasts this is abandoned; attribution is best-effort.
const HOOK_TIMEOUT_MS = 500

const SCOPE = { name: "terma-omp", version: "1" }

// --- config -------------------------------------------------------------------------

// effectiveConfig is CONFIG with the repository's own policy laid over it: a committed
// .omp/terma.json sets what ships from that repository — signals, and whether prompt
// and tool content are captured — and nothing else. In per-repo mode the shared global
// extension captures no content by default, so a repository that wants prompt or
// tool-content capture opts in through its own committed overlay; no repository's
// install can turn capture on for another.
function effectiveConfig(worktree) {
  const cfg = { ...(CONFIG ?? {}) }
  cfg.signals = Array.isArray(cfg.signals) ? cfg.signals : []
  cfg.includePrompts = cfg.includePrompts === true
  cfg.includeToolContent = cfg.includeToolContent === true
  cfg.resourceAttributes = { ...(cfg.resourceAttributes ?? {}) }
  if (!worktree) return cfg
  // Per-repo routing: point this session's export at whatever Terma project the
  // repository is bound to. The key for that project lives in its own helper script;
  // an unbound repository has no project, so the export stays inert (no endpoint).
  if (cfg.perRepo) {
    const projectID = readProjectID(worktree)
    if (!projectID) {
      cfg.endpoint = ""
      return cfg
    }
    cfg.headersHelper = join(cfg.helpersDir || "", (cfg.helperPrefix || "") + projectID)
    if (cfg.projectAttribute) cfg.resourceAttributes[cfg.projectAttribute] = projectID
  }
  try {
    const local = JSON.parse(readFileSync(join(worktree, ".omp", "terma.json"), "utf8"))
    if (Array.isArray(local.signals)) cfg.signals = local.signals
    if (typeof local.includePrompts === "boolean") cfg.includePrompts = local.includePrompts
    if (typeof local.includeToolContent === "boolean") cfg.includeToolContent = local.includeToolContent
  } catch {
    // No local policy, or one this extension cannot read: the global settings stand.
  }
  return cfg
}

// readProjectID walks up from a worktree to the first Terma binding and returns its
// project id, or "" when the repository is not bound to a Terma project.
function readProjectID(start) {
  let dir = resolvePath(start)
  for (;;) {
    try {
      const raw = readFileSync(join(dir, ".terma", "settings.json"), "utf8")
      const doc = JSON.parse(raw)
      const id = doc && doc.project && typeof doc.project.id === "string" ? doc.project.id : ""
      if (id) return id
    } catch {
      // No binding here; try the parent.
    }
    const parent = resolvePath(dir, "..")
    if (parent === dir) return ""
    dir = parent
  }
}

// --- credentials --------------------------------------------------------------------

// A helper script is how the key stays out of this file: it is a path Terma wrote,
// 0700, printing {"headers": {...}} when run. Synchronous on purpose: hooks load
// before initTelemetryExport is awaited, so the credential has to be in process.env
// before this factory returns or the first export goes unauthenticated.
function readHeaders(path) {
  if (!path) return {}
  try {
    const out = execFileSync(path, [], { timeout: 5000, encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] })
    const doc = JSON.parse(out)
    return doc && typeof doc === "object" && doc.headers && typeof doc.headers === "object" ? doc.headers : {}
  } catch {
    return {}
  }
}

// --- env ----------------------------------------------------------------------------

// applyEnv sets the OTEL_* process env the native exporter reads at startup. Variables
// already set in the shell win: exporting over them would surprise the developer who
// deliberately pointed somewhere else, and doctor reports that conflict instead.
function applyEnv(cfg, headers) {
  const set = (key, value) => {
    if (value === undefined || value === null || value === "") return
    if (process.env[key] !== undefined && process.env[key] !== "") return
    process.env[key] = String(value)
  }
  const has = (signal) => cfg.signals.includes(signal)
  // omp's exporter only supports http/protobuf; a mismatched OTEL_EXPORTER_OTLP_PROTOCOL
  // (left over from another tool's grpc setup, say) would silently disable export, so
  // this one is set unconditionally rather than deferring to the shell.
  process.env.OTEL_EXPORTER_OTLP_PROTOCOL = "http/protobuf"
  if (!cfg.endpoint) return
  set("OTEL_EXPORTER_OTLP_ENDPOINT", cfg.endpoint)
  set("OTEL_TRACES_EXPORTER", has("traces") ? "otlp" : "none")
  set("OTEL_LOGS_EXPORTER", has("logs") ? "otlp" : "none")
  set("OTEL_METRICS_EXPORTER", has("metrics") ? "otlp" : "none")
  const bag = Object.entries(headers).map(([k, v]) => `${k}=${v}`).join(",")
  if (bag) set("OTEL_EXPORTER_OTLP_HEADERS", bag)
  const attrs = Object.entries(cfg.resourceAttributes).map(([k, v]) => `${k}=${v}`).join(",")
  if (attrs) set("OTEL_RESOURCE_ATTRIBUTES", attrs)
  // Content capture is one upstream switch for prompts and responses alike; tool
  // content has no separate gate (it rides the tool spans), so both come from the
  // repository's prompt+tool choice.
  const capture = cfg.includePrompts || cfg.includeToolContent
  set("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", capture ? "true" : "false")
}

// --- hook fan-out -------------------------------------------------------------------

// termaHook hands an event to `terma hook omp-*`, asynchronously: the hook is not on
// the agent loop's critical path, and a terma that hangs or is absent must never slow
// or fail the session. Errors are swallowed on purpose.
function termaHook(hookCommand, event, payload) {
  if (!Array.isArray(hookCommand) || hookCommand.length === 0) return
  try {
    const child = spawn(hookCommand[0], [...hookCommand.slice(1), event], {
      stdio: ["pipe", "ignore", "ignore"],
    })
    const killer = setTimeout(() => { try { child.kill() } catch {} }, HOOK_TIMEOUT_MS)
    if (killer.unref) killer.unref()
    child.on("error", () => {})
    child.on("close", () => clearTimeout(killer))
    child.stdin.on("error", () => {})
    child.stdin.end(JSON.stringify(payload))
    if (child.unref) child.unref()
  } catch {
    // A hook that fails the agent uninstalls the product.
  }
}

// --- cost ----------------------------------------------------------------------------

// pricing is the per-million-token USD rates Terma knows, mirroring the server-side
// table. The native exporter has usage but no estimator, so the one figure it cannot
// produce — cost — is computed here and shipped as a companion log record per model
// call, keyed by session. { input, output, cacheRead?, cacheWrite? }.
const PRICING = {
  // Anthropic
  "claude-opus-4-1": { input: 15, output: 75, cacheRead: 1.5, cacheWrite: 18.75 },
  "claude-opus-4": { input: 15, output: 75, cacheRead: 1.5, cacheWrite: 18.75 },
  "claude-sonnet-4-5": { input: 3, output: 15, cacheRead: 0.3, cacheWrite: 3.75 },
  "claude-sonnet-4": { input: 3, output: 15, cacheRead: 0.3, cacheWrite: 3.75 },
  "claude-haiku-4-5": { input: 1, output: 5, cacheRead: 0.1, cacheWrite: 1.25 },
  "claude-haiku-3-5": { input: 0.8, output: 4, cacheRead: 0.08, cacheWrite: 1 },
  // OpenAI
  "gpt-5.2": { input: 2.5, output: 15, cacheRead: 0.25 },
  "gpt-5.1": { input: 1.25, output: 10, cacheRead: 0.125 },
  "gpt-5": { input: 1.25, output: 10, cacheRead: 0.125 },
  "gpt-5-mini": { input: 0.25, output: 2, cacheRead: 0.025 },
  "gpt-4.1": { input: 2, output: 8, cacheRead: 0.5 },
  "gpt-4.1-mini": { input: 0.4, output: 1.6, cacheRead: 0.1 },
  "gpt-4o": { input: 2.5, output: 10, cacheRead: 1.25 },
  "gpt-4o-mini": { input: 0.15, output: 0.6, cacheRead: 0.075 },
  o3: { input: 2, output: 8, cacheRead: 0.5 },
  "o4-mini": { input: 1.1, output: 4.4, cacheRead: 0.275 },
  // Google
  "gemini-3-pro": { input: 2, output: 12, cacheRead: 0.2 },
  "gemini-2.5-pro": { input: 1.25, output: 10, cacheRead: 0.125 },
  "gemini-2.5-flash": { input: 0.3, output: 2.5, cacheRead: 0.03 },
}

function pricingFor(model) {
  if (!model) return undefined
  const m = String(model).toLowerCase()
  if (PRICING[m]) return PRICING[m]
  // Prefix match so dated variants (claude-sonnet-4-5-20250929) resolve.
  const key = Object.keys(PRICING)
    .filter((k) => m === k || m.startsWith(k))
    .sort((a, b) => b.length - a.length)[0]
  return key ? PRICING[key] : undefined
}

function estimateCost(model, usage) {
  const p = pricingFor(model)
  if (!p || !usage) return undefined
  const u = usage
  const input = (u.input || 0) - (u.cache_read || 0) - (u.cache_write || 0)
  const total =
    Math.max(input, 0) * p.input +
    (u.output || 0) * p.output +
    (u.cache_read || 0) * (p.cacheRead ?? p.input) +
    (u.cache_write || 0) * (p.cacheWrite ?? p.input)
  return total / 1e6
}

// --- OTLP/JSON ------------------------------------------------------------------------

let timer = null
let queue = []

function attr(key, value) {
  if (typeof value === "number") return { key, value: { doubleValue: value } }
  if (typeof value === "boolean") return { key, value: { boolValue: value } }
  return { key, value: { stringValue: String(value) } }
}

function resourceAttrs(cfg) {
  const out = [
    attr("service.name", SCOPE.name),
    attr("service.version", SCOPE.version),
  ]
  for (const [k, v] of Object.entries(cfg.resourceAttributes)) out.push(attr(k, v))
  return out
}

function enqueue(cfg, sessionID, name, attributes) {
  if (!cfg.endpoint || !cfg.signals.includes("logs")) return
  queue.push({ cfg, sessionID, name, attributes })
  if (queue.length >= BATCH_SIZE) flush()
  else if (!timer) {
    timer = setTimeout(() => { flush() }, BATCH_DELAY_MS)
    if (timer.unref) timer.unref()
  }
}

function flush() {
  if (timer) { clearTimeout(timer); timer = null }
  const batch = queue
  queue = []
  if (!batch.length) return
  // Group by session so each log record carries its conversation id at record level.
  const cfg = batch[0].cfg
  const logRecords = batch.map((item) => {
    const now = String(Date.now() * 1e6)
    const attrs = [...item.attributes]
    if (item.sessionID) attrs.push(attr("gen_ai.conversation.id", item.sessionID))
    return {
      timeUnixNano: now,
      observedTimeUnixNano: now,
      severityNumber: 9,
      body: { stringValue: item.name },
      attributes: attrs,
    }
  })
  const body = JSON.stringify({
    resourceLogs: [{
      resource: { attributes: resourceAttrs(cfg) },
      scopeLogs: [{ scope: SCOPE, logRecords }],
    }],
  })
  post(cfg, body, MAX_RETRIES).catch(() => {})
}

async function post(cfg, body, retries) {
  const headers = { "content-type": "application/json" }
  const extra = cfg.headersHelper ? readHeaders(cfg.headersHelper) : {}
  Object.assign(headers, extra)
  if (!extra.Authorization && cfg.headers && cfg.headers.Authorization) {
    headers.Authorization = cfg.headers.Authorization
  }
  const url = cfg.endpoint.replace(/\/+$/, "") + "/v1/logs"
  for (let attempt = 0; ; attempt++) {
    try {
      const res = await fetch(url, { method: "POST", headers, body })
      if (res.ok) return
      if (res.status >= 400 && res.status < 500 && res.status !== 429) return
    } catch {
      // network down: fall through to retry
    }
    if (attempt >= retries) return
    await new Promise((r) => setTimeout(r, 250 * 2 ** attempt))
  }
}

// --- usage capture -------------------------------------------------------------------

// The session id is omp's own (ctx.sessionManager.getSessionId()): the id omp stamps
// on its native spans as gen_ai.conversation.id. terma hook is told the same id, so a
// claim for the local relay names the session omp's telemetry names — an invented id
// would claim a session no span belongs to. A random one only if a build offers none.
let sessionID
let model

// filesEditedFromInput pulls the path a write or edit touched out of its input, which
// for those tools always carries `path`. Anything else (bash, eval) reports no file,
// which matches what every other adapter can see.
function fileEditedFromInput(toolName, input) {
  if (!input || typeof input !== "object") return ""
  if (toolName === "write" || toolName === "edit") {
    return typeof input.path === "string" ? input.path : ""
  }
  return ""
}

export default function (pi) {
  // Configure the native exporter before it initializes: hooks load before
  // initTelemetryExport is awaited in main.ts, so this assignment lands first.
  const bootCfg = effectiveConfig(process.cwd())
  applyEnv(bootCfg, bootCfg.headersHelper ? readHeaders(bootCfg.headersHelper) : (bootCfg.headers || {}))

  pi.on("session_start", (_event, ctx) => {
    const own = ctx && ctx.sessionManager && typeof ctx.sessionManager.getSessionId === "function" ? ctx.sessionManager.getSessionId() : ""
    sessionID = own ? String(own) : randomUUID()
    model = ctx && ctx.model && ctx.model.id ? String(ctx.model.id) : undefined
    const cwd = ctx && ctx.cwd ? ctx.cwd : process.cwd()
    const cfg = effectiveConfig(cwd)
    termaHook(cfg.hookCommand, "omp-session-start", { session_id: sessionID, cwd, model })
  })

  pi.on("session_shutdown", (_event, ctx) => {
    if (!sessionID) return
    const cwd = ctx && ctx.cwd ? ctx.cwd : process.cwd()
    const cfg = effectiveConfig(cwd)
    termaHook(cfg.hookCommand, "omp-session-end", { session_id: sessionID, cwd })
    sessionID = undefined
  })

  pi.on("tool_result", (event, ctx) => {
    if (!sessionID) return
    const cwd = ctx && ctx.cwd ? ctx.cwd : process.cwd()
    const file = fileEditedFromInput(event.toolName, event.input)
    if (!file) return
    const cfg = effectiveConfig(cwd)
    termaHook(cfg.hookCommand, "omp-file-edit", {
      session_id: sessionID, cwd, model, tool: event.toolName, file,
    })
  })

  // The cost the native exporter cannot compute: after each agent turn, read the last
  // assistant message's usage off the session history and post a companion record.
  pi.on("agent_end", (_event, ctx) => {
    const cwd = ctx && ctx.cwd ? ctx.cwd : process.cwd()
    const cfg = effectiveConfig(cwd)
    if (!cfg.endpoint || !cfg.signals.includes("logs")) return
    try {
      const entries = ctx.sessionManager.getEntries()
      for (let i = entries.length - 1; i >= 0; i--) {
        const e = entries[i]
        if (e && e.type === "message" && e.message && e.message.role === "assistant" && e.message.usage) {
          const usage = e.message.usage
          const mdl = e.message.model || model
          const cost = estimateCost(mdl, usage)
          if (cost === undefined) return
          enqueue(cfg, sessionID, "pi.omp.agent.chat.cost.estimated", [
            attr("gen_ai.provider.name", e.message.provider || ""),
            attr("gen_ai.request.model", mdl || ""),
            attr("pi.gen_ai.usage.input_tokens", usage.input || 0),
            attr("pi.gen_ai.usage.output_tokens", usage.output || 0),
            attr("pi.gen_ai.usage.cache_read.input_tokens", usage.cache_read || 0),
            attr("pi.gen_ai.usage.cache_creation.input_tokens", usage.cache_write || 0),
            attr("pi.gen_ai.cost.estimated_usd", cost),
          ])
          return
        }
      }
    } catch {
      // best-effort
    }
  })
}
