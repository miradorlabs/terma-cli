// Terma telemetry plugin for OpenCode.
//
// Installed by `terma setup` into OpenCode's plugins directory, where OpenCode loads it at
// startup; `terma teardown` removes it. Do not edit: the next setup or update rewrites it.
// CONFIG below is the only part that differs per machine.
//
// What it does: turns OpenCode's own record of a session into OpenTelemetry (one span per
// model call, one per tool call, a log record per prompt and per title) and posts it as
// OTLP/JSON to terma's relay. Claiming a session for a repository, guarding what a tool
// reaches and reporting file edits are handed to `terma hook <event>`, where the logic
// lives. It changes no request and reads no response.
//
// One file serves both majors: OpenCode 1 calls `server`, OpenCode 2 calls `setup`, and
// both translate into the same emitter, so both send the same wire. OpenCode 2 evaluates
// the module once per directory, so everything the plugin keeps is data in one object on
// globalThis, shared by every copy; a copy evaluated later works on it with its own code.
//
// Dependency-free on purpose. OpenCode loads directory plugins without installing
// anything for them, so this file uses only what Bun and Node ship.

const CONFIG = {"version":1,"endpoint":"http://127.0.0.1:43180","headers":{"Authorization":"Bearer relay-token"},"signals":["traces","logs","metrics"],"includePrompts":true,"includeToolContent":true,"hookCommand":["/usr/local/bin/terma","hook"]}

import { createHash } from "node:crypto"
import { existsSync, readFileSync } from "node:fs"
import { spawn, execFile } from "node:child_process"
import { dirname, isAbsolute, join, resolve } from "node:path"
import { fileURLToPath } from "node:url"

// --- limits -------------------------------------------------------------------------

// Every rule here that runs on the clock, and why:
// - BATCH_DELAY_MS: a batch leaves this long after its first item, so a quiet session still sends.
// - post's back-off (1 s, doubling, MAX_RETRIES times) and HEADERS_TTL_MS: a relay may restart.
// - HOOK_TIMEOUT_MS (REACH_HOOK_TIMEOUT_MS for a reach check), HOOK_KILL_GRACE_MS: a hung hook
//   is ended, so its session's queue moves on.
// - HOOK_QUEUE_WAIT_MS: a hook's caller stops waiting once the hook has waited this long for its
//   turn in the hook queue. A tool held for a check also waits for its session's earlier work
//   (the record, the claim), each with its own limit here.
// - MISSING_RETRY_MS: a terma that could not be run is not tried again on every hook.
// - RECORD_WAIT_MS: a session with no record is given up on (fails closed), not held forever.
// - At exit, one after another: the entry's session queues get EXIT_QUEUE_MS, the hooks still
//   running HOOK_TIMEOUT_MS, the flush EXIT_FLUSH_MS, OpenCode 1's log LOG_SETTLE_MS.
// - update() stamps what follows a move 1 ms after its claim, so none of it sorts before the claim.
// - A reach check made after its tool ran is noted for the session's family (checked), 1 ms
//   after it finished, and no model or tool call the family sends afterwards ends
//   earlier, nor is a prompt or title stamped earlier. A patch is checked in
//   tool.execute.after, which OpenCode waits for, so it keeps its own end; a call no hook saw
//   is checked when its part arrives and ends at that check.

// Bodies are bounded so a single reply or tool result cannot make an export enormous.
const MAX_TEXT = 64 * 1024
const MAX_TOOL_CONTENT = 16 * 1024
const MAX_TOOL_TITLE = 512
const MAX_ERROR = 1024
// The exporter batches: this many items, or this long after the first, whichever first.
const BATCH_SIZE = 64
const BATCH_DELAY_MS = 3000
const MAX_RETRIES = 5
// Batches waiting while the relay does not answer; the oldest go first, so a long-running
// server cannot grow the queue forever.
const MAX_PENDING_BATCHES = 32
// What waits is bounded in bytes as well as in number.
const MAX_PENDING_BYTES = 8 * 1024 * 1024
// The headers helper is re-run this often, matching Claude Code's cadence.
const HEADERS_TTL_MS = 29 * 60 * 1000
// A session's hooks run one at a time; a hung one is terminated so its queue keeps moving.
const HOOK_TIMEOUT_MS = 2000
// A reach check decides whether a session is recorded at all, so it gets a little longer
// than `terma hook` allows itself, 5 s; every other hook keeps HOOK_TIMEOUT_MS.
const REACH_HOOK_TIMEOUT_MS = 5500
const HOOK_KILL_GRACE_MS = 250
const MAX_HOOK_QUEUE = 64
// How long a hook may wait in its session's hook queue before its caller stops waiting: a
// tool is held for its reach check, so that wait is bounded.
const HOOK_QUEUE_WAIT_MS = 10000
// A terma that could not be run is tried again this long after the last attempt.
const MISSING_RETRY_MS = 60 * 1000
// OpenCode 1 writes its log about once a second and drops what is unwritten when it exits:
// a process that ends sooner than this after terma's warning would lose it.
const LOG_SETTLE_MS = 1500
// How long the exit waits for what is still queued to be sent: a relay that is down must
// not hold OpenCode open.
const EXIT_FLUSH_MS = 4000
// The longest the exit waits for its sessions' queued work: a little more than a reach
// check may take.
const EXIT_QUEUE_MS = 8000
// How long a session waits for its record before it is given up on (fail closed).
const RECORD_WAIT_MS = 5000
// What the plugin remembers is bounded, so a long-running server cannot grow it forever;
// only the ids of sessions that failed closed are kept for good (see taint, darken).
const MAX_SESSIONS = 512
// Who opened whom is remembered far longer than a session is kept, so a family holds
// together on a long-running server (see parents).
const MAX_PARENTS = 65536
const MAX_EVENTS = 8192
const MAX_PER_SESSION = 1024
const MAX_SMALL = 256

const SCOPE = { name: "terma-opencode", version: "2" }
const DEFAULT_HOOK = ["terma", "hook"]
const EDIT_TOOLS = new Set(["write", "edit", "apply_patch", "patch"])
// OpenCode 1's own tools whose names could pass for <server>_<tool>.
const BUILTIN_UNDERSCORE_TOOLS = new Set(["apply_patch", "plan_enter", "plan_exit"])
// OpenCode 1.x names the placeholder this way until the title call answers.
const PLACEHOLDER_TITLE = /^New session - \d{4}-\d{2}-\d{2}T/
const RULE_REFUSAL = "The user has specified a rule which prevents"
// How OpenCode 1 opens the synthetic part for a file attached to a prompt.
const READ_ATTACHMENT = "Called the Read tool with the following input: "
// The path an attached file is reach-checked under when its marker names no path terma can
// use: two leading separators make it a reach terma cannot place.
const UNPLACED_ATTACHMENT = "//attached-file/unplaced"
// The shell command a session that failed closed sends as its reach: one terma cannot
// follow, so the hook marks the run Mixed. OMP's extension (internal/agents/omp/terma-omp.ts)
// sends the same one, and a test holds the two together.
const REACH_FAILED_COMMAND = "eval $TERMA_REACH_FAILED"

// --- state --------------------------------------------------------------------------

// The one shared object. Data, promises and the resolvers of those promises only: a later
// copy of this file (OpenCode 2 evaluates one per directory, and again after a rewrite) must
// be able to work on it with its own functions, so nothing in it does a copy's work.
// A change to its shape bumps the number in the key, so a copy never reads another shape.
const KEY = Symbol.for("terma.opencode.state/1")
const state = (globalThis[KEY] ??= newState())

// This copy's entry loggers; a missing terma is reported through the newest.
const loggers = []

function newState() {
  return {
    config: settings(null),
    exporter: {
      spans: [],
      records: [],
      timer: null,
      // Batches waiting to be sent, oldest first: { cfg, sends: [[path, payload]], items, bytes }.
      batches: [],
      pendingBytes: 0, // the bytes of the batches waiting
      draining: false,
      inflight: Promise.resolve(), // resolves once the batches queued so far are sent or given up on
      dropped: 0, // items in batches dropped while the relay did not answer
      droppedWarned: false,
      headers: null,
      headersAt: 0,
    },
    hooks: {
      missing: false,
      missingAt: 0,
      warned: false,
      // Every hook queue with a hook waiting or running, sessions that left the map included.
      running: new Set(),
      // The queue of hooks that name no session this process keeps.
      unowned: { hookQueue: Promise.resolve(), hookPending: 0 },
    },
    sessions: new Map(),
    tainted: new Set(),
    // The ids of sessions that went dark, kept for good as taints are: a session that has
    // ended or been forgotten still fails what is below it closed.
    dark: new Set(),
    // session id -> the id of the session that opened it, for every subagent seen: a family
    // is walked through this, never through sessions, which are forgotten when idle.
    parents: new Map(),
    // The time of the last reach check made after its tool ran, by family (familyOf), and
    // the late checks running now, which the family's queued work waits for (lateCheck).
    floors: new Map(),
    checking: new Map(),
    seenEvents: new Set(),
    owners: 0,
  }
}

function settings(raw) {
  const c = raw && typeof raw === "object" ? raw : {}
  const strings = (v) => (Array.isArray(v) ? v.filter((x) => typeof x === "string") : [])
  const hookCommand = strings(c.hookCommand)
  const resourceAttributes = {}
  for (const [k, v] of Object.entries(c.resourceAttributes ?? {})) {
    if (k && typeof v === "string" && v) resourceAttributes[k] = v
  }
  return {
    endpoint: typeof c.endpoint === "string" ? c.endpoint.replace(/\/+$/, "") : "",
    headers: c.headers && typeof c.headers === "object" ? { ...c.headers } : {},
    headersHelper: typeof c.headersHelper === "string" ? c.headersHelper : "",
    signals: strings(c.signals),
    includePrompts: c.includePrompts === true,
    includeToolContent: c.includeToolContent === true,
    resourceAttributes,
    hookCommand: hookCommand.length > 0 && hookCommand.length === c.hookCommand.length ? hookCommand : DEFAULT_HOOK,
  }
}

// configure makes this evaluation's CONFIG the one every copy uses. What is queued leaves
// with the destination it was collected for, and a refresh that moved terma gets a retry.
function configure(st, raw) {
  const next = settings(raw)
  const route = (c) => JSON.stringify([c.endpoint, c.headers, c.headersHelper, c.hookCommand])
  if (route(st.config) !== route(next)) {
    flush()
    st.exporter.headers = null
    st.exporter.headersAt = 0
    st.hooks.missing = false
    st.hooks.missingAt = 0
    st.hooks.warned = false
    st.exporter.droppedWarned = false
  }
  st.config = next
}

function remember(coll, key, value, max = MAX_PER_SESSION) {
  if (coll instanceof Map) {
    coll.delete(key)
    coll.set(key, value)
  } else {
    coll.add(key)
  }
  while (coll.size > max) coll.delete(coll.keys().next().value)
}

// fresh is whether this process has not handled the event yet: an event can reach it more
// than once (OpenCode 2 sends every event to every directory's subscription). An event
// with no id is never dropped.
function fresh(event) {
  const id = event?.id
  if (typeof id !== "string" || !id) return true
  if (state.seenEvents.has(id)) return false
  remember(state.seenEvents, id, undefined, MAX_EVENTS)
  return true
}

function sleep(ms, unref = false) {
  return new Promise((r) => {
    const t = setTimeout(r, ms)
    if (unref) t.unref?.()
  })
}

// --- ids and encoding ---------------------------------------------------------------

