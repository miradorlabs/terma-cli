// Terma telemetry plugin for OpenCode.
//
// Installed by `terma relay setup` into OpenCode's plugins directory, where it is loaded at
// startup; `terma teardown` removes it. Do not edit — the next setup or update rewrites it.
// CONFIG below is the only part that differs per machine.
//
// What it does: turns OpenCode's own events into OpenTelemetry — one span per model
// call carrying tokens and cost, one span per tool call, and events for prompts and
// session lifecycle — and posts them as OTLP/JSON to the Terma endpoint. The one change
// it makes to a request: an OpenRouter request from a subagent's session names its parent
// session and agent in the body's `trace` field, so OpenRouter's own trace export can place
// the subagent. Nothing else is read or changed. Commit attribution is not done here:
// session start/end and file edits are handed to `terma hook`, where the logic lives.
//
// It is one plugin for both of OpenCode's plugin APIs: OpenCode 1 calls `server`, OpenCode
// 2 calls `setup`, and either sends the same thing for the same session.
//
// Dependency-free on purpose. OpenCode loads directory plugins without installing
// anything for them, so this file uses only what Bun ships: fetch, node:crypto,
// node:fs and node:child_process.

const CONFIG = null /* terma:config */

import { createHash } from "node:crypto"
import { existsSync, readFileSync } from "node:fs"
import { spawn, execFile } from "node:child_process"
import { join, dirname, basename, isAbsolute, resolve } from "node:path"

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
// .terma/settings.json, matching the Go side (project.ResolveDir): a linked git worktree
// with no binding of its own uses its main checkout's, because a new worktree does not
// get a gitignored binding.
function readProjectID(worktree) {
  let dir = worktree
  for (let i = 0; i < 64 && dir; i++) {
    const id = bindingProjectID(dir)
    if (id) return id
    const parent = dirname(dir)
    if (parent === dir) break
    dir = parent
  }
  // No binding on the way up: only now, and only at the nearest git root, try the main
  // checkout of a linked worktree — the same order as ResolveDir (Find, then Resolve).
  dir = worktree
  for (let i = 0; i < 64 && dir; i++) {
    if (existsSync(join(dir, ".git"))) {
      const main = mainCheckout(dir)
      return main ? bindingProjectID(main) : ""
    }
    const parent = dirname(dir)
    if (parent === dir) break
    dir = parent
  }
  return ""
}

