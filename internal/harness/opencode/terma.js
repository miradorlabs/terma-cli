// Terma telemetry plugin for OpenCode.
//
// Installed by `terma connect opencode` into OpenCode's plugins directory, where it is
// loaded at startup. Managed by `terma disconnect opencode`; do not edit — the next
// connect rewrites it. CONFIG below is the only part that differs per machine.
//
// What it does: turns OpenCode's own events into OpenTelemetry — one span per model
// call carrying tokens and cost, one span per tool call, and events for prompts and
// session lifecycle — and posts them as OTLP/JSON to the Terma endpoint. Nothing else
// is read or changed. Commit attribution is not done here: session start/end and file
// edits are handed to `terma hook`, where the logic lives.
//
// Dependency-free on purpose. OpenCode loads directory plugins without installing
// anything for them, so this file uses only what Bun ships: fetch, node:crypto,
// node:fs and node:child_process.

const CONFIG = null /* terma:config */

import { createHash } from "node:crypto"
import { readFileSync } from "node:fs"
import { spawn, execFile } from "node:child_process"
import { join, dirname } from "node:path"

// --- limits -------------------------------------------------------------------------

// Bodies are bounded so a single tool result cannot make an export request enormous.
const MAX_TEXT = 64 * 1024
const MAX_TOOL_CONTENT = 16 * 1024
// The exporter batches: this many items, or this long after the first, whichever first.
const BATCH_SIZE = 64
const BATCH_DELAY_MS = 3000
const MAX_RETRIES = 5
// The headers helper is re-run this often, matching Claude Code's cadence.
const HEADERS_TTL_MS = 29 * 60 * 1000

const SCOPE = { name: "terma-opencode", version: "1" }

// --- config -------------------------------------------------------------------------

// effectiveConfig is CONFIG with the repository's own policy laid over it: a committed
// .opencode/terma.json sets what ships from that repository — signals, and whether
// prompt and tool content are captured — and nothing else. In per-repo mode the shared
// global plugin captures no content by default, so a repository that wants prompt or
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
    const local = JSON.parse(readFileSync(join(worktree, ".opencode", "terma.json"), "utf8"))
    if (Array.isArray(local.signals)) cfg.signals = local.signals
    if (typeof local.includePrompts === "boolean") cfg.includePrompts = local.includePrompts
    if (typeof local.includeToolContent === "boolean") cfg.includeToolContent = local.includeToolContent
  } catch {
    // No local policy, or one this plugin cannot read: the global settings stand.
  }
  return cfg
}

// readProjectID walks up from a worktree to the first Terma binding and returns its
// project id, or "" when the repository is not bound to a Terma project. It reads
// .terma/settings.json, matching the Go side (project.Find/Load).
function readProjectID(worktree) {
  let dir = worktree
  for (let i = 0; i < 64 && dir; i++) {
    const id = bindingProjectID(dir)
    if (id) return id
    const parent = dirname(dir)
    if (parent === dir) break
    dir = parent
  }
  return ""
}

// bindingProjectID reads the project id from a directory's binding.
// An id that is not safe as a path component is no binding at all: see validProjectID.
function bindingProjectID(dir) {
  try {
    const id = JSON.parse(readFileSync(join(dir, ".terma", "settings.json"), "utf8"))?.project?.id
    return id ? validProjectID(String(id)) : ""
  } catch {
    return ""
  }
}

// validProjectID returns id when it is safe to use as a path component, else "". It is
// project.ValidID from the Go side, and it is here for the same reason: the binding is a
// committed file, so it arrives from whoever wrote the repository, and the id is about to
// name the headers helper this plugin *executes*. join() resolves "..", so an unchecked
// id like "x/../../../../Projects/repo/run.sh" would run a script the repository chose.
function validProjectID(id) {
  return id.length <= 128 && /^[A-Za-z0-9._-]+$/.test(id) && !id.startsWith(".") ? id : ""
}

// --- ids and encoding ---------------------------------------------------------------

// Trace and span ids are derived, not random: the same session always maps to the same
// trace and the same message or tool call to the same span, so a redelivered export
// dedups on the receiving side instead of duplicating.
function traceId(sessionID) {
  return createHash("sha256").update("terma-opencode:" + sessionID).digest("hex").slice(0, 32)
}
function spanId(id) {
  return createHash("sha256").update("terma-opencode-span:" + id).digest("hex").slice(0, 16)
}
function nanos(ms) {
  return (BigInt(Math.round(ms)) * 1000000n).toString()
}
function truncate(s, max) {
  if (typeof s !== "string") return ""
  return s.length > max ? s.slice(0, max) + "…" : s
}