// Trace and span ids are derived, not random: the same session always maps to the same
// trace and the same call to the same span, so a redelivered export dedups downstream.
function sha(s) {
  return createHash("sha256").update(s).digest("hex")
}
function traceId(sessionID) {
  return sha("terma-opencode:" + sessionID).slice(0, 32)
}
function spanId(kind, id) {
  return sha("terma-opencode-span:" + kind + ":" + id).slice(0, 16)
}
// qualify names a tool call by its assistant message: some providers number their call ids
// per response (call_0, call_1), so the provider's id alone repeats within a session.
function qualify(messageID, callID) {
  return String(messageID ?? "") + ":" + String(callID ?? "")
}
function nanos(ms) {
  return (BigInt(Math.round(Number.isFinite(ms) ? ms : Date.now())) * 1000000n).toString()
}
function num(v) {
  return typeof v === "number" && Number.isFinite(v) ? v : undefined
}
function str(v) {
  return typeof v === "string" ? v : ""
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
    case "object": {
      if (!Array.isArray(value)) return null
      const values = value.filter((v) => typeof v === "string").map((v) => ({ stringValue: v }))
      return values.length > 0 ? { key, value: { arrayValue: { values } } } : null
    }
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

function exportItem(kind, item) {
  const ex = state.exporter
  ex[kind].push(item)
  if (ex.spans.length + ex.records.length >= BATCH_SIZE) {
    flush()
    return
  }
  if (ex.timer) return
  ex.timer = setTimeout(flush, BATCH_DELAY_MS)
  // Never keep the process alive for a pending export.
  ex.timer.unref?.()
}

// flush queues everything collected as one batch, its request bodies built now with the
// config in force, and returns a promise that resolves once every batch queued so far is sent
// or given up on. One drain sends them in order, so two sends never race, and a failed batch
// is retried with the same bytes. While the relay does not answer, the oldest waiting batches
// are dropped, past MAX_PENDING_BATCHES or MAX_PENDING_BYTES; the newest is never dropped for
// its size alone.
function flush() {
  const ex = state.exporter
  if (ex.timer) {
    clearTimeout(ex.timer)
    ex.timer = null
  }
  const spans = ex.spans
  const records = ex.records
  ex.spans = []
  ex.records = []
  if (spans.length === 0 && records.length === 0) return ex.inflight
  const cfg = state.config
  const resource = { attributes: attrs({ "service.name": "opencode", ...cfg.resourceAttributes }) }
  const sends = []
  if (spans.length > 0) sends.push(["/v1/traces", JSON.stringify({ resourceSpans: [{ resource, scopeSpans: [{ scope: SCOPE, spans }] }] })])
  if (records.length > 0) sends.push(["/v1/logs", JSON.stringify({ resourceLogs: [{ resource, scopeLogs: [{ scope: SCOPE, logRecords: records }] }] })])
  const bytes = sends.reduce((n, [, payload]) => n + Buffer.byteLength(payload), 0)
  ex.batches.push({ cfg, sends, items: spans.length + records.length, bytes })
  ex.pendingBytes += bytes
  while (ex.batches.length > MAX_PENDING_BATCHES || (ex.pendingBytes > MAX_PENDING_BYTES && ex.batches.length > 1)) {
    const old = ex.batches.shift()
    ex.pendingBytes -= old.bytes
    ex.dropped += old.items
    if (!ex.droppedWarned) {
      ex.droppedWarned = true
      const log = loggers[loggers.length - 1]
      try {
        log?.("terma: the relay is not answering; some OpenCode telemetry was dropped.")
      } catch {}
    }
  }
  if (!ex.draining) {
    ex.draining = true
    ex.inflight = drain(ex)
  }
  return ex.inflight
}

// drain sends the waiting batches one at a time, front first, until none is left, each
// batch's requests in order. A batch leaves the queue, and its bytes the count, as it is
// sent, so the one in flight is never the one dropped.
async function drain(ex) {
  try {
    while (ex.batches.length > 0) {
      const { cfg, sends, bytes } = ex.batches.shift()
      ex.pendingBytes -= bytes
      try {
        for (const [path, payload] of sends) await post(cfg, path, payload)
      } catch {}
    }
  } finally {
    ex.draining = false
  }
}

async function post(cfg, path, payload) {
  if (!cfg.endpoint) return
  for (let attempt = 0; attempt <= MAX_RETRIES; attempt++) {
    try {
      const auth = await authHeaders(cfg)
      const res = await fetch(cfg.endpoint + path, {
        method: "POST",
        headers: { "content-type": "application/json", ...auth },
        body: payload,
      })
      await res.arrayBuffer().catch(() => {})
      if (res.ok) return
      // A rejected credential will not fix itself by retrying; a 5xx or 429 might.
      if (res.status === 401 || res.status === 403) {
        if (state.config === cfg) state.exporter.headers = null
        return
      }
      if (res.status < 500 && res.status !== 429) return
    } catch {
      // Network error: retry below.
    }
    if (attempt < MAX_RETRIES) await sleep(Math.min(30000, 1000 * 2 ** attempt))
  }
}

// authHeaders runs the helper script (the key never sits in this file) or falls back to
// inline headers. Only the config in force is cached: a batch collected under an earlier
// one leaves with that one's headers.
async function authHeaders(cfg) {
  const ex = state.exporter
  const current = state.config === cfg
  if (current && ex.headers && Date.now() - ex.headersAt < HEADERS_TTL_MS) return ex.headers
  let headers = {}
  if (cfg.headersHelper) {
    try {
      const out = await new Promise((ok, fail) => {
        execFile(cfg.headersHelper, [], { timeout: 5000, maxBuffer: 64 * 1024 }, (err, stdout) => (err ? fail(err) : ok(stdout)))
      })
      const parsed = JSON.parse(out)
      if (parsed && typeof parsed === "object") headers = parsed
    } catch {
      headers = {}
    }
  } else {
    headers = { ...cfg.headers }
  }
  if (current) {
    ex.headers = headers
    ex.headersAt = Date.now()
  }
  return headers
}

// --- hook runner --------------------------------------------------------------------

// runHook hands an event to `terma hook <event>` on a queue: the session's own (a FIFO, so
// its claim always finishes before its other hooks) or, for a session this process does
// not keep, state.hooks.unowned. Queues do not wait for each other. A hook's limit is
// hookLimit(event), counted from its turn. What can happen to a hook, and what its caller
// gets:
//   runs and exits 0                         true
//   runs and exits non-zero                  false
//   waits past HOOK_QUEUE_WAIT_MS for its    false then; it still runs in its turn
//   turn
//   runs past its limit                      false; it is ended with its process group, and
//                                            its queue moves on once it is gone
//   queue full (MAX_HOOK_QUEUE) or terma     false at once; it is not started
//   cannot be run
function runHook(lane, event, payload) {
  const h = state.hooks
  if (termaMissing() || lane.hookPending >= MAX_HOOK_QUEUE) return Promise.resolve(false)
  const argv = state.config.hookCommand
  let done
  const answer = new Promise((r) => {
    done = r
  })
  let settled = false
  let running
  const settle = (ok) => {
    if (!settled) {
      settled = true
      clearTimeout(queued)
      clearTimeout(running)
      done(ok)
    }
  }
  const giveUp = (ms) => {
    const t = setTimeout(() => settle(false), ms)
    t.unref?.()
    return t
  }
  const queued = giveUp(HOOK_QUEUE_WAIT_MS)
  lane.hookPending++
  h.running.add(lane)
  const slot = lane.hookQueue.then(() => {
    // Its turn: the run limit starts now, not when it was queued.
    clearTimeout(queued)
    if (!settled) running = giveUp(hookLimit(event) + HOOK_KILL_GRACE_MS + 500)
    return spawnHook(argv, event, payload)
  })
  lane.hookQueue = slot.then((s) => s.exit, () => undefined).finally(() => {
    if (--lane.hookPending <= 0) h.running.delete(lane)
  })
  slot.then((s) => s.result, () => false).then(settle, () => settle(false))
  return answer
}

// hookLimit is how long a hook may run.
function hookLimit(event) {
  return event === "opencode-tool-reach" ? REACH_HOOK_TIMEOUT_MS : HOOK_TIMEOUT_MS
}

function spawnHook(argv, event, payload) {
  if (termaMissing()) return { result: Promise.resolve(false), exit: Promise.resolve() }
  const grouped = process.platform !== "win32"
  let resolveExit
  const exit = new Promise((r) => {
    resolveExit = r
  })
  const result = new Promise((done) => {
    let settled = false
    let exited = false
    let child = null
    let killTimer
    const finish = (ok) => {
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
    // The hook leads a process group of its own, so the group is signalled: a child of the
    // hook (git) ends with it. Where there is no group, or it cannot be signalled, the hook
    // alone is.
    const sendKill = (sig) => {
      try {
        if (grouped && child.pid > 1) return void process.kill(-child.pid, sig)
      } catch (err) {
        if (err?.code === "ESRCH") return markExited()
      }
      try {
        child.kill(sig)
      } catch (err) {
        if (err?.code === "ESRCH") markExited()
      }
    }
    const timer = setTimeout(() => {
      if (!child) {
        markExited()
        return finish(false)
      }
      sendKill("SIGTERM")
      killTimer = setTimeout(() => {
        sendKill("SIGKILL")
        child.unref?.()
        finish(false)
      }, HOOK_KILL_GRACE_MS)
      killTimer.unref?.()
    }, hookLimit(event))
    timer.unref?.()
    try {
      // In a process group of its own: OpenCode 2 signals the group a tool step ran in when
      // the step ends, which killed a hook started for that step (a file edit, the idle
      // hook that starts the flush) wherever terma took longer than that to start. Windows
      // has no process groups: before OpenCode is supported there, decide how a hook is kept
      // apart from the step's signals and how a hung hook's children are ended.
      child = spawn(argv[0], [...argv.slice(1), event], { stdio: ["pipe", "ignore", "ignore"], detached: grouped })
      child.on("spawn", termaFound)
      child.on("error", (err) => {
        if (err?.code === "ENOENT" || err?.code === "EACCES") termaLost(argv[0], err.code)
        if (!child?.pid || err?.code === "ESRCH") markExited()
        finish(false)
      })
      child.on("close", (code) => {
        markExited()
        finish(code === 0)
      })
      child.stdin.on("error", () => {})
      child.stdin.end(JSON.stringify(payload))
    } catch (err) {
      if (err?.code === "ENOENT" || err?.code === "EACCES") termaLost(argv[0], err.code)
      markExited()
      finish(false)
    }
  })
  return { result, exit }
}

// termaLost stops every further hook for a while and says so once per config, in
// OpenCode's own log: a session nobody claimed must not be exported, and a person should
// learn why nothing arrives. The first hook after MISSING_RETRY_MS tries terma again.
function termaLost(command, code) {
  const h = state.hooks
  h.missing = true
  h.missingAt = Date.now()
  if (h.warned) return
  h.warned = true
  const log = loggers[loggers.length - 1]
  try {
    log?.(`terma: cannot run ${command} (${code}). OpenCode sessions are not recorded until "terma setup" is run again.`)
  } catch {}
}

// termaFound clears a missing terma once a hook starts again. Sessions that went dark
// while it was missing stay dark; sessions seen from now on are claimed as usual.
function termaFound() {
  state.hooks.missing = false
  state.hooks.missingAt = 0
}

// termaMissing is whether hooks are off now: terma could not be run, recently.
function termaMissing() {
  const h = state.hooks
  return h.missing && Date.now() - h.missingAt < MISSING_RETRY_MS
}

// --- sessions -----------------------------------------------------------------------

function newSession(id, owner) {
  return {
    id,
    known: false,
    latest: null, // a record that arrived before the first one was applied
    recordArrived: null, // resolves firstSight's wait when a record arrives before the session is known
    first: null, // the session's first sight (its record and its claim), as a promise
    parentID: "",
    directory: "",
    askedDirectory: "", // the directory most recently moved to, applied or still queued
    movedAt: 0, // when the last move's claim finished; nothing after it is stamped earlier
    version: "",
    created: 0,
    title: "",
    sentTitle: "",
    lastTitleTime: 0,
    queue: Promise.resolve(),
    busy: 0,
    hookQueue: Promise.resolve(), // its hooks, one at a time, in the order they were asked for
    hookPending: 0,
    claimed: false,
    dark: false,
    policy: {},
    owner,
    messages: new Map(), // messageID -> { parentID, created, copied }
    texts: new Map(), // messageID -> Map(partID -> text), first-seen order
    doneCalls: new Set(),
    doneTools: new Set(),
    asked: new Map(), // permission request id -> qualified call id
    rejected: new Set(),
    guarded: new Map(), // provider call id -> { reachKey }
    fenced: false, // terma marked the session's run Mixed after a reach check failed
    fencing: false, // that request is running
    pendingTitle: null,
    entry: {}, // an entry point's own per-session data
  }
}

// sight is called for every hook and event that names a session, before anything else.
// The first time a process sees a session, its queue starts with "get the record, then
// claim", so everything the session sends waits, in order, behind its claim.
function sight(sessionID, { record, fetch, owner } = {}) {
  if (typeof sessionID !== "string" || !sessionID) return null
  let s = state.sessions.get(sessionID)
  if (!s) {
    s = newSession(sessionID, owner)
    state.sessions.set(sessionID, s)
    prune()
    if (record) s.latest = record
    s.first = run(s, () => firstSight(s, fetch))
  } else {
    // Kept in order of last use, so prune drops the session idle longest.
    state.sessions.delete(sessionID)
    state.sessions.set(sessionID, s)
    if (record) {
      if (s.known) update(s, record)
      else {
        s.latest = record
        s.recordArrived?.()
      }
    }
  }
  return s
}

function prune() {
  if (state.sessions.size <= MAX_SESSIONS) return
  for (const [id, s] of state.sessions) {
    if (s.busy === 0 && s.hookPending === 0) {
      state.sessions.delete(id)
      if (state.sessions.size <= MAX_SESSIONS) return
    }
  }
}

async function firstSight(s, lookup) {
  let rec = s.latest
  // No record yet: wait, RECORD_WAIT_MS at most, for OpenCode's answer to the lookup or an
  // event that carries one, whichever first; an answer with no record does not end the wait,
  // and an event's record wins. Without one the session's directory and parent are unknown,
  // so it is never claimed and never exported.
  if (!rec) {
    let timer
    let fetched = null
    await new Promise((r) => {
      s.recordArrived = r
      timer = setTimeout(r, RECORD_WAIT_MS)
      Promise.resolve()
        .then(lookup)
        .then((f) => f?.id === s.id && r((fetched = f)), () => {})
    })
    clearTimeout(timer)
    s.recordArrived = null
    rec = s.latest ?? fetched
  }
  if (!rec) {
    darken(s)
    return
  }
  s.latest = null
  s.known = true
  adopt(s, str(rec.parentID))
  s.directory = str(rec.directory)
  s.askedDirectory = s.directory
  s.version = str(rec.version)
  s.created = num(rec.created) ?? 0
  const titled = (s.title = str(rec.title))
  s.policy = repoPolicy(s.directory)
  s.claimed = await runHook(s, "opencode-session-start", base(s))
  // A timeout or a refusal is not fatal: the relay holds an unclaimed session and the
  // next prompt claims it again. A terma that cannot be run is.
  if (!s.claimed && termaMissing()) {
    darken(s)
    return
  }
  // The title the session came with, which is what titles a resumed session. One that
  // arrived during the claim is not sent here: it has its own place in the queue, behind
  // whatever was queued before it, a call still to be checked included.
  if (!s.parentID && isRealTitle(titled)) {
    s.sentTitle = titled
    emitTitle(s, titled)
  }
}

// adopt notes which session opened s, on the session and in parents.
function adopt(s, parentID) {
  s.parentID = parentID
  if (parentID && parentID !== s.id) remember(state.parents, s.id, parentID, MAX_PARENTS)
}

function update(s, rec) {
  // Compared with the last move asked for, not the last one applied: a move back while an
  // earlier move still waits in the queue is a move too.
  if (rec.directory && rec.directory !== (s.askedDirectory || s.directory)) {
    // A session that moved is under the policy of the repository it is in now. The move
    // takes its place in the queue: what was said before it leaves under the old one.
    const directory = rec.directory
    s.askedDirectory = directory
    run(s, async () => {
      s.directory = directory
      s.policy = repoPolicy(directory)
      // The relay files a record under the claim in force at its time: claim the session
      // in its new directory before anything more of it leaves, and stamp nothing earlier.
      await guardReach(s, { tool: "list", lists: [directory] })
      s.movedAt = Date.now() + 1 // strictly after the claim, never the same millisecond
    })
  }
  if (rec.parentID) adopt(s, rec.parentID)
  if (rec.title && rec.title !== s.title) {
    s.title = rec.title
    const time = Date.now()
    run(s, () => title(s, { title: rec.title, time }))
  }
}

// run appends fn to the session's queue. Everything a session emits or calls goes through
// it. Whether the session may still send is decided when fn runs, not when it was queued.
function run(s, fn) {
  if (!s) return Promise.resolve()
  s.busy++
  s.queue = s.queue.then(async () => {
    try {
      await state.checking.get(familyOf(s))
      if (!blocked(s)) await fn()
    } catch {
      // A telemetry failure must never surface in the agent.
    } finally {
      s.busy--
    }
  })
  return s.queue
}

// beside runs a check that OpenCode itself waits for (before a tool runs, or before its
// result is handed on) once the session's first sight is done, outside its queue. Such a
// check sends nothing, so it waits neither behind what the session has queued to send nor
// for a late check elsewhere in the family: the plugin's waits may delay what terma sends,
// never OpenCode's own work beyond the check that work needs. What the tool itself sends
// later goes through the queue as ever, and is held there. A session that failed closed is
// judged here (blocked), as run judges it for queued work.
async function beside(s, fn) {
  await s.first
  try {
    if (!blocked(s)) await fn()
  } catch {
    // A telemetry failure must never surface in the agent.
  }
}

function blocked(s) {
  if (!s.claimed && termaMissing()) darken(s)
  return s.dark || isTainted(s)
}

// darken gives up on a session that could not be claimed: nothing of it is exported.
function darken(s) {
  s.dark = true
  state.dark.add(s.id)
}

// A failed reach hook taints the session and every session above it: their content could
// now include what the child read across organizations, so none of them exports more. A
// taint is one short id and is never dropped: dropping one would let a session that failed
// closed export again.
function taint(s) {
  let id = s.id
  for (let i = 0; i < 16 && id; i++) {
    state.tainted.add(id)
    id = state.parents.get(id)
  }
}
// familyOf names s's family by the topmost session above it, whether or not the sessions
// between are still kept. A session and its subagents share one run in terma, and so one
// Mixed cutoff: a reach check made after its tool ran, anywhere in the family, holds the
// whole family's times (checked, checkFloor).
function familyOf(s) {
  let id = s.id
  for (let i = 0, parent; i < 16 && (parent = state.parents.get(id)); i++) id = parent
  return id
}

// checked notes that a reach check made after its tool ran has just finished in s's
// family, a millisecond on so that nothing shares its time, and returns that time.
function checked(s) {
  const at = Date.now() + 1
  remember(state.floors, familyOf(s), at, MAX_SESSIONS)
  return at
}

// lateCheck is guardReach for a call that has already run, with nothing waiting for its
// check. While it runs, the queued work of every session in the family waits for it (run),
// so nothing that may derive from the call is sent before the check is done, and what is
// sent afterwards is held to its time. A check is bounded by its hook's limits.
async function lateCheck(s, reach) {
  const family = familyOf(s)
  let done
  const mine = Promise.all([state.checking.get(family), new Promise((r) => (done = r))])
  state.checking.set(family, mine)
  try {
    const ok = await guardReach(s, reach)
    if (ok) checked(s)
    return ok
  } finally {
    if (state.checking.get(family) === mine) state.checking.delete(family)
    done()
  }
}

// checkFloor is the time nothing s sends may end or be stamped before: its family's last
// such check, or 0.
function checkFloor(s) {
  return state.floors.get(familyOf(s)) ?? 0
}

// isTainted is whether a session, or one above it, failed closed: tainted, or never given
// its record (dark), so nothing it read was checked and what is below it derives from it.
function isTainted(s) {
  let id = s.id
  for (let i = 0; i < 16 && id; i++) {
    if (state.tainted.has(id) || state.dark.has(id)) return true
    id = state.parents.get(id)
  }
  return false
}

// repoPolicy reads the repository's own overlay (.opencode/terma.json at the root of the
// session's checkout): which signals ship from it, and whether content is captured.
function repoPolicy(directory) {
  if (!directory) return {}
  let root = directory
  for (let d = directory, i = 0; d && i < 64; i++) {
    if (existsSync(join(d, ".git"))) {
      root = d
      break
    }
    const parent = dirname(d)
    if (parent === d) break
    d = parent
  }
  try {
    const local = JSON.parse(readFileSync(join(root, ".opencode", "terma.json"), "utf8"))
    const out = {}
    if (Array.isArray(local.signals)) out.signals = local.signals.filter((x) => typeof x === "string")
    if (typeof local.includePrompts === "boolean") out.includePrompts = local.includePrompts
    if (typeof local.includeToolContent === "boolean") out.includeToolContent = local.includeToolContent
    return out
  } catch {
    // No local policy, or one this plugin cannot read: the global settings stand.
    return {}
  }
}

function policyOf(s) {
  const c = state.config
  const p = s.policy ?? {}
  return {
    signals: p.signals ?? c.signals,
    includePrompts: p.includePrompts ?? c.includePrompts,
    includeToolContent: p.includeToolContent ?? c.includeToolContent,
  }
}

function isRealTitle(t) {
  return typeof t === "string" && t.trim() !== "" && !PLACEHOLDER_TITLE.test(t)
}

// --- emitter ------------------------------------------------------------------------
//
// The only place a wire name appears. Entries call these from inside run(session, ...).

function base(s) {
  return { session_id: s.id, cwd: s.directory, parent_session_id: s.parentID || undefined }
}

function common(s) {
  return {
    "session.id": s.id,
    "opencode.version": s.version,
    "opencode.parent_session.id": s.parentID || undefined,
  }
}

function emitSpan(s, span) {
  if (!state.config.endpoint || !policyOf(s).signals.includes("traces")) return
  exportItem("spans", { traceId: traceId(s.id), ...span })
}

function emitRecord(s, name, body, extra, time) {
  if (!state.config.endpoint || !policyOf(s).signals.includes("logs")) return
  exportItem("records", {
    timeUnixNano: nanos(time),
    severityNumber: 9,
    severityText: "INFO",
    body: { stringValue: body ?? "" },
    attributes: attrs({ "event.name": name, ...extra, ...common(s) }),
    traceId: traceId(s.id),
  })
}

// call is one model call: c = { id, kind?, messageID?, turn?, model, provider, agent?,
// start, end, tokens?: { input, output, reasoning, cacheRead, cacheWrite }, cost?,
// finish?, text?, error?: { type, message }, compactionReason? }.
function call(s, c) {
  const p = policyOf(s)
  const a = {
    "gen_ai.operation.name": "chat",
    "opencode.call.id": c.id,
    "opencode.call.kind": c.kind,
    "opencode.message.id": c.messageID,
    // An auxiliary call (title, compaction) belongs to no turn.
    "opencode.parent_message.id": c.kind ? undefined : c.turn,
    "gen_ai.request.model": c.model,
    "gen_ai.provider.name": c.provider,
    "opencode.agent": c.agent,
    "opencode.compaction.reason": c.compactionReason,
  }
  // A failed call reports what it was billed, and nothing where it was billed nothing:
  // OpenCode gives a call that never reached the model zeros, which are not a usage.
  const used = c.error ? (v) => (num(v) > 0 ? v : undefined) : num
  const t = c.tokens ?? {}
  a["gen_ai.usage.input_tokens"] = used(t.input)
  a["gen_ai.usage.output_tokens"] = used(t.output)
  a["opencode.usage.reasoning_tokens"] = used(t.reasoning)
  a["gen_ai.usage.cache_read.input_tokens"] = used(t.cacheRead)
  a["gen_ai.usage.cache_write.input_tokens"] = used(t.cacheWrite)
  a["gen_ai.usage.total_cost"] = used(c.cost)
  if (c.error) {
    a["error.type"] = c.error.type
    // A provider's error can quote a prompt or a tool's arguments.
    if (p.includePrompts && p.includeToolContent) a["opencode.error.message"] = truncate(str(c.error.message), MAX_ERROR)
  } else {
    a["gen_ai.response.finish_reasons"] = typeof c.finish === "string" ? c.finish : undefined
    const text = str(c.text)
    if (text) {
      // The length always ships, so a reply withheld by policy still counts as one.
      a["opencode.completion.length"] = text.length
      if (p.includePrompts) a["gen_ai.completion"] = truncate(text, MAX_TEXT)
    }
  }
  emitSpan(s, {
    spanId: spanId("call", c.id),
    name: "chat " + str(c.model),
    kind: 3,
    startTimeUnixNano: nanos(c.start),
    endTimeUnixNano: nanos(endAfterCheck(s, c.end)),
    attributes: attrs({ ...a, ...common(s) }),
    status: c.error ? { code: 2 } : { code: 1 },
  })
}

// tool is one tool call that ended, including one that never ran: t = { messageID, callID,
// name, turn?, start, end, status, denial?, exitCode?, input?, output?, title?, files?,
// mcp?: { server, tool }, subagentSession? }.
function tool(s, t) {
  const p = policyOf(s)
  const id = qualify(t.messageID, t.callID)
  const output = str(t.output)
  const files = Array.isArray(t.files) ? t.files.filter((f) => typeof f === "string" && f) : []
  const a = {
    "gen_ai.operation.name": "execute_tool",
    "gen_ai.tool.name": t.name,
    "gen_ai.tool.call.id": id,
    "opencode.parent_message.id": t.turn,
    "opencode.tool.status": t.status,
    "opencode.tool.denial": t.status === "denied" ? t.denial : undefined,
    "opencode.tool.exit_code": Number.isInteger(t.exitCode) ? t.exitCode : undefined,
    "opencode.tool.output_bytes": output.length,
    "opencode.mcp.server": t.mcp?.server,
    "opencode.mcp.tool": t.mcp?.tool,
    "opencode.tool.subagent_session.id": t.subagentSession,
  }
  if (p.includeToolContent) {
    a["gen_ai.tool.call.arguments"] = t.input === undefined ? undefined : truncate(JSON.stringify(t.input), MAX_TOOL_CONTENT)
    a["gen_ai.tool.call.result"] = truncate(output, MAX_TOOL_CONTENT)
    a["opencode.tool.files"] = files
    a["opencode.tool.title"] = truncate(str(t.title), MAX_TOOL_TITLE)
  }
  emitSpan(s, {
    spanId: spanId("tool", id),
    name: "execute_tool " + str(t.name),
    kind: 1,
    startTimeUnixNano: nanos(t.start),
    endTimeUnixNano: nanos(endAfterCheck(s, t.end)),
    attributes: attrs({ ...a, ...common(s) }),
    status: t.status === "completed" ? { code: 1 } : { code: 2 },
  })
  // The touch names the call, so the platform joins it to this span; only a completed
  // edit changed a file. The hook has its own policy, so it runs whatever content says.
  if (EDIT_TOOLS.has(t.name) && t.status === "completed" && files.length > 0) {
    runHook(s, "opencode-file-edit", { ...base(s), tool: t.name, tool_call_id: id, files })
  }
}

// endAfterCheck is a model or tool call's end, no earlier than the last reach check made
// after its tool ran in the session's family. The relay forwards a record that ends before
// the run went Mixed, and a call that overlapped that check may carry what the checked call
// read or wrote. When OpenCode waits for the check nothing overlaps it, and no time changes.
function endAfterCheck(s, end) {
  const floor = checkFloor(s)
  return floor && num(end) !== undefined && end < floor ? floor : end
}

// prompt is one prompt: p = { messageID, text, agent?, model?, provider?, time? }. No
// prompt text ever goes to a hook.
function prompt(s, pr) {
  const text = str(pr.text)
  // No earlier than the session's last move or its last check made after a tool ran: the
  // relay files a record by its time, under the claim and the Mixed cutoff in force then.
  const floor = Math.max(s.movedAt, checkFloor(s))
  const time = floor ? Math.max(num(pr.time) ?? 0, floor) : pr.time
  emitRecord(s, "opencode.user_prompt", policyOf(s).includePrompts ? truncate(text, MAX_TEXT) : "", {
    "opencode.message.id": pr.messageID,
    "opencode.prompt.length": text.length,
    "opencode.agent": pr.agent,
    "gen_ai.request.model": pr.model,
    "gen_ai.provider.name": pr.provider,
  }, time)
  runHook(s, "opencode-prompt", base(s))
}

// title records a root session's new title: never the placeholder, never for a subagent
// (its title would replace the parent's), never the same text twice in a row.
function title(s, t) {
  if (s.parentID || !isRealTitle(t.title) || t.title === s.sentTitle) return
  s.sentTitle = t.title
  emitTitle(s, t.title, t.time)
}

// emitTitle stamps the moment the plugin saw the title, made strictly later than the
// session's previous title, so two titles of a session never share an identity, and no
// earlier than its last move or its last check made after a tool ran, as a prompt is.
function emitTitle(s, text, time) {
  const at = Math.max(num(time) ?? Date.now(), s.lastTitleTime + 1, s.movedAt, checkFloor(s))
  s.lastTitleTime = at
  emitRecord(s, "opencode.session.title", policyOf(s).includePrompts ? text : "", {}, at)
}

function titleCallStarted(s, c) {
  if (s.pendingTitle) titleCallEnded(s, { end: num(c.start) ?? Date.now() })
  s.pendingTitle = { ...c }
}

function titleCallEnded(s, { end, tokens, cost } = {}) {
  const c = s.pendingTitle
  if (!c) return
  s.pendingTitle = null
  call(s, { ...c, kind: "title", end: num(end) ?? Date.now(), tokens, cost })
}

function idle(s) {
  runHook(s, "opencode-idle", base(s))
}

function end(s, reason) {
  if (s.pendingTitle) titleCallEnded(s, { end: Date.now() })
  runHook(s, "opencode-session-end", { session_id: s.id, cwd: s.directory, reason })
  if (state.sessions.get(s.id) === s) state.sessions.delete(s.id)
}

// endUnknown ends a session this process never kept, as when an old one is deleted from a
// list: it is not claimed, titled or exported, only ended.
function endUnknown(rec, reason) {
  if (isTainted(rec)) return
  runHook(state.hooks.unowned, "opencode-session-end", { session_id: rec.id, cwd: rec.directory, reason })
}

// shutdown sends the pending title call of every session this entry first saw, then waits
// in three steps, each with its own limit: for those sessions' queues (EXIT_QUEUE_MS, 8 s),
// for the hooks still running, any entry's (HOOK_TIMEOUT_MS and the kill grace, 2.25 s),
// and for the flush (EXIT_FLUSH_MS, 4 s). At worst the exit is held for their sum, about 14 s.
async function shutdown(owner) {
  const now = Date.now()
  for (const s of state.sessions.values()) {
    if (s.owner === owner) run(s, () => s.pendingTitle && titleCallEnded(s, { end: now }))
  }
  const queues = async () => {
    for (let i = 0; i < 32; i++) {
      const busy = [...state.sessions.values()].filter((s) => s.owner === owner && s.busy > 0)
      if (busy.length === 0) break
      await Promise.all(busy.map((s) => s.queue))
    }
  }
  await Promise.race([queues(), sleep(EXIT_QUEUE_MS, true)])
  const hooks = [...state.hooks.running].map((lane) => lane.hookQueue)
  await Promise.race([Promise.all(hooks), sleep(HOOK_TIMEOUT_MS + HOOK_KILL_GRACE_MS, true)])
  await Promise.race([flush(), sleep(EXIT_FLUSH_MS, true)])
}

// --- reach --------------------------------------------------------------------------
//
// Where anything leaves, and what it passes. "queued" is a step of the session's queue
// (run), where a session that failed closed is skipped and a late check in the family is
// waited for; the floor is checkFloor, applied as the span or record is built.
//   a model or tool call's span        queued; its end is held to the floor
//   a prompt or title record           queued; its time is held to the floor
//   the file-edit, prompt, idle and    queued, from the step that sends what they belong to;
//   session-end hooks                  they carry no content
//   the claim (session-start hook)     the queue's first step; nothing precedes it
//   a reach check, and the request     beside the queue or in it: they are the guard, carry
//   to mark a run Mixed                no content, and must not wait for what they protect
//   the end of a session never kept    its own hook queue; an id and a folder only
// The OpenCode 2 entry (setup) sends through the same functions, from:
//   every event of a session           queued (handle2), and so every span, title and hook
//                                      an event brings
//   the prompt hook                    the record queued; the attached files' check beside
//                                      the queue, or behind a move still queued (waited2)
//   the title, side-question and       queued
//   compaction hooks, a code-mode
//   call's end
//   the title its record brings when   queued at the end of the queue, not with the claim:
//   first looked up                    the lookup can answer after later events were queued
//   a deleted session never kept       the end of a session never kept, above
//
// Where a reach is checked:
//   site                 checks                         when                         waits  sets
//   tool.execute.before  a read, search, list, shell    before the tool runs         yes    nothing
//                        command, write or edit
//   tool.execute.after   what a call reaches known      after the tool ran, before   yes    the family's floor
//                        once it ran (a patch's files)  its result is handed on
//   onToolPart1          a call no hook saw (a shell    when its completed part      no     the family's floor,
//                        command the person runs, one   arrives                             call's end
//                        finished in an earlier run)
//   chat.message         the files attached to a        before the prompt is sent    yes    the prompt's time,
//                        prompt                         on                                  after the check
//   update (a move)      the directory moved into       at the move's turn in the    no     movedAt
//                                                       session's queue
// The OpenCode 2 entry (setup):
//   tool.execute.before  as above, and a patch on the   before the tool runs         yes    nothing
//                        files its text names
//   endTool2             a call no hook saw, or a patch  when its end event arrives   no     lateCheck: the
//                        whose text names no file                                           family's floor, the
//                                                                                           call's end
//   session.prompt       the files attached to a        before the prompt is sent    yes    the prompt's time,
//                        prompt                         on                                  after the check
//   session.shell.       a shell command the person     when it starts               no     lateCheck: the
//   started              runs                                                               family's floor
//   session.moved        the directory moved into       at the move's turn in the    no     movedAt
//                                                       session's queue
// "waits" is whether OpenCode waits for the check. A check it waits for runs beside the
// session's queue (beside); in the OpenCode 2 entry it waits in the queue only behind a move
// still queued (waited2). One it does not wait for holds the family's queues (lateCheck).

// Each major's name for a tool that reads, changes or runs something, mapped to one
// vocabulary. An edit is checked on the file it changes, as a read is on the file it reads.
const REACH_TOOLS = { read: "read", write: "write", edit: "edit", grep: "grep", glob: "glob", list: "list", bash: "bash", shell: "bash" }

// toolReach describes what a read, edit, search, list or shell call can reach, or null.
function toolReach(name, args) {
  const kind = REACH_TOOLS[name]
  if (!kind || !args || typeof args !== "object") return null
  const out = {}
  const path = typeof args.filePath === "string" && args.filePath ? args.filePath : typeof args.path === "string" && args.path ? args.path : ""
  switch (kind) {
    case "read":
    case "write":
    case "edit":
      if (path) out.paths = [path]
      break
    case "grep":
      out.paths = [path || "."]
      if (typeof args.include === "string" && args.include) out.globs = [args.include]
      break
    case "glob":
      out.paths = [path || "."]
      if (typeof args.pattern === "string" && args.pattern) out.globs = [args.pattern]
      break
    case "list":
      if (path) out.lists = [path]
      break
    case "bash": {
      if (typeof args.command === "string" && args.command) out.command = args.command
      const dir = typeof args.workdir === "string" && args.workdir ? args.workdir : typeof args.cwd === "string" && args.cwd ? args.cwd : ""
      if (dir) out.dir = dir
      break
    }
  }
  return Object.keys(out).length > 0 ? { tool: kind, ...out } : null
}

// guardReach runs the reach hook for a call; one that fails taints the session (fails
// closed, failClosed). Resolves false when the session may not export the call.
async function guardReach(s, reach) {
  const { tool: name, ...rest } = reach
  const ok = await runHook(s, "opencode-tool-reach", { ...base(s), tool: name, ...rest })
  if (!ok) failClosed(s)
  return ok
}

// failClosed taints a session whose reach could not be checked, says so once in OpenCode's
// log and fences its run, unless terma cannot be run, which has its own warning.
function failClosed(s) {
  const fresh = !isTainted(s)
  taint(s)
  if (termaMissing()) return
  if (fresh) {
    const log = loggers[loggers.length - 1]
    try {
      log?.(`terma: could not check what a tool reaches in session ${s.id}; nothing more of this session is recorded.`)
    } catch {}
  }
  fence(s)
}

// subagentChecked is whether the session a call opened for a subagent, id, ran under the
// guard. One that never got its record (dark) or that failed closed ran unchecked, and its
// answer is the call's result, so the session that opened it, s, fails closed too. A
// subagent still waiting for its record is judged once that wait is over. An entry calls
// this before it sends the call that opened a subagent.
async function subagentChecked(s, id) {
  if (typeof id !== "string" || id === s.id) return true
  const child = state.sessions.get(id)
  if (child) await child.queue
  // By id too: a subagent that has ended or been forgotten is still known to have failed.
  if (!state.dark.has(id) && !state.tainted.has(id) && !(child && isTainted(child))) return true
  failClosed(s)
  return false
}

// fence asks terma to mark the run of a session that failed closed Mixed, as OMP's
// extension does: with a shell reach terma cannot place, so the relay drops what is
// exported of the run from then on and `terma doctor` names it. Nothing waits for it. One
// that fails is tried again at the session's next tool call; it goes straight to the hook
// queue, since run() skips a tainted session's work.
function fence(s) {
  if (s.fenced || s.fencing || termaMissing()) return
  s.fencing = true
  runHook(s, "opencode-tool-reach", { ...base(s), tool: "bash", command: REACH_FAILED_COMMAND }).then((ok) => {
    s.fencing = false
    if (ok) s.fenced = true
  })
}

// --- OpenCode 1 entry ---------------------------------------------------------------

function recordOf(info) {
  if (!info || typeof info !== "object" || typeof info.id !== "string" || !info.id) return null
  return {
    id: info.id,
    parentID: str(info.parentID),
    directory: str(info.directory),
    version: str(info.version),
    created: num(info.time?.created) ?? 0,
    title: str(info.title),
  }
}

// reachOf1 is what a completed OpenCode 1 call reached: what its arguments name, or for
// an edit, the files it changed (a patch's are known only once it ran). Null if nothing.
function reachOf1(s, tool, input, metadata) {
  const reach = toolReach(tool, input)
  if (reach || !EDIT_TOOLS.has(tool)) return reach
  const files = filesOf1(s, tool, { input, metadata })
  return files.length > 0 ? { tool, paths: files } : null
}

// The files a file tool read or changed, absolute.
function filesOf1(s, name, st) {
  let list = []
  if (name === "read" || name === "write" || name === "edit") list = [st.input?.filePath]
  // A patch that moves a file wrote the new path and removed the old one.
  else if (name === "apply_patch" && Array.isArray(st.metadata?.files)) list = st.metadata.files.flatMap((f) => [f?.filePath, f?.movePath])
  const files = list
    .filter((f) => typeof f === "string" && f)
    .map((f) => (isAbsolute(f) || !s.directory ? f : resolve(s.directory, f)))
  return [...new Set(files)]
}

// OpenCode 1 names an MCP tool <server>_<tool>, the server's name with anything but
// letters, digits, "-" and "_" made "_". Each name is tried as configured, as OpenCode
// writes it, and with every non-alphanumeric made "_"; the longest match wins, then the
// earlier form. OpenCode's own tools are never a server's. servers is the names the
// tool's own directory configured.
function mcpOf1(name, servers) {
  if (BUILTIN_UNDERSCORE_TOOLS.has(name)) return undefined
  let best = null
  for (const server of servers) {
    const forms = [server, server.replace(/[^A-Za-z0-9_-]/g, "_"), server.replace(/[^A-Za-z0-9]/g, "_")]
    forms.forEach((prefix, rank) => {
      if (name.length <= prefix.length + 1 || !name.startsWith(prefix + "_")) return
      if (!best || prefix.length > best.prefix.length || (prefix.length === best.prefix.length && rank < best.rank)) {
        best = { prefix, server, rank }
      }
    })
  }
  return best ? { server: best.server, tool: name.slice(best.prefix.length + 1) } : undefined
}

// entry1 is a session's OpenCode 1 data, made once with its whole shape: steps, the
// assistant messages a model step was seen for; prompts, the user messages that were real
// prompts; lastPrompt, the last real prompt's id, the turn a self-started message joins;
// subagents, the session each task call opened, by qualified call id.
function entry1(s) {
  const e = s.entry
  if (!e.steps) {
    e.steps = new Set()
    e.prompts = new Set()
    e.lastPrompt = ""
    e.subagents = new Map()
  }
  return e
}

// The turn a message belongs to is the prompt that opened it. OpenCode also starts turns
// itself, under a user message no person sent (the call after a command's subagent returns,
// a shell command the person ran): those join the last prompt's turn, when there is one.
function turnOf1(s, parentID) {
  const e = entry1(s)
  if (!parentID || e.prompts.has(parentID)) return parentID || undefined
  return e.lastPrompt || parentID
}

function onAssistant1(s, info) {
  let m = s.messages.get(info.id)
  if (!m) {
    const created = num(info.time?.created)
    // A fork copies its parent's messages with their original times: a message older
    // than its session is a copy, and its cost was the parent's.
    m = { parentID: str(info.parentID), created, copied: created !== undefined && created < s.created }
    remember(s.messages, info.id, m)
  }
  if (m.copied || s.doneCalls.has(info.id)) return
  if (!num(info.time?.completed) && !info.error) return
  remember(s.doneCalls, info.id)
  const parts = s.texts.get(info.id)
  s.texts.delete(info.id)
  const { steps } = entry1(s)
  const stepped = steps.has(info.id)
  steps.delete(info.id)
  // Every model call has step parts. A shell command the person ran, or the message that
  // launches a command's subagent, has none, no error and no usage: no model answered it,
  // whatever its finish says. It is not a call.
  const t = info.tokens ?? {}
  const usage = [info.cost, t.input, t.output, t.reasoning, t.cache?.read, t.cache?.write]
  if (!info.error && !stepped && !usage.some((v) => num(v) > 0)) return
  const text = parts ? [...parts.values()].filter((t) => t).join("\n\n") : ""
  const tokens = info.tokens && typeof info.tokens === "object" ? info.tokens : null
  const compaction = info.summary === true
  call(s, {
    id: info.id,
    kind: compaction ? "compaction" : undefined,
    messageID: info.id,
    turn: turnOf1(s, m.parentID),
    model: str(info.modelID),
    provider: str(info.providerID),
    agent: info.mode ?? info.agent,
    start: num(info.time?.created),
    end: num(info.time?.completed) ?? Date.now(),
    tokens: tokens && {
      input: tokens.input,
      output: tokens.output,
      reasoning: tokens.reasoning,
      cacheRead: tokens.cache?.read,
      cacheWrite: tokens.cache?.write,
    },
    cost: info.cost,
    finish: info.finish,
    text,
    error: info.error ? { type: info.error.name, message: info.error.data?.message ?? info.error.name } : undefined,
  })
}

function keepText1(s, part) {
  let parts = s.texts.get(part.messageID)
  if (!parts) {
    parts = new Map()
    remember(s.texts, part.messageID, parts, MAX_SMALL)
  }
  if (!parts.has(part.id) && parts.size >= MAX_SMALL) return
  parts.set(part.id, str(part.text))
}

async function onToolPart1(s, part, servers) {
  const st = part.state ?? {}
  const id = qualify(part.messageID, part.callID)
  // A task's part names the session it opened from when it starts to run; one that ends in
  // error may not, so the name is kept from the first part that carries it.
  const { subagents } = entry1(s)
  if (part.tool === "task" && typeof st.metadata?.sessionId === "string") remember(subagents, id, st.metadata.sessionId, MAX_SMALL)
  if (st.status !== "completed" && st.status !== "error") return
  const subagent = subagents.get(id)
  subagents.delete(id)
  if (s.doneTools.has(id)) return
  remember(s.doneTools, id)
  // Pruning republishes an old call it compacted; it is not a new one.
  if (st.time?.compacted) return
  if (s.messages.get(part.messageID)?.copied) return
  if (num(st.time?.end) !== undefined && st.time.end < s.created) return
  // A call no hook saw (a shell command the person runs, a call finished in an earlier
  // process) is checked now; one checked before or after it ran is not checked again.
  const reach = reachOf1(s, part.tool, st.input, st.metadata)
  const guarded = s.guarded.get(part.callID)
  s.guarded.delete(part.callID)
  let end = num(st.time?.end)
  if (reach && guarded?.reachKey !== JSON.stringify(reach)) {
    if (!(await lateCheck(s, reach))) return
    // The relay judges a record by its time: a call checked only after it ran must not end
    // before its check.
    end = Math.max(end ?? 0, checkFloor(s))
  }
  if (part.tool === "task" && !(await subagentChecked(s, subagent))) return
  if (isTainted(s)) return
  let status = "completed"
  let denial
  if (st.status === "error") {
    if (s.rejected.has(id)) {
      status = "denied"
      denial = "ask"
    } else if (str(st.error).startsWith(RULE_REFUSAL)) {
      status = "denied"
      denial = "rule"
    } else {
      status = "error"
    }
  }
  tool(s, {
    messageID: part.messageID,
    callID: part.callID,
    name: part.tool,
    turn: turnOf1(s, s.messages.get(part.messageID)?.parentID),
    start: num(st.time?.start),
    end,
    status,
    denial,
    exitCode: num(st.metadata?.exit),
    input: st.input,
    output: st.status === "completed" ? st.output : st.error,
    title: st.title,
    files: filesOf1(s, part.tool, st),
    mcp: mcpOf1(str(part.tool), servers),
    subagentSession: subagent,
  })
}

// server is OpenCode 1's entry: called once per directory, in the process serving it.
async function server(input) {
  if (!CONFIG || typeof CONFIG.endpoint !== "string" || !CONFIG.endpoint) return {}
  const owner = "opencode1:" + ++state.owners
  // The MCP servers this directory configured; another directory's names are not its.
  const mcpServers = new Set()
  const client = input?.client
  let warnedAt = 0
  loggers.push((message) => {
    warnedAt = Date.now()
    const r = client?.app?.log?.({ body: { service: "terma", level: "warn", message } })
    r?.catch?.(() => {})
  })
  if (loggers.length > 8) loggers.shift()
  // A resumed session's first event is often a hook: ask OpenCode for its record.
  const lookup = typeof client?.session?.get === "function"
    ? (id) => async () => recordOf((await client.session.get({ path: { id } }))?.data)
    : () => null
  const see = (id, record) => sight(id, { record, fetch: lookup(id), owner })
  const guard = (fn) => async (...args) => {
    try {
      await fn(...args)
    } catch {
      // A telemetry failure must never surface in the agent.
    }
  }

  function onEvent(event) {
    if (!fresh(event)) return
    const p = event?.properties ?? {}
    switch (event?.type) {
      case "session.created":
      case "session.updated": {
        const rec = recordOf(p.info)
        if (!rec) return
        // The title call ends when its title arrives; its span goes before the title.
        const known = state.sessions.get(rec.id)
        if (known?.known && isRealTitle(rec.title) && rec.title !== known.title) {
          const at = Date.now()
          run(known, () => known.pendingTitle && titleCallEnded(known, { end: at }))
        }
        see(rec.id, rec)
        return
      }
      case "session.deleted": {
        const rec = recordOf(p.info)
        if (!rec) return
        if (!state.sessions.has(rec.id)) {
          endUnknown(rec, "deleted")
          return
        }
        const s = see(rec.id, rec)
        run(s, () => end(s, "deleted"))
        return
      }
      case "session.idle": {
        const s = see(p.sessionID)
        run(s, () => idle(s))
        return
      }
      case "message.updated": {
        const info = p.info
        if (info?.role !== "assistant" || typeof info.id !== "string") return
        const s = see(info.sessionID)
        run(s, () => onAssistant1(s, info))
        return
      }
      case "message.part.updated": {
        const part = p.part
        if (!part || typeof part.messageID !== "string") return
        if (part.type === "text") {
          const s = see(part.sessionID)
          run(s, () => keepText1(s, part))
        } else if (part.type === "tool") {
          const s = see(part.sessionID)
          run(s, () => onToolPart1(s, part, mcpServers))
        } else if (part.type === "step-start" || part.type === "step-finish") {
          const s = see(part.sessionID)
          run(s, () => remember(entry1(s).steps, part.messageID))
        }
        return
      }
      case "permission.asked": {
        const s = see(p.sessionID)
        if (typeof p.id === "string" && p.tool?.messageID && p.tool?.callID) {
          const id = qualify(p.tool.messageID, p.tool.callID)
          run(s, () => remember(s.asked, p.id, id, MAX_SMALL))
        }
        return
      }
      case "permission.replied": {
        const s = see(p.sessionID)
        if ((p.reply ?? p.response) === "reject") {
          run(s, () => {
            const id = s.asked.get(p.requestID)
            if (id) remember(s.rejected, id, undefined, MAX_SMALL)
          })
        }
        return
      }
    }
  }

  return {
    event: guard(async ({ event } = {}) => onEvent(event)),

    config: guard(async (cfg) => {
      for (const name of Object.keys(cfg?.mcp ?? {})) remember(mcpServers, name, undefined, MAX_SMALL)
    }),

    "chat.message": guard(async (inp, out) => {
      const s = see(inp?.sessionID)
      if (!s) return
      const msg = out?.message ?? {}
      const parts = Array.isArray(out?.parts) ? out.parts : []
      // An attached file (or folder) is read into synthetic text parts with no tool call:
      // reach-check it as a read or a list, and keep its content out of the prompt.
      const reaches = []
      const taken = new Set()
      for (const x of parts) {
        if (x?.type !== "file" || typeof x.url !== "string" || !x.url.startsWith("file:")) continue
        let path
        try {
          path = fileURLToPath(x.url)
        } catch {
          continue
        }
        reaches.push(x.mime === "application/x-directory" ? { tool: "list", lists: [path] } : { tool: "read", paths: [path] })
        taken.add(path)
      }
      // An attached image or PDF arrives as a data: URL; where it came from is only in the
      // synthetic part that says OpenCode read it. OpenCode may write that path unescaped
      // (a Windows path), so a marker that is not JSON is read for the path as written, and
      // one that names no path at all is a reach no one can place.
      let unplaced = false
      for (const x of parts) {
        if (x?.type !== "text" || x.synthetic !== true || !str(x.text).startsWith(READ_ATTACHMENT)) continue
        const raw = x.text.slice(READ_ATTACHMENT.length)
        let path
        try {
          path = JSON.parse(raw)?.filePath
        } catch {}
        if (typeof path !== "string" || !path) path = /^\{"filePath":"([\s\S]*)"\}$/.exec(raw)?.[1]
        if (!path) {
          unplaced = true
          continue
        }
        if (taken.has(path)) continue
        reaches.push({ tool: "read", paths: [path] })
        taken.add(path)
      }
      if (unplaced) reaches.push({ tool: "read", paths: [UNPLACED_ATTACHMENT] })
      // Under `opencode run` the hook's own input names no agent or model; the message does.
      const pr = {
        messageID: msg.id ?? inp?.messageID,
        text: parts.filter((x) => x?.type === "text" && x.synthetic !== true).map((x) => str(x.text)).join("\n"),
        agent: msg.agent ?? inp?.agent ?? undefined,
        model: msg.model?.modelID ?? inp?.model?.modelID,
        provider: msg.model?.providerID ?? inp?.model?.providerID,
        time: num(msg.time?.created),
      }
      const sent = run(s, async () => {
        for (const reach of reaches) if (!(await guardReach(s, reach))) return
        // A reach that crosses organizations marks the run from the moment its hook did; the
        // prompt is stamped after it, and OpenCode goes on only once the checks are done.
        if (reaches.length > 0) pr.time = Math.max(pr.time ?? 0, Date.now() + 1)
        if (isTainted(s)) return
        if (pr.messageID) {
          const e = entry1(s)
          remember(e.prompts, pr.messageID)
          e.lastPrompt = pr.messageID
        }
        prompt(s, pr)
      })
      if (reaches.length > 0) await sent
    }),

    // Only to see the title call start. It writes nothing to the request.
    "chat.params": guard(async (inp) => {
      if (inp?.agent !== "title" || typeof inp?.message?.id !== "string") return
      const s = see(inp.sessionID)
      const c = { id: "title:" + inp.message.id, model: inp.model?.id, provider: inp.model?.providerID, agent: "title", start: Date.now() }
      run(s, () => titleCallStarted(s, c))
    }),

    // The reach guard: the tool waits for the hook, and a failure taints the session.
    "tool.execute.before": guard(async (inp, out) => {
      const s = see(inp?.sessionID)
      if (!s || typeof inp?.callID !== "string") return
      // A session that failed closed asks again, if it must, for its run to be fenced; the
      // tool does not wait for it.
      if (isTainted(s)) fence(s)
      const reach = toolReach(inp.tool, out?.args ?? inp.args)
      if (!reach) return
      remember(s.guarded, inp.callID, { reachKey: JSON.stringify(reach) }, MAX_SMALL)
      await beside(s, () => guardReach(s, reach))
    }),

    // The guard for what a call is known to reach only once it ran (a patch's files):
    // OpenCode waits for this hook before it hands the result on, sets the call's end and
    // starts the next model step, so nothing that saw the result ends before the check.
    "tool.execute.after": guard(async (inp, out) => {
      const s = see(inp?.sessionID)
      if (!s || typeof inp?.callID !== "string") return
      const callID = inp.callID
      // A call that reaches nothing, or that was checked before it ran, does not wait.
      const before = toolReach(inp.tool, inp.args)
      if (before ? s.guarded.get(callID)?.reachKey === JSON.stringify(before) : !EDIT_TOOLS.has(inp.tool)) return
      // After its first sight: a relative path resolves against the session's directory.
      await beside(s, async () => {
        const reach = reachOf1(s, inp.tool, inp.args, out?.metadata)
        if (!reach) return
        const reachKey = JSON.stringify(reach)
        if (s.guarded.get(callID)?.reachKey === reachKey) return
        if (!(await guardReach(s, reach))) return
        checked(s)
        remember(s.guarded, callID, { reachKey }, MAX_SMALL)
      })
    }),

    dispose: guard(async () => {
      await shutdown(owner)
      // A short `opencode run` would otherwise end before OpenCode wrote the warning down.
      const left = warnedAt ? LOG_SETTLE_MS - (Date.now() - warnedAt) : 0
      if (left > 0) await sleep(left)
    }),
  }
}

// --- OpenCode 2 entry ---------------------------------------------------------------

// OpenCode 1.18 and later call setup too, with a context that has no app, event or session.
function isOpenCode2(ctx) {
  return typeof ctx?.event?.subscribe === "function" && typeof ctx?.session?.hook === "function" && /^[2-9]\d*\./.test(String(ctx?.app?.version ?? ""))
}

// The events the OpenCode 2 entry reads. Any other one is ignored without a look at its
// session: a session that is only listed or viewed is not claimed.
const EVENTS2 = new Set([
  "session.created",
  "session.moved",
  "session.deleted",
  "session.inbox.delivered",
  "session.step.started",
  "session.step.ended",
  "session.step.failed",
  "session.text.ended",
  "session.tool.input.started",
  "session.tool.called",
  "session.tool.progress",
  "session.tool.success",
  "session.tool.failed",
  "permission.asked",
  "session.renamed",
  "session.usage.updated",
  "session.compaction.started",
  "session.compaction.ended",
  "session.compaction.failed",
  "session.execution.succeeded",
  "session.execution.failed",
  "session.execution.interrupted",
  "session.shell.started",
])

// What `opencode run -f` appends to a prompt's text for each text file: its name and content.
const INLINED_FILES = /(?:^|\n\n)<file name="[\s\S]*">\n[\s\S]*\n<\/file>$/

// data2 is the OpenCode 2 entry's own data for a session, kept in its entry slot.
function data2(s) {
  const e = s.entry
  if (!e.steps) {
    e.turn = "" // the prompt whose delivery started the current turn
    e.lastPrompt = "" // the latest prompt asked, which names a title call
    e.prompts = new Set() // prompt ids, to tell a prompt's delivery from any other inbox item
    e.steps = new Map() // assistantMessageID -> { turn, agent, model, provider, start, texts }
    e.tools = new Map() // qualified call id -> { messageID, callID, name, input, start, turn, mcp, subagent }
    e.asked = new Set() // qualified call ids OpenCode asked permission for
    e.code = new Map() // qualified call id -> start of a code-mode call
    e.usage = null // the session's running totals as last seen
    e.ended = [] // the usage of steps that ended, until the usage update that brings it
    e.unsure = false // a call the plugin does not report ran since the title was last sent
    e.titleRise = null // the last usage update's rise, when nothing else explains it
    e.compactionRequest = null
    e.compaction = null
    e.movedFrom = "" // where a move found the session, when it arrived before the record
    e.moves = 0 // moves queued and not yet done (waited2)
    e.floor = 0 // the session's claim, or just after its last reach check: no prompt is stamped earlier
    e.stamps = new Map() // prompt id -> the time its record carries
  }
  return e
}

// checked2 runs a reach check OpenCode waits for, before what it checks runs, and notes when
// it was done. The relay judges a record by its time, so a prompt that follows a check is
// stamped after it, never in its millisecond.
async function checked2(s, reach) {
  const ok = await guardReach(s, reach)
  data2(s).floor = Date.now() + 1
  return ok
}

// waited2 runs a reach check OpenCode waits for. It runs beside the session's queue (the
// core's beside), so it waits neither for a late check in the family nor behind what the
// session has queued to send. A move still waiting in the queue is the exception: the check
// waits in the queue behind it, so it is made in the directory the session moved to, after
// the session's claim there.
function waited2(s, fn) {
  const moving = data2(s).moves > 0 || (s.askedDirectory && s.askedDirectory !== s.directory)
  return moving ? run(s, fn) : beside(s, fn)
}

// lateChecked2 is checked2 for what already ran with nothing waiting for its check (a call
// no hook saw, a shell command the person runs): the core's lateCheck holds the session's
// family while it runs and sets the family's floor (checkFloor) when it passes.
async function lateChecked2(s, reach) {
  const ok = await lateCheck(s, reach)
  data2(s).floor = Date.now() + 1
  return ok
}

// begun2 is a start or end of a span of a turn: OpenCode's own time, but never before the
// turn's prompt, whose record is held back behind the session's claim, a move or a reach
// check ahead of it. A reader that orders a session by time then finds each call under the
// prompt that asked for it. A call that ran wholly inside that wait (OpenCode does not wait
// for a claim) starts and ends at the prompt.
function begun2(e, turn, t) {
  const stamp = e.stamps.get(turn)
  return stamp !== undefined && t < stamp ? stamp : t
}

// unexplainedRise2 is how far a usage update raised the session's totals when nothing the
// plugin reports explains it, else null: OpenCode 2 gives a title call no usage of its own,
// only the session's totals, so such a rise right before a rename is the title call's. A
// step that ended explains exactly what it used (taken off e.ended), in any order, and a
// compaction in flight brings its own. Totals only rise; a fall explains nothing.
function unexplainedRise2(e, now) {
  const prev = e.usage ?? totals2(null)
  e.usage = now
  const r = {}
  for (const k of Object.keys(now)) r[k] = now[k] - prev[k]
  r.cost = Number(r.cost.toFixed(12))
  const step = e.ended.findIndex((u) => Object.keys(r).every((k) => (k === "cost" ? Number(u.cost.toFixed(12)) : u[k]) === r[k]))
  if (step >= 0) e.ended.splice(step, 1)
  const values = Object.values(r)
  return step < 0 && !e.compaction && values.every((v) => v >= 0) && values.some((v) => v > 0) ? r : null
}

function totals2(rec) {
  const t = rec?.tokens ?? {}
  return {
    cost: num(rec?.cost) ?? 0,
    input: num(t.input) ?? 0,
    output: num(t.output) ?? 0,
    reasoning: num(t.reasoning) ?? 0,
    cacheRead: num(t.cache?.read) ?? 0,
    cacheWrite: num(t.cache?.write) ?? 0,
  }
}

function tokens2(t) {
  if (!t || typeof t !== "object") return undefined
  return { input: t.input, output: t.output, reasoning: t.reasoning, cacheRead: t.cache?.read, cacheWrite: t.cache?.write }
}

function texts2(content) {
  return Array.isArray(content) ? content.filter((p) => p?.type === "text").map((p) => str(p.text)).join("\n") : ""
}

// reach2 is what a call can reach, known before it runs: the core's for a read, an edit, a
// search or a shell call, and for a patch the files its text names. OpenCode 2 waits for
// tool.execute.before, so a patch is checked before it changes anything.
function reach2(name, input) {
  const reach = toolReach(name, input)
  if (reach || name !== "patch") return reach
  const paths = patchFiles2(input?.patchText)
  return paths.length > 0 ? { tool: name, paths } : null
}

// patchFiles2 lists the files a patch text adds, changes, deletes or moves to, as written.
function patchFiles2(text) {
  if (typeof text !== "string") return []
  const out = []
  for (const m of text.matchAll(/^\*\*\* (?:Add File|Update File|Delete File|Move to): (.+?)\s*$/gm)) {
    if (m[1] && !out.includes(m[1])) out.push(m[1])
  }
  return out
}

// The files a file tool read or changed, absolute: OpenCode 2 names them relative to the
// directory the session runs in.
function filesOf2(s, name, input, metadata) {
  let list = []
  if (name === "read" || name === "write" || name === "edit") list = [input?.path]
  // A patch that moves a file names the new path; the one it removed heads its diff.
  else if (name === "patch" && Array.isArray(metadata?.files)) list = metadata.files.flatMap((f) => [f?.file, /^Index: (.+)$/m.exec(str(f?.patch))?.[1]])
  const files = list
    .filter((f) => typeof f === "string" && f)
    .map((f) => (isAbsolute(f) || !s.directory ? f : resolve(s.directory, f)))
  return [...new Set(files)]
}

// A code-mode call names its MCP tool `<server>_<tool>` as its id and the tool alone as its
// name.
function mcpOf2(info) {
  const id = info?.id
  const name = info?.name
  if (typeof id !== "string" || typeof name !== "string" || !name || id.length <= name.length + 1 || !id.endsWith("_" + name)) return undefined
  return { server: id.slice(0, -(name.length + 1)), tool: name }
}

function pendingTool2(e, d) {
  const q = qualify(d.assistantMessageID, d.id)
  let t = e.tools.get(q)
  if (!t) {
    t = { messageID: str(d.assistantMessageID), callID: str(d.id), name: "", turn: e.steps.get(d.assistantMessageID)?.turn ?? e.turn }
    remember(e.tools, q, t, MAX_SMALL)
  }
  return t
}

// handle2 applies one of a session's events, from inside its queue.
function handle2(s, event) {
  const e = data2(s)
  const d = event.data ?? {}
  const at = num(event.created)
  // A rise in usage is the title call's only when its update is the last event before the
  // rename.
  const rise = e.titleRise
  if (event.type !== "session.usage.updated") e.titleRise = null
  switch (event.type) {
    case "session.inbox.delivered":
      // The turn changes when a prompt reaches the model, not when it is queued: a prompt
      // can be queued while a step of the previous one still runs.
      if (e.prompts.has(d.inboxID)) e.turn = d.inboxID
      return
    case "session.step.started": {
      const id = str(d.assistantMessageID)
      if (!id) return
      let step = e.steps.get(id)
      if (!step) {
        step = { turn: e.turn }
        remember(e.steps, id, step, MAX_SMALL)
      }
      // A retried step keeps its id and starts again: the last attempt is the call.
      step.agent = str(d.agent)
      step.model = str(d.model?.id)
      step.provider = str(d.model?.providerID)
      step.start = num(d.started) ?? at
      step.texts = new Map()
      return
    }
    case "session.text.ended": {
      const step = e.steps.get(d.assistantMessageID)
      if (!step) return
      const ordinal = num(d.ordinal) ?? step.texts.size
      if (!step.texts.has(ordinal) && step.texts.size >= MAX_SMALL) return
      step.texts.set(ordinal, str(d.text))
      return
    }
    case "session.step.ended":
    case "session.step.failed": {
      const id = str(d.assistantMessageID)
      if (!id || s.doneCalls.has(id)) return
      remember(s.doneCalls, id)
      const failed = event.type === "session.step.failed"
      // What the step used arrives once more, as a rise of the session's totals; a step
      // that failed before it used anything brings none.
      const used = totals2(d)
      if (Object.values(used).some((v) => v > 0)) {
        e.ended.push(used)
        if (e.ended.length > 8) e.ended.shift()
      }
      const step = e.steps.get(id)
      e.steps.delete(id)
      if (!step) return
      const text = [...step.texts.entries()]
        .sort((a, b) => a[0] - b[0])
        .map(([, t]) => t)
        .filter((t) => t)
        .join("\n\n")
      call(s, {
        id,
        messageID: id,
        turn: step.turn || undefined,
        model: step.model,
        provider: step.provider,
        agent: step.agent,
        start: begun2(e, step.turn, step.start),
        end: begun2(e, step.turn, at),
        tokens: tokens2(d.tokens),
        cost: d.cost,
        finish: d.finish,
        text,
        error: failed ? { type: d.error?.type, message: d.error?.message } : undefined,
      })
      return
    }
    case "session.tool.input.started":
      pendingTool2(e, d).name = str(d.name)
      return
    case "session.tool.called": {
      const t = pendingTool2(e, d)
      t.input = d.input
      t.start = at
      return
    }
    case "session.tool.progress": {
      const t = e.tools.get(qualify(d.assistantMessageID, d.id))
      // The session a subagent call opened is named from its first progress on: a call that
      // ends in error may not name it again.
      if (t && typeof d.metadata?.sessionID === "string") t.subagent ||= d.metadata.sessionID
      return
    }
    case "permission.asked":
      if (d.source?.type === "tool" && d.source.messageID && d.source.id) remember(e.asked, qualify(d.source.messageID, d.source.id), undefined, MAX_SMALL)
      return
    case "session.tool.success":
    case "session.tool.failed":
      return endTool2(s, e, event)
    case "session.usage.updated":
      e.titleRise = unexplainedRise2(e, totals2(d))
      return
    case "session.renamed": {
      // The title call's span goes before its title. A side question's usage cannot be told
      // from the title's, so a title that shared its time with one is sent without any.
      const unsure = e.unsure
      e.unsure = false
      // Like the turn's own calls, the title call and its title go no earlier than the
      // prompt they title (begun2): a fast title call can end while the claim is still made.
      let end = begun2(e, s.pendingTitle?.turn ?? e.lastPrompt, at)
      // Nor before the title call began: the hook's time is the plugin's, the event's OpenCode's.
      if (end !== undefined && s.pendingTitle?.start > end) end = s.pendingTitle.start
      if (rise && !unsure) {
        const { cost, ...tokens } = rise
        titleCallEnded(s, { end, tokens, cost })
      } else {
        titleCallEnded(s, { end })
      }
      const text = str(d.title)
      if (!text) return
      s.title = text
      // The core stamps it no earlier than the session's last move or its last check made
      // after a tool ran, whatever time OpenCode gave the event.
      title(s, { title: text, time: end })
      return
    }
    case "session.compaction.started":
      e.compaction = { inputID: str(d.inputID), reason: str(d.reason), start: at }
      return
    case "session.compaction.ended":
    case "session.compaction.failed": {
      const c = e.compaction ?? { inputID: str(d.inputID), reason: str(d.reason), start: at }
      e.compaction = null
      const id = "compaction:" + (c.inputID || str(event.id))
      if (s.doneCalls.has(id)) return
      remember(s.doneCalls, id)
      const req = e.compactionRequest ?? {}
      e.compactionRequest = null
      const failed = event.type === "session.compaction.failed"
      call(s, {
        id,
        kind: "compaction",
        model: d.model?.id ?? req.model,
        provider: d.model?.providerID ?? req.provider,
        agent: req.agent,
        start: c.start,
        end: at,
        tokens: tokens2(d.tokens),
        cost: d.cost,
        text: failed ? undefined : d.text,
        error: failed ? { type: d.error?.type, message: d.error?.message } : undefined,
        compactionReason: str(d.reason) || c.reason,
      })
      return
    }
    case "session.execution.succeeded":
    case "session.execution.failed":
    case "session.execution.interrupted":
      idle(s)
      return
    case "session.shell.started": {
      // A shell command the person ran in the session: its output joins the conversation
      // with no tool call, so it is reach-checked as the agent's own shell calls are.
      const command = str(d.shell?.command)
      if (!command) return
      return lateChecked2(s, { tool: "bash", command, dir: str(d.shell?.cwd) || undefined })
    }
  }
}

async function endTool2(s, e, event) {
  const d = event.data ?? {}
  const q = qualify(d.assistantMessageID, d.id)
  if (s.doneTools.has(q)) return
  remember(s.doneTools, q)
  const t = e.tools.get(q)
  e.tools.delete(q)
  const asked = e.asked.delete(q)
  if (!t?.name) return
  // The call may have reached something the guard before it did not see; check again. An
  // edit whose files its input did not name (a patch text that names none) is checked on
  // the files OpenCode reports it changed, only now.
  let reach = reach2(t.name, t.input)
  if (!reach && EDIT_TOOLS.has(t.name)) {
    const files = filesOf2(s, t.name, t.input, d.metadata)
    if (files.length > 0) reach = { tool: t.name, paths: files }
  }
  const guarded = s.guarded.get(q)
  s.guarded.delete(q)
  const recheck = !!reach && guarded?.reachKey !== JSON.stringify(reach)
  // Checked only after it ran: it, and every call the session's family sends from now on,
  // ends no earlier than the check (endAfterCheck), as the relay judges a span by its times.
  if (recheck && !(await lateChecked2(s, reach))) return
  if (isTainted(s)) return
  // A subagent's result is the child's work (on 2.4.1 its answer reaches the parent later,
  // as an inbox item the parent's next reply can quote): a child that ran unchecked takes
  // its parent with it, before any of what it returned leaves.
  if (t.name === "subagent") {
    // On 2.4.1 the call returns as soon as the child starts. The core judges a child it knows
    // once its first sight is over (after which it cannot go dark); one this process has not
    // seen yet is waited for, a bounded time, and one that never appears fails the parent.
    const child = t.subagent || str(d.metadata?.sessionID) || ""
    if (child && !(await childSeen2(child))) {
      failClosed(s)
      return
    }
    if (!(await subagentChecked(s, child))) return
  }
  const ok = event.type === "session.tool.success"
  const end = recheck ? Math.max(num(event.created) ?? 0, checkFloor(s)) : num(event.created)
  tool(s, {
    messageID: t.messageID,
    callID: t.callID,
    name: t.name,
    turn: t.turn || undefined,
    start: begun2(e, t.turn, t.start ?? end),
    end: begun2(e, t.turn, end),
    status: ok ? "completed" : d.error?.type === "permission.rejected" ? "denied" : "error",
    denial: asked ? "ask" : "rule",
    exitCode: num(d.metadata?.exit),
    input: t.input,
    output: ok ? texts2(d.content) : str(d.error?.message),
    files: filesOf2(s, t.name, t.input, d.metadata),
    mcp: t.mcp,
    subagentSession: t.name === "subagent" ? t.subagent || str(d.metadata?.sessionID) || undefined : undefined,
  })
}

// childSeen2 waits, RECORD_WAIT_MS at most, for a subagent's session to reach this process.
async function childSeen2(id) {
  for (const until = Date.now() + RECORD_WAIT_MS; Date.now() < until; ) {
    if (state.sessions.has(id) || state.tainted.has(id)) return true
    await sleep(25)
  }
  return state.sessions.has(id) || state.tainted.has(id)
}

// codeCall2 reports an MCP call made in code mode, which exists only in the hook. When
// OpenCode also reports the call as its own (it does for an MCP tool the model called by
// its flat name), the events report it and the hook only names its server.
function codeCall2(s, input, at) {
  const e = data2(s)
  const q = qualify(input.messageID, input.id)
  const mcp = mcpOf2(input.info)
  const pending = e.tools.get(q)
  if (pending) {
    if (mcp) pending.mcp = mcp
    return
  }
  if (s.doneTools.has(q)) return
  remember(s.doneTools, q)
  const start = e.code.get(q)
  e.code.delete(q)
  const turn = e.steps.get(input.messageID)?.turn ?? e.turn
  const ok = input.status === "completed"
  tool(s, {
    messageID: str(input.messageID),
    callID: str(input.id),
    name: str(input.tool),
    turn: turn || undefined,
    start: begun2(e, turn, start ?? at),
    end: begun2(e, turn, at),
    // The hook cannot tell a denial from a failure.
    status: ok ? "completed" : "error",
    input: input.input,
    output: ok ? (typeof input.result?.output === "string" ? input.result.output : texts2(input.result?.content)) : str(input.error?.message),
    mcp,
  })
}

// setup is OpenCode 2's entry: called once per directory, in the server serving it, and
// every copy's subscription receives every directory's events.
async function setup(ctx) {
  if (!isOpenCode2(ctx)) return
  if (!CONFIG || typeof CONFIG.endpoint !== "string" || !CONFIG.endpoint) return
  // A session belongs to its directory's copy, whichever copy's subscription saw it first:
  // a copy that is reloaded then closes its own sessions and no other directory's.
  const ownerOf = (directory) => "opencode2:" + directory
  const here = str(ctx.location?.directory)
  const owner = here ? ownerOf(here) : "opencode2:" + ++state.owners
  const version = String(ctx.app.version)
  // OpenCode 2 gives a plugin no log call; this reaches the server's own stderr.
  loggers.push((message) => console.error(message))
  if (loggers.length > 8) loggers.shift()

  // The session's record, and its running totals as the baseline of its usage updates.
  const lookup = (id) => async () => {
    if (typeof ctx.session?.get !== "function") return null
    const rec = await ctx.session.get({ sessionID: id })
    if (!rec || typeof rec !== "object" || rec.id !== id) return null
    const s = state.sessions.get(id)
    if (s) data2(s).usage ??= totals2(rec)
    // The title as it is now, not as the session came with it: it may be newer than events
    // already queued behind this lookup, so it takes its own place at the end of the queue
    // (title), behind a call still to be checked, rather than going out with the claim.
    const named = str(rec.title)
    if (s && named) {
      const at = Date.now()
      run(s, () => title(s, { title: named, time: at }))
    }
    return {
      id,
      parentID: str(rec.parentID),
      // A move that arrived while the record was on its way is applied in its own place in
      // the queue; until then the session is where that move found it, which the record may
      // already have left behind.
      directory: (s && data2(s).movedFrom) || str(rec.location?.directory),
      version,
      created: num(rec.time?.created) ?? 0,
      title: "",
    }
  }
  const see = (id, record, directory) => {
    const first = typeof id === "string" && !state.sessions.has(id)
    const s = sight(id, { record, fetch: lookup(id), owner: directory ? ownerOf(directory) : owner })
    // Next in the session's queue after its claim: the relay files a record under the claim
    // and the run in force at its time, so no prompt is stamped before this one's claim.
    if (s && first) run(s, () => void (data2(s).floor = Math.max(data2(s).floor, Date.now())))
    return s
  }
  const guard = (fn) => async (input) => {
    try {
      await fn(input ?? {})
    } catch {
      // A telemetry failure must never surface in the agent.
    }
  }

  function onEvent(event) {
    // The type first: the stream is mostly deltas, which must not fill the memory of seen ids.
    if (!EVENTS2.has(event?.type) || !fresh(event)) return
    const d = event.data ?? {}
    const id = d.sessionID
    if (typeof id !== "string" || !id) return
    // Where the event's session runs, when the event says: its own directory's copy owns it.
    const directory = str(event.location?.directory)
    let s
    switch (event.type) {
      case "session.created": {
        const created = str(d.location?.directory) || directory
        s = see(id, { id, parentID: str(d.parentID), directory: created, version: str(d.version) || version, created: num(event.created) ?? 0, title: str(d.title) }, created)
        break
      }
      case "session.deleted":
        if (!state.sessions.has(id)) {
          endUnknown({ id, directory: str(event.location?.directory), parentID: "" }, "deleted")
          return
        }
        s = see(id)
        run(s, () => end(s, "deleted"))
        return
      case "session.moved": {
        // The event's own location is still the old directory; its data names the new one.
        // The move takes this event's place in the session's queue, whether or not the
        // session's record has arrived: what was asked before it runs where it was asked,
        // under that repository's policy, and what follows it under the new one's. Whether
        // it is a move at all is decided when its turn comes, so a move back is one too. It
        // does what update does for a move: the new policy, then a claim there, and nothing
        // of the session stamped earlier than that.
        const to = str(d.location?.directory)
        s = see(id, undefined, directory)
        if (!s.known && directory) data2(s).movedFrom ||= directory
        if (to) {
          // Counted until its turn is done: a check OpenCode waits for waits for it (waited2).
          const e = data2(s)
          e.moves++
          run(s, async () => {
            if (to === s.directory) return
            s.askedDirectory = to
            s.directory = to
            s.policy = repoPolicy(to)
            await guardReach(s, { tool: "list", lists: [to] })
            s.movedAt = Date.now() + 1
          }).then(() => e.moves--)
        }
        break
      }
      default:
        s = see(id, undefined, directory)
    }
    run(s, () => handle2(s, event))
  }

  const hooks = {
    session: {
      prompt: async (input) => {
        const s = see(input.sessionID)
        if (!s) return
        const pr = { messageID: str(input.messageID), text: str(input.prompt?.text) }
        const asked = Date.now()
        // A file attached to a prompt is read into the conversation with no tool call:
        // reach-check it as a read before the prompt leaves. `opencode run -f` gives a file
        // without its place: a text file as its name and content at the end of the prompt's
        // text, an image or a PDF as a data: URL. Such a file cannot be reach-checked.
        const reaches = []
        let unplaced = INLINED_FILES.test(pr.text)
        for (const f of Array.isArray(input.prompt?.files) ? input.prompt.files : []) {
          const uri = [f?.source?.uri, f?.uri].find((u) => typeof u === "string" && u.startsWith("file:"))
          let path
          try {
            path = uri && fileURLToPath(uri)
          } catch {}
          if (path) reaches.push({ tool: "read", paths: [path] })
          // An image pasted from the clipboard has no file behind it: it is the person's own
          // input, like pasted text, and nothing of it is exported.
          else if (!(f?.name === "clipboard" && f.source === undefined && str(f.uri).startsWith("data:image/"))) unplaced = true
        }
        // A prompt must not wait for telemetry; one with a file waits for its reach check,
        // so the run is marked before OpenCode reads the file into the conversation. The
        // check sends nothing, so it waits for no late check in the family (waited2).
        let ok = true
        const checking =
          unplaced || reaches.length > 0
            ? waited2(s, async () => {
                // A file without its place fails closed, as an unknown reach does: terma marks
                // the run, which holds in a later process too.
                if (unplaced) await checked2(s, { tool: "read", paths: [UNPLACED_ATTACHMENT] })
                else for (const reach of reaches) if (!(ok = await checked2(s, reach))) return
              })
            : undefined
        run(s, async () => {
          await checking
          const e = data2(s)
          if (pr.messageID) {
            remember(e.prompts, pr.messageID, undefined, MAX_SMALL)
            e.lastPrompt = pr.messageID
          }
          // In its turn: what the session queued before the prompt still leaves; this
          // process hands nothing of the session over from the prompt on.
          if (unplaced) {
            taint(s)
            return
          }
          if (!ok) return
          // The prompt carries the moment it was asked, held back only behind what the relay
          // must judge it by: the session's claim, its own reach checks, and a move or a
          // check that was ahead of it in the queue. The spans of its turn start no earlier
          // (begun2), so the calls a prompt starts come after it.
          pr.time = Math.max(asked, e.floor, s.movedAt, checkFloor(s))
          if (pr.messageID) remember(e.stamps, pr.messageID, pr.time, MAX_SMALL)
          if (!isTainted(s)) prompt(s, pr)
        })
        await checking
      },
      title: async (input) => {
        const s = see(input.sessionID)
        if (!s) return
        const start = Date.now()
        const model = input.model?.id
        const provider = input.model?.providerID
        // The id is taken in the queue, after the prompt's own entry named the prompt.
        run(s, () => {
          const id = "title:" + (data2(s).lastPrompt || s.id)
          // OpenCode asks again, with another model, when the first gave no title: one call.
          if (s.pendingTitle?.id === id) {
            s.pendingTitle.model = model
            s.pendingTitle.provider = provider
            return
          }
          const turn = data2(s).lastPrompt
          titleCallStarted(s, { id, model, provider, agent: "title", turn, start: begun2(data2(s), turn, start) })
        })
      },
      // A side question is a model call the plugin does not report; its usage is not a
      // title's.
      generate: async (input) => {
        const s = see(input.sessionID)
        if (!s) return
        run(s, () => {
          data2(s).unsure = true
        })
      },
      compaction: async (input) => {
        const s = see(input.sessionID)
        if (!s) return
        const req = { model: input.model?.id, provider: input.model?.providerID, agent: input.agent }
        run(s, () => {
          data2(s).compactionRequest = req
        })
      },
    },
    tool: {
      // The reach guard: the tool waits for the hook, and a failure taints the session.
      "execute.before": async (input) => {
        const s = see(input.sessionID)
        if (!s || typeof input.id !== "string") return
        // A session that failed closed asks again, if it must, for its run to be fenced; the
        // tool does not wait for it.
        if (isTainted(s)) fence(s)
        const q = qualify(input.messageID, input.id)
        if (input.codemode === true) {
          remember(data2(s).code, q, Date.now(), MAX_SMALL)
          return
        }
        const reach = reach2(input.tool, input.input)
        if (!reach) return
        remember(s.guarded, q, { reachKey: JSON.stringify(reach) }, MAX_SMALL)
        await waited2(s, () => checked2(s, reach))
      },
      // Only for code mode: every other call is reported from the events.
      "execute.after": async (input) => {
        if (input.codemode !== true || typeof input.id !== "string") return
        const s = see(input.sessionID)
        if (!s) return
        const at = Date.now()
        run(s, () => codeCall2(s, input, at))
      },
    },
  }
  for (const [domain, fns] of Object.entries(hooks)) {
    for (const [name, fn] of Object.entries(fns)) {
      // A build without one of these hooks must not stop the rest.
      try {
        await ctx[domain].hook(name, guard(fn))
      } catch {}
    }
  }

  ;(async () => {
    try {
      for await (const event of ctx.event.subscribe()) {
        try {
          onEvent(event)
        } catch {
          // One event that cannot be read must not stop the rest.
        }
      }
    } catch {
      // The stream ended badly: there is nothing more to read.
    }
  })()

  // OpenCode 2 calls this on a reload and when the server stops.
  return async () => {
    try {
      await shutdown(owner)
    } catch {}
  }
}

configure(state, CONFIG)

export default { id: "terma", server, setup }