// mainCheckout returns the main checkout's root when dir is the root of a linked git
// worktree, else "". Its .git is a file naming the worktree's git directory, whose
// commondir leads to the main .git. It is gitx.LinkedWorktreeFS on the Go side.
function mainCheckout(dir) {
  try {
    const line = readFileSync(join(dir, ".git"), "utf8").split("\n")[0].trim()
    if (!line.startsWith("gitdir:")) return ""
    let gitDir = line.slice("gitdir:".length).trim()
    if (!isAbsolute(gitDir)) gitDir = resolve(dir, gitDir)
    let common = readFileSync(join(gitDir, "commondir"), "utf8").trim()
    if (!common) return ""
    if (!isAbsolute(common)) common = resolve(gitDir, common)
    return basename(common) === ".git" ? dirname(common) : ""
  } catch {
    return ""
  }
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

// A terma that was not there is not tried again for this long: the telemetry export does
// not depend on it, and a spawn error per event would be noise. It is tried again after,
// since an update or a reinstall puts it back while OpenCode runs on.
const MISSING_RETRY_MS = 60 * 1000

// hookRunner hands session and file events to `terma hook <event>`, fire and forget.
function hookRunner(cfg) {
  const argv = Array.isArray(cfg.hookCommand) && cfg.hookCommand.length > 0 ? cfg.hookCommand : ["terma", "hook"]
  let missingUntil = 0
  return function run(event, payload) {
    if (Date.now() < missingUntil) return
    try {
      const child = spawn(argv[0], [...argv.slice(1), event], { stdio: ["pipe", "ignore", "ignore"] })
      child.on("error", (err) => {
        if (err && err.code === "ENOENT") missingUntil = Date.now() + MISSING_RETRY_MS
      })
      child.stdin.on("error", () => {})
      child.stdin.end(JSON.stringify(payload))
      if (typeof child.unref === "function") child.unref()
    } catch {
      // Attribution is best effort.
    }
  }
}

// --- what both plugin APIs send -----------------------------------------------------

// telemetry is everything the plugin sends, in OpenCode's terms but in neither API's
// shapes: the OpenCode 1 hooks and the OpenCode 2 setup below each translate their own
// events into these calls, so one session yields the same spans, records and hook calls
// under either.
function telemetry(cfg) {
  const exporter = new Exporter(cfg)
  const hook = hookRunner(cfg)

  // Bookkeeping, bounded so a long-lived OpenCode cannot grow it without limit.
  const seenMessages = new Set() // assistant messages already exported
  const texts = new Map() // messageID -> latest assistant text (only when prompts are on)
  const tools = new Map() // callID -> { sessionID, start }
  const remember = (set, key) => {
    set.add(key)
    if (set.size > 4096) set.delete(set.values().next().value)
  }

  // Sessions the task tool opened for a subagent, by id -> parent id. A child's spans
  // carry the parent too, so the link survives a traces-only policy, which drops the
  // session.created record that is the only other place it is written.
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

  return {
    parentOf,

    // exported reports whether a model call's span is already sent.
    exported(messageID) {
      return seenMessages.has(messageID)
    },

    sessionCreated(info) {
      this.sessionRecorded(info)
      this.sessionStarted(info.id, info.directory, info.parentID)
    },

    // sessionRecorded is session.created's record, and the parent it names.
    sessionRecorded({ id, parentID, title, projectID, directory, version }) {
      if (id && parentID) {
        parents.set(id, parentID)
        if (parents.size > 256) parents.delete(parents.keys().next().value)
      }
      record(id, "opencode.session.created", cfg.includePrompts ? title : "", {
        "opencode.project.id": projectID,
        "opencode.session.directory": directory,
        "opencode.version": version,
        // Set when the task tool opened this session for a subagent.
        "opencode.parent_session.id": parentID,
      })
    },

    // sessionStarted announces a session to terma, in the directory it works in.
    sessionStarted(id, directory, parentID) {
      hook("opencode-session-start", { session_id: id, cwd: directory, parent_session_id: parentID })
    },

    sessionDeleted(id, directory) {
      record(id, "opencode.session.deleted", "")
      hook("opencode-session-end", { session_id: id, cwd: directory, reason: "deleted" })
    },

    sessionIdle(id) {
      record(id, "opencode.session.idle", "")
    },

    sessionError(id, message, type) {
      record(id, "opencode.session.error", truncate(message, 4096), { "error.type": type }, 17)
    },

    assistantText(messageID, text) {
      if (!cfg.includePrompts || !messageID) return
      texts.set(messageID, text)
      if (texts.size > 256) texts.delete(texts.keys().next().value)
    },

    // modelCall is one completed model call: the assistant message it produced. error,
    // when set, is { message, type }.
    modelCall(m) {
      if (seenMessages.has(m.id)) return
      remember(seenMessages, m.id)
      const tokens = m.tokens ?? {}
      const cache = tokens.cache ?? {}
      const attributes = {
        "gen_ai.operation.name": "chat",
        "gen_ai.provider.name": m.providerID,
        "gen_ai.system": m.providerID,
        "gen_ai.request.model": m.modelID,
        "gen_ai.usage.input_tokens": tokens.input,
        "gen_ai.usage.output_tokens": tokens.output,
        "gen_ai.usage.cache_read.input_tokens": cache.read,
        "gen_ai.usage.cache_write.input_tokens": cache.write,
        "gen_ai.usage.total_cost": m.cost,
        "gen_ai.response.finish_reasons": m.finish,
        "opencode.usage.reasoning_tokens": tokens.reasoning,
        "opencode.agent": m.agent,
        "opencode.message.id": m.id,
        "opencode.parent_message.id": m.parentID,
        "opencode.parent_session.id": parentOf(m.sessionID),
        "session.id": m.sessionID,
      }
      if (cfg.includePrompts) attributes["gen_ai.completion"] = truncate(texts.get(m.id), MAX_TEXT)
      texts.delete(m.id)
      const status = m.error ? { code: 2, message: truncate(m.error.message, 1024) } : { code: 1 }
      if (m.error) attributes["error.type"] = m.error.type
      exporter.span({
        traceId: traceId(m.sessionID),
        spanId: spanId("message:" + m.id),
        name: m.modelID ? "chat " + m.modelID : "chat",
        kind: 3,
        startTimeUnixNano: nanos(m.start ?? m.end),
        endTimeUnixNano: nanos(m.end),
        attributes: attrs(attributes),
        status,
      })
    },

    userPrompt(sessionID, text, { messageID, agent, modelID, providerID }, time) {
      record(sessionID, "opencode.user_prompt", cfg.includePrompts ? truncate(text, MAX_TEXT) : "", {
        "opencode.message.id": messageID,
        "opencode.agent": agent,
        "gen_ai.request.model": modelID,
        "gen_ai.provider.name": providerID,
        "opencode.prompt.length": text.length,
      }, 9, time)
    },

    toolStarted(callID, sessionID) {
      tools.set(callID, { sessionID, start: Date.now() })
      if (tools.size > 1024) tools.delete(tools.keys().next().value)
    },

    // toolStarted's session, when the call was seen starting.
    toolSession(callID) {
      return tools.get(callID)?.sessionID
    },

    // toolAbandoned forgets a call that ended without a result: OpenCode 1 never reports
    // one, so neither does its span.
    toolAbandoned(callID) {
      tools.delete(callID)
    },

    // toolFinished is one tool call's span. output is its text result; filePath, the file
    // it named, absolute.
    toolFinished({ callID, sessionID, tool, args, title, output, filePath }) {
      const started = tools.get(callID)
      tools.delete(callID)
      const end = Date.now()
      const start = started?.start ?? end
      const attributes = {
        "gen_ai.operation.name": "execute_tool",
        "gen_ai.tool.name": tool,
        "gen_ai.tool.call.id": callID,
        "opencode.tool.title": truncate(title, 512),
        "opencode.tool.output_bytes": typeof output === "string" ? output.length : 0,
        "opencode.parent_session.id": parentOf(sessionID),
        "session.id": sessionID,
      }
      if (cfg.includeToolContent) {
        attributes["gen_ai.tool.call.arguments"] = truncate(JSON.stringify(args), MAX_TOOL_CONTENT)
        attributes["gen_ai.tool.call.result"] = truncate(output, MAX_TOOL_CONTENT)
        if (typeof filePath === "string") attributes["opencode.tool.file_path"] = filePath
      }
      exporter.span({
        traceId: traceId(sessionID),
        spanId: spanId("tool:" + callID),
        name: "execute_tool " + (tool ?? ""),
        kind: 1,
        startTimeUnixNano: nanos(start),
        endTimeUnixNano: nanos(end),
        attributes: attrs(attributes),
        status: { code: 1 },
      })
    },

    fileEdited(sessionID, cwd, file, tool) {
      hook("opencode-file-edit", { session_id: sessionID, cwd, file, tool })
    },

    // openRouterTrace is the `trace` object a subagent's OpenRouter request carries:
    // OpenRouter's trace export reports it as trace.metadata.*, and nothing else the
    // request sends names the parent. Keys the developer configured are kept.
    openRouterTrace(sessionID, agent, ...configured) {
      const parent = parentOf(sessionID)
      if (!parent) return undefined
      return Object.assign({}, ...configured, { parent_session_id: parent, agent })
    },

    flush() {
      return exporter.flush()
    },
  }
}

// The edit tools: their file is the one the call names.
const EDIT_TOOLS = ["edit", "write"]

// --- OpenCode 1 ---------------------------------------------------------------------

// RUNNING are the directories server runs for in this process, shared by every copy of the
// module OpenCode loads.
const RUNNING = (globalThis[Symbol.for("terma.opencode.server")] ??= new Set())

// server is the OpenCode 1 plugin: hooks keyed by name, OpenCode's bus events through
// `event`.
async function server({ directory, worktree }) {
  const cfg = effectiveConfig(worktree || directory)
  // No config, or per-repo mode in a repository bound to no Terma project: stay inert,
  // exporting nothing and firing no hooks.
  if (!CONFIG || !cfg.endpoint) return {}
  // One OpenCode 1 that called both the named export and the default's server would
  // report everything twice: the second call for a directory still running is inert.
  const key = `${worktree || ""}\0${directory || ""}`
  if (RUNNING.has(key)) return {}
  RUNNING.add(key)
  const t = telemetry(cfg)

  let lastSession = "" // the session the most recent activity belonged to
  function sessionOf(id) {
    if (id) lastSession = id
    return lastSession
  }

  function modelCall(info) {
    if (!info || info.role !== "assistant" || !info.time || !info.time.completed) return
    if (t.exported(info.id)) return
    const error = info.error ? { message: info.error.data?.message ?? info.error.name ?? "error", type: info.error.name } : undefined
    t.modelCall({
      id: info.id,
      sessionID: sessionOf(info.sessionID),
      providerID: info.providerID,
      modelID: info.modelID,
      agent: info.mode,
      parentID: info.parentID,
      tokens: info.tokens,
      cost: info.cost,
      finish: info.finish,
      error,
      start: info.time.created,
      end: info.time.completed,
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
            t.sessionCreated({ id: info.id, parentID: info.parentID, title: info.title, projectID: info.projectID, directory: info.directory, version: info.version })
            break
          }
          case "session.deleted": {
            const info = p.info ?? {}
            t.sessionDeleted(info.id, info.directory)
            break
          }
          case "session.idle":
            t.sessionIdle(sessionOf(p.sessionID))
            break
          case "session.error": {
            const err = p.error ?? {}
            t.sessionError(sessionOf(p.sessionID), err.data?.message ?? err.name ?? "error", err.name)
            break
          }
          case "file.edited":
            if (p.file) t.fileEdited(lastSession, worktree || directory, p.file)
            break
          case "message.updated":
            modelCall(p.info)
            break
          case "message.part.updated": {
            const part = p.part
            if (part && part.type === "text") t.assistantText(part.messageID, part.text)
            break
          }
        }
      } catch {
        // A telemetry failure must never surface in the agent.
      }
    },

    // The SDK spreads these options over the provider's extraBody, so its trace keys are kept.
    "chat.params": async (input, output) => {
      try {
        if (input?.model?.api?.npm !== "@openrouter/ai-sdk-provider" || !output?.options) return
        const trace = t.openRouterTrace(input?.sessionID, input.agent, input.provider?.options?.extraBody?.trace, output.options.trace)
        if (trace) output.options.trace = trace
      } catch {}
    },

    "chat.message": async (input, output) => {
      try {
        const sid = sessionOf(input?.sessionID)
        const parts = Array.isArray(output?.parts) ? output.parts : []
        const text = parts.filter((x) => x && x.type === "text").map((x) => x.text ?? "").join("\n")
        t.userPrompt(sid, text, {
          messageID: input?.messageID ?? output?.message?.id,
          agent: input?.agent,
          modelID: input?.model?.modelID,
          providerID: input?.model?.providerID,
        })
      } catch {}
    },

    "tool.execute.before": async (input) => {
      try {
        if (!input?.callID) return
        t.toolStarted(input.callID, sessionOf(input.sessionID))
      } catch {}
    },

    "tool.execute.after": async (input, output) => {
      try {
        if (!input?.callID) return
        const sid = sessionOf(input.sessionID || t.toolSession(input.callID))
        const args = input.args ?? {}
        t.toolFinished({
          callID: input.callID,
          sessionID: sid,
          tool: input.tool,
          args,
          title: output?.title,
          output: output?.output,
          filePath: args.filePath,
        })
        // The edit tools name their file in the arguments, which is the reliable route
        // to attribution: unlike file.edited, this event knows its session.
        if (EDIT_TOOLS.includes(input.tool) && typeof args.filePath === "string") {
          t.fileEdited(sid, worktree || directory, args.filePath, input.tool)
        }
      } catch {}
    },

    dispose: async () => {
      RUNNING.delete(key)
      try {
        await t.flush()
      } catch {}
    },
  }
}

