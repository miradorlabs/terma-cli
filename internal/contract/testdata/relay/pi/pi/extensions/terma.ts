// Terma extension for Pi (@earendil-works/pi-coding-agent).
//
// Written by `terma relay setup --harness pi` into ~/.pi/agent/extensions/terma.ts; do not
// edit — the next setup rewrites it. CONFIG below is the only part that differs per machine
// (the raw template ships `CONFIG = null` and is inert).
//
// Pi has no OpenTelemetry of its own, so this extension is its exporter, following the
// GenAI semantic conventions under scope `terma-pi` version 2:
// - one `chat {model}` span per model call, built at `turn_end` from Pi's own message, entry
//   id and clock, and one per compaction and per other spend Pi stores in the session;
// - one `execute_tool {tool}` span per tool call, from `tool_execution_start` and `_end`,
//   held until the `turn_end` of the call that issued it;
// - a log record per prompt, per message an extension put into the conversation and per
//   session name, each read from the session's own entries, with the entry's id and time.
// Every span and record names its session (`session.id`, Pi's own id), the Pi build and
// mode, and, where they apply, the session a subagent works for and the one a fork came
// from. Ids are derived, never random, so a redelivered export is the same export.
//
// Session lifecycle, prompts, what a tool reaches and file edits are handed to
// `terma hook pi-*`, which claims the session for the local relay and records commit
// attribution. The relay decides what leaves the machine and withholds content per team;
// this extension sends prompt and tool content only as CONFIG allows.
//
// Pi runs the factory again for every session a process opens, a session nested inside a
// process shares it, and `/reload` evaluates this module again from scratch. So everything
// the extension keeps, queues and timers included, is data in one object on globalThis,
// shared by every copy of the module; a copy evaluated later works on it with its own code.
//
// Dependency-free: Pi loads extensions with jiti, and this file uses only Node's fetch and
// node:crypto, node:child_process, node:fs, node:os, node:path and node:url.

const CONFIG: TermaConfig | null = {"version":1,"endpoint":"http://127.0.0.1:43180","headers":{"Authorization":"Bearer relay-token"},"includePrompts":true,"includeToolContent":true,"hookCommand":["/usr/local/bin/terma","hook"]} /* terma:config */

import { createHash } from "node:crypto"
import { execFileSync, spawn } from "node:child_process"
import { closeSync, openSync, readFileSync, readSync } from "node:fs"
import { homedir } from "node:os"
import { join, resolve } from "node:path"
import { fileURLToPath } from "node:url"

type TermaConfig = {
  version: 1
  endpoint: string
  headers: Record<string, string>
  includePrompts: boolean
  includeToolContent: boolean
  hookCommand: string[]
}

// --- limits -------------------------------------------------------------------------

// The oldest Pi this extension records: 1.1.0 brought `durationMs`, 1.5.0 the flag that tells
// a provider's billed cost from Pi's own estimate.
const MIN_PI = "1.5.0"

// Bodies are bounded so a single reply or tool result cannot make an export enormous.
const MAX_TEXT = 64 * 1024
const MAX_TOOL_CONTENT = 16 * 1024
const MAX_ERROR = 1024
// The exporter batches: this many items, or this long after the first, whichever first.
const BATCH_SIZE = 64
const BATCH_DELAY_MS = 1000
// A failed post is sent again, the same bytes, after 1 s, doubling, this many times.
const MAX_RETRIES = 5
// Batches waiting while the relay does not answer; the oldest go first.
const MAX_PENDING_BATCHES = 32
const MAX_PENDING_BYTES = 8 * 1024 * 1024
const POST_TIMEOUT_MS = 10000
// A hook that hangs is terminated, so the queue behind it moves on.
const HOOK_TIMEOUT_MS = 2000
const HOOK_KILL_GRACE_MS = 250
const MAX_HOOK_QUEUE = 64
// How long leaving a session waits for its end hook and the last export together.
const EXIT_MS = 4000
// What the extension remembers is bounded, so a long-running process cannot grow it forever.
const MAX_SESSIONS = 256
const MAX_TOOLS = 1024
const MAX_HELD = 1024
// How far a walk up the session tree goes, and how far the walk for a tool call's issuer.
const MAX_WALK = 100000
const MAX_ISSUER_WALK = 256
// The parent link: how many processes up the walk looks, and how long `ps` may take.
const MAX_ANCESTORS = 32
const PS_TIMEOUT_MS = 1000
// A fork's source is the id in the first line of its file, read at most this far.
const MAX_HEADER = 16 * 1024

const AGENT = "pi"
const SCOPE = { name: "terma-pi", version: "2" }
const SPAN_KIND_INTERNAL = 1
const SPAN_KIND_CLIENT = 3
const FILE_TOOLS = new Set(["read", "write", "edit"])
const EDIT_TOOLS = new Set(["write", "edit"])
// The shell command a session that failed closed sends as its reach: one terma cannot
// follow, so the hook marks the run Mixed. OMP's and OpenCode's extensions send the same one.
const REACH_FAILED_COMMAND = "eval $TERMA_REACH_FAILED"
// Pi turns each of these into a plain space before it resolves a path.
const UNICODE_SPACES = /[\u00A0\u2000-\u200A\u202F\u205F\u3000]/g

// --- state --------------------------------------------------------------------------

// A tool call between its `tool_execution_start` and `_end`.
type Tool = {
  name: string
  args: unknown
  start: number
  // The assistant entry that issued it, found at its start; "" when not found.
  entry: string
  files: string[] | null
}

// A tool span waiting for the `turn_end` of the call that issued it.
type Held = {
  toolCallId: string
  entry: string
  name: string
  start: number
  end: number
  attrs: Record<string, unknown>
  error: boolean
}

type Session = {
  id: string
  // How many of getEntries() have been read.
  read: number
  mode: string
  forkedFrom: string
  // Whether a session_start opened it here; only then does it carry the inherited link.
  started: boolean
  parent: string
  tools: Map<string, Tool>
  held: Held[]
  // What session_before_compact kept for the next compaction entry.
  compact: { reason: string; at: number } | null
  // The fence, as OMP's extension keeps it: the run's generation, whether the session
  // failed closed, the generation whose run terma has marked Mixed, the fence being set,
  // and whether the last try to set it failed.
  gen: number
  tainted: boolean
  fenced: number
  fencing: Promise<boolean> | null
  unfenced: boolean
}

