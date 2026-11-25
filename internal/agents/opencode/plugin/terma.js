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

const CONFIG = null /* terma:config */

import { createHash } from "node:crypto"
import { existsSync, readFileSync } from "node:fs"
import { spawn, execFile } from "node:child_process"
import { dirname, isAbsolute, join, resolve } from "node:path"
import { fileURLToPath } from "node:url"

// --- limits -------------------------------------------------------------------------

// Every rule here that runs on the clock, and why:
// - BATCH_DELAY_MS: a batch leaves this long after its first item, so a quiet session still sends.
// - post's back-off (1 s, doubling, MAX_RETRIES times) and HEADERS_TTL_MS: a relay may restart.
// - HOOK_TIMEOUT_MS, HOOK_KILL_GRACE_MS: a hung hook is ended, so its session's queue moves on.
// - HOOK_QUEUE_WAIT_MS: a tool held for a reach check waits this long at most for its turn.
// - MISSING_RETRY_MS: a terma that could not be run is not tried again on every hook.
// - RECORD_WAIT_MS: a session with no record is given up on (fails closed), not held forever.
// - At exit: the hooks get their run limit, the flush EXIT_FLUSH_MS, OpenCode 1's log LOG_SETTLE_MS.
// - update() stamps what follows a move 1 ms after its claim, so none of it sorts before the claim.

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
// The headers helper is re-run this often, matching Claude Code's cadence.
const HEADERS_TTL_MS = 29 * 60 * 1000
// A session's hooks run one at a time; a hung one is terminated so its queue keeps moving.
const HOOK_TIMEOUT_MS = 2000
const HOOK_KILL_GRACE_MS = 250
const MAX_HOOK_QUEUE = 64
// How long a hook may wait in its session's queue before its caller stops waiting: a tool
// is held for its reach check, so the wait is bounded.
const HOOK_QUEUE_WAIT_MS = 10000
// A terma that could not be run is tried again this long after the last attempt.
const MISSING_RETRY_MS = 60 * 1000
// OpenCode 1 writes its log about once a second and drops what is unwritten when it exits:
// a process that ends sooner than this after terma's warning would lose it.
const LOG_SETTLE_MS = 1500
// How long the exit waits for what is still queued to be sent: a relay that is down must
// not hold OpenCode open.
const EXIT_FLUSH_MS = 4000
// How long a session waits for its record before it is given up on (fail closed).
const RECORD_WAIT_MS = 5000
// What the plugin remembers is bounded, so a long-running server cannot grow it forever.
const MAX_SESSIONS = 512
const MAX_EVENTS = 8192
const MAX_TAINTED = 4096
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
      // Batches waiting to be sent, oldest first: { cfg, spans, records }.
      batches: [],
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

// flush queues everything collected as one batch, with the config in force now, and returns
// a promise that resolves once every batch queued so far is sent or given up on. One drain
// sends them in order, so two sends never race, and a failed batch is retried with the same
// bytes. While the relay does not answer, the oldest waiting batches are dropped.
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
  ex.batches.push({ cfg: state.config, spans, records })
  while (ex.batches.length > MAX_PENDING_BATCHES) {
    const old = ex.batches.shift()
    ex.dropped += old.spans.length + old.records.length
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

// drain sends the waiting batches one at a time, front first, until none is left. A batch
// leaves the queue as it is sent, so the one in flight is never the one dropped.
async function drain(ex) {
  try {
    while (ex.batches.length > 0) {
      const { cfg, spans, records } = ex.batches.shift()
      try {
        const resource = { attributes: attrs({ "service.name": "opencode", ...cfg.resourceAttributes }) }
        if (spans.length > 0) {
          await post(cfg, "/v1/traces", { resourceSpans: [{ resource, scopeSpans: [{ scope: SCOPE, spans }] }] })
        }
        if (records.length > 0) {
          await post(cfg, "/v1/logs", { resourceLogs: [{ resource, scopeLogs: [{ scope: SCOPE, logRecords: records }] }] })
        }
      } catch {}
    }
  } finally {
    ex.draining = false
  }
}

async function post(cfg, path, body) {
  if (!cfg.endpoint) return
  const payload = JSON.stringify(body)
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
// not keep, state.hooks.unowned. Queues do not wait for each other. A timed-out child is
// terminated, and its queue advances only once it is gone. Resolves true iff the hook
// exited 0; the caller stops waiting, with false, once the hook has waited
// HOOK_QUEUE_WAIT_MS for its turn or run past its limit. A hook given up on still runs in
// its turn.
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
    if (!settled) running = giveUp(HOOK_TIMEOUT_MS + HOOK_KILL_GRACE_MS + 500)
    return spawnHook(argv, event, payload)
  })
  lane.hookQueue = slot.then((s) => s.exit, () => undefined).finally(() => {
    if (--lane.hookPending <= 0) h.running.delete(lane)
  })
  slot.then((s) => s.result, () => false).then(settle, () => settle(false))
  return answer
}

