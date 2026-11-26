// Runs the plugin the way OpenCode would (CONFIG spliced in, hooks and events delivered in
// the order OpenCode delivered them) against a local OTLP receiver and a fake `terma`, and
// checks the wire that arrives and the hooks that ran.
//
// The replay tests feed testdata/1.19.6/<case>/plugin-view.jsonl, what a plugin saw in real
// OpenCode 1.19.6 sessions (testdata/PROVENANCE.md), through the OpenCode 1 entry.
//
// `bun test` in this folder.
import { test, expect, beforeAll, afterAll, describe, setSystemTime } from "bun:test"
import { mkdtempSync, readFileSync, writeFileSync, mkdirSync, chmodSync, existsSync, rmSync, renameSync, statSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { execFileSync } from "node:child_process"

const KEY = Symbol.for("terma.opencode.state/1")
const SOURCE = readFileSync(join(import.meta.dir, "terma.js"), "utf8")
const PLACEHOLDER = /^const CONFIG = null \/\* terma:config \*\/$/m
const FIXTURES = join(import.meta.dir, "testdata", "1.19.6")
const CASES = [
  "01-plain",
  "02-tools",
  "03-failed-tool",
  "04a-denied-rule",
  "04b-denied-ask",
  "05-mcp",
  "06-write-edit",
  "07-patch",
  "09-subagent",
  "10-resume-fork",
  "11-api-error",
  "12-serve",
  "20-custom-provider-marked",
]
const REPLAY_TIMEOUT = 60_000

let server
let root
let seq = 0
const inbox = new Map() // rig tag -> [{ path, headers, body }]

beforeAll(() => {
  root = mkdtempSync(join(tmpdir(), "terma-opencode-"))
  server = Bun.serve({
    port: 0,
    async fetch(req) {
      const [, tag, ...rest] = new URL(req.url).pathname.split("/")
      if (!inbox.has(tag)) inbox.set(tag, [])
      inbox.get(tag).push({ path: "/" + rest.join("/"), headers: Object.fromEntries(req.headers), body: await req.json() })
      return new Response("{}")
    },
  })
})
afterAll(() => {
  server?.stop(true)
  rmSync(root, { recursive: true, force: true })
})

// --- rig ----------------------------------------------------------------------------

// A fake `terma hook`: a session's hooks run one at a time, so one log keeps their order
// within each session; each hook appends its line in one write. Each line is the event, a
// tab, and the JSON it got on stdin. A hook naming a session in slow hangs after logging.
// An event in delayed takes that many seconds, then touches <event>.done: when it finished
// is that file's modification time, read with finishedAt (a shell's own clock is not
// trusted for milliseconds). A fence (the reach that asks terma to mark a run Mixed) fails
// as any reach does, until <dir>/fence.ok exists.
function fakeTerma(dir, { fail = [], slow = [], delayed = {} } = {}) {
  const log = join(dir, "hooks.log")
  const path = join(dir, "terma")
  const failing = fail.map((e) => `[ "$2" = "${e}" ] && exit 1`).join("\n")
  const hanging = slow.map((id) => `case "$input" in *'"${id}"'*) exec sleep 30;; esac`).join("\n")
  const waiting = Object.entries(delayed)
    .map(([e, secs]) => `[ "$2" = "${e}" ] && sleep ${secs} && : > '${join(dir, e + ".done")}'`)
    .join("\n")
  const fencing = `case "$input" in *TERMA_REACH_FAILED*) [ -e '${join(dir, "fence.ok")}' ] && exit 0;; esac`
  writeFileSync(path, `#!/bin/sh\ninput=$(cat)\nprintf '%s\\t%s\\n' "$2" "$input" >> '${log}'\n${hanging}\n${waiting}\n${fencing}\n${failing}\nexit 0\n`)
  chmodSync(path, 0o700)
  return {
    argv: [path, "hook"],
    calls() {
      if (!existsSync(log)) return []
      return readFileSync(log, "utf8")
        .split("\n")
        .filter(Boolean)
        .map((l) => {
          const i = l.indexOf("\t")
          return { event: l.slice(0, i), payload: JSON.parse(l.slice(i + 1)) }
        })
    },
  }
}

// finishedAt is when a delayed hook finished, in milliseconds, or undefined if it has not.
function finishedAt(dir, event) {
  const path = join(dir, event + ".done")
  return existsSync(path) ? statSync(path).mtimeMs : undefined
}

function rig(overrides = {}, termaOptions) {
  const tag = "r" + ++seq
  const dir = mkdtempSync(join(root, tag + "-"))
  const terma = fakeTerma(dir, termaOptions)
  const config = {
    version: 1,
    endpoint: `http://127.0.0.1:${server.port}/${tag}/`,
    headers: { Authorization: "Bearer test" },
    signals: ["traces", "logs", "metrics"],
    includePrompts: true,
    includeToolContent: true,
    resourceAttributes: { "enduser.id": "dev@example.com" },
    hookCommand: terma.argv,
    ...overrides,
  }
  const requests = () => inbox.get(tag) ?? []
  return {
    tag,
    dir,
    config,
    terma,
    requests,
    spans: () =>
      requests()
        .filter((r) => r.path === "/v1/traces")
        .flatMap((r) => r.body.resourceSpans.flatMap((rs) => rs.scopeSpans.flatMap((ss) => ss.spans.map((s) => ({ ...s, scope: ss.scope }))))),
    logs: () =>
      requests()
        .filter((r) => r.path === "/v1/logs")
        .flatMap((r) => r.body.resourceLogs.flatMap((rl) => rl.scopeLogs.flatMap((sl) => sl.logRecords.map((x) => ({ ...x, scope: sl.scope }))))),
  }
}

// load evaluates a fresh copy of the plugin. Unless keep is set, the shared state of an
// earlier copy is dropped first, as in a new OpenCode process.
async function load(config, { keep = false, source = SOURCE } = {}) {
  if (!keep) delete globalThis[KEY]
  const src = config === null ? source : source.replace(PLACEHOLDER, `const CONFIG = ${JSON.stringify(config)}`)
  if (config !== null) expect(src).not.toBe(source)
  // Bun caches directory listings during module resolution: a fresh directory per copy.
  const path = join(mkdtempSync(join(root, "module-")), "terma.js")
  writeFileSync(path, src)
  return import(path)
}

function fakeClient(lookup, warnings = []) {
  return {
    app: { log: async ({ body }) => void warnings.push(body) },
    session: lookup ? { get: async ({ path }) => ({ data: lookup(path.id) }) } : {},
  }
}

function plain(v) {
  if ("stringValue" in v) return v.stringValue
  if ("intValue" in v) return Number(v.intValue)
  if ("doubleValue" in v) return v.doubleValue
  if ("boolValue" in v) return v.boolValue
  if ("arrayValue" in v) return v.arrayValue.values.map(plain)
  return undefined
}
const A = (item) => Object.fromEntries(item.attributes.map((x) => [x.key, plain(x.value)]))
const kindOf = (span) => A(span)["opencode.call.kind"]
const sessionOf = (item) => A(item)["session.id"]
const named = (logs, name) => logs.filter((r) => A(r)["event.name"] === name)
const chats = (spans) => spans.filter((s) => s.name.startsWith("chat "))
const tools = (spans) => spans.filter((s) => s.name.startsWith("execute_tool "))

// --- replay -------------------------------------------------------------------------

function readCase(name) {
  return readFileSync(join(FIXTURES, name, "plugin-view.jsonl"), "utf8")
    .split("\n")
    .filter(Boolean)
    .map((l) => JSON.parse(l))
}

function eventSession(rec) {
  if (rec.kind !== "event") return rec.data?.input?.sessionID
  const p = rec.data.properties ?? {}
  return p.sessionID ?? p.part?.sessionID ?? p.info?.sessionID ?? (rec.name.startsWith("session.") ? p.info?.id : undefined)
}

const isSessionRecord = (r) => r.kind === "event" && (r.name === "session.created" || r.name === "session.updated")

// replayProcess loads the plugin once for one OpenCode process of a case and calls server
// for each directory that process served. session.get answers from the case's session
// records: the latest before the record being replayed, as OpenCode would have.
async function replayProcess(r, records, items, { mcp }) {
  const mod = await load(r.config)
  const warnings = []
  let cursor = 0
  const info = (id) => {
    for (let i = cursor; i >= 0; i--) if (isSessionRecord(records[i]) && records[i].data.properties.info.id === id) return structuredClone(records[i].data.properties.info)
    for (let i = cursor; i < records.length; i++) if (isSessionRecord(records[i]) && records[i].data.properties.info.id === id) return structuredClone(records[i].data.properties.info)
    return null
  }
  const client = fakeClient(info, warnings)
  const instances = []
  for (const { rec } of items.filter((x) => x.rec.kind === "probe")) {
    instances.push({ directory: rec.data.directory, hooks: await mod.default.server({ client, directory: rec.data.directory, worktree: rec.data.worktree }) })
  }
  const route = (sessionID) => {
    const dir = sessionID ? info(sessionID)?.directory : undefined
    return (instances.find((i) => i.directory === dir) ?? instances[0]).hooks
  }
  let configs = 0
  const before = r.terma.calls().length
  // The clock reads each record's own time while it is delivered: what the plugin stamps
  // itself (a reach check made in a hook, a title) falls among the times OpenCode recorded,
  // not hours after them.
  try {
    for (const { index, rec } of items) {
      cursor = index
      setSystemTime(new Date(rec.at))
      if (rec.kind === "event") {
        await route(eventSession(rec)).event({ event: structuredClone(rec.data) })
      } else if (rec.kind === "hook" && rec.name === "config") {
        const cfg = mcp ? { mcp: Object.fromEntries(mcp.map((n) => [n, { type: "local", command: ["mcp"] }])) } : {}
        await instances[Math.min(configs++, instances.length - 1)].hooks.config(cfg)
      } else if (rec.kind === "hook" && rec.name !== "dispose") {
        await route(eventSession(rec))[rec.name]?.(structuredClone(rec.data.input), structuredClone(rec.data.output))
      }
    }
    for (const inst of instances) await inst.hooks.dispose()
  } finally {
    setSystemTime()
  }
  return { pid: items[0].rec.pid, warnings, hooks: r.terma.calls().slice(before) }
}

async function replay(name, { mcp } = {}) {
  const records = readCase(name)
  const r = rig()
  const byPid = new Map()
  records.forEach((rec, index) => {
    if (!byPid.has(rec.pid)) byPid.set(rec.pid, [])
    byPid.get(rec.pid).push({ index, rec })
  })
  const processes = []
  for (const items of byPid.values()) {
    const spansBefore = r.spans().length
    const logsBefore = r.logs().length
    const p = await replayProcess(r, records, items, { mcp })
    processes.push({ ...p, spans: r.spans().slice(spansBefore), logs: r.logs().slice(logsBefore) })
  }
  return { records, processes, spans: r.spans(), logs: r.logs(), hooks: r.terma.calls(), warnings: processes.flatMap((p) => p.warnings) }
}

const MCP = { "05-mcp": ["capture"] }
const replays = new Map()
function replayed(name) {
  if (!replays.has(name)) replays.set(name, replay(name, { mcp: MCP[name] }))
  return replays.get(name)
}

// Fixture facts, read from the records rather than written out by hand.
function sessionInfos(records) {
  const out = new Map()
  for (const r of records) if (isSessionRecord(r)) out.set(r.data.properties.info.id, r.data.properties.info)
  return out
}
function completedAssistants(records) {
  const out = new Map()
  for (const r of records) {
    const i = r.kind === "event" && r.name === "message.updated" ? r.data.properties.info : null
    if (i?.role === "assistant" && (i.time?.completed || i.error)) out.set(i.id, i)
  }
  return [...out.values()]
}
function endedToolParts(records) {
  const out = new Map()
  for (const r of records) {
    const p = r.kind === "event" && r.name === "message.part.updated" ? r.data.properties.part : null
    if (p?.type === "tool" && (p.state.status === "completed" || p.state.status === "error")) out.set(p.messageID + ":" + p.callID, p)
  }
  return [...out.values()]
}
const prompts = (records) => records.filter((r) => r.kind === "hook" && r.name === "chat.message")

// The wire minus the clock: what two replays of the same process must agree on.
function identity(result) {
  const spans = result.spans.map((s) => JSON.stringify([s.traceId, s.spanId, s.name, s.kind, s.attributes, s.status])).sort()
  const logs = result.logs.map((l) => JSON.stringify([l.traceId, l.body, l.attributes])).sort()
  // Hooks of different sessions run side by side: only each session's own order is fixed.
  const hooks = [...result.hooks].sort((a, b) => (a.payload.session_id < b.payload.session_id ? -1 : a.payload.session_id > b.payload.session_id ? 1 : 0))
  return { spans, logs, hooks }
}

// --- every case ---------------------------------------------------------------------

const RETIRED_KEYS = ["gen_ai.system", "opencode.tool.file_path", "opencode.project.id", "opencode.session.directory"]
const RETIRED_EVENTS = ["opencode.session.created", "opencode.session.idle", "opencode.session.error", "opencode.session.deleted"]

describe("every 1.19.6 case", () => {
  for (const name of CASES) {
    test(
      name,
      async () => {
        const res = await replayed(name)
        expect(res.spans.length).toBeGreaterThan(0)
        expect(res.logs.length).toBeGreaterThan(0)
        for (const item of [...res.spans, ...res.logs]) {
          expect(item.scope).toEqual({ name: "terma-opencode", version: "2" })
          const a = A(item)
          expect(a["session.id"]).toMatch(/^ses_/)
          expect(a["opencode.version"]).toBe("1.19.6")
          for (const key of RETIRED_KEYS) expect(a[key]).toBeUndefined()
          expect(RETIRED_EVENTS).not.toContain(a["event.name"])
        }
        const ids = res.spans.map((s) => s.spanId)
        expect(new Set(ids).size).toBe(ids.length)

        // Each process's first hook for a session is its claim, from the session's own directory.
        const infos = sessionInfos(res.records)
        for (const p of res.processes) {
          const first = new Map()
          for (const c of p.hooks) if (!first.has(c.payload.session_id)) first.set(c.payload.session_id, c)
          for (const [sid, c] of first) {
            expect(c.event).toBe("opencode-session-start")
            expect(c.payload.cwd).toBe(infos.get(sid).directory)
          }
        }
        expect(res.hooks.filter((c) => c.event === "opencode-prompt").length).toBe(prompts(res.records).length)
        expect(named(res.logs, "opencode.user_prompt").length).toBe(prompts(res.records).length)
        const idles = new Set(res.records.filter((r) => r.kind === "event" && r.name === "session.idle").map((r) => r.data.id))
        expect(res.hooks.filter((c) => c.event === "opencode-idle").length).toBe(idles.size)
        expect(res.warnings).toEqual([])

        const again = await replay(name, { mcp: MCP[name] })
        expect(identity(again)).toEqual(identity(res))
      },
      REPLAY_TIMEOUT,
    )
  }
})

// --- per case -----------------------------------------------------------------------

test("01-plain: the call, the prompt, the title and the title call", async () => {
  const res = await replayed("01-plain")
  const sessions = [...new Set(res.spans.map(sessionOf))]
  expect(sessions.length).toBe(2)
  for (const sid of sessions) {
    const mine = (xs) => xs.filter((x) => sessionOf(x) === sid)
    const [promptRec] = mine(named(res.logs, "opencode.user_prompt"))
    const promptID = A(promptRec)["opencode.message.id"]
    const calls = mine(chats(res.spans)).filter((s) => !kindOf(s))
    expect(calls.length).toBe(1)
    const assistant = completedAssistants(res.records).find((m) => m.sessionID === sid)
    expect(A(calls[0])["opencode.call.id"]).toBe(assistant.id)
    expect(A(calls[0])["opencode.parent_message.id"]).toBe(promptID)

    const titles = mine(named(res.logs, "opencode.session.title")).map((r) => r.body.stringValue)
    const generated = [...sessionInfos(res.records).values()].find((i) => i.id === sid).title
    expect(titles).toEqual([generated])
    expect(titles.some((t) => t.startsWith("New session - "))).toBe(false)

    const titleCalls = mine(chats(res.spans)).filter((s) => kindOf(s) === "title")
    expect(titleCalls.length).toBe(1)
    const t = A(titleCalls[0])
    expect(t["opencode.call.id"]).toBe("title:" + promptID)
    expect(t["gen_ai.request.model"]).toBe("anthropic/claude-haiku-5.5")
    expect(t["opencode.agent"]).toBe("title")
    for (const key of Object.keys(t)) expect(key).not.toMatch(/tokens|cost|completion/)
    expect(t["opencode.parent_message.id"]).toBeUndefined()
  }
}, REPLAY_TIMEOUT)

test("02-tools: tokens as sent, tool calls from their parts", async () => {
  const res = await replayed("02-tools")
  const calls = chats(res.spans).filter((s) => !kindOf(s)).sort((a, b) => Number(BigInt(a.startTimeUnixNano) - BigInt(b.startTimeUnixNano)))
  const first = A(calls[0])
  expect(first["gen_ai.usage.input_tokens"]).toBe(4)
  expect(first["gen_ai.usage.cache_write.input_tokens"]).toBe(10581)
  expect(first["gen_ai.usage.cache_read.input_tokens"]).toBe(0)
  expect(first["gen_ai.usage.output_tokens"]).toBe(50)
  expect(first["opencode.usage.reasoning_tokens"]).toBe(65)
  expect(first["gen_ai.usage.total_cost"]).toBe(0.001380525)
  expect(first["gen_ai.response.finish_reasons"]).toBe("tool-calls")

  const promptID = A(named(res.logs, "opencode.user_prompt")[0])["opencode.message.id"]
  const parts = endedToolParts(res.records)
  const spans = tools(res.spans)
  expect(spans.length).toBe(3)
  for (const part of parts) {
    const span = spans.find((s) => A(s)["gen_ai.tool.call.id"] === part.messageID + ":" + part.callID)
    expect(span).toBeDefined()
    expect(part.callID).toMatch(/^toolu_/)
    const a = A(span)
    expect(a["opencode.tool.status"]).toBe("completed")
    expect(a["opencode.parent_message.id"]).toBe(promptID)
    expect(span.startTimeUnixNano).toBe(String(BigInt(part.state.time.start) * 1000000n))
    expect(span.endTimeUnixNano).toBe(String(BigInt(part.state.time.end) * 1000000n))
    if (part.tool === "read") expect(a["opencode.tool.files"]).toEqual([part.state.input.filePath])
    else expect(a["opencode.tool.files"]).toBeUndefined()
  }
  const last = A(calls[calls.length - 1])
  expect(last["gen_ai.completion"]).toBe("3")
  expect(last["opencode.completion.length"]).toBe(1)
}, REPLAY_TIMEOUT)

test("03-failed-tool: exit 3 is a completed call with its exit code", async () => {
  const res = await replayed("03-failed-tool")
  const [bash] = tools(res.spans)
  expect(A(bash)["opencode.tool.exit_code"]).toBe(3)
  expect(A(bash)["opencode.tool.status"]).toBe("completed")
})

for (const [name, denial] of [["04a-denied-rule", "rule"], ["04b-denied-ask", "ask"]]) {
  test(`${name}: a call that never ran is a denied span`, async () => {
    const res = await replayed(name)
    const [part] = endedToolParts(res.records)
    const spans = tools(res.spans)
    expect(spans.length).toBe(1)
    const a = A(spans[0])
    expect(a["opencode.tool.status"]).toBe("denied")
    expect(a["opencode.tool.denial"]).toBe(denial)
    expect(a["gen_ai.tool.call.result"]).toBe(part.state.error)
    expect(spans[0].status).toEqual({ code: 2 })
  }, REPLAY_TIMEOUT)
}

test("05-mcp: MCP names, the real result, and the failed call", async () => {
  const res = await replayed("05-mcp")
  const echo = tools(res.spans).find((s) => A(s)["gen_ai.tool.name"] === "capture_echo")
  expect(A(echo)["opencode.mcp.server"]).toBe("capture")
  expect(A(echo)["opencode.mcp.tool"]).toBe("echo")
  expect(A(echo)["gen_ai.tool.call.result"]).toBe("MCP-CANARY")
  const fail = tools(res.spans).find((s) => A(s)["gen_ai.tool.name"] === "capture_fail")
  expect(A(fail)["opencode.tool.status"]).toBe("error")
  expect(A(fail)["opencode.mcp.tool"]).toBe("fail")
}, REPLAY_TIMEOUT)

test("06-write-edit: one file-edit hook per completed edit, naming its call", async () => {
  const res = await replayed("06-write-edit")
  const edits = res.hooks.filter((c) => c.event === "opencode-file-edit")
  const editSpans = tools(res.spans).filter((s) => ["write", "edit"].includes(A(s)["gen_ai.tool.name"]))
  expect(edits.length).toBe(2)
  expect(editSpans.length).toBe(2)
  for (const span of editSpans) {
    const a = A(span)
    const hook = edits.find((c) => c.payload.tool_call_id === a["gen_ai.tool.call.id"])
    expect(hook.payload.tool).toBe(a["gen_ai.tool.name"])
    expect(hook.payload.files).toEqual(a["opencode.tool.files"])
    expect(hook.payload.cwd).toBe("/home/user/work/oc-files")
  }
}, REPLAY_TIMEOUT)

test("07-patch: apply_patch names its files from the metadata", async () => {
  const res = await replayed("07-patch")
  const patch = tools(res.spans).find((s) => A(s)["gen_ai.tool.name"] === "apply_patch")
  expect(A(patch)["opencode.tool.files"]).toEqual(["/home/user/work/oc-patch/notes.txt"])
  const edits = res.hooks.filter((c) => c.event === "opencode-file-edit")
  expect(edits.length).toBe(1)
  expect(edits[0].payload).toMatchObject({ tool: "apply_patch", tool_call_id: A(patch)["gen_ai.tool.call.id"], files: ["/home/user/work/oc-patch/notes.txt"] })
  // Checked in tool.execute.after, once: the span ends when OpenCode said, but for the
  // millisecond the check takes on the replay's clock, and nothing after it moves.
  const checks = res.hooks.filter((c) => c.event === "opencode-tool-reach" && c.payload.tool === "apply_patch")
  expect(checks.map((c) => c.payload.paths)).toEqual([["/home/user/work/oc-patch/notes.txt"]])
  const part = endedToolParts(res.records).find((p) => p.tool === "apply_patch")
  expect(endOf(patch) - part.state.time.end).toBeWithin(0, 2)
  for (const m of completedAssistants(res.records)) {
    const span = chats(res.spans).find((s) => A(s)["opencode.call.id"] === m.id)
    if (span && m.time.completed) expect(endOf(span)).toBe(m.time.completed)
  }
}, REPLAY_TIMEOUT)

test("09-subagent: the child names its parent everywhere", async () => {
  const res = await replayed("09-subagent")
  const infos = [...sessionInfos(res.records).values()]
  const child = infos.find((i) => i.parentID)
  const parentID = child.parentID
  const childItems = [...res.spans, ...res.logs].filter((x) => sessionOf(x) === child.id)
  expect(childItems.length).toBeGreaterThan(2)
  for (const x of childItems) expect(A(x)["opencode.parent_session.id"]).toBe(parentID)
  for (const x of [...res.spans, ...res.logs].filter((x) => sessionOf(x) === parentID)) expect(A(x)["opencode.parent_session.id"]).toBeUndefined()
  const start = res.hooks.find((c) => c.event === "opencode-session-start" && c.payload.session_id === child.id)
  expect(start.payload.parent_session_id).toBe(parentID)
  const task = tools(res.spans).find((s) => A(s)["gen_ai.tool.name"] === "task")
  expect(A(task)["opencode.tool.subagent_session.id"]).toBe(child.id)
  expect(named(res.logs, "opencode.session.title").filter((r) => sessionOf(r) === child.id)).toEqual([])
  const ids = tools(res.spans).map((s) => A(s)["gen_ai.tool.call.id"])
  expect(new Set(ids).size).toBe(ids.length)
  for (const c of res.hooks.filter((c) => c.payload.session_id === child.id)) expect(c.payload.parent_session_id).toBe(parentID)
}, REPLAY_TIMEOUT)

test("10-resume-fork: each process claims and exports its own call; a fork only its own", async () => {
  const res = await replayed("10-resume-fork")
  expect(res.processes.length).toBe(4)
  const fork = [...sessionInfos(res.records).values()].find((i) => i.title.includes("(fork"))
  for (const [i, p] of res.processes.entries()) {
    const sid = i < 3 ? p.hooks[0].payload.session_id : fork.id
    expect(p.hooks[0]).toMatchObject({ event: "opencode-session-start", payload: { session_id: sid } })
    const own = completedAssistants(res.records.filter((r) => r.pid === p.pid)).filter((m) => m.time.created >= sessionInfos(res.records).get(m.sessionID).time.created)
    expect(own.length).toBe(1)
    const calls = chats(p.spans).filter((s) => !kindOf(s))
    expect(calls.map((s) => A(s)["opencode.call.id"])).toEqual([own[0].id])
  }
  // The resumed processes title the session once each, with its current title.
  for (const p of res.processes.slice(1, 3)) {
    expect(named(p.logs, "opencode.session.title").map((r) => r.body.stringValue)).toEqual(["Memory word KIWI instruction"])
  }
  expect(named(res.processes[3].logs, "opencode.session.title").map((r) => r.body.stringValue)).toEqual([fork.title])
  // The fork copied three calls and exports none of them.
  expect(chats(res.processes[3].spans).filter((s) => !kindOf(s)).length).toBe(1)
}, REPLAY_TIMEOUT)

test("11-api-error: one failed call, no usage, the error under both switches", async () => {
  const res = await replayed("11-api-error")
  const calls = chats(res.spans).filter((s) => !kindOf(s))
  expect(calls.length).toBe(1)
  expect(calls[0].status).toEqual({ code: 2 })
  const a = A(calls[0])
  expect(a["error.type"]).toBe("APIError")
  expect(a["opencode.error.message"]).toContain("is not a valid model ID")
  for (const key of Object.keys(a)) expect(key).not.toMatch(/tokens|cost|completion|finish/)
}, REPLAY_TIMEOUT)

test("12-serve: two directories on one process, renames, compaction", async () => {
  const res = await replayed("12-serve")
  const infos = sessionInfos(res.records)
  for (const c of res.hooks) expect(c.payload.cwd).toBe(infos.get(c.payload.session_id).directory)
  expect(new Set(res.hooks.map((c) => c.payload.cwd)).size).toBe(2)

  const titles = named(res.logs, "opencode.session.title")
  for (const sid of infos.keys()) {
    const times = titles.filter((r) => sessionOf(r) === sid).map((r) => BigInt(r.timeUnixNano))
    for (let i = 1; i < times.length; i++) expect(times[i] > times[i - 1]).toBe(true)
  }
  const renamed = [...infos.values()].find((i) => i.title.includes("RENAME-CANARY"))
  expect(titles.filter((r) => sessionOf(r) === renamed.id).map((r) => r.body.stringValue)).toEqual(["Reading notes.txt first line", "Renamed by hand RENAME-CANARY"])
  expect(titles.length).toBe(4)

  const compactions = chats(res.spans).filter((s) => kindOf(s) === "compaction")
  expect(compactions.length).toBe(1)
  const c = A(compactions[0])
  expect(c["gen_ai.usage.output_tokens"]).toBe(268)
  expect(c["gen_ai.usage.cache_write.input_tokens"]).toBe(831)
  expect(c["gen_ai.usage.total_cost"]).toBe(0.000238275)
  expect(c["opencode.parent_message.id"]).toBeUndefined()
  expect(c["gen_ai.completion"]).toStartWith("## Objective")
}, REPLAY_TIMEOUT)

test("20-custom-provider-marked: cost 0 and tokens as OpenCode reported them", async () => {
  const res = await replayed("20-custom-provider-marked")
  const calls = chats(res.spans).filter((s) => !kindOf(s))
  const byID = new Map(calls.map((s) => [A(s)["opencode.call.id"], A(s)]))
  for (const m of completedAssistants(res.records)) {
    const a = byID.get(m.id)
    expect(a["gen_ai.usage.total_cost"]).toBe(0)
    expect(a["gen_ai.usage.input_tokens"]).toBe(m.tokens.input)
    expect(a["gen_ai.usage.output_tokens"]).toBe(m.tokens.output)
    expect(a["gen_ai.usage.cache_read.input_tokens"]).toBe(m.tokens.cache.read)
  }
}, REPLAY_TIMEOUT)

// --- unit ---------------------------------------------------------------------------

const ev = (type, properties, id) => ({ event: { id, type, properties } })
const sessionCreated = (id, extra = {}) =>
  ev("session.created", { info: { id, directory: "/repo", version: "1.19.6", title: "New session - 2026-11-25T00:00:00.000Z", time: { created: 1000, updated: 1000 }, ...extra } })
const assistantDone = (sessionID, id, extra = {}) =>
  ev("message.updated", {
    info: {
      id,
      sessionID,
      role: "assistant",
      parentID: "msg_user",
      modelID: "m1",
      providerID: "p1",
      mode: "build",
      cost: 0.01,
      tokens: { input: 1, output: 2, reasoning: 3, cache: { read: 4, write: 5 } },
      finish: "stop",
      time: { created: 2000, completed: 3000 },
      ...extra,
    },
  })
const textPart = (sessionID, messageID, id, text) => ev("message.part.updated", { part: { id, sessionID, messageID, type: "text", text } })
const toolPart = (sessionID, messageID, callID, tool, state) =>
  ev("message.part.updated", { part: { id: "prt_" + messageID + "_" + callID, sessionID, messageID, callID, type: "tool", tool, state } })
const RULE_ERROR = "The user has specified a rule which prevents you from using this specific tool call."
// The shell reach a session that failed closed sends, so terma marks its run Mixed.
const FENCE = "eval $TERMA_REACH_FAILED"
const done = (input, output, extra = {}) => ({ status: "completed", input, output, title: "t", metadata: {}, time: { start: 2100, end: 2200 }, ...extra })

async function start(overrides, { terma, client, directory = "/repo", source } = {}) {
  const r = rig(overrides, terma)
  if (source) expect(source).not.toBe(SOURCE)
  const mod = await load(r.config, { source })
  const warnings = []
  const hooks = await mod.default.server({ client: client ?? fakeClient(null, warnings), directory, worktree: directory })
  return { r, mod, hooks, warnings }
}

describe("content switches", () => {
  async function scenario(overrides) {
    const { r, hooks } = await start(overrides)
    await hooks.config({ mcp: { capture: {} } })
    await hooks.event(sessionCreated("ses_c"))
    await hooks.event(ev("session.updated", { info: { id: "ses_c", directory: "/repo", version: "1.19.6", title: "A real title", time: { created: 1000 } } }))
    await hooks["chat.message"]({ sessionID: "ses_c" }, { message: { id: "msg_user", agent: "build", model: { modelID: "m1", providerID: "p1" } }, parts: [{ type: "text", text: "delete everything" }] })
    await hooks.event(assistantDone("ses_c", "msg_a0", { time: { created: 1500, completed: 1600 } }))
    await hooks.event(toolPart("ses_c", "msg_a1", "c1", "bash", done({ command: "rm -rf /" }, "secret output", { metadata: { exit: 1 } })))
    await hooks.event(toolPart("ses_c", "msg_a1", "c2", "capture_echo", done({ text: "x" }, "mcp secret")))
    await hooks.event(toolPart("ses_c", "msg_a1", "c3", "read", done({ filePath: "/repo/a.txt" }, "file secret")))
    await hooks.event(textPart("ses_c", "msg_a1", "p1", "the reply"))
    await hooks.event(assistantDone("ses_c", "msg_a1"))
    await hooks.event(assistantDone("ses_c", "msg_a2", { error: { name: "APIError", data: { message: "provider said: delete everything" } }, time: { created: 3100, completed: 3200 } }))
    await hooks.dispose()
    return r
  }

  test("prompts off: no prompt, title or reply text, but their lengths", async () => {
    const r = await scenario({ includePrompts: false })
    const everything = JSON.stringify(r.requests())
    expect(everything).not.toContain("delete everything")
    expect(everything).not.toContain("the reply")
    expect(everything).not.toContain("A real title")
    const [prompt] = named(r.logs(), "opencode.user_prompt")
    expect(prompt.body.stringValue).toBe("")
    expect(A(prompt)["opencode.prompt.length"]).toBe(17)
    expect(named(r.logs(), "opencode.session.title")[0].body.stringValue).toBe("")
    const call = A(chats(r.spans()).find((s) => A(s)["opencode.call.id"] === "msg_a1"))
    expect(call["gen_ai.completion"]).toBeUndefined()
    expect(call["opencode.completion.length"]).toBe(9)
    // Tool content is a separate switch.
    expect(everything).toContain("secret output")
    const failed = A(chats(r.spans()).find((s) => A(s)["opencode.call.id"] === "msg_a2"))
    expect(failed["error.type"]).toBe("APIError")
    expect(failed["opencode.error.message"]).toBeUndefined()
  })

  test("tool content off: no arguments, results, files or titles, but status, exit code and MCP names", async () => {
    const r = await scenario({ includeToolContent: false })
    const everything = JSON.stringify(r.requests())
    for (const secret of ["rm -rf", "secret output", "mcp secret", "file secret", "/repo/a.txt"]) expect(everything).not.toContain(secret)
    const byName = Object.fromEntries(tools(r.spans()).map((s) => [A(s)["gen_ai.tool.name"], A(s)]))
    expect(byName.bash["opencode.tool.status"]).toBe("completed")
    expect(byName.bash["opencode.tool.exit_code"]).toBe(1)
    expect(byName.bash["opencode.tool.output_bytes"]).toBe(13)
    expect(byName.capture_echo["opencode.mcp.server"]).toBe("capture")
    expect(byName.capture_echo["opencode.mcp.tool"]).toBe("echo")
    for (const a of Object.values(byName)) {
      for (const key of ["gen_ai.tool.call.arguments", "gen_ai.tool.call.result", "opencode.tool.files", "opencode.tool.title"]) expect(a[key]).toBeUndefined()
    }
    const failed = A(chats(r.spans()).find((s) => A(s)["opencode.call.id"] === "msg_a2"))
    expect(failed["opencode.error.message"]).toBeUndefined()
    // Prompts are still on.
    expect(everything).toContain("delete everything")
  })

  test("both on: the error message ships", async () => {
    const r = await scenario({})
    const failed = A(chats(r.spans()).find((s) => A(s)["opencode.call.id"] === "msg_a2"))
    expect(failed["opencode.error.message"]).toBe("provider said: delete everything")
    expect(chats(r.spans()).find((s) => A(s)["opencode.call.id"] === "msg_a2").status).toEqual({ code: 2 })
  })
})

test("a reply in two text parts is both, joined by a blank line", async () => {
  const { r, hooks } = await start()
  await hooks.event(sessionCreated("ses_t"))
  await hooks.event(textPart("ses_t", "msg_a", "p1", ""))
  await hooks.event(textPart("ses_t", "msg_a", "p1", "first"))
  await hooks.event(textPart("ses_t", "msg_a", "p2", "second"))
  await hooks.event(assistantDone("ses_t", "msg_a"))
  // A second update of a completed message is not a second call.
  await hooks.event(assistantDone("ses_t", "msg_a"))
  await hooks.dispose()
  const calls = chats(r.spans())
  expect(calls.length).toBe(1)
  expect(A(calls[0])["gen_ai.completion"]).toBe("first\n\nsecond")
  expect(A(calls[0])["opencode.completion.length"]).toBe(13)
})

test("a failed call keeps what it was billed, and sends no usage when it was billed nothing", async () => {
  const { r, hooks } = await start()
  await hooks.event(sessionCreated("ses_t"))
  const error = { name: "APIError", data: { message: "stopped" } }
  await hooks.event(assistantDone("ses_t", "msg_billed", { error, finish: undefined }))
  await hooks.event(
    assistantDone("ses_t", "msg_free", { error, finish: undefined, cost: 0, tokens: { input: 0, output: 0, reasoning: 0, cache: { read: 0, write: 0 } } }),
  )
  await hooks.dispose()
  const byID = Object.fromEntries(chats(r.spans()).map((s) => [A(s)["opencode.call.id"], s]))
  const billed = A(byID.msg_billed)
  expect(byID.msg_billed.status).toEqual({ code: 2 })
  expect(billed["gen_ai.usage.input_tokens"]).toBe(1)
  expect(billed["gen_ai.usage.output_tokens"]).toBe(2)
  expect(billed["opencode.usage.reasoning_tokens"]).toBe(3)
  expect(billed["gen_ai.usage.cache_read.input_tokens"]).toBe(4)
  expect(billed["gen_ai.usage.cache_write.input_tokens"]).toBe(5)
  expect(billed["gen_ai.usage.total_cost"]).toBe(0.01)
  expect(billed["gen_ai.response.finish_reasons"]).toBeUndefined()
  expect(Object.keys(A(byID.msg_free)).filter((k) => k.includes("usage"))).toEqual([])
})

test("a provider that reuses a call id in two messages gives two tool spans", async () => {
  const { r, hooks } = await start()
  await hooks.event(sessionCreated("ses_d"))
  await hooks.event(toolPart("ses_d", "msg_1", "call_0", "bash", done({ command: "true" }, "")))
  await hooks.event(toolPart("ses_d", "msg_2", "call_0", "bash", done({ command: "true" }, "")))
  await hooks.dispose()
  const spans = tools(r.spans())
  expect(spans.length).toBe(2)
  expect(spans[0].spanId).not.toBe(spans[1].spanId)
  expect(spans.map((s) => A(s)["gen_ai.tool.call.id"]).sort()).toEqual(["msg_1:call_0", "msg_2:call_0"])
})

test("a fork exports nothing for what it copied, tool calls included", async () => {
  const { r, hooks } = await start()
  await hooks.event(sessionCreated("ses_f", { time: { created: 5000 } }))
  // A copied message: older than the fork.
  await hooks.event(assistantDone("ses_f", "msg_old", { time: { created: 2000, completed: 3000 } }))
  await hooks.event(toolPart("ses_f", "msg_old", "c1", "write", done({ filePath: "/repo/a" }, "ok")))
  // A part whose message was never seen, but which ended before the fork existed.
  await hooks.event(toolPart("ses_f", "msg_unseen", "c2", "edit", done({ filePath: "/repo/b" }, "ok", { time: { start: 3000, end: 4000 } })))
  // The fork's own call.
  await hooks.event(assistantDone("ses_f", "msg_new", { time: { created: 6000, completed: 7000 } }))
  await hooks.dispose()
  expect(tools(r.spans())).toEqual([])
  expect(chats(r.spans()).map((s) => A(s)["opencode.call.id"])).toEqual(["msg_new"])
  expect(r.terma.calls().filter((c) => c.event === "opencode-file-edit")).toEqual([])
})

test("only a completed edit reports a file edit", async () => {
  const { r, hooks } = await start()
  await hooks.event(sessionCreated("ses_w"))
  await hooks.event(toolPart("ses_w", "msg_1", "c1", "edit", { status: "error", input: { filePath: "/repo/a" }, error: "oldString not found", time: { start: 2100, end: 2200 } }))
  await hooks.event(toolPart("ses_w", "msg_1", "c2", "write", { status: "error", input: { filePath: "/repo/b" }, error: RULE_ERROR, time: { start: 2100, end: 2200 } }))
  await hooks.event(toolPart("ses_w", "msg_1", "c3", "write", done({ filePath: "c.txt" }, "ok")))
  await hooks.dispose()
  const edits = r.terma.calls().filter((c) => c.event === "opencode-file-edit")
  expect(edits.map((c) => c.payload)).toEqual([{ session_id: "ses_w", cwd: "/repo", tool: "write", tool_call_id: "msg_1:c3", files: ["/repo/c.txt"] }])
  const status = Object.fromEntries(tools(r.spans()).map((s) => [A(s)["gen_ai.tool.call.id"], [A(s)["opencode.tool.status"], A(s)["opencode.tool.denial"]]]))
  expect(status).toEqual({ "msg_1:c1": ["error", undefined], "msg_1:c2": ["denied", "rule"], "msg_1:c3": ["completed", undefined] })
})

test("a subagent's title is never sent, at first sight or when it changes", async () => {
  const { r, hooks } = await start()
  await hooks.event(sessionCreated("ses_root", { title: "Root title" }))
  await hooks.event(sessionCreated("ses_kid", { parentID: "ses_root", title: "Kid title (@general subagent)" }))
  await hooks.event(ev("session.updated", { info: { id: "ses_kid", parentID: "ses_root", directory: "/repo", version: "1.19.6", title: "Kid renamed", time: { created: 1000 } } }))
  await hooks.event(ev("session.updated", { info: { id: "ses_root", directory: "/repo", version: "1.19.6", title: "Root renamed", time: { created: 1000 } } }))
  await hooks.dispose()
  const titles = named(r.logs(), "opencode.session.title")
  expect(titles.every((x) => sessionOf(x) === "ses_root")).toBe(true)
  // A rename that lands while the first claim runs replaces the first-sight title.
  expect(titles.at(-1).body.stringValue).toBe("Root renamed")
})

test("a session whose claim failed goes dark once terma cannot be run", async () => {
  const r = rig()
  // The claim fails and takes terma with it: the next hook cannot start.
  writeFileSync(r.terma.argv[0], `#!/bin/sh\nrm -f "$0"\nexit 1\n`)
  const mod = await load(r.config)
  const warnings = []
  const hooks = await mod.default.server({ client: fakeClient(null, warnings), directory: "/repo", worktree: "/repo" })
  await hooks.event(sessionCreated("ses_g"))
  await hooks.event(assistantDone("ses_g", "msg_1"))
  await hooks["chat.message"]({ sessionID: "ses_g" }, { message: { id: "msg_u" }, parts: [{ type: "text", text: "hi" }] })
  for (let i = 0; i < 100 && warnings.length === 0; i++) await Bun.sleep(20)
  await hooks.event(assistantDone("ses_g", "msg_2", { time: { created: 4000, completed: 5000 } }))
  await hooks.dispose()
  expect(warnings.length).toBe(1)
  expect(chats(r.spans()).map((s) => A(s)["opencode.call.id"])).toEqual(["msg_1"])
})

test("chat.params changes nothing in the request, a subagent's on OpenRouter included", async () => {
  const { hooks } = await start()
  await hooks.event(sessionCreated("ses_parent"))
  await hooks.event(sessionCreated("ses_child", { parentID: "ses_parent" }))
  for (const agent of ["explore", "title"]) {
    const output = { temperature: 0, topP: 1, topK: 0, maxOutputTokens: undefined, options: { usage: { include: true } } }
    const before = structuredClone(output)
    await hooks["chat.params"](
      { sessionID: "ses_child", agent, model: { id: "x", providerID: "openrouter", api: { npm: "@openrouter/ai-sdk-provider" } }, provider: { options: {} }, message: { id: "msg_u" } },
      output,
    )
    expect(output).toEqual(before)
  }
  await hooks.dispose()
})

test("an event delivered twice is handled once; one with no id every time", async () => {
  const { r, hooks } = await start()
  await hooks.event(sessionCreated("ses_e"))
  await hooks.event({ event: { ...ev("session.idle", { sessionID: "ses_e" }).event, id: "evt_1" } })
  await hooks.event({ event: { ...ev("session.idle", { sessionID: "ses_e" }).event, id: "evt_1" } })
  await hooks.event(ev("session.idle", { sessionID: "ses_e" }))
  await hooks.event(ev("session.idle", { sessionID: "ses_e" }))
  await hooks.dispose()
  expect(r.terma.calls().filter((c) => c.event === "opencode-idle").length).toBe(3)
})

test("two copies of the module in one process share one state", async () => {
  const r = rig({ headers: undefined, headersHelper: join(root, "helper") })
  writeFileSync(join(root, "helper"), `#!/bin/sh\necho '{"Authorization": "Bearer from-helper"}'\n`)
  chmodSync(join(root, "helper"), 0o700)
  const first = await load(r.config)
  const second = await load(r.config, { keep: true })
  const a = await first.default.server({ client: fakeClient(null), directory: "/repo", worktree: "/repo" })
  const b = await second.default.server({ client: fakeClient(null), directory: "/repo", worktree: "/repo" })
  await a.event(sessionCreated("ses_m"))
  await a.event(assistantDone("ses_m", "msg_1"))
  await b.event(assistantDone("ses_m", "msg_2", { time: { created: 3000, completed: 4000 } }))
  await b["chat.message"]({ sessionID: "ses_m" }, { message: { id: "msg_u2" }, parts: [{ type: "text", text: "hi" }] })
  await a.dispose()
  await b.dispose()
  expect(r.terma.calls().filter((c) => c.event === "opencode-session-start").length).toBe(1)
  const traces = r.requests().filter((q) => q.path === "/v1/traces")
  expect(traces.length).toBe(1)
  expect(traces[0].body.resourceSpans[0].scopeSpans[0].spans.length).toBe(2)
  expect(traces[0].headers["authorization"]).toBe("Bearer from-helper")
  const res = Object.fromEntries(traces[0].body.resourceSpans[0].resource.attributes.map((x) => [x.key, x.value.stringValue]))
  expect(res).toEqual({ "service.name": "opencode", "enduser.id": "dev@example.com" })
})

test("setup is inert: OpenCode 1.19's reduced context and an OpenCode 2 one", async () => {
  const mod = await load(rig().config)
  const reduced = { options: {}, agent: {}, aisdk: {}, catalog: {}, command: {}, integration: {}, plugin: {}, reference: {}, skill: {} }
  expect(await mod.default.setup(reduced)).toBeUndefined()
  expect(await mod.default.setup(undefined)).toBeUndefined()
  const touched = []
  const two = {
    app: { version: "2.4.1" },
    event: { subscribe: (...a) => touched.push(["subscribe", a]) },
    session: { hook: (...a) => touched.push(["hook", a]), get: (...a) => touched.push(["get", a]) },
  }
  expect(await mod.default.setup(two)).toBeUndefined()
  expect(touched).toEqual([])
})

test("a terma that cannot be run: one warning, nothing exported, no further attempts", async () => {
  const r = rig()
  const missing = join(r.dir, "gone", "terma")
  r.config.hookCommand = [missing, "hook"]
  const mod = await load(r.config)
  const warnings = []
  const hooks = await mod.default.server({ client: fakeClient(null, warnings), directory: "/repo", worktree: "/repo" })
  await hooks.event(sessionCreated("ses_x"))
  await hooks["chat.message"]({ sessionID: "ses_x" }, { message: { id: "msg_u" }, parts: [{ type: "text", text: "hi" }] })
  await hooks.event(assistantDone("ses_x", "msg_a"))
  // OpenCode writes its log about once a second: the plugin holds the exit until the
  // warning has had that long, and not at all when it is older.
  const before = Date.now()
  await hooks.dispose()
  expect(warnings.length).toBe(1)
  expect(Date.now() - before).toBeGreaterThan(1000)
  const again = Date.now()
  await hooks.dispose()
  expect(Date.now() - again).toBeLessThan(500)
  expect(warnings[0]).toMatchObject({ service: "terma", level: "warn" })
  expect(warnings[0].message).toBe(`terma: cannot run ${missing} (ENOENT). OpenCode sessions are not recorded until "terma setup" is run again.`)
  expect(r.requests()).toEqual([])

  // terma appears at that path: this process still calls no hook and exports nothing.
  mkdirSync(join(r.dir, "gone"))
  const log = join(r.dir, "late.log")
  writeFileSync(missing, `#!/bin/sh\necho "$2" >> '${log}'\n`)
  chmodSync(missing, 0o700)
  await hooks.event(sessionCreated("ses_y"))
  await hooks.event(assistantDone("ses_y", "msg_b"))
  await hooks.event(ev("session.idle", { sessionID: "ses_x" }))
  await hooks.dispose()
  expect(existsSync(log)).toBe(false)
  expect(warnings.length).toBe(1)
  expect(r.requests()).toEqual([])
})

test("a session whose record never arrives exports nothing and claims nothing", async () => {
  const { r, hooks } = await start()
  await hooks["chat.message"]({ sessionID: "ses_n" }, { message: { id: "msg_u" }, parts: [{ type: "text", text: "hi" }] })
  await hooks.event(assistantDone("ses_n", "msg_a"))
  await hooks.event(ev("session.idle", { sessionID: "ses_n" }))
  await hooks.dispose()
  expect(r.requests()).toEqual([])
  expect(r.terma.calls()).toEqual([])
}, 15_000)

test("a lookup that never answers leaves the session dark, and its tools do not wait for it", async () => {
  const client = { app: { log: async () => {} }, session: { get: () => new Promise(() => {}) } }
  const source = SOURCE.replace("const RECORD_WAIT_MS = 5000", "const RECORD_WAIT_MS = 300")
  const { r, hooks } = await start({}, { client, source })
  await hooks["tool.execute.before"]({ tool: "read", sessionID: "ses_hang", callID: "c1" }, { args: { filePath: "/repo/a" } })
  expect(globalThis[KEY].sessions.get("ses_hang").dark).toBe(true)
  await hooks.event(toolPart("ses_hang", "msg_1", "c1", "read", done({ filePath: "/repo/a" }, "ok")))
  await hooks.dispose()
  expect(r.requests()).toEqual([])
  expect(r.terma.calls()).toEqual([])
})

// How a task's part ends: completed, naming the child; or in error with no metadata, the
// child named only while the task ran.
const DARK_TASKS = {
  "completed": [done({ prompt: "look" }, "the child's answer", { metadata: { sessionId: "ses_kid" } })],
  "in error, the child named only while it ran": [
    { status: "running", input: { prompt: "look" }, metadata: { sessionId: "ses_kid" }, time: { start: 2100 } },
    { status: "error", input: { prompt: "look" }, error: "the child's answer", time: { start: 2100, end: 2200 } },
  ],
}
test.each(Object.keys(DARK_TASKS))("a subagent that went dark fails the session that opened it closed: a task %s", async (how) => {
  // The child's record never comes, so nothing it read was checked, and its answer is the
  // task call's result. The task's part arrives while the child still waits for its record.
  const warnings = []
  const source = SOURCE.replace("const RECORD_WAIT_MS = 5000", "const RECORD_WAIT_MS = 300")
  const { r, hooks } = await start({}, { client: fakeClient(() => null, warnings), source })
  await hooks.event(sessionCreated("ses_par"))
  const read = hooks["tool.execute.before"]({ tool: "read", sessionID: "ses_kid", callID: "k1" }, { args: { filePath: "/other/secret" } })
  for (const state of DARK_TASKS[how]) await hooks.event(toolPart("ses_par", "msg_1", "t1", "task", state))
  await hooks.event(assistantDone("ses_par", "msg_2"))
  await read
  await hooks.dispose()
  expect(globalThis[KEY].sessions.get("ses_kid").dark).toBe(true)
  expect(r.spans()).toEqual([])
  expect(globalThis[KEY].tainted.has("ses_par")).toBe(true)
  expect(warnings.map((w) => w.message)).toEqual(["terma: could not check what a tool reaches in session ses_par; nothing more of this session is recorded."])
  expect(r.terma.calls().filter((c) => c.payload.command === FENCE).map((c) => c.payload.session_id)).toEqual(["ses_par"])
})

test("a subagent's late reach check holds the session that opened it to the check's time", async () => {
  // A parent and its subagents share one run, so one Mixed cutoff: the task's result and
  // the parent's reply carry what the child read, and must not end before the child's check.
  const { r, hooks } = await start({}, { terma: { delayed: { "opencode-tool-reach": 0.3 } } })
  await hooks.event(sessionCreated("ses_up"))
  await hooks.event(sessionCreated("ses_dn", { parentID: "ses_up" }))
  await Promise.all([...globalThis[KEY].sessions.values()].map((s) => s.queue))
  // The child's shell call had no hook, so it is checked when its part arrives; meanwhile
  // the task returns and the parent replies.
  await hooks.event(toolPart("ses_dn", "msg_k", "k1", "bash", done({ command: "cat /other/x" }, "x")))
  await hooks.event(toolPart("ses_up", "msg_1", "t1", "task", done({ prompt: "look" }, "x", { metadata: { sessionId: "ses_dn" } })))
  await hooks.event(assistantDone("ses_up", "msg_2", { time: { created: 2000, completed: 3000 } }))
  await hooks.dispose()
  const finished = finishedAt(r.dir, "opencode-tool-reach")
  const up = r.spans().filter((sp) => A(sp)["session.id"] === "ses_up")
  expect(up.map((sp) => sp.name).sort()).toEqual(["chat m1", "execute_tool task"])
  for (const sp of up) expect(endOf(sp)).toBeGreaterThan(finished)
})

test("while a late reach check runs, what the rest of the family sends waits for it and is held to its time", async () => {
  // Nothing ties the parent to the child here (no task part): the parent goes on, as with a
  // subagent in the background, while the child's shell call is still being checked.
  const { r, hooks } = await start({}, { terma: { delayed: { "opencode-tool-reach": 0.3 } } })
  await hooks.event(sessionCreated("ses_fg"))
  await hooks.event(sessionCreated("ses_bg", { parentID: "ses_fg" }))
  await Promise.all([...globalThis[KEY].sessions.values()].map((s) => s.queue))
  await hooks.event(toolPart("ses_bg", "msg_k", "k1", "bash", done({ command: "cat /other/x" }, "x")))
  await hooks.event(assistantDone("ses_fg", "msg_1", { time: { created: 2000, completed: Date.now() } }))
  await hooks.event(ev("session.updated", { info: { id: "ses_fg", directory: "/repo", version: "1.19.6", title: "A real title", time: { created: 1000 } } }))
  await hooks.dispose()
  const finished = finishedAt(r.dir, "opencode-tool-reach")
  const [call] = chats(r.spans()).filter((sp) => A(sp)["session.id"] === "ses_fg")
  expect(endOf(call)).toBeGreaterThan(finished)
  expect(Number(BigInt(named(r.logs(), "opencode.session.title")[0].timeUnixNano) / 1000000n)).toBeGreaterThan(finished)
})

test("a tool's own check waits neither for a late check elsewhere in its family nor behind what its session has queued to send", async () => {
  const { r, hooks } = await start({}, { terma: { delayed: { "opencode-tool-reach": 0.4 } } })
  await hooks.event(sessionCreated("ses_fg"))
  await hooks.event(sessionCreated("ses_bg", { parentID: "ses_fg" }))
  await Promise.all([...globalThis[KEY].sessions.values()].map((s) => s.queue))
  const began = Date.now()
  // The child's late check, 400 ms; a reply of the parent's, queued to send and held by it;
  // then a tool of the parent's, whose own check is another 400 ms.
  await hooks.event(toolPart("ses_bg", "msg_k", "k1", "bash", done({ command: "cat /other/x" }, "x")))
  await hooks.event(assistantDone("ses_fg", "msg_1", { time: { created: 2000, completed: began } }))
  await hooks["tool.execute.before"]({ tool: "read", sessionID: "ses_fg", callID: "c1" }, { args: { filePath: "/repo/a" } })
  expect(Date.now() - began).toBeLessThan(650)
  await hooks.dispose()
  const [call] = chats(r.spans()).filter((sp) => A(sp)["session.id"] === "ses_fg")
  expect(endOf(call)).toBeGreaterThanOrEqual(began + 400)
})

test("a family's floor outlives its root, and a dark session still blocks what is under it once forgotten", async () => {
  const source = SOURCE.replace("const RECORD_WAIT_MS = 5000", "const RECORD_WAIT_MS = 300")
  const { r, hooks } = await start({}, { client: fakeClient(() => null), source, terma: { delayed: { "opencode-tool-reach": 0.2 } } })
  const settled = () => Promise.all([...globalThis[KEY].sessions.values()].map((s) => s.queue))
  await hooks.event(sessionCreated("ses_top"))
  await hooks.event(sessionCreated("ses_low", { parentID: "ses_top" }))
  await hooks.event(toolPart("ses_low", "msg_k", "k1", "bash", done({ command: "ls" }, "x")))
  await settled() // the check is done while the root is still kept
  await hooks.event(ev("session.deleted", { info: { id: "ses_top", directory: "/repo", version: "1.19.6", time: { created: 1000 } } }))
  await settled()
  expect(globalThis[KEY].sessions.has("ses_top")).toBe(false)
  await hooks.event(assistantDone("ses_low", "msg_2", { time: { created: 2000, completed: 3000 } }))
  // A session with no record goes dark, is forgotten as an eviction would, and its child comes.
  await hooks["tool.execute.before"]({ tool: "read", sessionID: "ses_dk", callID: "c1" }, { args: { filePath: "/repo/a" } })
  globalThis[KEY].sessions.delete("ses_dk")
  await hooks.event(sessionCreated("ses_orphan", { parentID: "ses_dk" }))
  await hooks.event(assistantDone("ses_orphan", "msg_3"))
  await hooks.dispose()
  const [call] = chats(r.spans()).filter((sp) => A(sp)["session.id"] === "ses_low")
  expect(endOf(call)).toBeGreaterThan(finishedAt(r.dir, "opencode-tool-reach"))
  expect(r.spans().filter((sp) => A(sp)["session.id"] === "ses_orphan")).toEqual([])
}, 15_000)

test("a session under one that went dark is not exported", async () => {
  const source = SOURCE.replace("const RECORD_WAIT_MS = 5000", "const RECORD_WAIT_MS = 300")
  const { r, hooks } = await start({}, { client: fakeClient(() => null), source })
  await hooks["tool.execute.before"]({ tool: "read", sessionID: "ses_dk", callID: "c1" }, { args: { filePath: "/repo/a" } })
  await hooks.event(sessionCreated("ses_under", { parentID: "ses_dk" }))
  await hooks.event(assistantDone("ses_under", "msg_1"))
  await hooks.dispose()
  expect(globalThis[KEY].sessions.get("ses_dk").dark).toBe(true)
  expect(r.spans()).toEqual([])
})

test("the exit waits a bounded time for its own sessions' queues, and not for another entry's", async () => {
  const { mod, hooks: b } = await start({}, { source: SOURCE.replace("const EXIT_QUEUE_MS = 8000", "const EXIT_QUEUE_MS = 1000") })
  const a = await mod.default.server({ client: fakeClient(null), directory: "/other", worktree: "/other" })
  // b's session waits RECORD_WAIT_MS, 5 s, for a record that never comes.
  await b["chat.message"]({ sessionID: "ses_wait" }, { message: { id: "msg_u" }, parts: [{ type: "text", text: "hi" }] })
  let began = Date.now()
  await a.dispose()
  expect(Date.now() - began).toBeLessThan(500)
  began = Date.now()
  await b.dispose()
  expect(Date.now() - began).toBeGreaterThanOrEqual(900)
  expect(Date.now() - began).toBeLessThan(2500)
}, 15_000)

test("a resumed session gets its record from OpenCode and is claimed from it", async () => {
  const record = { id: "ses_r", parentID: "ses_p", directory: "/elsewhere", version: "1.18.0", title: "Resumed title", time: { created: 1000 } }
  const { r, hooks } = await start({}, { client: fakeClient((id) => (id === "ses_r" ? record : null)) })
  await hooks["chat.message"]({ sessionID: "ses_r" }, { message: { id: "msg_u" }, parts: [{ type: "text", text: "again" }] })
  await hooks.event(assistantDone("ses_r", "msg_a"))
  await hooks.dispose()
  expect(r.terma.calls()[0]).toEqual({ event: "opencode-session-start", payload: { session_id: "ses_r", cwd: "/elsewhere", parent_session_id: "ses_p" } })
  const [call] = chats(r.spans())
  expect(A(call)["opencode.version"]).toBe("1.18.0")
  expect(A(call)["opencode.parent_session.id"]).toBe("ses_p")
  // A subagent's title is never sent.
  expect(named(r.logs(), "opencode.session.title")).toEqual([])
})

test("the repository's policy applies per session directory", async () => {
  const quiet = mkdtempSync(join(root, "quiet-"))
  mkdirSync(join(quiet, ".git"))
  mkdirSync(join(quiet, ".opencode"))
  writeFileSync(join(quiet, ".opencode", "terma.json"), JSON.stringify({ signals: [] }))
  mkdirSync(join(quiet, "sub"))
  const private_ = mkdtempSync(join(root, "private-"))
  mkdirSync(join(private_, ".git"))
  mkdirSync(join(private_, ".opencode"))
  writeFileSync(join(private_, ".opencode", "terma.json"), JSON.stringify({ includePrompts: false }))
  const open = mkdtempSync(join(root, "open-"))
  mkdirSync(join(open, ".git"))

  const { r, hooks } = await start()
  for (const [id, dir] of [["ses_q", join(quiet, "sub")], ["ses_p", private_], ["ses_o", open]]) {
    await hooks.event(sessionCreated(id, { directory: dir }))
    await hooks["chat.message"]({ sessionID: id }, { message: { id: "msg_u_" + id }, parts: [{ type: "text", text: "secret prompt" }] })
    await hooks.event(assistantDone(id, "msg_a_" + id))
  }
  await hooks.dispose()
  const sessions = new Set([...r.spans(), ...r.logs()].map(sessionOf))
  expect(sessions).toEqual(new Set(["ses_p", "ses_o"]))
  const bodies = Object.fromEntries(named(r.logs(), "opencode.user_prompt").map((x) => [sessionOf(x), x.body.stringValue]))
  expect(bodies).toEqual({ ses_p: "", ses_o: "secret prompt" })
  // Hooks run whatever the export policy says. Sessions' claims run side by side, in any order.
  expect(r.terma.calls().filter((c) => c.event === "opencode-session-start").map((c) => c.payload.session_id).sort()).toEqual(["ses_o", "ses_p", "ses_q"])
})

test("a session that moves takes the policy of the repository it moved into", async () => {
  const open = mkdtempSync(join(root, "open-"))
  mkdirSync(join(open, ".git"))
  const private_ = mkdtempSync(join(root, "private-"))
  mkdirSync(join(private_, ".git"))
  mkdirSync(join(private_, ".opencode"))
  writeFileSync(join(private_, ".opencode", "terma.json"), JSON.stringify({ includePrompts: false }))

  const { r, hooks } = await start()
  await hooks.event(sessionCreated("ses_m", { directory: open }))
  await hooks["chat.message"]({ sessionID: "ses_m" }, { message: { id: "msg_before" }, parts: [{ type: "text", text: "before the move" }] })
  await hooks.event(ev("session.updated", { info: { id: "ses_m", directory: private_, version: "1.19.6", title: "New session - 2026-11-25T00:00:00.000Z", time: { created: 1000, updated: 2000 } } }))
  await hooks["chat.message"]({ sessionID: "ses_m" }, { message: { id: "msg_after" }, parts: [{ type: "text", text: "after the move" }] })
  await hooks.dispose()
  const bodies = Object.fromEntries(named(r.logs(), "opencode.user_prompt").map((x) => [A(x)["opencode.message.id"], x.body.stringValue]))
  expect(bodies).toEqual({ msg_before: "before the move", msg_after: "" })
  // Hooks after the move name the new directory.
  const prompts = r.terma.calls().filter((c) => c.event === "opencode-prompt").map((c) => c.payload.cwd)
  expect(prompts).toEqual([open, private_])
})

function repos() {
  const open = mkdtempSync(join(root, "open-"))
  mkdirSync(join(open, ".git"))
  const private_ = mkdtempSync(join(root, "private-"))
  mkdirSync(join(private_, ".git"))
  mkdirSync(join(private_, ".opencode"))
  writeFileSync(join(private_, ".opencode", "terma.json"), JSON.stringify({ includePrompts: false }))
  return { open, private_ }
}
const moved = (id, directory) => ev("session.updated", { info: { id, directory, version: "1.19.6", title: "New session - 2026-11-25T00:00:00.000Z", time: { created: 1000, updated: 2000 } } })

test("a session moved away and straight back while its queue is busy ends where it is", async () => {
  const { open, private_ } = repos()
  const { r, hooks } = await start({}, { terma: { delayed: { "opencode-session-start": 0.5 } } })
  await hooks.event(sessionCreated("ses_back", { directory: private_ }))
  // Both moves arrive while the claim still holds the queue.
  for (let i = 0; i < 100 && !r.terma.calls().some((c) => c.event === "opencode-session-start"); i++) await Bun.sleep(10)
  await hooks.event(moved("ses_back", open))
  await hooks.event(moved("ses_back", private_))
  await hooks["chat.message"]({ sessionID: "ses_back" }, { message: { id: "msg_u" }, parts: [{ type: "text", text: "secret prompt" }] })
  await hooks.dispose()
  expect(named(r.logs(), "opencode.user_prompt").map((x) => x.body.stringValue)).toEqual([""])
  expect(r.terma.calls().filter((c) => c.event === "opencode-prompt").map((c) => c.payload.cwd)).toEqual([private_])
}, 15_000)

test("a move claims the session in its new directory before its next prompt, which is stamped after it", async () => {
  const { open, private_ } = repos()
  const { r, hooks } = await start({}, { terma: { delayed: { "opencode-tool-reach": 0.3 } } })
  await hooks.event(sessionCreated("ses_mv", { directory: open }))
  await hooks.event(moved("ses_mv", private_))
  await hooks["chat.message"]({ sessionID: "ses_mv" }, { message: { id: "msg_u", time: { created: 1500 } }, parts: [{ type: "text", text: "hi" }] })
  await hooks.dispose()
  expect(r.terma.calls()).toEqual([
    { event: "opencode-session-start", payload: { session_id: "ses_mv", cwd: open } },
    { event: "opencode-tool-reach", payload: { session_id: "ses_mv", cwd: private_, tool: "list", lists: [private_] } },
    { event: "opencode-prompt", payload: { session_id: "ses_mv", cwd: private_ } },
  ])
  const finished = finishedAt(r.dir, "opencode-tool-reach")
  const [p] = named(r.logs(), "opencode.user_prompt")
  expect(Number(BigInt(p.timeUnixNano) / 1000000n)).toBeGreaterThan(finished)
}, 15_000)

test("a move whose claim fails sends nothing more of the session", async () => {
  const { open, private_ } = repos()
  const { r, hooks } = await start({}, { terma: { fail: ["opencode-tool-reach"] } })
  await hooks.event(sessionCreated("ses_mf", { directory: open }))
  await hooks["chat.message"]({ sessionID: "ses_mf" }, { message: { id: "msg_before" }, parts: [{ type: "text", text: "before" }] })
  await hooks.event(moved("ses_mf", private_))
  await hooks["chat.message"]({ sessionID: "ses_mf" }, { message: { id: "msg_after" }, parts: [{ type: "text", text: "after" }] })
  await hooks.event(assistantDone("ses_mf", "msg_a"))
  await hooks.event(toolPart("ses_mf", "msg_a", "c1", "bash", done({ command: "true" }, "")))
  await hooks.dispose()
  expect(r.spans()).toEqual([])
  expect(named(r.logs(), "opencode.user_prompt").map((x) => A(x)["opencode.message.id"])).toEqual(["msg_before"])
  // The failed claim is followed by the fence.
  expect(r.terma.calls().map((c) => c.event)).toEqual(["opencode-session-start", "opencode-prompt", "opencode-tool-reach", "opencode-tool-reach"])
  expect(r.terma.calls().at(-1).payload.command).toBe(FENCE)
})

test("a null CONFIG makes the plugin inert", async () => {
  const mod = await load(null)
  expect(await mod.default.server({ client: fakeClient(null), directory: root, worktree: root })).toEqual({})
})

test("the module's only export is the default, with id, server and setup", async () => {
  const mod = await load(rig().config)
  expect(Object.keys(mod)).toEqual(["default"])
  expect(Object.keys(mod.default).sort()).toEqual(["id", "server", "setup"])
  expect(mod.default.id).toBe("terma")
})

test("deleting a session this process never saw only ends it", async () => {
  const { r, hooks } = await start()
  await hooks.event(ev("session.deleted", { info: { id: "ses_old", directory: "/old", version: "1.19.6", title: "Old title", time: { created: 1000 } } }))
  await hooks.dispose()
  expect(r.terma.calls()).toEqual([{ event: "opencode-session-end", payload: { session_id: "ses_old", cwd: "/old", reason: "deleted" } }])
  expect(r.requests()).toEqual([])
  expect(globalThis[KEY].sessions.has("ses_old")).toBe(false)
})

test("deleting a session ends it", async () => {
  const { r, hooks } = await start()
  await hooks.event(sessionCreated("ses_z"))
  await hooks.event(ev("session.deleted", { info: { id: "ses_z", directory: "/repo", version: "1.19.6", time: { created: 1000 } } }))
  await hooks.dispose()
  expect(r.terma.calls()).toEqual([
    { event: "opencode-session-start", payload: { session_id: "ses_z", cwd: "/repo" } },
    { event: "opencode-session-end", payload: { session_id: "ses_z", cwd: "/repo", reason: "deleted" } },
  ])
})

test("MCP names on OpenCode 1: as OpenCode writes them, never a built-in tool", async () => {
  const { r, hooks } = await start()
  await hooks.config({ mcp: { "my-server": {}, "my.server": {}, apply: {}, plan: {} } })
  await hooks.event(sessionCreated("ses_mcp"))
  for (const [callID, name] of [["c1", "my-server_echo"], ["c2", "my_server_echo"], ["c3", "apply_patch"], ["c4", "plan_enter"], ["c5", "plan_exit"]]) {
    await hooks.event(toolPart("ses_mcp", "msg_1", callID, name, done({}, "ok")))
  }
  await hooks.dispose()
  const byName = Object.fromEntries(tools(r.spans()).map((s) => [A(s)["gen_ai.tool.name"], A(s)]))
  expect(byName["my-server_echo"]["opencode.mcp.server"]).toBe("my-server")
  expect(byName["my-server_echo"]["opencode.mcp.tool"]).toBe("echo")
  expect(byName["my_server_echo"]["opencode.mcp.server"]).toBe("my.server")
  expect(byName["my_server_echo"]["opencode.mcp.tool"]).toBe("echo")
  for (const name of ["apply_patch", "plan_enter", "plan_exit"]) {
    expect(byName[name]).toBeDefined()
    expect(byName[name]["opencode.mcp.server"]).toBeUndefined()
    expect(byName[name]["opencode.mcp.tool"]).toBeUndefined()
  }
})

test("a slow session's hooks do not hold up another session's", async () => {
  const { r, hooks } = await start({}, { terma: { slow: ["ses_slow"] } })
  await hooks.event(sessionCreated("ses_slow"))
  // A burst from the slow session, all queued behind its hung claim.
  for (let i = 0; i < 8; i++) hooks["tool.execute.before"]({ tool: "read", sessionID: "ses_slow", callID: "s" + i }, { args: { filePath: "/repo/s" + i } })
  await hooks.event(sessionCreated("ses_fast"))
  const began = Date.now()
  await hooks["tool.execute.before"]({ tool: "read", sessionID: "ses_fast", callID: "f1" }, { args: { filePath: "/repo/f" } })
  expect(Date.now() - began).toBeLessThan(1000)
  await hooks.event(toolPart("ses_fast", "msg_f", "f1", "read", done({ filePath: "/repo/f" }, "ok")))
  await hooks.event(assistantDone("ses_fast", "msg_f"))
  await hooks.dispose()
  expect(globalThis[KEY].tainted.has("ses_fast")).toBe(false)
  const fast = r.terma.calls().filter((c) => c.payload.session_id === "ses_fast").map((c) => c.event)
  expect(fast).toEqual(["opencode-session-start", "opencode-tool-reach"])
  expect(new Set(tools(r.spans()).map(sessionOf))).toEqual(new Set(["ses_fast"]))
  expect(chats(r.spans()).map((s) => A(s)["opencode.call.id"])).toEqual(["msg_f"])
  // The slow session's claim ran first in its queue and timed out; the rest gave up behind it.
  expect(r.terma.calls().find((c) => c.payload.session_id === "ses_slow").event).toBe("opencode-session-start")
}, 15_000)

test("a missing terma is tried again after a minute; only sessions seen after it works are recorded", async () => {
  const r = rig()
  const path = r.terma.argv[0]
  const away = path + ".away"
  renameSync(path, away)
  const mod = await load(r.config)
  const warnings = []
  const hooks = await mod.default.server({ client: fakeClient(null, warnings), directory: "/repo", worktree: "/repo" })
  const st = globalThis[KEY]
  const age = () => {
    st.hooks.missingAt -= 60_000
  }

  await hooks.event(sessionCreated("ses_x"))
  await hooks["chat.message"]({ sessionID: "ses_x" }, { message: { id: "msg_ux" }, parts: [{ type: "text", text: "hi" }] })
  await hooks.event(assistantDone("ses_x", "msg_x1"))
  for (let i = 0; i < 100 && warnings.length === 0; i++) await Bun.sleep(20)
  expect(warnings.length).toBe(1)
  expect(st.hooks.missing).toBe(true)

  // A minute on, terma is still missing: one new attempt, which restarts the clock.
  age()
  const aged = st.hooks.missingAt
  await hooks.event(sessionCreated("ses_v"))
  await hooks.event(assistantDone("ses_v", "msg_v1"))
  await hooks.dispose()
  expect(st.hooks.missing).toBe(true)
  expect(st.hooks.missingAt).toBeGreaterThan(aged)

  // terma is back, but inside the window: nothing is spawned.
  renameSync(away, path)
  await hooks.event(sessionCreated("ses_w"))
  await hooks.event(assistantDone("ses_w", "msg_w1"))
  await hooks.event(ev("session.idle", { sessionID: "ses_x" }))
  await hooks.dispose()
  expect(r.terma.calls()).toEqual([])

  // After the window the next hook runs terma; a session first seen now is claimed.
  age()
  await hooks.event(sessionCreated("ses_y"))
  await hooks["chat.message"]({ sessionID: "ses_y" }, { message: { id: "msg_uy" }, parts: [{ type: "text", text: "hello" }] })
  await hooks.event(assistantDone("ses_y", "msg_y1"))
  // The sessions that went dark stay dark.
  for (const id of ["ses_x", "ses_v", "ses_w"]) {
    await hooks.event(assistantDone(id, "msg_" + id + "_late", { time: { created: 4000, completed: 5000 } }))
    await hooks.event(ev("session.idle", { sessionID: id }))
  }
  await hooks.dispose()
  expect(st.hooks.missing).toBe(false)
  expect(r.terma.calls().map((c) => [c.event, c.payload.session_id])).toEqual([
    ["opencode-session-start", "ses_y"],
    ["opencode-prompt", "ses_y"],
  ])
  expect(new Set([...r.spans(), ...r.logs()].map(sessionOf))).toEqual(new Set(["ses_y"]))
  expect(chats(r.spans()).map((s) => A(s)["opencode.call.id"])).toEqual(["msg_y1"])
  expect(warnings.length).toBe(1)
}, 15_000)

test("a file attached to a prompt is reach-checked first, and its content is not the prompt", async () => {
  const message = {
    message: { id: "msg_u" },
    parts: [
      { type: "text", synthetic: true, text: 'Called the Read tool with the following input: {"filePath":"/repo/notes.txt"}' },
      { type: "text", synthetic: true, text: "<path>/repo/notes.txt</path>\n<type>file</type>\n<content>\n1: alpha FILE-CANARY" },
      { type: "file", mime: "text/plain", url: "file:///repo/notes.txt", filename: "notes.txt" },
      { type: "text", text: "summarize it" },
    ],
  }
  const { r, hooks } = await start()
  await hooks.event(sessionCreated("ses_at"))
  await hooks["chat.message"]({ sessionID: "ses_at" }, structuredClone(message))
  await hooks.dispose()
  const [p] = named(r.logs(), "opencode.user_prompt")
  expect(p.body.stringValue).toBe("summarize it")
  expect(A(p)["opencode.prompt.length"]).toBe(12)
  expect(JSON.stringify(r.requests())).not.toContain("FILE-CANARY")
  expect(r.terma.calls()).toEqual([
    { event: "opencode-session-start", payload: { session_id: "ses_at", cwd: "/repo" } },
    { event: "opencode-tool-reach", payload: { session_id: "ses_at", cwd: "/repo", tool: "read", paths: ["/repo/notes.txt"] } },
    { event: "opencode-prompt", payload: { session_id: "ses_at", cwd: "/repo" } },
  ])

  // A reach that fails taints the session: the prompt is not exported.
  const refused = await start({}, { terma: { fail: ["opencode-tool-reach"] } })
  await refused.hooks.event(sessionCreated("ses_at2"))
  await refused.hooks["chat.message"]({ sessionID: "ses_at2" }, structuredClone(message))
  await refused.hooks.dispose()
  expect(named(refused.r.logs(), "opencode.user_prompt")).toEqual([])
  expect(refused.r.terma.calls().map((c) => [c.event, c.payload.command])).toEqual([
    ["opencode-session-start", undefined],
    ["opencode-tool-reach", undefined],
    ["opencode-tool-reach", FENCE],
  ])
})

test("an attached image is reach-checked by the path OpenCode read it from", async () => {
  const message = {
    message: { id: "msg_u" },
    parts: [
      { type: "text", synthetic: true, text: 'Called the Read tool with the following input: {"filePath":"/repo/dot.png"}' },
      { type: "text", synthetic: true, text: "Image read successfully" },
      { type: "file", synthetic: true, mime: "image/png", url: "data:image/png;base64,iVBORw0KGgo=", filename: "dot.png" },
      { type: "text", text: "what is this" },
    ],
  }
  const { r, hooks } = await start()
  await hooks.event(sessionCreated("ses_img"))
  await hooks["chat.message"]({ sessionID: "ses_img" }, structuredClone(message))
  await hooks.dispose()
  expect(r.terma.calls()).toEqual([
    { event: "opencode-session-start", payload: { session_id: "ses_img", cwd: "/repo" } },
    { event: "opencode-tool-reach", payload: { session_id: "ses_img", cwd: "/repo", tool: "read", paths: ["/repo/dot.png"] } },
    { event: "opencode-prompt", payload: { session_id: "ses_img", cwd: "/repo" } },
  ])
  expect(named(r.logs(), "opencode.user_prompt")[0].body.stringValue).toBe("what is this")
})

test("an attachment marker that is not JSON is reach-checked by its path as written, or as unplaced", async () => {
  const reachOf = async (marker) => {
    const { r, hooks } = await start()
    await hooks.event(sessionCreated("ses_mk"))
    const parts = [
      { type: "text", synthetic: true, text: "Called the Read tool with the following input: " + marker },
      { type: "text", text: "what is this" },
    ]
    await hooks["chat.message"]({ sessionID: "ses_mk" }, { message: { id: "msg_u" }, parts })
    await hooks.dispose()
    return r.terma.calls().filter((c) => c.event === "opencode-tool-reach").map((c) => c.payload.paths)
  }
  expect(await reachOf(String.raw`{"filePath":"C:\Users\a\dot.png"}`)).toEqual([[String.raw`C:\Users\a\dot.png`]])
  expect(await reachOf("not json")).toEqual([["//attached-file/unplaced"]])
})

test("a prompt with an attachment waits for its reach check and is stamped after it", async () => {
  const delayed = { "opencode-tool-reach": 0.3, "opencode-session-start": 0.3 }
  const { r, hooks } = await start({}, { terma: { delayed } })
  const done = (e) => finishedAt(r.dir, e)
  const message = (id, parts) => ({ message: { id, time: { created: 1500 } }, parts })

  // No attachment: OpenCode goes on at once, before the slow claim, at the message's time.
  await hooks.event(sessionCreated("ses_plain"))
  const t0 = Date.now()
  await hooks["chat.message"]({ sessionID: "ses_plain" }, message("msg_plain", [{ type: "text", text: "hi" }]))
  expect(Date.now() - t0).toBeLessThan(200)

  await hooks.event(sessionCreated("ses_att"))
  await hooks["chat.message"](
    { sessionID: "ses_att" },
    message("msg_att", [
      { type: "file", mime: "text/plain", url: "file:///repo/notes.txt", filename: "notes.txt" },
      { type: "text", text: "summarize it" },
    ]),
  )
  const finished = done("opencode-tool-reach")
  expect(finished).toBeNumber()
  await hooks.dispose()

  const at = (id) => Number(BigInt(named(r.logs(), "opencode.user_prompt").find((l) => A(l)["opencode.message.id"] === id).timeUnixNano) / 1000000n)
  expect(at("msg_plain")).toBe(1500)
  expect(at("msg_att")).toBeGreaterThan(finished)
}, 15_000)

test("a patch that moves a file names both paths", async () => {
  const { r, hooks } = await start()
  await hooks.event(sessionCreated("ses_mv"))
  const files = [{ filePath: "/repo/notes.txt", relativePath: "moved.txt", type: "move", movePath: "/repo/moved.txt" }]
  await hooks.event(toolPart("ses_mv", "msg_1", "c1", "apply_patch", done({ patchText: "*** Begin Patch" }, "Success", { metadata: { files } })))
  await hooks.dispose()
  const [span] = tools(r.spans())
  expect(A(span)["opencode.tool.files"]).toEqual(["/repo/notes.txt", "/repo/moved.txt"])
  const edits = r.terma.calls().filter((c) => c.event === "opencode-file-edit")
  expect(edits.map((c) => c.payload.files)).toEqual([["/repo/notes.txt", "/repo/moved.txt"]])
})

test("an assistant message no model answered is not a call; its tool part still is", async () => {
  const { r, hooks } = await start()
  await hooks.event(sessionCreated("ses_sh"))
  const zero = { cost: 0, tokens: { input: 0, output: 0, reasoning: 0, cache: { read: 0, write: 0 } }, finish: undefined }
  await hooks.event(toolPart("ses_sh", "msg_sh", "c1", "bash", done({ command: "pwd" }, "/repo\n", { metadata: { exit: 0 } })))
  await hooks.event(assistantDone("ses_sh", "msg_sh", zero))
  await hooks.event(assistantDone("ses_sh", "msg_err", { ...zero, error: { name: "APIError", data: { message: "stopped" } } }))
  await hooks.dispose()
  expect(chats(r.spans()).map((s) => [A(s)["opencode.call.id"], s.status.code])).toEqual([["msg_err", 2]])
  expect(tools(r.spans()).map((s) => A(s)["gen_ai.tool.call.id"])).toEqual(["msg_sh:c1"])
})

test("a turn OpenCode starts itself joins the last prompt's turn, when the session has one", async () => {
  const { r, hooks } = await start()
  const created = (messageID, parentID) =>
    ev("message.updated", { info: { id: messageID, sessionID: "ses_turn", role: "assistant", parentID, modelID: "m1", providerID: "p1", mode: "build", time: { created: 2000 } } })
  await hooks.event(sessionCreated("ses_turn"))
  // Before any prompt: a shell command the person ran. Its turn stays the message OpenCode made.
  await hooks.event(created("msg_sh0", "msg_auto0"))
  await hooks.event(toolPart("ses_turn", "msg_sh0", "c0", "bash", done({ command: "pwd" }, "/repo\n")))
  // A prompt, and a call that answers it.
  await hooks["chat.message"]({ sessionID: "ses_turn" }, { message: { id: "msg_p1" }, parts: [{ type: "text", text: "go" }] })
  await hooks.event(assistantDone("ses_turn", "msg_a1", { parentID: "msg_p1" }))
  // Then OpenCode goes on by itself: a call and a tool call under a user message nobody sent.
  await hooks.event(created("msg_a2", "msg_auto1"))
  await hooks.event(toolPart("ses_turn", "msg_a2", "c2", "bash", done({ command: "ls" }, "a\n")))
  await hooks.event(assistantDone("ses_turn", "msg_a2", { parentID: "msg_auto1" }))
  await hooks.dispose()
  const turns = Object.fromEntries(chats(r.spans()).map((s) => [A(s)["opencode.call.id"], A(s)["opencode.parent_message.id"]]))
  expect(turns).toEqual({ msg_a1: "msg_p1", msg_a2: "msg_p1" })
  const toolTurns = Object.fromEntries(tools(r.spans()).map((s) => [A(s)["gen_ai.tool.call.id"], A(s)["opencode.parent_message.id"]]))
  expect(toolTurns).toEqual({ "msg_sh0:c0": "msg_auto0", "msg_a2:c2": "msg_p1" })
})

test("a model call is a message with step parts, not one with a finish", async () => {
  const { r, hooks } = await start()
  await hooks.event(sessionCreated("ses_cmd"))
  const zero = { cost: 0, tokens: { input: 0, output: 0, reasoning: 0, cache: { read: 0, write: 0 } } }
  const step = (messageID, type) => ev("message.part.updated", { part: { id: "prt_" + messageID + "_" + type, sessionID: "ses_cmd", messageID, type } })
  // A command's subagent is launched by a message no model wrote.
  await hooks.event(toolPart("ses_cmd", "msg_launch", "c1", "task", done({ prompt: "review" }, "done", { metadata: { sessionId: "ses_child" } })))
  await hooks.event(assistantDone("ses_cmd", "msg_launch", { ...zero, finish: "tool-calls" }))
  // A provider that reports no usage still made a call.
  await hooks.event(step("msg_free", "step-start"))
  await hooks.event(step("msg_free", "step-finish"))
  await hooks.event(assistantDone("ses_cmd", "msg_free", { ...zero, finish: "stop" }))
  await hooks.dispose()
  expect(chats(r.spans()).map((s) => A(s)["opencode.call.id"])).toEqual(["msg_free"])
  const [task] = tools(r.spans())
  expect(A(task)["gen_ai.tool.call.id"]).toBe("msg_launch:c1")
  expect(A(task)["opencode.tool.subagent_session.id"]).toBe("ses_child")
})

test("MCP names belong to the directory that configured them", async () => {
  const r = rig()
  const mod = await load(r.config)
  const a = await mod.default.server({ client: fakeClient(null), directory: "/repo", worktree: "/repo" })
  const b = await mod.default.server({ client: fakeClient(null), directory: "/other", worktree: "/other" })
  await a.config({ mcp: { acme: {} } })
  await b.config({ mcp: { acme_git: {} } })
  await a.event(sessionCreated("ses_a"))
  await a.event(toolPart("ses_a", "msg_1", "c1", "acme_git_status", done({}, "ok")))
  await a.dispose()
  await b.dispose()
  const [span] = tools(r.spans())
  expect(A(span)["opencode.mcp.server"]).toBe("acme")
  expect(A(span)["opencode.mcp.tool"]).toBe("git_status")
})

test("a tool part OpenCode compacted is not a new call", async () => {
  const { r, hooks } = await start()
  await hooks.event(sessionCreated("ses_cp"))
  await hooks.event(toolPart("ses_cp", "msg_1", "c1", "edit", done({ filePath: "/repo/a" }, "ok", { time: { start: 2100, end: 2200, compacted: 9000 } })))
  await hooks.dispose()
  expect(tools(r.spans())).toEqual([])
  expect(r.terma.calls().map((c) => c.event)).toEqual(["opencode-session-start"])
})

test("the session left is the one used least recently", async () => {
  const r = rig()
  const source = SOURCE.replace("const MAX_SESSIONS = 512", "const MAX_SESSIONS = 2")
  expect(source).not.toBe(SOURCE)
  const mod = await load(r.config, { source })
  const hooks = await mod.default.server({ client: fakeClient(null), directory: "/repo", worktree: "/repo" })
  await hooks.event(sessionCreated("ses_1"))
  await hooks.event(sessionCreated("ses_2"))
  await hooks.dispose()
  await hooks.event(ev("session.idle", { sessionID: "ses_1" }))
  await hooks.dispose()
  await hooks.event(sessionCreated("ses_3"))
  expect([...globalThis[KEY].sessions.keys()].sort()).toEqual(["ses_1", "ses_3"])
  await hooks.dispose()
})

test("a reach check queued behind slow hooks waits for its turn, not from when it was asked", async () => {
  // Three idle hooks of about a second each: the check waits longer in the queue than it
  // may run (its limit made HOOK_TIMEOUT_MS here, + HOOK_KILL_GRACE_MS + 500), but each of
  // them finishes.
  const r = rig({}, { delayed: { "opencode-idle": 1.1 } })
  const source = SOURCE.replace("const REACH_HOOK_TIMEOUT_MS = 5500", "const REACH_HOOK_TIMEOUT_MS = 2000")
  expect(source).not.toBe(SOURCE)
  const mod = await load(r.config, { source })
  const warnings = []
  const hooks = await mod.default.server({ client: fakeClient(null, warnings), directory: "/repo", worktree: "/repo" })
  await hooks.event(sessionCreated("ses_q"))
  for (let i = 0; i < 3; i++) await hooks.event(ev("session.idle", { sessionID: "ses_q" }))
  await globalThis[KEY].sessions.get("ses_q").queue // the idle hooks are in the hook queue
  const began = Date.now()
  await hooks["tool.execute.before"]({ tool: "read", sessionID: "ses_q", callID: "c1" }, { args: { filePath: "/repo/a" } })
  expect(Date.now() - began).toBeGreaterThan(2000 + 250 + 500)
  expect(globalThis[KEY].tainted.has("ses_q")).toBe(false)
  await hooks.event(toolPart("ses_q", "msg_1", "c1", "read", done({ filePath: "/repo/a" }, "ok")))
  await hooks.dispose()
  expect(r.terma.calls().map((c) => c.event)).toEqual(["opencode-session-start", "opencode-idle", "opencode-idle", "opencode-idle", "opencode-tool-reach"])
  expect(tools(r.spans()).map((s) => A(s)["gen_ai.tool.call.id"])).toEqual(["msg_1:c1"])
  expect(warnings).toEqual([])
}, 15_000)

test("a failed reach check taints the session and says so once in OpenCode's log", async () => {
  const { r, hooks, warnings } = await start({}, { terma: { fail: ["opencode-tool-reach"] } })
  await hooks.event(sessionCreated("ses_rf"))
  await hooks["tool.execute.before"]({ tool: "read", sessionID: "ses_rf", callID: "c1" }, { args: { filePath: "/repo/a" } })
  await hooks["tool.execute.before"]({ tool: "read", sessionID: "ses_rf", callID: "c2" }, { args: { filePath: "/repo/b" } })
  await hooks.event(toolPart("ses_rf", "msg_1", "c1", "bash", done({ command: "cat /repo/c" }, "")))
  // Another session's failure is its own line.
  await hooks.event(sessionCreated("ses_rg"))
  await hooks["tool.execute.before"]({ tool: "read", sessionID: "ses_rg", callID: "c3" }, { args: { filePath: "/repo/d" } })
  await hooks.dispose()
  expect(globalThis[KEY].tainted.has("ses_rf")).toBe(true)
  expect(warnings.map((w) => w.message)).toEqual([
    "terma: could not check what a tool reaches in session ses_rf; nothing more of this session is recorded.",
    "terma: could not check what a tool reaches in session ses_rg; nothing more of this session is recorded.",
  ])
  expect(warnings[0]).toMatchObject({ service: "terma", level: "warn" })
  expect(r.spans()).toEqual([])
})

test("the exit waits a bounded time for a relay that is not there", async () => {
  // A port nothing listens on: each batch would retry for about 31 s.
  const closed = Bun.serve({ port: 0, fetch: () => new Response("{}") })
  const port = closed.port
  closed.stop(true)
  const { hooks } = await start({ endpoint: `http://127.0.0.1:${port}/` })
  await hooks.event(sessionCreated("ses_down"))
  await hooks.event(assistantDone("ses_down", "msg_1"))
  const began = Date.now()
  await hooks.dispose()
  expect(Date.now() - began).toBeLessThan(5000)
  expect(globalThis[KEY].exporter.draining).toBe(true)
}, 15_000)

const idsOf = (payload) => JSON.parse(payload).resourceSpans[0].scopeSpans[0].spans.map((s) => A(s)["opencode.call.id"])
const waiting = (ex) => ex.batches.flatMap((b) => idsOf(b.sends[0][1]))

// stalled loads the plugin with one item per batch and a fetch that answers only when let
// go, so batches pile up behind the one being sent.
async function stalled(original = SOURCE) {
  const r = rig()
  const source = original.replace("const BATCH_SIZE = 64", "const BATCH_SIZE = 1")
  expect(source).not.toBe(SOURCE)
  const mod = await load(r.config, { source })
  const warnings = []
  const hooks = await mod.default.server({ client: fakeClient(null, warnings), directory: "/repo", worktree: "/repo" })
  const real = globalThis.fetch
  const sent = []
  let release
  const gate = new Promise((ok) => {
    release = () => ok(new Response("{}"))
  })
  globalThis.fetch = async (url, init) => {
    // An earlier test's export may still be retrying.
    if (!String(url).startsWith(r.config.endpoint)) return real(url, init)
    sent.push(idsOf(init.body)[0])
    return gate
  }
  const finish = async () => {
    release()
    try {
      await hooks.dispose()
    } finally {
      globalThis.fetch = real
    }
  }
  const calls = async (sid, n) => {
    await hooks.event(sessionCreated(sid))
    for (let i = 0; i < n; i++) await hooks.event(assistantDone(sid, "msg_" + i))
    await Promise.all([...globalThis[KEY].sessions.values()].map((s) => s.queue))
  }
  return { r, hooks, warnings, sent, finish, calls, ex: globalThis[KEY].exporter }
}

test("while the relay does not answer, the oldest waiting batches are dropped, and that is said once", async () => {
  const { warnings, sent, finish, calls, ex } = await stalled()
  try {
    await calls("ses_cap", 40)
    // msg_0 is being sent; 39 wait behind it, and the 7 oldest of them go.
    expect(sent).toEqual(["msg_0"])
    expect(ex.batches.length).toBe(32)
    expect(ex.dropped).toBe(7)
    expect(waiting(ex)).toEqual(Array.from({ length: 32 }, (_, i) => "msg_" + (i + 8)))
    expect(warnings.map((w) => w.message)).toEqual(["terma: the relay is not answering; some OpenCode telemetry was dropped."])
  } finally {
    await finish()
  }
  expect(sent).toEqual(["msg_0", ...Array.from({ length: 32 }, (_, i) => "msg_" + (i + 8))])
  expect(warnings.length).toBe(1)
}, 15_000)

test("batches waiting for a slow relay leave in order, none dropped below the cap", async () => {
  const { warnings, sent, finish, calls, ex } = await stalled()
  try {
    await calls("ses_ord", 20)
    expect(ex.batches.length).toBe(19)
  } finally {
    await finish()
  }
  expect(sent).toEqual(Array.from({ length: 20 }, (_, i) => "msg_" + i))
  expect(ex.batches).toEqual([])
  expect(ex.dropped).toBe(0)
  expect(warnings).toEqual([])
}, 15_000)

test("waiting batches are dropped by size too, oldest first, and the bytes waiting are counted", async () => {
  const { sent, finish, calls, ex } = await stalled(SOURCE.replace("const MAX_PENDING_BYTES = 8 * 1024 * 1024", "const MAX_PENDING_BYTES = 8 * 1024"))
  let left
  try {
    await calls("ses_bïg", 20) // not ASCII: the bound is in bytes
    left = waiting(ex)
    expect(left.length).toBeGreaterThan(1)
    expect(left.length).toBeLessThan(19)
    expect(left).toEqual(Array.from({ length: left.length }, (_, i) => "msg_" + (20 - left.length + i)))
    expect(ex.dropped).toBe(19 - left.length)
    expect(ex.pendingBytes).toBe(ex.batches.flatMap((b) => b.sends).reduce((n, [, p]) => n + Buffer.byteLength(p), 0))
  } finally {
    await finish()
  }
  expect(sent).toEqual(["msg_0", ...left])
  expect(ex.pendingBytes).toBe(0)
}, 15_000)

test("a record that arrives while a session waits for one is picked up when it arrives", async () => {
  const { r, hooks } = await start()
  // No record and no way to ask OpenCode for one: the session waits.
  await hooks["chat.message"]({ sessionID: "ses_late" }, { message: { id: "msg_u" }, parts: [{ type: "text", text: "hi" }] })
  await Bun.sleep(100)
  const began = Date.now()
  await hooks.event(sessionCreated("ses_late"))
  for (let i = 0; i < 200 && !r.terma.calls().some((c) => c.event === "opencode-session-start"); i++) await Bun.sleep(10)
  expect(Date.now() - began).toBeLessThan(1000)
  await hooks.dispose()
  expect(r.terma.calls().map((c) => c.event)).toEqual(["opencode-session-start", "opencode-prompt"])
  expect(globalThis[KEY].sessions.get("ses_late").recordArrived).toBeNull()
}, 15_000)

test("a reach check that finds terma gone gives only the cannot-run warning", async () => {
  const { r, hooks, warnings } = await start()
  await hooks.event(sessionCreated("ses_gone"))
  for (let i = 0; i < 100 && !r.terma.calls().some((c) => c.event === "opencode-session-start"); i++) await Bun.sleep(10)
  await Promise.all([...globalThis[KEY].sessions.values()].map((s) => s.queue))
  rmSync(r.terma.argv[0])
  await hooks["tool.execute.before"]({ tool: "read", sessionID: "ses_gone", callID: "c1" }, { args: { filePath: "/repo/a" } })
  await hooks.dispose()
  expect(globalThis[KEY].tainted.has("ses_gone")).toBe(true)
  expect(warnings.map((w) => w.message)).toEqual([`terma: cannot run ${r.terma.argv[0]} (ENOENT). OpenCode sessions are not recorded until "terma setup" is run again.`])
})

test("terma's hooks run in a process group of their own: OpenCode 2 signals a tool step's group when the step ends", async () => {
  // Seen on OpenCode 2.0.0 and 2.4.1: when a tool step ends, OpenCode sends SIGTERM to the
  // process group it ran in, and a hook the plugin had just started for that step (the
  // file edit, the idle hook that starts the flush) died with it unless it was quick.
  const dir = mkdtempSync(join(root, "pgid-"))
  const hook = join(dir, "terma")
  writeFileSync(hook, `#!/bin/sh\ncat > /dev/null\nps -o pgid= -p $$ | tr -d ' ' > '${dir}/'"$2".pgid\n`)
  chmodSync(hook, 0o700)
  const { hooks } = await start({ hookCommand: [hook, "hook"] })
  await hooks.event(sessionCreated("ses_pg"))
  await hooks["chat.message"]({ sessionID: "ses_pg" }, { message: { id: "msg_u" }, parts: [{ type: "text", text: "hi" }] })
  await hooks.dispose()
  const mine = execFileSync("ps", ["-o", "pgid=", "-p", String(process.pid)]).toString().trim()
  for (const event of ["opencode-session-start", "opencode-prompt"]) {
    const theirs = readFileSync(join(dir, event + ".pgid"), "utf8").trim()
    expect(theirs).not.toBe("")
    expect(theirs).not.toBe(mine)
  }
})

test("a hook that runs past its limit is ended with the processes it started", async () => {
  // The hook starts a child and waits for it: ending the hook alone would leave the child.
  const dir = mkdtempSync(join(root, "kill-"))
  const hook = join(dir, "terma")
  writeFileSync(hook, `#!/bin/sh\ncat > /dev/null\necho $$ > '${dir}/hook.pid'\nsleep 30 &\necho $! > '${dir}/child.pid'\nwait\n`)
  chmodSync(hook, 0o700)
  const alive = (pid) => {
    try {
      process.kill(pid, 0)
      return true
    } catch {
      return false
    }
  }
  const { hooks } = await start({ hookCommand: [hook, "hook"] })
  await hooks.event(sessionCreated("ses_kill"))
  await Promise.all([...globalThis[KEY].sessions.values()].map((s) => s.queue))
  const pids = ["hook.pid", "child.pid"].map((f) => Number(readFileSync(join(dir, f), "utf8")))
  for (const pid of pids) expect(pid).toBeGreaterThan(1)
  for (let i = 0; i < 100 && pids.some(alive); i++) await Bun.sleep(10)
  expect(pids.map(alive)).toEqual([false, false])
  await hooks.dispose()
}, 15_000)

// settled waits until a session's hooks asked for so far, the fence included, are done.
async function settled(id) {
  const s = globalThis[KEY].sessions.get(id)
  await s.hookQueue
  for (let i = 0; i < 200 && s.fencing; i++) await Bun.sleep(5)
}
const fences = (r) => r.terma.calls().filter((c) => c.event === "opencode-tool-reach" && c.payload.command === FENCE)

test("a reach check may take as long as terma hook allows itself", async () => {
  // 3 s: past every other hook's limit, inside the reach check's.
  const { r, hooks, warnings } = await start({}, { terma: { delayed: { "opencode-tool-reach": 3 } } })
  await hooks.event(sessionCreated("ses_long"))
  await hooks["tool.execute.before"]({ tool: "read", sessionID: "ses_long", callID: "c1" }, { args: { filePath: "/repo/a" } })
  expect(finishedAt(r.dir, "opencode-tool-reach")).toBeNumber()
  expect(globalThis[KEY].tainted.has("ses_long")).toBe(false)
  await hooks.event(toolPart("ses_long", "msg_1", "c1", "read", done({ filePath: "/repo/a" }, "ok")))
  await hooks.dispose()
  expect(tools(r.spans()).map((s) => A(s)["gen_ai.tool.call.id"])).toEqual(["msg_1:c1"])
  expect(warnings).toEqual([])
}, 15_000)

test("a failed reach check asks terma once to mark the run Mixed, and the tool does not wait for it", async () => {
  const { r, hooks } = await start({}, { terma: { fail: ["opencode-tool-reach"], delayed: { "opencode-tool-reach": 0.3 } } })
  writeFileSync(join(r.dir, "fence.ok"), "")
  await hooks.event(sessionCreated("ses_fn"))
  await hooks["tool.execute.before"]({ tool: "read", sessionID: "ses_fn", callID: "c1" }, { args: { filePath: "/repo/a" } })
  // The check is done; the fence it started is not.
  expect(r.terma.calls().filter((c) => c.event === "opencode-tool-reach").length).toBe(1)
  await settled("ses_fn")
  expect(fences(r).length).toBe(1)
  // Once it succeeded, a later tool call asks nothing more.
  await hooks["tool.execute.before"]({ tool: "read", sessionID: "ses_fn", callID: "c2" }, { args: { filePath: "/repo/b" } })
  await settled("ses_fn")
  await hooks.dispose()
  expect(r.terma.calls().map((c) => c.event)).toEqual(["opencode-session-start", "opencode-tool-reach", "opencode-tool-reach"])
  expect(fences(r).map((c) => c.payload)).toEqual([{ session_id: "ses_fn", cwd: "/repo", tool: "bash", command: FENCE }])
  expect(globalThis[KEY].sessions.get("ses_fn").fenced).toBe(true)
}, 15_000)

test("a fence that fails is tried again at the session's next tool call, until it succeeds", async () => {
  const { r, hooks } = await start({}, { terma: { fail: ["opencode-tool-reach"] } })
  await hooks.event(sessionCreated("ses_fr"))
  const call = async (callID) => {
    await hooks["tool.execute.before"]({ tool: "read", sessionID: "ses_fr", callID }, { args: { filePath: "/repo/" + callID } })
    await settled("ses_fr")
  }
  await call("c1")
  expect(fences(r).length).toBe(1)
  await call("c2")
  expect(fences(r).length).toBe(2)
  writeFileSync(join(r.dir, "fence.ok"), "")
  await call("c3")
  expect(fences(r).length).toBe(3)
  await call("c4")
  await hooks.dispose()
  expect(fences(r).length).toBe(3)
  // The tainted session's own checks are not run again.
  expect(r.terma.calls().filter((c) => c.event === "opencode-tool-reach" && c.payload.command !== FENCE).length).toBe(1)
})

test("a session that failed closed stays tainted, however many sessions fail after it", async () => {
  const { r, hooks } = await start({}, { terma: { fail: ["opencode-tool-reach"] } })
  await hooks.event(sessionCreated("ses_first"))
  await hooks["tool.execute.before"]({ tool: "read", sessionID: "ses_first", callID: "c1" }, { args: { filePath: "/repo/a" } })
  // Others that failed since, many of them.
  const st = globalThis[KEY]
  for (let i = 0; i < 5000; i++) st.tainted.add("ses_filler_" + i)
  await hooks.event(sessionCreated("ses_last"))
  await hooks["tool.execute.before"]({ tool: "read", sessionID: "ses_last", callID: "c2" }, { args: { filePath: "/repo/b" } })
  expect(st.tainted.has("ses_first")).toBe(true)
  // A tool with no reach and a model call: nothing checks them, so only the taint stops them.
  await hooks.event(toolPart("ses_first", "msg_1", "c3", "webfetch", done({ url: "https://example.com" }, "page")))
  await hooks.event(assistantDone("ses_first", "msg_1"))
  await hooks.dispose()
  expect(r.spans()).toEqual([])
})

test("a write and an edit are reach-checked on their file before they run", async () => {
  const { r, hooks } = await start()
  await hooks.event(sessionCreated("ses_ed"))
  await hooks["tool.execute.before"]({ tool: "write", sessionID: "ses_ed", callID: "c1" }, { args: { filePath: "/repo/new.txt", content: "x" } })
  await hooks["tool.execute.before"]({ tool: "edit", sessionID: "ses_ed", callID: "c2" }, { args: { filePath: "/repo/a.txt", oldString: "a", newString: "b" } })
  const before = r.terma.calls()
  await hooks.event(toolPart("ses_ed", "msg_1", "c1", "write", done({ filePath: "/repo/new.txt", content: "x" }, "ok")))
  await hooks.event(toolPart("ses_ed", "msg_1", "c2", "edit", done({ filePath: "/repo/a.txt", oldString: "a", newString: "b" }, "ok")))
  await hooks.dispose()
  expect(before).toEqual([
    { event: "opencode-session-start", payload: { session_id: "ses_ed", cwd: "/repo" } },
    { event: "opencode-tool-reach", payload: { session_id: "ses_ed", cwd: "/repo", tool: "write", paths: ["/repo/new.txt"] } },
    { event: "opencode-tool-reach", payload: { session_id: "ses_ed", cwd: "/repo", tool: "edit", paths: ["/repo/a.txt"] } },
  ])
  // The parts repeat what was checked: no second check, and the edits are reported after.
  expect(r.terma.calls().slice(before.length).map((c) => [c.event, c.payload.tool])).toEqual([
    ["opencode-file-edit", "write"],
    ["opencode-file-edit", "edit"],
  ])
  // A call checked before it ran keeps OpenCode's own times.
  expect(tools(r.spans()).map((s) => s.endTimeUnixNano)).toEqual([nanosOf(2200), nanosOf(2200)])
})
const nanosOf = (ms) => String(BigInt(ms) * 1000000n)
const endOf = (span) => Number(BigInt(span.endTimeUnixNano) / 1000000n)

test("a call checked only when its part arrives ends after its check; one checked before keeps its time", async () => {
  const files = [{ filePath: "/repo/notes.txt", relativePath: "notes.txt", type: "update" }]
  const cases = [
    // A patch's files are known only once it ran.
    ["apply_patch", done({ patchText: "*** Begin Patch" }, "Success", { metadata: { files } }), { tool: "apply_patch", paths: ["/repo/notes.txt"] }],
    // A shell command the person ran with !cmd fires no tool.execute.before.
    ["bash", done({ command: "ls" }, "a\n"), { tool: "bash", command: "ls" }],
  ]
  for (const [name, state, reach] of cases) {
    const { r, hooks } = await start({}, { terma: { delayed: { "opencode-tool-reach": 0.3 } } })
    await hooks.event(sessionCreated("ses_late"))
    await hooks.event(toolPart("ses_late", "msg_1", "c1", name, state))
    await hooks.dispose()
    expect(r.terma.calls().filter((c) => c.event === "opencode-tool-reach").map((c) => c.payload)).toEqual([{ session_id: "ses_late", cwd: "/repo", ...reach }])
    const [span] = tools(r.spans())
    expect(endOf(span)).toBeGreaterThan(finishedAt(r.dir, "opencode-tool-reach"))
    expect(span.startTimeUnixNano).toBe(nanosOf(2100))
  }

  const { r, hooks } = await start({}, { terma: { delayed: { "opencode-tool-reach": 0.3 } } })
  await hooks.event(sessionCreated("ses_early"))
  await hooks["tool.execute.before"]({ tool: "read", sessionID: "ses_early", callID: "c1" }, { args: { filePath: "/repo/a" } })
  await hooks.event(toolPart("ses_early", "msg_1", "c1", "read", done({ filePath: "/repo/a" }, "ok")))
  await hooks.dispose()
  expect(tools(r.spans()).map((s) => s.endTimeUnixNano)).toEqual([nanosOf(2200)])
}, 15_000)

const PATCH = { patchText: "*** Begin Patch" }
const patchFiles = (...paths) => paths.map((filePath) => ({ filePath, relativePath: filePath, type: "update" }))
const afterPatch = (hooks, sessionID, callID, files) =>
  hooks["tool.execute.after"]({ tool: "apply_patch", sessionID, callID, args: PATCH }, { title: "Success", output: "Success", metadata: { files } })
const reaches = (r) => r.terma.calls().filter((c) => c.event === "opencode-tool-reach" && c.payload.command !== FENCE)

test("a patch is checked in tool.execute.after, once, and keeps its own end", async () => {
  // A resumed session: its directory is known only once OpenCode answered for its record,
  // and the patch's file is relative, so the check must wait for it.
  const record = (id) => ({ id, directory: "/repo", version: "1.19.6", title: "A title", time: { created: 1000 } })
  const { r, hooks } = await start({}, { client: fakeClient(record) })
  const files = patchFiles("notes.txt")
  await hooks["tool.execute.before"]({ tool: "apply_patch", sessionID: "ses_pa", callID: "c1" }, { args: PATCH })
  await afterPatch(hooks, "ses_pa", "c1", files)
  const checked = reaches(r).map((c) => c.payload)
  // OpenCode sets the end once the hook returned.
  const end = Date.now() + 50
  await hooks.event(toolPart("ses_pa", "msg_1", "c1", "apply_patch", done(PATCH, "Success", { metadata: { files }, time: { start: 2100, end } })))
  await hooks.dispose()
  expect(checked).toEqual([{ session_id: "ses_pa", cwd: "/repo", tool: "apply_patch", paths: ["/repo/notes.txt"] }])
  expect(reaches(r).length).toBe(1)
  const [span] = tools(r.spans())
  expect(span.endTimeUnixNano).toBe(nanosOf(end))
  expect(span.startTimeUnixNano).toBe(nanosOf(2100))
  expect(A(span)["opencode.tool.files"]).toEqual(["/repo/notes.txt"])
})

test("tool.execute.after holds the tool until its check is done; a part that ends earlier ends at the check", async () => {
  const { r, hooks } = await start({}, { terma: { delayed: { "opencode-tool-reach": 0.3 } } })
  await hooks.event(sessionCreated("ses_pw"))
  const files = patchFiles("/repo/notes.txt")
  await afterPatch(hooks, "ses_pw", "c1", files)
  const finished = finishedAt(r.dir, "opencode-tool-reach")
  expect(finished).toBeNumber()
  // A build that set the end before the hook returned: the span ends after the check.
  await hooks.event(toolPart("ses_pw", "msg_1", "c1", "apply_patch", done(PATCH, "Success", { metadata: { files } })))
  await hooks.dispose()
  expect(reaches(r).length).toBe(1)
  const [span] = tools(r.spans())
  expect(endOf(span)).toBeGreaterThan(finished)
  expect(span.startTimeUnixNano).toBe(nanosOf(2100))
}, 15_000)

test("nothing the session sends after a check made after its tool ran ends before that check", async () => {
  const { r, hooks } = await start({}, { terma: { delayed: { "opencode-tool-reach": 0.3 } } })
  await hooks.event(sessionCreated("ses_fl"))
  await globalThis[KEY].sessions.get("ses_fl").queue // claimed: a title is no longer the first sight's
  // A shell command the person ran: no hook saw it, so it is checked when its part arrives.
  await hooks.event(toolPart("ses_fl", "msg_1", "c1", "bash", done({ command: "ls" }, "a\n")))
  // A title and a prompt that came in during the check, a model call that overlapped it,
  // and one that ended after it.
  await hooks.event(ev("session.updated", { info: { id: "ses_fl", directory: "/repo", version: "1.19.6", title: "A real title", time: { created: 1000 } } }))
  const asked = hooks["chat.message"]({ sessionID: "ses_fl" }, { message: { id: "msg_u", time: { created: 1000 } }, parts: [{ type: "text", text: "hi" }] })
  await hooks.event(assistantDone("ses_fl", "msg_2", { time: { created: 2000, completed: 3000 } }))
  const later = Date.now() + 60_000
  await hooks.event(assistantDone("ses_fl", "msg_3", { time: { created: 2000, completed: later } }))
  await asked
  await hooks.dispose()
  const finished = finishedAt(r.dir, "opencode-tool-reach")
  for (const name of ["opencode.session.title", "opencode.user_prompt"]) {
    const [rec] = named(r.logs(), name)
    expect(Number(BigInt(rec.timeUnixNano) / 1000000n)).toBeGreaterThan(finished)
  }
  const calls = Object.fromEntries(chats(r.spans()).map((s) => [A(s)["opencode.call.id"], s]))
  expect(endOf(calls.msg_2)).toBeGreaterThan(finished)
  expect(calls.msg_2.startTimeUnixNano).toBe(nanosOf(2000))
  expect(endOf(calls.msg_3)).toBe(later)
}, 15_000)

test("a patch whose check fails in tool.execute.after exports nothing of it", async () => {
  const { r, hooks } = await start({}, { terma: { fail: ["opencode-tool-reach"] } })
  await hooks.event(sessionCreated("ses_pe"))
  const files = patchFiles("/other/b.txt")
  await afterPatch(hooks, "ses_pe", "c1", files)
  expect(globalThis[KEY].tainted.has("ses_pe")).toBe(true)
  await hooks.event(toolPart("ses_pe", "msg_1", "c1", "apply_patch", done(PATCH, "Success", { metadata: { files } })))
  await hooks.event(assistantDone("ses_pe", "msg_1"))
  await hooks.dispose()
  expect(r.spans()).toEqual([])
  expect(reaches(r).map((c) => c.payload.paths)).toEqual([["/other/b.txt"]])
  expect(r.terma.calls().filter((c) => c.event === "opencode-file-edit")).toEqual([])
})

test("a call checked before it ran is not checked again in tool.execute.after", async () => {
  const { r, hooks } = await start()
  await hooks.event(sessionCreated("ses_ab"))
  const calls = [
    ["write", { filePath: "/repo/new.txt", content: "x" }],
    ["edit", { filePath: "/repo/a.txt", oldString: "a", newString: "b" }],
    ["read", { filePath: "/repo/a.txt" }],
    ["bash", { command: "ls" }],
  ]
  for (const [i, [tool, args]] of calls.entries()) {
    await hooks["tool.execute.before"]({ tool, sessionID: "ses_ab", callID: "c" + i }, { args })
    await hooks["tool.execute.after"]({ tool, sessionID: "ses_ab", callID: "c" + i, args }, { title: "t", output: "ok", metadata: {} })
  }
  await hooks.dispose()
  expect(reaches(r).map((c) => c.payload.tool)).toEqual(["write", "edit", "read", "bash"])
})

test("a call that reaches nothing does not wait in tool.execute.after", async () => {
  const { r, hooks } = await start({}, { terma: { delayed: { "opencode-session-start": 0.5 } } })
  await hooks.event(sessionCreated("ses_nw"))
  const began = Date.now()
  await hooks["tool.execute.after"]({ tool: "capture_echo", sessionID: "ses_nw", callID: "c1", args: { text: "x" } }, { title: "", output: "x", metadata: {} })
  expect(Date.now() - began).toBeLessThan(250)
  await hooks.dispose()
  expect(reaches(r)).toEqual([])
}, 15_000)

test("an edit whose reach check fails exports nothing of it", async () => {
  const { r, hooks } = await start({}, { terma: { fail: ["opencode-tool-reach"] } })
  await hooks.event(sessionCreated("ses_ef"))
  await hooks["tool.execute.before"]({ tool: "edit", sessionID: "ses_ef", callID: "c1" }, { args: { filePath: "/other/a.txt", oldString: "a", newString: "b" } })
  await hooks.event(toolPart("ses_ef", "msg_1", "c1", "edit", done({ filePath: "/other/a.txt", oldString: "a", newString: "b" }, "ok")))
  // A patch no guard saw fails its check when its part arrives.
  const files = [{ filePath: "/other/b.txt", relativePath: "b.txt", type: "update" }]
  await hooks.event(sessionCreated("ses_pf"))
  await hooks.event(toolPart("ses_pf", "msg_2", "c2", "apply_patch", done({ patchText: "*** Begin Patch" }, "Success", { metadata: { files } })))
  await hooks.dispose()
  expect(r.spans()).toEqual([])
  expect(r.terma.calls().filter((c) => c.event === "opencode-file-edit")).toEqual([])
})
