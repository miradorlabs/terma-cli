// Replays one recorded Pi process through terma's Pi extension under Node, with a stand-in
// for Pi's extension API built from the recording, and writes what the extension sent.
//
// Usage: node driver.mjs '<options as JSON>'
//   ext          the rendered extension (its directory gets the fake host modules)
//   recording    one <case>.<n>.jsonl file: one Pi process
//   out          a directory for the result and stand-in files
//   host         the fake @earendil-works/pi-coding-agent's exports ({VERSION, CONFIG_DIR_NAME});
//                VERSION null takes the recording's; the whole option null leaves the module out
//   modules      other resolvable bare modules (e.g. "@oh-my-pi/pi-utils")
//   failPosts    how many posts answer 503 before the relay answers 200
//
// The hook command is the extension's own CONFIG; the Go test points it at a script that
// logs each call. Date.now() returns the current event's recorded time.
//
// Recording lines (see trim.py in the space's tools): _process, _session (first sight of a
// session, or its file opened again), _entries (what a session gained before the next
// event), and Pi's events with the parts of ctx an extension reads.

import { mkdirSync, readFileSync, writeFileSync } from "node:fs"
import { dirname, join } from "node:path"
import { pathToFileURL } from "node:url"

const opts = JSON.parse(process.argv[2])
const lines = readFileSync(opts.recording, "utf8").split("\n").filter(Boolean).map((l) => JSON.parse(l))
mkdirSync(opts.out, { recursive: true })

// --- the host's modules ---

const proc = lines.find((l) => l.e === "_process") ?? {}
const extDir = dirname(opts.ext)
function fakeModule(name, source) {
  const dir = join(extDir, "node_modules", ...name.split("/"))
  mkdirSync(dir, { recursive: true })
  writeFileSync(join(dir, "package.json"), JSON.stringify({ name, type: "module", main: "index.js" }))
  writeFileSync(join(dir, "index.js"), source)
}
if (opts.host) {
  const exports = { ...opts.host }
  if (exports.VERSION === null || exports.VERSION === undefined) exports.VERSION = proc.version
  let src = ""
  for (const [k, v] of Object.entries(exports)) if (v !== undefined && v !== null) src += `export const ${k} = ${JSON.stringify(v)}\n`
  fakeModule("@earendil-works/pi-coding-agent", src)
}
for (const m of opts.modules ?? []) fakeModule(m, "export const present = true\n")

// --- the clock and the relay ---

let now = lines.find((l) => typeof l.t === "number")?.t ?? 0
Date.now = () => now

const posts = []
let failLeft = opts.failPosts ?? 0
globalThis.fetch = async (url, init) => {
  const u = new URL(String(url))
  posts.push({ path: u.pathname, body: String(init?.body ?? ""), status: failLeft > 0 ? 503 : 200 })
  if (failLeft > 0) {
    failLeft--
    return new Response("", { status: 503 })
  }
  return new Response("", { status: 200 })
}

// --- sessions ---

const sessions = new Map()
let standIns = 0
function session(id) {
  let s = sessions.get(id)
  if (!s) {
    s = { id, entries: [], byId: new Map(), header: null, file: undefined }
    sessions.set(id, s)
  }
  return s
}

function manager(s, leaf) {
  return {
    getSessionId: () => s.id,
    getSessionFile: () => s.file,
    getHeader: () => (s.header ? { ...s.header } : null),
    getEntries: () => s.entries.slice(),
    getEntry: (id) => s.byId.get(id),
    getLeafId: () => leaf ?? null,
    getLeafEntry: () => (leaf ? s.byId.get(leaf) : undefined),
    getBranch: (from) => {
      const out = []
      for (let cur = from ?? leaf; cur; ) {
        const e = s.byId.get(cur)
        if (!e) break
        out.unshift(e)
        cur = e.parentId
      }
      return out
    },
    getSessionName: () => {
      for (let i = s.entries.length - 1; i >= 0; i--) if (s.entries[i].type === "session_info") return s.entries[i].name || undefined
      return undefined
    },
  }
}