type Batch = { cfg: TermaConfig; sends: [string, string][]; items: number; bytes: number }

type State = {
  config: TermaConfig | null
  version: string
  exporter: {
    spans: unknown[]
    records: unknown[]
    timer: any
    batches: Batch[]
    pendingBytes: number
    draining: boolean
    inflight: Promise<void>
    dropped: number
  }
  hooks: { queue: Promise<void>; pending: number }
  // TERMA_PI_PARENT as this process inherited it, read before anything here writes it, and
  // the parent session it names once judged ("" none; null not judged yet).
  inherited: string
  link: string | null
  sessions: Map<string, Session>
}

// The one shared object. A change to its shape bumps the number in the key, so a copy of
// this file never reads another shape.
const KEY = Symbol.for("terma.pi.state/1")
const st: State = ((globalThis as any)[KEY] ??= newState())

function newState(): State {
  let inherited = ""
  try {
    inherited = typeof process.env.TERMA_PI_PARENT === "string" ? process.env.TERMA_PI_PARENT : ""
  } catch {}
  return {
    config: null,
    version: "",
    exporter: {
      spans: [],
      records: [],
      timer: null,
      batches: [],
      pendingBytes: 0,
      draining: false,
      inflight: Promise.resolve(),
      dropped: 0,
    },
    hooks: { queue: Promise.resolve(), pending: 0 },
    inherited,
    link: null,
    sessions: new Map(),
  }
}

function remember<V>(coll: Map<string, V>, key: string, value: V, max: number) {
  coll.delete(key)
  coll.set(key, value)
  while (coll.size > max) coll.delete(coll.keys().next().value as string)
}

// within settles with p, or with fallback after ms. Its timer does not keep the process
// alive, unless hold asks it to: leaving a session waits for its last export, and a retry
// that waits on its own unref'd timer must still get its turn within that bound.
function within<T>(p: Promise<T>, ms: number, fallback: T, hold = false): Promise<T> {
  let t: any
  return Promise.race([
    p,
    new Promise<T>((r) => {
      t = setTimeout(() => r(fallback), ms)
      if (!hold) t?.unref?.()
    }),
  ]).finally(() => clearTimeout(t))
}

function sleep(ms: number): Promise<void> {
  return new Promise((r) => {
    const t: any = setTimeout(r, ms)
    t?.unref?.()
  })
}

// --- values -------------------------------------------------------------------------

function str(v: unknown): string {
  return typeof v === "string" ? v : ""
}

function num(v: unknown): number | undefined {
  return typeof v === "number" && Number.isFinite(v) ? v : undefined
}

function cut(s: string, max: number): string {
  return s.length > max ? s.slice(0, max) : s
}

// textOf is the text parts of a message's content, in order, joined by a newline; content
// may be a plain string. Thinking parts are never read.
function textOf(content: unknown): string {
  if (typeof content === "string") return content
  if (!Array.isArray(content)) return ""
  return content
    .filter((p: any) => p && p.type === "text" && typeof p.text === "string")
    .map((p: any) => p.text)
    .join("\n")
}

// timeOf is an entry's own time, in ms.
function timeOf(e: any): number {
  const t = typeof e?.timestamp === "string" ? Date.parse(e.timestamp) : num(e?.timestamp)
  return t !== undefined && Number.isFinite(t) ? t : Date.now()
}

function atLeast(version: string, min: string): boolean {
  const parse = (v: string) => {
    const m = /^(\d+)\.(\d+)\.(\d+)/.exec(v)
    return m ? [Number(m[1]), Number(m[2]), Number(m[3])] : null
  }
  const a = parse(version)
  const b = parse(min)
  if (!a || !b) return false
  for (let i = 0; i < 3; i++) if (a[i] !== b[i]) return a[i] > b[i]
  return true
}

// --- ids and encoding ---------------------------------------------------------------

function sha(s: string): string {
  return createHash("sha256").update(s).digest("hex")
}

// traceId is stable per session, so a session's spans form one trace.
function traceId(session: string): string {
  return sha(AGENT + ":" + session).slice(0, 32)
}

function callSpanId(session: string, callId: string): string {
  return sha("terma-pi-span:call:" + session + ":" + callId).slice(0, 16)
}

function toolSpanId(toolCallId: string): string {
  return sha("terma-pi-span:tool:" + toolCallId).slice(0, 16)
}

// qualify names a tool call by its session and the model call that issued it: entry ids
// are unique only inside one session, and some providers repeat their own call ids.
function qualify(session: string, callId: string, toolCallId: string): string {
  return session + ":" + callId + ":" + toolCallId
}

function nanos(ms: number): string {
  return (BigInt(Math.round(ms)) * 1000000n).toString()
}

// A number sent as a double whatever its value (a cost of 0 or 1 included).
type Double = { __double: number }

function double(n: number): Double {
  return { __double: n }
}

type AnyValue = {
  stringValue?: string
  intValue?: string
  doubleValue?: number
  boolValue?: boolean
  arrayValue?: { values: { stringValue: string }[] }
}

// attr renders one OTLP attribute. false and 0 are sent; undefined, null, "" and an empty
// list are not.
function attr(key: string, v: unknown): { key: string; value: AnyValue } | null {
  if (v === undefined || v === null || v === "") return null
  switch (typeof v) {
    case "string":
      return { key, value: { stringValue: v } }
    case "boolean":
      return { key, value: { boolValue: v } }
    case "number":
      if (!Number.isFinite(v)) return null
      return Number.isInteger(v) ? { key, value: { intValue: String(v) } } : { key, value: { doubleValue: v } }
    case "object": {
      if (Array.isArray(v)) {
        const values = v.filter((x) => typeof x === "string").map((x) => ({ stringValue: x as string }))
        return values.length > 0 ? { key, value: { arrayValue: { values } } } : null
      }
      const d = (v as Double).__double
      return typeof d === "number" && Number.isFinite(d) ? { key, value: { doubleValue: d } } : null
    }
    default:
      return null
  }
}

function attrs(m: Record<string, unknown>) {
  const out: { key: string; value: AnyValue }[] = []
  for (const [k, v] of Object.entries(m)) {
    const a = attr(k, v)
    if (a) out.push(a)
  }
  return out
}