function spawnHook(argv, event, payload) {
  if (termaMissing()) return { result: Promise.resolve(false), exit: Promise.resolve() }
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
    // hook (git) ends with it. Where a group cannot be signalled, the hook alone is.
    const sendKill = (sig) => {
      try {
        if (child.pid > 1) return void process.kill(-child.pid, sig)
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
    }, HOOK_TIMEOUT_MS)
    timer.unref?.()
    try {
      // In a process group of its own: OpenCode 2 signals the group a tool step ran in when
      // the step ends, which killed a hook started for that step (a file edit, the idle
      // hook that starts the flush) wherever terma took longer than that to start.
      child = spawn(argv[0], [...argv.slice(1), event], { stdio: ["pipe", "ignore", "ignore"], detached: true })
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
    run(s, () => firstSight(s, fetch))
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
  if (!rec && typeof lookup === "function") {
    let fetched = null
    try {
      fetched = await lookup()
    } catch {}
    rec = s.latest ?? (fetched?.id === s.id ? fetched : null)
  }
  // No record yet: wait for an event that carries one. Without it the session's
  // directory and parent are unknown, so it is never claimed and never exported.
  if (!rec) {
    let timer
    await new Promise((r) => {
      s.recordArrived = r
      timer = setTimeout(r, RECORD_WAIT_MS)
    })
    clearTimeout(timer)
    s.recordArrived = null
    rec = s.latest
  }
  if (!rec) {
    s.dark = true
    return
  }
  s.latest = null
  s.known = true
  s.parentID = str(rec.parentID)
  s.directory = str(rec.directory)
  s.askedDirectory = s.directory
  s.version = str(rec.version)
  s.created = num(rec.created) ?? 0
  s.title = str(rec.title)
  s.policy = repoPolicy(s.directory)
  s.claimed = await runHook(s, "opencode-session-start", base(s))
  // A timeout or a refusal is not fatal: the relay holds an unclaimed session and the
  // next prompt claims it again. A terma that cannot be run is.
  if (!s.claimed && termaMissing()) {
    s.dark = true
    return
  }
  // The title as it stands now (a later update may have arrived during the claim); this
  // is what titles a resumed session.
  if (!s.parentID && isRealTitle(s.title)) {
    s.sentTitle = s.title
    emitTitle(s, s.title)
  }
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
  if (rec.parentID) s.parentID = rec.parentID
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
      if (!blocked(s)) await fn()
    } catch {
      // A telemetry failure must never surface in the agent.
    } finally {
      s.busy--
    }
  })
  return s.queue
}

function blocked(s) {
  if (!s.claimed && termaMissing()) s.dark = true
  return s.dark || isTainted(s)
}

// A failed reach hook taints the session and every session above it: their content could
// now include what the child read across organizations, so none of them exports more.
function taint(s) {
  let id = s.id
  for (let i = 0; i < 16 && id; i++) {
    remember(state.tainted, id, undefined, MAX_TAINTED)
    const parent = id === s.id ? s.parentID : state.sessions.get(id)?.parentID
    if (!parent || parent === id) break
    id = parent
  }
}
function isTainted(s) {
  let id = s.id
  for (let i = 0; i < 16 && id; i++) {
    if (state.tainted.has(id)) return true
    const parent = id === s.id ? s.parentID : state.sessions.get(id)?.parentID
    if (!parent || parent === id) break
    id = parent
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
    endTimeUnixNano: nanos(c.end),
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
    endTimeUnixNano: nanos(t.end),
    attributes: attrs({ ...a, ...common(s) }),
    status: t.status === "completed" ? { code: 1 } : { code: 2 },
  })
  // The touch names the call, so the platform joins it to this span; only a completed
  // edit changed a file. The hook has its own policy, so it runs whatever content says.
  if (EDIT_TOOLS.has(t.name) && t.status === "completed" && files.length > 0) {
    runHook(s, "opencode-file-edit", { ...base(s), tool: t.name, tool_call_id: id, files })
  }
}