function context(c) {
  const s = session(c.sessionId)
  const model = c.modelProvider || c.modelId ? { provider: c.modelProvider, id: c.modelId } : undefined
  return {
    cwd: c.cwd,
    mode: c.mode,
    hasUI: c.mode === "tui" || c.mode === "rpc",
    model,
    sessionManager: manager(s, c.leafId),
    modelRegistry: { isUsingOAuth: (m) => (m === model ? c.oauth : false) },
    isIdle: () => true,
    signal: undefined,
  }
}

// --- the extension ---

const extURL = pathToFileURL(opts.ext).href
let mod = null
let imports = 0
let factoryRuns = 0
let registered = 0
const tables = new Map()
const errors = []

async function load(fresh) {
  if (!mod || fresh) mod = await import(extURL + (imports++ > 0 ? "?reload=" + imports : ""))
}

async function runFactory(sessionId) {
  const table = new Map()
  const api = {
    on: (name, fn) => {
      registered++
      if (!table.has(name)) table.set(name, [])
      table.get(name).push(fn)
    },
  }
  factoryRuns++
  await mod.default(api)
  tables.set(sessionId, table)
  return table
}

for (const l of lines) {
  if (l.e === "_process") continue
  if (l.e === "_session") {
    const s = session(l.sessionId)
    // The session's file opened again (a reload, a resume): its entries are listed again.
    s.entries = []
    s.byId = new Map()
    s.file = l.sessionFile || undefined
    s.header = l.header ? { ...l.header } : null
    if (s.header && s.header.parentSession && l.parentHeaderId) {
      const p = join(opts.out, "parent-" + ++standIns + ".jsonl")
      writeFileSync(p, JSON.stringify({ type: "session", version: 3, id: l.parentHeaderId, cwd: l.parentHeaderCwd }) + "\n")
      s.header.parentSession = p
    }
    continue
  }
  if (l.e === "_entries") {
    const s = session(l.sessionId)
    for (const e of l.entries) {
      if (s.byId.has(e.id)) continue
      s.entries.push(e)
      s.byId.set(e.id, e)
    }
    continue
  }
  if (typeof l.t === "number") now = l.t
  const ctx = context(l.ctx)
  const id = l.ctx.sessionId
  let table = tables.get(id)
  if (l.e === "session_start") {
    const reload = l.event?.reason === "reload"
    await load(reload)
    table = await runFactory(id)
  } else if (!table) {
    // A session nested inside the process has a runner of its own.
    await load(false)
    table = await runFactory(id)
  }
  for (const fn of table.get(l.e) ?? []) {
    try {
      const r = await fn(l.event, ctx)
      if (r !== undefined) errors.push(`${l.e} returned ${JSON.stringify(r)}`)
    } catch (err) {
      errors.push(`${l.e} threw ${err?.stack ?? err}`)
    }
  }
}

// --- wait for the queues ---

const state = globalThis[Symbol.for("terma.pi.state/1")]
function idle() {
  if (!state) return true
  const ex = state.exporter
  const fencing = [...state.sessions.values()].some((s) => s.fencing)
  return ex.spans.length === 0 && ex.records.length === 0 && ex.batches.length === 0 && !ex.draining && state.hooks.pending === 0 && !fencing
}
// Real time: the fake clock stands still after the last event.
const start = performance.now()
let quiet = 0
while (performance.now() - start < 30000) {
  if (idle()) {
    if (++quiet >= 3) break
  } else quiet = 0
  await new Promise((r) => setTimeout(r, 50))
}

writeFileSync(
  join(opts.out, "result.json"),
  JSON.stringify({
    pid: process.pid,
    registered,
    factoryRuns,
    imports,
    idle: idle(),
    errors,
    envParent: process.env.TERMA_PI_PARENT ?? null,
    posts,
  }),
)
process.exit(0)