// attr renders one OTLP attribute; unsupported or empty values are dropped.
function attr(key, value) {
  if (value === undefined || value === null || value === "") return null
  switch (typeof value) {
    case "string":
      return { key, value: { stringValue: value } }
    case "boolean":
      return { key, value: { boolValue: value } }
    case "number":
      if (!Number.isFinite(value)) return null
      return Number.isInteger(value) ? { key, value: { intValue: String(value) } } : { key, value: { doubleValue: value } }
    default:
      return null
  }
}
function attrs(obj) {
  const out = []
  for (const [k, v] of Object.entries(obj)) {
    const a = attr(k, v)
    if (a) out.push(a)
  }
  return out
}

// --- exporter -----------------------------------------------------------------------

class Exporter {
  constructor(cfg) {
    this.cfg = cfg
    this.base = String(cfg.endpoint ?? "").replace(/\/+$/, "")
    this.traces = cfg.signals.includes("traces")
    this.logs = cfg.signals.includes("logs")
    this.spans = []
    this.records = []
    this.timer = null
    this.inflight = Promise.resolve()
    this.headers = null
    this.headersAt = 0
    this.disabled = !this.base
  }

  resource() {
    return {
      attributes: attrs({
        "service.name": "opencode",
        ...this.cfg.resourceAttributes,
      }),
    }
  }

  // authHeaders runs the helper script (the key never sits in this file) or falls back
  // to inline headers; either way the result is cached.
  async authHeaders() {
    const now = Date.now()
    if (this.headers && now - this.headersAt < HEADERS_TTL_MS) return this.headers
    let headers = {}
    if (this.cfg.headersHelper) {
      try {
        const out = await new Promise((resolve, reject) => {
          execFile(this.cfg.headersHelper, [], { timeout: 5000, maxBuffer: 64 * 1024 }, (err, stdout) =>
            err ? reject(err) : resolve(stdout),
          )
        })
        const parsed = JSON.parse(out)
        if (parsed && typeof parsed === "object") headers = parsed
      } catch {
        headers = {}
      }
    } else if (this.cfg.headers && typeof this.cfg.headers === "object") {
      headers = { ...this.cfg.headers }
    }
    this.headers = headers
    this.headersAt = now
    return headers
  }

  span(s) {
    if (this.disabled || !this.traces) return
    this.spans.push(s)
    this.schedule()
  }
  record(r) {
    if (this.disabled || !this.logs) return
    this.records.push(r)
    this.schedule()
  }

  schedule() {
    if (this.spans.length + this.records.length >= BATCH_SIZE) {
      this.flush()
      return
    }
    if (this.timer) return
    this.timer = setTimeout(() => this.flush(), BATCH_DELAY_MS)
    // Never keep the process alive for a pending export.
    if (typeof this.timer.unref === "function") this.timer.unref()
  }

  // flush sends everything queued. Sends are chained so two flushes never race, and a
  // failed batch is retried with backoff rather than dropped or duplicated.
  flush() {
    if (this.timer) {
      clearTimeout(this.timer)
      this.timer = null
    }
    const spans = this.spans
    const records = this.records
    this.spans = []
    this.records = []
    if (spans.length === 0 && records.length === 0) return this.inflight
    this.inflight = this.inflight.then(async () => {
      if (spans.length > 0) {
        await this.post("/v1/traces", {
          resourceSpans: [{ resource: this.resource(), scopeSpans: [{ scope: SCOPE, spans }] }],
        })
      }
      if (records.length > 0) {
        await this.post("/v1/logs", {
          resourceLogs: [{ resource: this.resource(), scopeLogs: [{ scope: SCOPE, logRecords: records }] }],
        })
      }
    })
    return this.inflight
  }

  async post(path, body) {
    const payload = JSON.stringify(body)
    for (let attempt = 0; attempt <= MAX_RETRIES; attempt++) {
      try {
        const auth = await this.authHeaders()
        const res = await fetch(this.base + path, {
          method: "POST",
          headers: { "content-type": "application/json", ...auth },
          body: payload,
        })
        if (res.ok) return
        // A rejected credential will not fix itself by retrying; a 5xx or 429 might.
        if (res.status === 401 || res.status === 403) {
          this.headers = null
          return
        }
        if (res.status < 500 && res.status !== 429) return
      } catch {
        // Network error: retry below.
      }
      await new Promise((r) => setTimeout(r, Math.min(30000, 1000 * 2 ** attempt)))
    }
  }
}

// --- attribution hooks --------------------------------------------------------------