// --- exporter -----------------------------------------------------------------------

function exportItem(kind: "spans" | "records", item: unknown) {
  const ex = st.exporter
  ex[kind].push(item)
  if (ex.spans.length + ex.records.length >= BATCH_SIZE) {
    void flush()
    return
  }
  if (ex.timer) return
  ex.timer = setTimeout(() => void flush(), BATCH_DELAY_MS)
  // Never keep the process alive for a pending export.
  ex.timer?.unref?.()
}

// flush queues everything collected as one batch, its request bodies serialised now, and
// returns a promise that resolves once every batch queued so far is sent or given up on.
// One drain sends them in order, and a failed batch is retried with the same bytes. While
// the relay does not answer, the oldest waiting batches are dropped.
function flush(): Promise<void> {
  const ex = st.exporter
  if (ex.timer) {
    clearTimeout(ex.timer)
    ex.timer = null
  }
  const spans = ex.spans
  const records = ex.records
  ex.spans = []
  ex.records = []
  const cfg = st.config
  if ((spans.length === 0 && records.length === 0) || !cfg) return ex.inflight
  const resource = { attributes: attrs({ "service.name": AGENT, "telemetry.sdk.name": "terma-" + AGENT }) }
  const sends: [string, string][] = []
  if (spans.length > 0) sends.push(["/v1/traces", JSON.stringify({ resourceSpans: [{ resource, scopeSpans: [{ scope: SCOPE, spans }] }] })])
  if (records.length > 0) sends.push(["/v1/logs", JSON.stringify({ resourceLogs: [{ resource, scopeLogs: [{ scope: SCOPE, logRecords: records }] }] })])
  const bytes = sends.reduce((n, [, payload]) => n + Buffer.byteLength(payload), 0)
  ex.batches.push({ cfg, sends, items: spans.length + records.length, bytes })
  ex.pendingBytes += bytes
  while (ex.batches.length > MAX_PENDING_BATCHES || (ex.pendingBytes > MAX_PENDING_BYTES && ex.batches.length > 1)) {
    const old = ex.batches.shift()!
    ex.pendingBytes -= old.bytes
    ex.dropped += old.items
  }
  if (!ex.draining) {
    ex.draining = true
    ex.inflight = drain()
  }
  return ex.inflight
}

async function drain() {
  const ex = st.exporter
  try {
    while (ex.batches.length > 0) {
      const { cfg, sends, bytes } = ex.batches.shift()!
      ex.pendingBytes -= bytes
      for (const [path, payload] of sends) {
        try {
          await post(cfg, path, payload)
        } catch {}
      }
    }
  } finally {
    ex.draining = false
  }
}

async function post(cfg: TermaConfig, path: string, payload: string) {
  if (!cfg.endpoint) return
  for (let attempt = 0; attempt <= MAX_RETRIES; attempt++) {
    try {
      const res = await fetch(cfg.endpoint.replace(/\/+$/, "") + path, {
        method: "POST",
        headers: { "content-type": "application/json", ...cfg.headers },
        body: payload,
        signal: AbortSignal.timeout(POST_TIMEOUT_MS),
      })
      await res.arrayBuffer().catch(() => {})
      if (res.ok) return
      // A rejected credential will not fix itself by retrying; a 5xx or 429 might.
      if (res.status === 401 || res.status === 403) return
      if (res.status < 500 && res.status !== 429) return
    } catch {
      // Network error: retry below. A telemetry failure never surfaces in the agent.
    }
    if (attempt < MAX_RETRIES) await sleep(Math.min(30000, 1000 * 2 ** attempt))
  }
}

// common are the attributes every span and record of a session carries.
function common(s: Session): Record<string, unknown> {
  return {
    "session.id": s.id,
    "pi.version": st.version,
    "pi.mode": s.mode,
    "pi.parent_session.id": s.parent,
    "pi.session.forked_from.id": s.forkedFrom,
  }
}

// emitSpan queues a span; a session that failed closed exports nothing more.
function emitSpan(s: Session, spanId: string, name: string, kind: number, start: number, end: number, a: Record<string, unknown>, error: boolean) {
  if (s.tainted) return
  exportItem("spans", {
    traceId: traceId(s.id),
    spanId,
    name,
    kind,
    startTimeUnixNano: nanos(start),
    endTimeUnixNano: nanos(Math.max(start, end)),
    attributes: attrs({ ...common(s), ...a }),
    status: error ? { code: 2 } : {},
  })
}

// emitRecord queues a log record stamped with its entry's own time, never the clock.
function emitRecord(s: Session, event: string, body: string, a: Record<string, unknown>, time: number) {
  if (s.tainted) return
  const t = nanos(time)
  exportItem("records", {
    timeUnixNano: t,
    observedTimeUnixNano: t,
    severityNumber: 9,
    severityText: "INFO",
    body: { stringValue: body },
    attributes: attrs({ "event.name": event, ...common(s), ...a }),
    traceId: traceId(s.id),
  })
}

// --- terma hook ---------------------------------------------------------------------

// spawnHook runs `terma hook <event>` with the payload on stdin. result settles with whether
// it exited cleanly in time; exit once the child has exited, so the queue behind it moves
// only then. A timed-out child is terminated.
function spawnHook(cfg: TermaConfig, event: string, payload: Record<string, unknown>): { result: Promise<boolean>; exit: Promise<void> } {
  let resolveExit!: () => void
  const exit = new Promise<void>((r) => {
    resolveExit = r
  })
  const result = new Promise<boolean>((done) => {
    let settled = false
    let exited = false
    let child: ReturnType<typeof spawn> | null = null
    let killTimer: any
    const finish = (ok: boolean) => {
      if (!settled) {
        settled = true
        clearTimeout(timer)
        clearTimeout(killTimer)
        done(ok)
      }
    }
    const markExited = () => {
      if (!exited) {
        exited = true
        clearTimeout(timer)
        clearTimeout(killTimer)
        resolveExit()
      }
    }
    const sendKill = (sig: NodeJS.Signals) => {
      try {
        child?.kill(sig)
      } catch (err: any) {
        if (err?.code === "ESRCH") markExited()
      }
    }
    const timer: any = setTimeout(() => {
      if (!child) {
        markExited()
        return finish(false)
      }
      sendKill("SIGTERM")
      killTimer = setTimeout(() => {
        sendKill("SIGKILL")
        child?.unref?.()
        finish(false)
      }, HOOK_KILL_GRACE_MS)
      killTimer?.unref?.()
    }, HOOK_TIMEOUT_MS)
    timer?.unref?.()
    try {
      const [cmd, ...args] = cfg.hookCommand
      child = spawn(cmd, [...args, event], { stdio: ["pipe", "ignore", "ignore"], detached: false })
      child.on("error", (err: any) => {
        if (!child?.pid || err?.code === "ESRCH") markExited()
        finish(false)
      })
      child.on("close", (code: number | null) => {
        markExited()
        finish(code === 0)
      })
      child.stdin?.on("error", () => {})
      child.stdin?.end(JSON.stringify(payload))
    } catch {
      markExited()
      finish(false)
    }
  })
  return { result, exit }
}