// prompt is one prompt: p = { messageID, text, agent?, model?, provider?, time? }. No
// prompt text ever goes to a hook.
function prompt(s, pr) {
  const text = str(pr.text)
  const time = s.movedAt ? Math.max(num(pr.time) ?? 0, s.movedAt) : pr.time
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
// session's previous title, so two titles of a session never share an identity.
function emitTitle(s, text, time) {
  const at = Math.max(num(time) ?? Date.now(), s.lastTitleTime + 1)
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

// shutdown sends the pending title call of every session this entry first saw, waits for
// the sessions' queues and, briefly, for the hooks they started, then flushes, for at most
// EXIT_FLUSH_MS.
async function shutdown(owner) {
  const now = Date.now()
  for (const s of state.sessions.values()) {
    if (s.owner === owner) run(s, () => s.pendingTitle && titleCallEnded(s, { end: now }))
  }
  for (let i = 0; i < 32; i++) {
    const busy = [...state.sessions.values()].filter((s) => s.busy > 0)
    if (busy.length === 0) break
    await Promise.all(busy.map((s) => s.queue))
  }
  const hooks = [...state.hooks.running].map((lane) => lane.hookQueue)
  await Promise.race([Promise.all(hooks), sleep(HOOK_TIMEOUT_MS + HOOK_KILL_GRACE_MS, true)])
  await Promise.race([flush(), sleep(EXIT_FLUSH_MS, true)])
}

// --- reach --------------------------------------------------------------------------

// Each major's name for a tool that reads or runs something, mapped to one vocabulary.
const REACH_TOOLS = { read: "read", grep: "grep", glob: "glob", list: "list", bash: "bash", shell: "bash" }

// toolReach describes what a read, search, list or shell call can reach, or null.
function toolReach(name, args) {
  const kind = REACH_TOOLS[name]
  if (!kind || !args || typeof args !== "object") return null
  const out = {}
  const path = typeof args.filePath === "string" && args.filePath ? args.filePath : typeof args.path === "string" && args.path ? args.path : ""
  switch (kind) {
    case "read":
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
// closed) and says so once in OpenCode's log, unless terma cannot be run, which has its own
// warning. Resolves false when the session may not export the call.
async function guardReach(s, reach) {
  const { tool: name, ...rest } = reach
  const ok = await runHook(s, "opencode-tool-reach", { ...base(s), tool: name, ...rest })
  if (!ok) {
    const fresh = !isTainted(s)
    taint(s)
    if (fresh && !termaMissing()) {
      const log = loggers[loggers.length - 1]
      try {
        log?.(`terma: could not check what a tool reaches in session ${s.id}; nothing more of this session is recorded.`)
      } catch {}
    }
  }
  return ok
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
// prompts; lastPrompt, the last real prompt's id, the turn a self-started message joins.
function entry1(s) {
  const e = s.entry
  if (!e.steps) {
    e.steps = new Set()
    e.prompts = new Set()
    e.lastPrompt = ""
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
  if (st.status !== "completed" && st.status !== "error") return
  const id = qualify(part.messageID, part.callID)
  if (s.doneTools.has(id)) return
  remember(s.doneTools, id)
  // Pruning republishes an old call it compacted; it is not a new one.
  if (st.time?.compacted) return
  if (s.messages.get(part.messageID)?.copied) return
  if (num(st.time?.end) !== undefined && st.time.end < s.created) return
  // The call may have reached something the guard before it did not see; check again.
  const reach = toolReach(part.tool, st.input)
  const guarded = s.guarded.get(part.callID)
  s.guarded.delete(part.callID)
  if (reach && guarded?.reachKey !== JSON.stringify(reach) && !(await guardReach(s, reach))) return
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
    end: num(st.time?.end),
    status,
    denial,
    exitCode: num(st.metadata?.exit),
    input: st.input,
    output: st.status === "completed" ? st.output : st.error,
    title: st.title,
    files: filesOf1(s, part.tool, st),
    mcp: mcpOf1(str(part.tool), servers),
    subagentSession: part.tool === "task" ? st.metadata?.sessionId || undefined : undefined,
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
    const eventID = event?.id
    if (typeof eventID === "string" && eventID) {
      if (state.seenEvents.has(eventID)) return
      remember(state.seenEvents, eventID, undefined, MAX_EVENTS)
    }
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
      const reach = toolReach(inp.tool, out?.args ?? inp.args)
      if (!reach) return
      remember(s.guarded, inp.callID, { reachKey: JSON.stringify(reach) }, MAX_SMALL)
      await run(s, () => guardReach(s, reach))
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

async function setup(ctx) {
  if (!isOpenCode2(ctx)) return
  // OpenCode 2 support is not in this build.
}

configure(state, CONFIG)

export default { id: "terma", server, setup }