// --- OpenCode 2 ---------------------------------------------------------------------

// OpenRouter's provider id: the context hook below runs for its requests alone.
const OPENROUTER = "openrouter"

// A patch call's file headers: the files it adds, updates, deletes and moves to.
const PATCH_FILE = /^\*\*\* (?:Add File|Update File|Delete File|Move to): (.+)$/

// setup is the OpenCode 2 plugin. One shared OpenCode server runs every session on the
// machine and calls setup once per location (a directory OpenCode was opened in). Every
// instance hears every event, so events are taken only for this location's sessions.
// Hooks reach the instance of the session's own location alone (2.0.30 runs a tool's
// hooks once with two locations open), and a session an event placed elsewhere is
// refused there too. The returned cleanup flushes.
async function setup(ctx) {
  // OpenCode 1 from 1.18 calls setup too, with a context that has none of these: there the
  // plugin is server's alone.
  const location = ctx?.location
  if (!location?.directory || typeof ctx.event?.subscribe !== "function" || typeof ctx.session?.hook !== "function" || typeof ctx.tool?.hook !== "function") {
    return undefined
  }
  const here = location.directory
  // The checkout's root, which the OpenCode 1 plugin was handed as its worktree.
  const root = location.project?.directory || here
  const cfg = effectiveConfig(root)
  if (!CONFIG || !cfg.endpoint) return undefined
  const t = telemetry(cfg)

  // Bounded, oldest out first; a session in use is moved to the back.
  const LIMIT = 1024
  const keep = (map, key, value) => {
    map.delete(key)
    map.set(key, value)
    if (map.size > LIMIT) map.delete(map.keys().next().value)
  }
  // This location's sessions, by id -> the directory each runs in. session.deleted names
  // only the session, and tool paths may be relative to it.
  const own = new Map()
  const elsewhere = new Map() // sessions an event placed in another location
  const announced = new Map() // sessions terma has been told of, by session-start

  function claim(sessionID, directory) {
    if (!sessionID) return false
    if (directory) elsewhere.delete(sessionID)
    else if (elsewhere.has(sessionID)) return false
    keep(own, sessionID, directory || own.get(sessionID) || here)
    return true
  }
  function release(sessionID) {
    own.delete(sessionID)
    announced.delete(sessionID)
    keep(elsewhere, sessionID, true)
  }
  // mine reports whether an event belongs to this location: one that places its session
  // here, or names a session claimed here already.
  function mine(event) {
    const sessionID = event?.data?.sessionID
    if (!sessionID) return false
    const at = event.location?.directory
    if (at && at !== here) {
      if (own.has(sessionID)) release(sessionID)
      else keep(elsewhere, sessionID, true)
      return false
    }
    if (at || own.has(sessionID)) return claim(sessionID)
    return false
  }
  // announce tells terma of a session it has not heard of: one forked, or resumed after
  // the server restarted, which OpenCode does not create again. A fork is not a subagent,
  // so it names no parent.
  function announce(sessionID, parentID) {
    if (announced.has(sessionID)) return
    keep(announced, sessionID, true)
    t.sessionStarted(sessionID, own.get(sessionID) ?? here, parentID)
  }

  // A model call is a step: its start names the model and agent, its end the usage.
  const steps = new Map() // assistantMessageID -> { agent, model, start }
  // A prompt's record names the model that answers it, which the first step names, so it
  // waits for that step, or for the turn to end without one.
  const prompts = new Map() // sessionID -> { text, messageID, time }
  const idled = new Map() // sessions whose turn has ended and been recorded

  function promptAnswered(sessionID, agent, model) {
    const p = prompts.get(sessionID)
    if (!p) return
    prompts.delete(sessionID)
    t.userPrompt(sessionID, p.text, { messageID: p.messageID, agent, modelID: model?.id, providerID: model?.providerID }, p.time)
  }

  // turnEnded is OpenCode 1's session.idle: OpenCode 2 ends a turn with an execution's
  // outcome, and may also say idle; the turn is recorded once.
  function turnEnded(sessionID) {
    promptAnswered(sessionID)
    if (idled.has(sessionID)) return
    keep(idled, sessionID, true)
    t.sessionIdle(sessionID)
  }

  // stepEnded is the model call's span. A step whose start this instance missed still
  // sends its usage and cost, naming no model. OpenCode 2's messages name no parent
  // message, so the span names none.
  function stepEnded(event, error) {
    const d = event.data
    const step = steps.get(d.assistantMessageID) ?? {}
    steps.delete(d.assistantMessageID)
    t.modelCall({
      id: d.assistantMessageID,
      sessionID: d.sessionID,
      providerID: step.model?.providerID,
      modelID: step.model?.id,
      agent: step.agent,
      tokens: d.tokens,
      cost: d.cost,
      finish: d.finish,
      error,
      start: step.start,
      end: event.created ?? Date.now(),
    })
  }

  function onEvent(event) {
    const d = event?.data ?? {}
    switch (event?.type) {
      case "session.created": {
        const at = d.location?.directory || event.location?.directory
        if (at !== here) {
          if (d.sessionID) keep(elsewhere, d.sessionID, true)
          return
        }
        claim(d.sessionID, d.subpath ? join(at, d.subpath) : at)
        t.sessionRecorded({ id: d.sessionID, parentID: d.parentID, title: d.title, projectID: d.projectID, directory: own.get(d.sessionID), version: d.version })
        announce(d.sessionID, d.parentID)
        return
      }
      case "session.moved": {
        // A session moved here is announced where it now works; one moved away is
        // another location's from now on.
        const at = d.location?.directory
        if (!d.sessionID || !at) return
        if (at !== here) {
          if (own.has(d.sessionID)) release(d.sessionID)
          return
        }
        announced.delete(d.sessionID)
        claim(d.sessionID, d.subpath ? join(at, d.subpath) : at)
        announce(d.sessionID)
        return
      }
      case "session.forked":
        if (mine(event)) announce(d.sessionID)
        return
      case "session.deleted":
        if (!mine(event)) return
        t.sessionDeleted(d.sessionID, own.get(d.sessionID))
        own.delete(d.sessionID)
        announced.delete(d.sessionID)
        return
      case "session.execution.started":
        if (mine(event)) idled.delete(d.sessionID)
        return
      case "session.execution.failed":
        if (!mine(event)) return
        promptAnswered(d.sessionID)
        t.sessionError(d.sessionID, d.error?.message ?? d.error?.type ?? "error", d.error?.type)
        turnEnded(d.sessionID)
        return
      case "session.execution.succeeded":
      case "session.execution.interrupted":
      case "session.idle":
        if (mine(event)) turnEnded(d.sessionID)
        return
      case "session.step.started":
        if (!mine(event)) return
        // The stream carries session.created ahead of a session's first step, so a
        // session not announced by now was not created here.
        announce(d.sessionID)
        keep(steps, d.assistantMessageID, { agent: d.agent, model: d.model, start: d.started ?? event.created })
        promptAnswered(d.sessionID, d.agent, d.model)
        return
      case "session.text.ended":
        if (mine(event)) t.assistantText(d.assistantMessageID, d.text)
        return
      case "session.step.ended":
        if (mine(event)) stepEnded(event)
        return
      case "session.step.failed":
        if (mine(event)) stepEnded(event, { message: d.error?.message ?? d.error?.type ?? "error", type: d.error?.type })
        return
    }
  }

  // The text a tool call returned, as the OpenCode 1 hook's output string.
  function toolOutput(call) {
    const content = call.result?.content
    if (typeof content === "string") return content
    if (Array.isArray(content)) {
      return content.filter((c) => c && c.type === "text").map((c) => c.text ?? "").join("\n")
    }
    return typeof call.result?.output === "string" ? call.result.output : undefined
  }

  // inSession makes a tool's path absolute: relative ones are the session's directory's.
  function inSession(sessionID, path) {
    return isAbsolute(path) ? path : resolve(own.get(sessionID) || here, path)
  }

  // The file an edit or write call named, absolute: the call takes a path, and a write
  // reports the file it wrote.
  function toolFile(call) {
    const target = call.result?.output?.target
    if (typeof target === "string" && isAbsolute(target)) return target
    const path = call.input?.path
    if (typeof path !== "string" || path === "") return undefined
    return inSession(call.sessionID, path)
  }

  // The files a patch call changed, each once: those it reports applying, and those its
  // text names, which include a moved file's old path.
  function patchFiles(call) {
    const files = new Set()
    for (const a of Array.isArray(call.result?.output?.applied) ? call.result.output.applied : []) {
      if (typeof a?.target === "string" && isAbsolute(a.target)) files.add(resolve(a.target))
    }
    const text = call.input?.patchText
    if (typeof text === "string") {
      for (const line of text.split("\n")) {
        const m = PATCH_FILE.exec(line.trim())
        if (m && m[1].trim()) files.add(inSession(call.sessionID, m[1].trim()))
      }
    }
    return [...files]
  }

  const register = async (domain, name, callback, options) => {
    try {
      await domain.hook(name, callback, options)
    } catch {
      // A hook this OpenCode does not offer: the rest still report.
    }
  }

  await register(ctx.session, "prompt", (input) => {
    try {
      if (!claim(input?.sessionID)) return
      idled.delete(input.sessionID)
      keep(prompts, input.sessionID, { text: input.prompt?.text ?? "", messageID: input.messageID, time: Date.now() })
    } catch {}
  })

  // OpenCode 2's provider options are the request's own: a configured trace is in them.
  await register(ctx.session, "context", (input) => {
    try {
      if (input?.model?.providerID !== OPENROUTER || !input.options || !claim(input.sessionID)) return
      const trace = t.openRouterTrace(input.sessionID, input.agent, input.options.trace)
      if (trace) input.options.trace = trace
    } catch {}
  }, { providerID: OPENROUTER })

  await register(ctx.tool, "execute.before", (call) => {
    try {
      if (!call?.id || !claim(call.sessionID)) return
      announce(call.sessionID)
      t.toolStarted(call.id, call.sessionID)
    } catch {}
  })

  await register(ctx.tool, "execute.after", (call) => {
    try {
      if (!call?.id || !claim(call.sessionID)) return
      // OpenCode 1 reports no call that failed, so neither span nor edit is sent for one.
      if (call.status !== "completed") {
        t.toolAbandoned(call.id)
        return
      }
      announce(call.sessionID)
      const edited = call.tool === "patch" ? patchFiles(call) : EDIT_TOOLS.includes(call.tool) ? [toolFile(call)].filter(Boolean) : []
      t.toolFinished({
        callID: call.id,
        sessionID: call.sessionID,
        tool: call.tool,
        args: call.input ?? {},
        title: call.result?.metadata?.title,
        output: toolOutput(call),
        filePath: call.tool === "patch" ? undefined : edited[0],
      })
      for (const file of edited) t.fileEdited(call.sessionID, root, file, call.tool)
    } catch {}
  })

  // Events arrive on a stream that can drop; it is reopened until cleanup.
  const stop = new AbortController()
  ;(async () => {
    while (!stop.signal.aborted) {
      try {
        for await (const event of ctx.event.subscribe({ signal: stop.signal })) {
          try {
            onEvent(event)
          } catch {}
        }
      } catch {}
      if (stop.signal.aborted) break
      await new Promise((r) => {
        const timer = setTimeout(r, 1000)
        if (typeof timer.unref === "function") timer.unref()
      })
    }
  })()

  return async () => {
    stop.abort()
    try {
      for (const sessionID of [...prompts.keys()]) promptAnswered(sessionID)
      await t.flush()
    } catch {}
  }
}

// One module for every OpenCode: OpenCode 2 runs the default export's setup; OpenCode 1
// from 1.3.17 runs its server and ignores the named export, and from 1.2.11 to then runs
// the named export alone. Each registers the hooks once.
export const TermaPlugin = (input) => server(input)
export default { id: "terma", setup, server }