// queuedHook hands an event to terma in FIFO order, so a session's start always finishes
// before its reach checks (a late start would clear an earlier Mixed mark) and a reach
// finishes before the session ends. The next hook starts only once the last has exited.
function queuedHook(event: string, payload: Record<string, unknown>): Promise<boolean> {
  const cfg = st.config
  const h = st.hooks
  if (!cfg || h.pending >= MAX_HOOK_QUEUE) return Promise.resolve(false)
  h.pending++
  const slot = h.queue.then(() => spawnHook(cfg, AGENT + "-" + event, payload))
  h.queue = slot
    .then(
      (s) => s.exit,
      () => undefined,
    )
    .finally(() => {
      h.pending--
    })
  return within(
    slot.then(
      (s) => s.result,
      () => false,
    ),
    HOOK_TIMEOUT_MS + HOOK_KILL_GRACE_MS + 500,
    false,
  )
}

// --- paths --------------------------------------------------------------------------

// normalized is a path as Pi's normalizePath leaves it, in Pi's order: Unicode spaces become
// spaces, one leading @ goes, ~ becomes the home directory, otherwise a file:// URL becomes
// its path.
function normalized(p: string, piTool: boolean): string {
  let s = piTool ? p.replace(UNICODE_SPACES, " ") : p
  if (piTool && s.startsWith("@")) s = s.slice(1)
  if (s === "~") return homedir()
  if (s.startsWith("~/")) return join(homedir(), s.slice(2))
  if (/^file:\/\//.test(s)) return fileURLToPath(s)
  return s
}

// where is the absolute path Pi's tools open for a path argument (its resolveToCwd): the
// reach check and the file attributes judge that place, not the path as the model wrote it.
// It throws for a value Pi could not open either.
function where(p: string, cwd: string): string {
  return resolve(normalized(cwd, false), normalized(p, true))
}

// --- sessions -----------------------------------------------------------------------

function sessionIdOf(ctx: any): string {
  try {
    const id = ctx?.sessionManager?.getSessionId?.()
    return typeof id === "string" ? id : id === undefined || id === null ? "" : String(id)
  } catch {
    return ""
  }
}

function cwdOf(ctx: any): string {
  try {
    return str(ctx?.cwd)
  } catch {
    return ""
  }
}

function modeOf(ctx: any): string {
  try {
    return str(ctx?.mode)
  } catch {
    return ""
  }
}

function modelOf(ctx: any): any {
  try {
    return ctx?.model ?? undefined
  } catch {
    return undefined
  }
}

function entriesOf(ctx: any): any[] {
  try {
    const e = ctx?.sessionManager?.getEntries?.()
    return Array.isArray(e) ? e : []
  } catch {
    return []
  }
}

function entryOf(ctx: any, id: string): any {
  try {
    return id ? ctx?.sessionManager?.getEntry?.(id) : undefined
  } catch {
    return undefined
  }
}

function leafOf(ctx: any): string {
  try {
    return str(ctx?.sessionManager?.getLeafId?.())
  } catch {
    return ""
  }
}

// forkSource is the id of the session a fork or clone came from: its header names the
// source's file, and the source's id is the one in that file's own header line, never the
// file's name (a session opened with --session can have any name).
function forkSource(ctx: any): string {
  let fd: number | undefined
  try {
    const parent = ctx?.sessionManager?.getHeader?.()?.parentSession
    if (typeof parent !== "string" || !parent) return ""
    const head = Buffer.alloc(MAX_HEADER)
    fd = openSync(parent, "r")
    const n = readSync(fd, head, 0, head.length, 0)
    const text = head.subarray(0, n).toString("utf8")
    const nl = text.indexOf("\n")
    if (nl < 0 && n === head.length) return ""
    const id = JSON.parse(nl < 0 ? text : text.slice(0, nl))?.id
    return typeof id === "string" ? id : ""
  } catch {
    return ""
  } finally {
    if (fd !== undefined) {
      try {
        closeSync(fd)
      } catch {}
    }
  }
}

// parentPid is a process's parent, read from /proc on Linux and from ps on macOS; 0 when it
// cannot be told.
function parentPid(pid: number): number {
  try {
    if (process.platform === "linux") {
      const stat = readFileSync(`/proc/${pid}/stat`, "utf8")
      const fields = stat.slice(stat.lastIndexOf(")") + 2).split(" ")
      return Number(fields[1]) || 0
    }
    if (process.platform === "darwin") {
      const out = execFileSync("ps", ["-o", "ppid=", "-p", String(pid)], { timeout: PS_TIMEOUT_MS, stdio: ["ignore", "pipe", "ignore"] })
      return Number(String(out).trim()) || 0
    }
  } catch {}
  return 0
}

// link is the session this process works for: the inherited TERMA_PI_PARENT, `<pid>:<session>`,
// accepted only when that pid is another process and an ancestor of this one, so a stale
// value left in a daemon's or a reparented process's environment links nothing. Judged once.
function link(): string {
  if (st.link !== null) return st.link
  st.link = ""
  try {
    const raw = st.inherited
    const colon = raw.indexOf(":")
    if (colon <= 0) return st.link
    const pidText = raw.slice(0, colon)
    const session = raw.slice(colon + 1)
    if (!/^\d+$/.test(pidText) || !session) return st.link
    const pid = Number(pidText)
    if (!Number.isSafeInteger(pid) || pid <= 1 || pid === process.pid) return st.link
    let cur = process.ppid
    for (let i = 0; i < MAX_ANCESTORS && cur > 1; i++) {
      if (cur === pid) {
        st.link = session
        break
      }
      cur = parentPid(cur)
    }
  } catch {}
  return st.link
}

function newSession(id: string, ctx: any): Session {
  return {
    id,
    read: 0,
    mode: modeOf(ctx),
    forkedFrom: forkSource(ctx),
    started: false,
    parent: "",
    tools: new Map(),
    held: [],
    compact: null,
    gen: 0,
    tainted: false,
    fenced: -1,
    fencing: null,
    unfenced: false,
  }
}

// sight is the session an event belongs to. Every handler asks it first: the first time a
// process sees a session it starts reading that session's entries at their end, so nothing
// it already held is sent (a resume, a fork's copied history), and sends its current name.
// A session first seen on an event other than its start, a session nested inside the
// process, is claimed here.
function sight(ctx: any, viaStart = false): Session | null {
  const id = sessionIdOf(ctx)
  if (!id) return null
  const known = st.sessions.get(id)
  if (known) {
    known.mode = modeOf(ctx) || known.mode
    return known
  }
  const s = newSession(id, ctx)
  remember(st.sessions, id, s, MAX_SESSIONS)
  if (viaStart) {
    s.started = true
    s.parent = link()
  } else {
    const model = modelOf(ctx)
    void queuedHook("session-start", { session_id: id, cwd: cwdOf(ctx), model: str(model?.id) || undefined })
  }
  setPosition(s, ctx)
  return s
}

// setPosition starts the read of a session's entries at their end, and sends the session's
// current name: a name set before this process looked (--name, a fork's, a name another
// process gave it) has no event of its own here. The same entry is the same event.
function setPosition(s: Session, ctx: any) {
  const entries = entriesOf(ctx)
  s.read = entries.length
  if (s.parent) return
  for (let i = entries.length - 1; i >= 0; i--) {
    const e = entries[i]
    if (e && e.type === "session_info" && typeof e.name === "string" && e.name.trim()) {
      title(s, e)
      return
    }
  }
}

// --- the read of new entries --------------------------------------------------------

// readNew sends what the session gained since the last read, from Pi's own entries: prompts,
// messages an extension put into the conversation, names, compactions and other spend. In
// one open session the list only grows, in the order Pi wrote it.
function readNew(s: Session, ctx: any) {
  const entries = entriesOf(ctx)
  if (s.read > entries.length) s.read = entries.length
  const from = s.read
  s.read = entries.length
  for (let i = from; i < entries.length; i++) {
    try {
      onEntry(s, ctx, entries[i])
    } catch {}
  }
}

function onEntry(s: Session, ctx: any, e: any) {
  if (!e || typeof e !== "object" || typeof e.id !== "string" || !e.id) return
  switch (e.type) {
    case "message":
      if (e.message?.role === "user") prompt(s, e)
      return
    case "custom_message":
      customMessage(s, ctx, e)
      return
    case "session_info":
      title(s, e)
      return
    case "compaction":
      compaction(s, ctx, e)
      return
    case "usage":
      spend(s, e)
      return
  }
}

// turnOf is the turn an entry is in: the first user message at or above it, else the
// nearest message an extension put in (a run such a message started). A child session's
// events carry no turn: its prompts are the parent agent's text.
function turnOf(s: Session, ctx: any, id: string): string {
  if (s.parent) return ""
  let custom = ""
  let cur = id
  for (let i = 0; cur && i < MAX_WALK; i++) {
    const e = entryOf(ctx, cur)
    if (!e) break
    if (e.type === "message" && e.message?.role === "user") return str(e.id)
    if (!custom && e.type === "custom_message") custom = str(e.id)
    cur = str(e.parentId)
  }
  return custom
}

// prompt: every prompt of the person is a turn of its own.
function prompt(s: Session, e: any) {
  const cfg = st.config!
  const text = textOf(e.message?.content)
  emitRecord(s, AGENT + ".user_prompt", cfg.includePrompts ? cut(text, MAX_TEXT) : "", {
    "pi.entry.id": e.id,
    "pi.prompt.length": text.length,
    "pi.turn.id": s.parent ? undefined : e.id,
  }, timeOf(e))
}

// customMessage: an extension can put anything into one, a file's content included, so its
// text leaves only when both prompts and tool content are collected.
function customMessage(s: Session, ctx: any, e: any) {
  const cfg = st.config!
  const text = textOf(e.content)
  emitRecord(s, AGENT + ".custom_message", cfg.includePrompts && cfg.includeToolContent ? cut(text, MAX_TEXT) : "", {
    "pi.entry.id": e.id,
    "pi.prompt.length": text.length,
    "pi.turn.id": turnOf(s, ctx, e.id) || undefined,
  }, timeOf(e))
}

// title: a child session's name is not sent; its events are filed under its parent's.
function title(s: Session, e: any) {
  if (s.parent) return
  const name = typeof e.name === "string" ? e.name : ""
  if (!name.trim()) return
  emitRecord(s, AGENT + ".session.title", st.config!.includePrompts ? name : "", { "pi.entry.id": e.id }, timeOf(e))
}

// usageAttrs are a call's token counts and cost. A call that completed sends every count,
// zeros included; a failed or aborted one leaves out each count that is 0, and its cost
// unless it is above 0 or the provider billed it.
function usageAttrs(u: any, failed: boolean): Record<string, unknown> {
  const out: Record<string, unknown> = {}
  if (!u || typeof u !== "object") return out
  const count = (key: string, v: unknown) => {
    const n = num(v)
    if (n !== undefined && !(failed && n === 0)) out[key] = n
  }
  count("gen_ai.usage.input_tokens", u.input)
  count("gen_ai.usage.output_tokens", u.output)
  count("gen_ai.usage.cache_read.input_tokens", u.cacheRead)
  count("gen_ai.usage.cache_creation.input_tokens", u.cacheWrite)
  count("gen_ai.usage.reasoning.output_tokens", u.reasoning)
  const fromProvider = typeof u.cost?.totalFromProvider === "boolean" ? (u.cost.totalFromProvider as boolean) : undefined
  const total = num(u.cost?.total)
  if (total !== undefined && (!failed || total > 0 || fromProvider === true)) out["gen_ai.usage.total_cost"] = double(total)
  if (fromProvider !== undefined) out["pi.cost.from_provider"] = fromProvider
  return out
}

// oauth is whether the call used a sign-in rather than a key, which Pi can tell only for the
// model in force: asked only when that is the call's own model.
function oauth(ctx: any, provider: unknown, model: unknown): boolean | undefined {
  try {
    const m = ctx?.model
    if (!m || m.provider !== provider || m.id !== model) return undefined
    const v = ctx?.modelRegistry?.isUsingOAuth?.(m)
    return typeof v === "boolean" ? v : undefined
  } catch {
    return undefined
  }
}

// compaction is one chat span in no turn. The entry is the identity, never the object on
// the session_compact event, which Pi picks by its summary text.
function compaction(s: Session, ctx: any, e: any) {
  const cfg = st.config!
  const kept = s.compact
  s.compact = null
  const end = timeOf(e)
  const start = kept && kept.at <= end ? kept.at : end
  // A summary an extension wrote was made with a model of its own.
  const model = e.fromHook === true ? undefined : modelOf(ctx)
  const provider = model ? str(model.provider) : ""
  const modelId = model ? str(model.id) : ""
  const summary = typeof e.summary === "string" ? e.summary : ""
  emitSpan(s, callSpanId(s.id, e.id), modelId ? "chat " + modelId : "chat", SPAN_KIND_CLIENT, start, end, {
    "gen_ai.operation.name": "chat",
    "pi.call.id": e.id,
    "pi.call.kind": "compaction",
    "gen_ai.provider.name": provider,
    "gen_ai.request.model": modelId,
    ...usageAttrs(e.usage, false),
    "pi.auth.oauth": modelId ? oauth(ctx, provider, modelId) : undefined,
    "pi.compaction.reason": kept?.reason || undefined,
    "pi.compaction.tokens_before": num(e.tokensBefore),
    "pi.compaction.summary": cfg.includePrompts && cfg.includeToolContent && summary ? cut(summary, MAX_TEXT) : undefined,
  }, false)
}

// spend is a call Pi paid for outside the conversation (a cache warming), stored only as a
// usage entry: a chat span of its kind, in no turn.
function spend(s: Session, e: any) {
  const t = timeOf(e)
  const model = str(e.model)
  emitSpan(s, callSpanId(s.id, e.id), model ? "chat " + model : "chat", SPAN_KIND_CLIENT, t, t, {
    "gen_ai.operation.name": "chat",
    "pi.call.id": e.id,
    "pi.call.kind": str(e.kind) || undefined,
    "gen_ai.provider.name": str(e.provider),
    "gen_ai.request.model": model,
    ...usageAttrs(e.usage, false),
  }, false)
}

// --- model calls and tools ----------------------------------------------------------

// chat is a model call at its turn_end, which names its entry; Pi's own clock times it.
function chat(s: Session, ctx: any, m: any, callId: string, turn: string) {
  if (!m || typeof m !== "object") return
  const cfg = st.config!
  const stop = str(m.stopReason)
  const failed = stop === "error" || stop === "aborted"
  const start = num(m.timestamp) ?? Date.now()
  const end = start + (num(m.durationMs) ?? 0)
  const text = textOf(m.content)
  const model = str(m.model)
  const error = typeof m.errorMessage === "string" ? m.errorMessage : ""
  emitSpan(s, callSpanId(s.id, callId), "chat " + model, SPAN_KIND_CLIENT, start, end, {
    "gen_ai.operation.name": "chat",
    "pi.call.id": callId,
    "pi.turn.id": turn,
    "gen_ai.provider.name": str(m.provider),
    "gen_ai.request.model": model,
    "gen_ai.response.model": str(m.responseModel),
    "gen_ai.response.id": str(m.responseId),
    "gen_ai.response.finish_reasons": stop,
    "pi.thinking_level": str(m.thinkingLevel),
    ...usageAttrs(m.usage, failed),
    "pi.auth.oauth": oauth(ctx, m.provider, m.model),
    "pi.completion.length": text.length > 0 ? text.length : undefined,
    "gen_ai.completion": cfg.includePrompts && text ? cut(text, MAX_TEXT) : undefined,
    "pi.error.message": stop === "error" && cfg.includePrompts && cfg.includeToolContent && error ? cut(error, MAX_ERROR) : undefined,
  }, stop === "error")
}

// issuer is the assistant entry that issued a top-level tool call: Pi stores it before the
// tool starts, so a walk up from the leaf finds it.
function issuer(ctx: any, toolCallId: string): string {
  let cur = leafOf(ctx)
  for (let i = 0; cur && i < MAX_ISSUER_WALK; i++) {
    const e = entryOf(ctx, cur)
    if (!e) return ""
    const content = e.type === "message" && e.message?.role === "assistant" ? e.message.content : undefined
    if (Array.isArray(content) && content.some((p: any) => p && p.type === "toolCall" && p.id === toolCallId)) return str(e.id)
    cur = str(e.parentId)
  }
  return ""
}

// sendTool queues a held tool span as a call of the model call that issued it.
function sendTool(s: Session, ctx: any, h: Held, callId: string, turn: string) {
  const id = qualify(s.id, callId, h.toolCallId)
  emitSpan(s, toolSpanId(id), "execute_tool " + h.name, SPAN_KIND_INTERNAL, h.start, h.end, {
    "gen_ai.operation.name": "execute_tool",
    "gen_ai.tool.name": h.name,
    "gen_ai.tool.call.id": id,
    "pi.call.id": callId,
    "pi.turn.id": turn,
    ...h.attrs,
  }, h.error)
}

// toolReach describes what a read, search, list or shell call can reach, each path resolved
// as the tool resolves it (`.` when absent). It throws for a path Pi could not open either.
function toolReach(name: string, input: any, cwd: string): Record<string, unknown> | null {
  const a = input && typeof input === "object" ? input : {}
  const path = typeof a.path === "string" ? a.path : ""
  switch (name) {
    case "read":
      return path ? { paths: [where(path, cwd)] } : null
    case "grep": {
      const out: Record<string, unknown> = { paths: [where(path, cwd)] }
      if (typeof a.glob === "string" && a.glob) out.globs = [a.glob]
      return out
    }
    case "find": {
      const out: Record<string, unknown> = { paths: [where(path, cwd)] }
      if (typeof a.pattern === "string" && a.pattern) out.globs = [a.pattern]
      return out
    }
    case "ls":
      return { lists: [where(path, cwd)] }
    case "bash":
      return typeof a.command === "string" && a.command ? { command: a.command } : null
    default:
      return null
  }
}

// --- the fence ----------------------------------------------------------------------

// A session whose reach check failed is tainted: this extension exports nothing more of it
// and hands terma nothing more of it, and asks terma to fence its run (mark it Mixed) so
// that nothing terma collected after the reach leaves either. A tool is never blocked or
// failed for it: with terma gone, signed out or not set up, nothing claims the session.

function isFenced(s: Session): boolean {
  return s.fenced === s.gen
}

function taint(s: Session, cwd: string, gen: number): Promise<boolean> {
  if (s.gen === gen) {
    s.tainted = true
    s.held = []
  }
  if (isFenced(s)) return Promise.resolve(true)
  const active = s.fencing
  if (active) return s.unfenced ? Promise.resolve(false) : active
  // Fence: once the hooks ahead have run (bounded), a reach terma cannot follow flips the run
  // to Mixed. The tool waits for it once; if the fence fails too, every later call tries it
  // again in the background and does not wait.
  const p: Promise<boolean> = (async () => {
    const h = st.hooks
    const cfg = st.config
    if (!cfg) return false
    const drained = await within(h.queue.then(() => true, () => true), HOOK_TIMEOUT_MS + HOOK_KILL_GRACE_MS, false)
    if (!drained) return false
    const run = spawnHook(cfg, AGENT + "-tool-reach", {
      session_id: s.id,
      cwd,
      tool: "bash",
      command: REACH_FAILED_COMMAND,
      parent_session_id: s.parent || undefined,
    })
    h.queue = h.queue.then(() => run.exit, () => run.exit)
    const ok = await within(run.result, HOOK_TIMEOUT_MS + HOOK_KILL_GRACE_MS + 500, false)
    if (ok && s.gen === gen) s.fenced = gen
    return ok
  })()
    .catch(() => false)
    .finally(() => {
      if (s.fencing === p) s.fencing = null
    })
  s.fencing = p
  const waited = s.unfenced ? Promise.resolve(false) : p
  void p.then((ok) => {
    if (s.gen === gen) s.unfenced = !ok
  })
  return waited
}

// reach asks terma whether a tool may reach what it names, and fences the run when the
// answer does not come back yes.
function reach(s: Session, cwd: string, tool: string, what: Record<string, unknown>): Promise<boolean> {
  const gen = s.gen
  if (isFenced(s)) return Promise.resolve(true)
  if (s.tainted) return taint(s, cwd, gen)
  return queuedHook("tool-reach", { session_id: s.id, cwd, tool, parent_session_id: s.parent || undefined, ...what }).then((ok) =>
    ok ? true : taint(s, cwd, gen),
  )
}

// --- the host -----------------------------------------------------------------------

// hostVersion is the version of the Pi this extension records in, "" for any other host.
// A host rewrites literal import specifiers to its own modules, so the literal import of
// Pi's package always gives the host's own: Pi's in Pi, and Oh My Pi's own package, with its
// own version, inside Oh My Pi. That module's CONFIG_DIR_NAME says which host it is. Only a
// host that does not export it is asked whether Oh My Pi's own module is there, which inside
// Pi can also resolve to a stray package on disk. The retired @mariozechner Pi has no
// @earendil-works package and is not recorded.
async function hostVersion(): Promise<string> {
  let host: any
  try {
    host = await import("@earendil-works/pi-coding-agent")
  } catch {
    return ""
  }
  const dir = host?.CONFIG_DIR_NAME
  if (typeof dir === "string") {
    if (dir !== ".pi") return ""
  } else {
    try {
      await import("@oh-my-pi/pi-utils")
      return ""
    } catch {}
  }
  const version = typeof host?.VERSION === "string" ? host.VERSION : ""
  return atLeast(version, MIN_PI) ? version : ""
}

// --- the extension ------------------------------------------------------------------

export default async function (pi: any) {
  if (!CONFIG) return
  if (!pi || typeof pi.on !== "function") return
  const version = await hostVersion()
  if (!version) return
  st.config = CONFIG
  st.version = version

  // Every handler returns nothing, whatever happens inside it: a result would change what Pi
  // does, and an error in some handlers blocks the action.
  const on = (name: string, fn: (event: any, ctx: any) => unknown) =>
    pi.on(name, (event: any, ctx: any) => {
      try {
        const r: any = fn(event, ctx)
        return r && typeof r.then === "function" ? r.then(() => undefined, () => undefined) : undefined
      } catch {
        return undefined
      }
    })

  // A start: the session becomes the active one for the next commit and a new run in terma.
  // A reload is the same run, so it starts nothing and resets nothing.
  on("session_start", (event, ctx) => {
    const id = sessionIdOf(ctx)
    if (!id) return
    if (str(event?.reason) === "reload") {
      sight(ctx, true)
      return
    }
    try {
      process.env.TERMA_PI_PARENT = process.pid + ":" + id
    } catch {}
    const known = st.sessions.get(id)
    const s = known ?? sight(ctx, true)!
    s.gen++
    s.fenced = -1
    s.fencing = null
    s.unfenced = false
    if (known) {
      s.mode = modeOf(ctx) || s.mode
      s.started = true
      s.parent = link()
      if (!s.parent) s.tainted = false
      // A /resume back into a session opens its file afresh: what another process wrote to
      // it meanwhile is not this process's to send.
      setPosition(s, ctx)
    }
    const model = modelOf(ctx)
    return queuedHook("session-start", {
      session_id: id,
      cwd: cwdOf(ctx),
      model: str(model?.id) || undefined,
      reason: str(event?.reason) || undefined,
      parent_session_id: s.parent || undefined,
    })
  })

  // A session nested inside the process fires no start; its first event is its input, which
  // comes before its prompt is stored.
  on("input", (_event, ctx) => {
    sight(ctx)
  })

  // Every prompt is a claim too: a session whose start was missed, or a relay that stopped
  // between turns, is covered before the turn exports anything.
  on("before_agent_start", (_event, ctx) => {
    const s = sight(ctx)
    if (!s || s.tainted) return
    void queuedHook("prompt", { session_id: s.id, cwd: cwdOf(ctx) })
  })

  on("turn_end", (event, ctx) => {
    const s = sight(ctx)
    if (!s) return
    readNew(s, ctx)
    const callId = str(event?.messageEntryId)
    if (!callId) return
    const turn = turnOf(s, ctx, callId)
    chat(s, ctx, event?.message, callId, turn)
    const held = s.held
    s.held = []
    for (const h of held) sendTool(s, ctx, h, callId, turn)
  })

  on("agent_settled", (_event, ctx) => {
    const s = sight(ctx)
    if (s) readNew(s, ctx)
    void flush()
  })

  on("session_info_changed", (_event, ctx) => {
    const s = sight(ctx)
    if (s) readNew(s, ctx)
  })

  on("session_before_compact", (event, ctx) => {
    const s = sight(ctx)
    if (s) s.compact = { reason: str(event?.reason), at: Date.now() }
  })

  on("session_compact", (_event, ctx) => {
    const s = sight(ctx)
    if (s) readNew(s, ctx)
  })

  on("session_compact_failed", (_event, ctx) => {
    const s = sight(ctx)
    if (s) s.compact = null
  })

  // The reach is judged, or the run fenced, before the tool runs.
  on("tool_call", async (event, ctx) => {
    const s = sight(ctx)
    if (!s) return
    const cwd = cwdOf(ctx)
    const tool = str(event?.toolName)
    let what: Record<string, unknown> | null
    try {
      what = toolReach(tool, event?.input, cwd)
    } catch {
      await taint(s, cwd, s.gen)
      return
    }
    if (what) await reach(s, cwd, tool, what)
  })

  // A person's `!` command enters the model's context, so what it reaches is judged as the
  // agent's bash is; `!!` stays out of it. Neither is exported.
  on("user_bash", async (event, ctx) => {
    const s = sight(ctx)
    if (!s || event?.excludeFromContext === true) return
    const command = str(event?.command)
    if (!command) return
    const what: Record<string, unknown> = { command }
    if (typeof event?.cwd === "string" && event.cwd) what.dir = event.cwd
    await reach(s, cwdOf(ctx), "bash", what)
  })

  on("tool_execution_start", (event, ctx) => {
    const s = sight(ctx)
    if (!s) return
    const id = str(event?.toolCallId)
    if (!id) return
    const name = str(event?.toolName)
    const parent = str(event?.parentToolCallId)
    // A nested call's entry holds only the outer call, so it is its parent's.
    const entry = parent ? (s.tools.get(parent)?.entry ?? "") : issuer(ctx, id)
    let files: string[] | null = null
    const path = event?.args?.path
    if (FILE_TOOLS.has(name) && typeof path === "string" && path) {
      try {
        files = [where(path, cwdOf(ctx))]
      } catch {}
    }
    remember(s.tools, id, { name, args: event?.args, start: Date.now(), entry, files }, MAX_TOOLS)
  })

  // The edit is handed over as the tool returns, so a commit in the same reply sees it.
  on("tool_result", (event, ctx) => {
    const s = sight(ctx)
    if (!s || s.tainted) return
    const tool = str(event?.toolName)
    if (!EDIT_TOOLS.has(tool) || event?.isError !== false) return
    const path = event?.input?.path
    if (typeof path !== "string" || !path) return
    const cwd = cwdOf(ctx)
    let file: string
    try {
      file = where(path, cwd)
    } catch {
      return
    }
    const id = str(event?.toolCallId)
    const entry = id ? (s.tools.get(id)?.entry ?? "") : ""
    void queuedHook("file-edit", {
      session_id: s.id,
      cwd,
      tool,
      files: [file],
      tool_call_id: entry ? qualify(s.id, entry, id) : undefined,
      parent_session_id: s.parent || undefined,
    })
  })

  // The span ends on this extension's clock, after the reach check awaited for it.
  on("tool_execution_end", (event, ctx) => {
    const s = sight(ctx)
    if (!s) return
    const id = str(event?.toolCallId)
    if (!id) return
    const t = s.tools.get(id)
    s.tools.delete(id)
    const cfg = st.config!
    const end = Date.now()
    const duration = num(event?.durationMs)
    const start = duration !== undefined ? end - duration : (t?.start ?? end)
    const isError = event?.isError === true
    const r = event?.result && typeof event.result === "object" ? event.result : {}
    const server = r.details?.server
    const mcpTool = r.details?.tool
    if (s.tainted) return
    s.held.push({
      toolCallId: id,
      entry: t?.entry ?? "",
      name: str(event?.toolName) || t?.name || "",
      start,
      end,
      error: isError,
      attrs: {
        "pi.tool.outcome": !isError ? "ok" : duration !== undefined ? "error" : "not_run",
        "pi.tool.exit_code": num(r.structuredContent?.exit_code),
        "pi.mcp.server": typeof server === "string" ? server : undefined,
        "pi.mcp.tool": typeof mcpTool === "string" ? mcpTool : undefined,
        "pi.tool.files": cfg.includeToolContent ? (t?.files ?? undefined) : undefined,
        "gen_ai.tool.call.arguments": cfg.includeToolContent && t && t.args !== undefined ? cut(JSON.stringify(t.args), MAX_TOOL_CONTENT) : undefined,
        "gen_ai.tool.call.result": cfg.includeToolContent ? cut(textOf(r.content), MAX_TOOL_CONTENT) : undefined,
      },
    })
    if (s.held.length > MAX_HELD) s.held.shift()
  })

  // Leaving a session: what it gained is read and sent, and its end handed over, within
  // EXIT_MS together. A reload is the same run: it only sends.
  on("session_shutdown", async (event, ctx) => {
    const s = sight(ctx)
    if (!s) return
    readNew(s, ctx)
    if (str(event?.reason) === "reload") {
      await within(flush(), EXIT_MS, undefined, true)
      return
    }
    const held = s.held
    s.held = []
    for (const h of held) if (h.entry) sendTool(s, ctx, h, h.entry, turnOf(s, ctx, h.entry))
    s.tools.clear()
    const end = queuedHook("session-end", { session_id: s.id, cwd: cwdOf(ctx) })
    await within(Promise.all([end, flush()]), EXIT_MS, [false, undefined], true)
  })
}