// hookRunner hands session and file events to `terma hook <event>`, fire and forget.
// A terma binary that is not on PATH is noticed once and then left alone: the
// telemetry export does not depend on it, and a spawn error per event would be noise.
function hookRunner(cfg) {
  const argv = Array.isArray(cfg.hookCommand) && cfg.hookCommand.length > 0 ? cfg.hookCommand : ["terma", "hook"]
  let missing = false
  return function run(event, payload) {
    if (missing) return
    try {
      const child = spawn(argv[0], [...argv.slice(1), event], { stdio: ["pipe", "ignore", "ignore"] })
      child.on("error", (err) => {
        if (err && err.code === "ENOENT") missing = true
      })
      child.stdin.on("error", () => {})
      child.stdin.end(JSON.stringify(payload))
      if (typeof child.unref === "function") child.unref()
    } catch {
      // Attribution is best effort.
    }
  }
}

// --- the plugin ---------------------------------------------------------------------

export const TermaPlugin = async ({ directory, worktree }) => {
  const cfg = effectiveConfig(worktree || directory)
  // No config, or per-repo mode in a repository bound to no Terma project: stay inert,
  // exporting nothing and firing no hooks.
  if (!CONFIG || !cfg.endpoint) return {}
  const exporter = new Exporter(cfg)
  const hook = hookRunner(cfg)

  // Per-process bookkeeping, bounded so a long session cannot grow it without limit.
  const seenMessages = new Set() // assistant messages already exported
  const texts = new Map() // messageID -> latest assistant text (only when prompts are on)
  const tools = new Map() // callID -> { sessionID, start }
  let lastSession = "" // the session the most recent activity belonged to
  const remember = (set, key) => {
    set.add(key)
    if (set.size > 4096) set.delete(set.values().next().value)
  }

  function sessionOf(id) {
    if (id) lastSession = id
    return lastSession
  }

  // Sessions the task tool opened for a subagent, by id -> parent id. A child's spans
  // carry the parent too, so the link survives a traces-only policy, which drops the
  // session.created record that is the only other place it is written. Bounded: a
  // long-lived OpenCode opens subagent sessions for as long as it runs.
  const parents = new Map()
  function parentOf(sessionID) {
    return sessionID ? parents.get(sessionID) : undefined
  }

  function record(sessionID, name, body, extra = {}, severity = 9, time = Date.now()) {
    exporter.record({
      timeUnixNano: nanos(time),
      severityNumber: severity,
      severityText: severity >= 17 ? "ERROR" : "INFO",
      body: { stringValue: body ?? "" },
      attributes: attrs({ "event.name": name, "session.id": sessionID, ...extra }),
      traceId: sessionID ? traceId(sessionID) : undefined,
    })
  }

  function modelCall(info) {
    if (!info || info.role !== "assistant" || !info.time || !info.time.completed) return
    if (seenMessages.has(info.id)) return
    remember(seenMessages, info.id)
    const sid = sessionOf(info.sessionID)
    const tokens = info.tokens ?? {}
    const cache = tokens.cache ?? {}
    const attributes = {
      "gen_ai.operation.name": "chat",
      "gen_ai.provider.name": info.providerID,
      "gen_ai.system": info.providerID,
      "gen_ai.request.model": info.modelID,
      "gen_ai.usage.input_tokens": tokens.input,
      "gen_ai.usage.output_tokens": tokens.output,
      "gen_ai.usage.cache_read.input_tokens": cache.read,
      "gen_ai.usage.cache_write.input_tokens": cache.write,
      "gen_ai.usage.total_cost": info.cost,
      "gen_ai.response.finish_reasons": info.finish,
      "opencode.usage.reasoning_tokens": tokens.reasoning,
      "opencode.agent": info.mode,
      "opencode.message.id": info.id,
      "opencode.parent_message.id": info.parentID,
      "opencode.parent_session.id": parentOf(sid),
      "session.id": sid,
    }
    if (cfg.includePrompts) attributes["gen_ai.completion"] = truncate(texts.get(info.id), MAX_TEXT)
    texts.delete(info.id)
    const status = info.error ? { code: 2, message: truncate(info.error.data?.message ?? info.error.name ?? "error", 1024) } : { code: 1 }
    if (info.error) attributes["error.type"] = info.error.name
    exporter.span({
      traceId: traceId(sid),
      spanId: spanId("message:" + info.id),
      name: "chat " + (info.modelID ?? ""),
      kind: 3,
      startTimeUnixNano: nanos(info.time.created ?? info.time.completed),
      endTimeUnixNano: nanos(info.time.completed),
      attributes: attrs(attributes),
      status,
    })
  }

  return {
    event: async ({ event }) => {
      try {
        const p = event?.properties ?? {}
        switch (event?.type) {
          case "session.created": {
            const info = p.info ?? {}
            sessionOf(info.id)
            if (info.id && info.parentID) {
              parents.set(info.id, info.parentID)
              if (parents.size > 256) parents.delete(parents.keys().next().value)
            }
            record(info.id, "opencode.session.created", cfg.includePrompts ? info.title : "", {
              "opencode.project.id": info.projectID,
              "opencode.session.directory": info.directory,
              "opencode.version": info.version,
              // Set when the task tool opened this session for a subagent.
              "opencode.parent_session.id": info.parentID,
            })
            hook("opencode-session-start", { session_id: info.id, cwd: info.directory, parent_session_id: info.parentID })
            break
          }
          case "session.deleted": {
            const info = p.info ?? {}
            record(info.id, "opencode.session.deleted", "")
            hook("opencode-session-end", { session_id: info.id, cwd: info.directory, reason: "deleted" })
            break
          }
          case "session.idle":
            record(sessionOf(p.sessionID), "opencode.session.idle", "")
            break
          case "session.error": {
            const err = p.error ?? {}
            record(sessionOf(p.sessionID), "opencode.session.error", truncate(err.data?.message ?? err.name ?? "error", 4096), {
              "error.type": err.name,
            }, 17)
            break
          }
          case "file.edited":
            if (p.file) hook("opencode-file-edit", { session_id: lastSession, cwd: worktree || directory, file: p.file })
            break
          case "message.updated":
            modelCall(p.info)
            break
          case "message.part.updated": {
            const part = p.part
            if (cfg.includePrompts && part && part.type === "text" && part.messageID) {
              texts.set(part.messageID, part.text)
              if (texts.size > 256) texts.delete(texts.keys().next().value)
            }
            break
          }
        }
      } catch {
        // A telemetry failure must never surface in the agent.
      }
    },

    "chat.message": async (input, output) => {
      try {
        const sid = sessionOf(input?.sessionID)
        const parts = Array.isArray(output?.parts) ? output.parts : []
        const text = parts.filter((x) => x && x.type === "text").map((x) => x.text ?? "").join("\n")
        record(sid, "opencode.user_prompt", cfg.includePrompts ? truncate(text, MAX_TEXT) : "", {
          "opencode.message.id": input?.messageID ?? output?.message?.id,
          "opencode.agent": input?.agent,
          "gen_ai.request.model": input?.model?.modelID,
          "gen_ai.provider.name": input?.model?.providerID,
          "opencode.prompt.length": text.length,
        })
      } catch {}
    },

    "tool.execute.before": async (input) => {
      try {
        if (!input?.callID) return
        tools.set(input.callID, { sessionID: sessionOf(input.sessionID), start: Date.now() })
        if (tools.size > 1024) tools.delete(tools.keys().next().value)
      } catch {}
    },

    "tool.execute.after": async (input, output) => {
      try {
        if (!input?.callID) return
        const started = tools.get(input.callID)
        tools.delete(input.callID)
        const sid = sessionOf(input.sessionID || started?.sessionID)
        const end = Date.now()
        const start = started?.start ?? end
        const args = input.args ?? {}
        const attributes = {
          "gen_ai.operation.name": "execute_tool",
          "gen_ai.tool.name": input.tool,
          "gen_ai.tool.call.id": input.callID,
          "opencode.tool.title": truncate(output?.title, 512),
          "opencode.tool.output_bytes": typeof output?.output === "string" ? output.output.length : 0,
          "opencode.parent_session.id": parentOf(sid),
          "session.id": sid,
        }
        if (cfg.includeToolContent) {
          attributes["gen_ai.tool.call.arguments"] = truncate(JSON.stringify(args), MAX_TOOL_CONTENT)
          attributes["gen_ai.tool.call.result"] = truncate(output?.output, MAX_TOOL_CONTENT)
          if (typeof args.filePath === "string") attributes["opencode.tool.file_path"] = args.filePath
        }
        exporter.span({
          traceId: traceId(sid),
          spanId: spanId("tool:" + input.callID),
          name: "execute_tool " + (input.tool ?? ""),
          kind: 1,
          startTimeUnixNano: nanos(start),
          endTimeUnixNano: nanos(end),
          attributes: attrs(attributes),
          status: { code: 1 },
        })
        // The edit tools name their file in the arguments, which is the reliable route
        // to attribution: unlike file.edited, this event knows its session.
        if ((input.tool === "edit" || input.tool === "write") && typeof args.filePath === "string") {
          hook("opencode-file-edit", { session_id: sid, cwd: worktree || directory, file: args.filePath, tool: input.tool })
        }
      } catch {}
    },

    dispose: async () => {
      try {
        await exporter.flush()
      } catch {}
    },
  }
}
