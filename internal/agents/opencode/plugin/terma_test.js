// Runs the plugin the way OpenCode would (CONFIG spliced in, hooks and events delivered in
// the order OpenCode delivered them) against a local OTLP receiver and a fake `terma`, and
// checks the wire that arrives and the hooks that ran.
//
// The replay tests feed testdata/1.19.6/<case>/plugin-view.jsonl, what a plugin saw in real
// OpenCode 1.19.6 sessions (testdata/PROVENANCE.md), through the OpenCode 1 entry; and
// testdata/2.4.1/<case> and testdata/2.x-scripted/<version>/<case>, real OpenCode 2.4.1
// sessions and scripted ones on 2.0.0 and 2.4.1, through the OpenCode 2 entry.
//
// `bun test` in this folder.
import { test, expect, beforeAll, afterAll, describe, setSystemTime } from "bun:test"
import { mkdtempSync, readFileSync, writeFileSync, mkdirSync, chmodSync, existsSync, rmSync, renameSync, statSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { execFileSync } from "node:child_process"

const KEY = Symbol.for("terma.opencode.state/2")
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
// trusted for milliseconds).
function fakeTerma(dir, { fail = [], slow = [], delayed = {} } = {}) {
  const log = join(dir, "hooks.log")
  const path = join(dir, "terma")
  const failing = fail.map((e) => `[ "$2" = "${e}" ] && exit 1`).join("\n")
  const hanging = slow.map((id) => `case "$input" in *'"${id}"'*) exec sleep 30;; esac`).join("\n")
  const waiting = Object.entries(delayed)
    .map(([e, secs]) => `[ "$2" = "${e}" ] && sleep ${secs} && : > '${join(dir, e + ".done")}'`)
    .join("\n")
  writeFileSync(path, `#!/bin/sh\ninput=$(cat)\nprintf '%s\\t%s\\n' "$2" "$input" >> '${log}'\n${hanging}\n${waiting}\n${failing}\nexit 0\n`)
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
  // The span ends when OpenCode said, and nothing after it moves.
  const part = endedToolParts(res.records).find((p) => p.tool === "apply_patch")
  expect(endOf(patch)).toBe(part.state.time.end)
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

test("setup is inert on OpenCode 1.19's reduced context, and with a null CONFIG", async () => {
  const mod = await load(rig().config)
  const reduced = { options: {}, agent: {}, aisdk: {}, catalog: {}, command: {}, integration: {}, plugin: {}, reference: {}, skill: {} }
  expect(await mod.default.setup(reduced)).toBeUndefined()
  expect(await mod.default.setup(undefined)).toBeUndefined()
  const touched = []
  const two = {
    app: { version: "2.4.1" },
    event: { subscribe: (...a) => touched.push(["subscribe", a]) },
    session: { hook: (...a) => touched.push(["hook", a]), get: (...a) => touched.push(["get", a]) },
    tool: { hook: (...a) => touched.push(["tool", a]) },
  }
  // OpenCode 1.19 under a 2.x-looking name: no OpenCode 2 context, no entry.
  expect(await mod.default.setup({ ...two, app: { version: "1.19.6" } })).toBeUndefined()
  expect(touched).toEqual([])
  const inert = await load(null)
  expect(await inert.default.setup(two)).toBeUndefined()
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

test("a lookup that never answers leaves the session dark", async () => {
  const client = { app: { log: async () => {} }, session: { get: () => new Promise(() => {}) } }
  const source = SOURCE.replace("const RECORD_WAIT_MS = 5000", "const RECORD_WAIT_MS = 300")
  const { r, hooks } = await start({}, { client, source })
  await hooks.event(ev("session.idle", { sessionID: "ses_hang" }))
  await globalThis[KEY].sessions.get("ses_hang").first
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
test("a session under one that went dark is not exported", async () => {
  const source = SOURCE.replace("const RECORD_WAIT_MS = 5000", "const RECORD_WAIT_MS = 300")
  const { r, hooks } = await start({}, { client: fakeClient(() => null), source })
  await hooks.event(ev("session.idle", { sessionID: "ses_dk" }))
  await globalThis[KEY].sessions.get("ses_dk").first
  await hooks.event(sessionCreated("ses_under", { parentID: "ses_dk" }))
  await hooks.event(assistantDone("ses_under", "msg_1"))
  await hooks.dispose()
  expect(globalThis[KEY].sessions.get("ses_dk").dark).toBe(true)
  expect(r.spans()).toEqual([])
})

test("the exit waits a bounded time for its own sessions' queues, and not for another entry's", async () => {
  const { mod, hooks: b } = await start({}, { source: SOURCE.replace("const EXIT_QUEUE_MS = 8000", "const EXIT_QUEUE_MS = 1000") })
  const a = await mod.default.server({ client: fakeClient(null), directory: "/other", worktree: "/other" })
  // b's session waits RECORD_WAIT_MS, 5 s, for a record that never comes (and so does the
  // prompt hook, for the first claim: the exit does not wait for it).
  void b["chat.message"]({ sessionID: "ses_wait" }, { message: { id: "msg_u" }, parts: [{ type: "text", text: "hi" }] })
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
  // Hooks after the move name the new directory, the move's own claim first.
  const prompts = r.terma.calls().filter((c) => c.event === "opencode-prompt").map((c) => c.payload.cwd)
  expect(prompts).toEqual([open, private_, private_])
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
  // Each move claims the session where it moved, in turn; the prompt is the last directory's.
  expect(r.terma.calls().filter((c) => c.event === "opencode-prompt").map((c) => c.payload.cwd)).toEqual([open, private_, private_])
}, 15_000)

test("a move claims the session in its new directory before its next prompt, which is stamped after it", async () => {
  const { open, private_ } = repos()
  const { r, hooks } = await start({}, { terma: { delayed: { "opencode-prompt": 0.3 } } })
  await hooks.event(sessionCreated("ses_mv", { directory: open }))
  await hooks.event(moved("ses_mv", private_))
  await hooks["chat.message"]({ sessionID: "ses_mv" }, { message: { id: "msg_u", time: { created: 1500 } }, parts: [{ type: "text", text: "hi" }] })
  await hooks.dispose()
  expect(r.terma.calls()).toEqual([
    { event: "opencode-session-start", payload: { session_id: "ses_mv", cwd: open } },
    { event: "opencode-prompt", payload: { session_id: "ses_mv", cwd: private_ } },
    { event: "opencode-prompt", payload: { session_id: "ses_mv", cwd: private_ } },
  ])
  // The move's claim finished (movedAt) before the prompt was stamped, and only one claim
  // of the session's start was asked for.
  const movedAt = globalThis[KEY].sessions.get("ses_mv").movedAt
  expect(movedAt).toBeGreaterThan(1500)
  const [p] = named(r.logs(), "opencode.user_prompt")
  expect(Number(BigInt(p.timeUnixNano) / 1000000n)).toBeGreaterThanOrEqual(movedAt)
}, 15_000)

test("a failed move claim darkens the session so later prompts do not export under the old claim", async () => {
  const { open, private_ } = repos()
  const { r, hooks } = await start({}, { terma: { fail: ["opencode-prompt"] } })
  await hooks.event(sessionCreated("ses_mv_fail", { directory: open }))
  await hooks.event(moved("ses_mv_fail", private_))
  await hooks["chat.message"]({ sessionID: "ses_mv_fail" }, { message: { id: "msg_u", time: { created: 1500 } }, parts: [{ type: "text", text: "after failed move" }] })
  await hooks.dispose()
  expect(r.terma.calls()).toEqual([
    { event: "opencode-session-start", payload: { session_id: "ses_mv_fail", cwd: open } },
    { event: "opencode-prompt", payload: { session_id: "ses_mv_fail", cwd: private_ } },
  ])
  expect(named(r.logs(), "opencode.user_prompt")).toEqual([])
  const s = globalThis[KEY].sessions.get("ses_mv_fail")
  expect(s.dark).toBe(true)
  expect(s.movedAt).toBe(0)
}, 15_000)

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
  for (let i = 0; i < 8; i++) hooks["chat.message"]({ sessionID: "ses_slow" }, { message: { id: "msg_s" + i }, parts: [{ type: "text", text: "s" + i }] })
  await hooks.event(sessionCreated("ses_fast"))
  const began = Date.now()
  await hooks["chat.message"]({ sessionID: "ses_fast" }, { message: { id: "msg_uf" }, parts: [{ type: "text", text: "f" }] })
  expect(Date.now() - began).toBeLessThan(1000)
  await hooks.event(toolPart("ses_fast", "msg_f", "f1", "read", done({ filePath: "/repo/f" }, "ok")))
  await hooks.event(assistantDone("ses_fast", "msg_f"))
  await hooks.dispose()
  const fast = r.terma.calls().filter((c) => c.payload.session_id === "ses_fast").map((c) => c.event)
  expect(fast).toEqual(["opencode-session-start", "opencode-prompt"])
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

// A resumed session's run starts at its claim. OpenCode 1 stamps the prompt with its own
// time, before this process claimed the session.
test("a resumed session's first prompt is stamped after its claim, and no call or tool of its turn starts before it", async () => {
  const record = { id: "ses_res", directory: "/repo", version: "1.19.6", title: "Resumed title", time: { created: 1000 } }
  const { r, hooks } = await start({}, { client: fakeClient((id) => (id === "ses_res" ? record : null)), terma: { delayed: { "opencode-session-start": 0.3 } } })
  const ms = (ns) => Number(BigInt(ns) / 1000000n)
  const asked = Date.now()
  await hooks["chat.message"]({ sessionID: "ses_res" }, { message: { id: "msg_u", time: { created: asked - 20 } }, parts: [{ type: "text", text: "again" }] })
  // OpenCode 1 waits for the hook before it starts the turn: the turn's call and tool start now.
  const turn = Date.now()
  const claimed = finishedAt(r.dir, "opencode-session-start")
  expect(claimed).toBeNumber()
  expect(turn).toBeGreaterThanOrEqual(Math.floor(claimed))
  const msg = { id: "msg_a", sessionID: "ses_res", role: "assistant", parentID: "msg_u", modelID: "m1", providerID: "p1", mode: "build", time: { created: turn } }
  await hooks.event(ev("message.updated", { info: msg }))
  await hooks.event(toolPart("ses_res", "msg_a", "c1", "read", done({ filePath: "/repo/a.txt" }, "a", { time: { start: turn + 1, end: turn + 2 } })))
  await hooks.event(ev("message.updated", { info: { ...msg, cost: 0, tokens: { input: 1, output: 1, reasoning: 0, cache: { read: 0, write: 0 } }, finish: "stop", time: { created: turn, completed: turn + 3 } } }))
  // A later prompt OpenCode gave no time is stamped when it came, never back at the claim.
  await new Promise((done) => setTimeout(done, 50))
  const untimed = Date.now()
  await hooks["chat.message"]({ sessionID: "ses_res" }, { message: { id: "msg_v" }, parts: [{ type: "text", text: "no time" }] })
  await hooks.dispose()

  const prompts = named(r.logs(), "opencode.user_prompt")
  const p = prompts.find((l) => A(l)["opencode.message.id"] === "msg_u")
  const v = prompts.find((l) => A(l)["opencode.message.id"] === "msg_v")
  expect(ms(v.timeUnixNano)).toBeGreaterThanOrEqual(untimed)
  const at = ms(p.timeUnixNano)
  expect(at).toBeGreaterThanOrEqual(Math.floor(claimed))
  expect(at).toBeLessThanOrEqual(turn)
  // The title the session came with is not held to the claim: the relay judges it by when it
  // was seen, before the claim.
  const [titled] = named(r.logs(), "opencode.session.title")
  expect(titled.body.stringValue).toBe("Resumed title")
  expect(ms(titled.timeUnixNano)).toBeGreaterThanOrEqual(asked)
  expect(ms(titled.timeUnixNano)).toBeLessThan(Math.floor(claimed))
  const turnSpans = r.spans().filter((sp) => A(sp)["opencode.parent_message.id"] === "msg_u")
  expect(turnSpans.map((sp) => sp.name).sort()).toEqual(["chat m1", "execute_tool read"])
  for (const sp of turnSpans) expect(ms(sp.startTimeUnixNano)).toBeGreaterThanOrEqual(at)
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
  // No record and no way to ask OpenCode for one: the session waits, and its first prompt
  // with it, for the claim.
  const asked = hooks["chat.message"]({ sessionID: "ses_late" }, { message: { id: "msg_u" }, parts: [{ type: "text", text: "hi" }] })
  await Bun.sleep(100)
  const began = Date.now()
  await hooks.event(sessionCreated("ses_late"))
  for (let i = 0; i < 200 && !r.terma.calls().some((c) => c.event === "opencode-session-start"); i++) await Bun.sleep(10)
  expect(Date.now() - began).toBeLessThan(1000)
  await asked
  await hooks.dispose()
  expect(r.terma.calls().map((c) => c.event)).toEqual(["opencode-session-start", "opencode-prompt"])
  expect(globalThis[KEY].sessions.get("ses_late").recordArrived).toBeNull()
}, 15_000)

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

const nanosOf = (ms) => String(BigInt(ms) * 1000000n)
const endOf = (span) => Number(BigInt(span.endTimeUnixNano) / 1000000n)

// --- OpenCode 2 ---------------------------------------------------------------------

const FIXTURES2 = join(import.meta.dir, "testdata", "2.4.1")
const SCRIPTED2 = join(import.meta.dir, "testdata", "2.x-scripted")
const CASES2 = [
  "01-plain",
  "02-tools",
  "03-failed-tool",
  "04a-denied-rule",
  "04b-denied-ask",
  "05-mcp",
  "06-write-edit",
  "07-patch",
  "08-commit",
  "09-subagent",
  "10-resume-fork",
  "11-api-error",
  "12-background-service",
  "13-tui-acp",
  "14-serve-compact-rename",
  "20-custom-provider-marked",
]
const SCRIPTED_CASES = ["sub", "retry", "always", "err", "mcp", "codemcp", "compact", "rel", "rm", "svc2"]
const ALL2 = [
  ...CASES2.map((name) => ({ label: "2.4.1/" + name, file: join(FIXTURES2, name, "plugin-view.jsonl"), scripted: false })),
  ...["2.0.0", "2.4.1"].flatMap((v) =>
    [...SCRIPTED_CASES, ...(v === "2.4.1" ? ["svc"] : [])].map((name) => ({ label: `scripted ${v}/${name}`, file: join(SCRIPTED2, v, name, "plugin-view.jsonl"), scripted: true })),
  ),
]

// What the provider billed for each session's title call, from the meta.json beside each
// 2.4.1 capture (fixtures-scrubbed/2.4.1/<case>/meta.json): only sessions that made one.
const TITLE_BILLED = {
  ses_ded4bfaa2ffeqpGY8h75MquWib: 8.315e-5,
  ses_ded4bbd95ffeQwuwKd592iRIN7: 2.642e-5,
  ses_ded4bbd8effe6eH1rEdqSE0qvP: 2.3295e-5,
  ses_ded4bbd8effes3tb2iA6Kz6b2d: 2.1295e-5,
  ses_ded4ba92cffevUuuMeaTaS8nRb: 2.117e-5,
  ses_ded4ba925ffeA2F92KYPtajqWZ: 2.392e-5,
  ses_ded4ba92bffeCu0MmRyX7OgSSd: 2.592e-5,
  ses_ded4b9956ffeSRyHbCtGWF8F78: 2.067e-5,
  ses_ded4b995bffenHDUR989x4pCKL: 2.342e-5,
  ses_ded4b995fffebbZZ4c6XUNhf8B: 2.2045e-5,
  ses_ded4b743fffeaOV9o8IGrXs2xZ: 1.692e-5,
  ses_ded45fc93ffej5GgB17YKuMcEu: 1.792e-5,
  ses_ded44ea2dffeZ7NmeMjR6wjkv2: 1.6795e-5,
  ses_ded3d754cffeLjkOI75RSGVC1S: 1.667e-5,
  ses_ded46a5c2ffePUg7NLlH4iMao5: 1.6795e-5,
}

// The hooks the OpenCode 2 entry registers; the cases also hold two it does not.
const HOOKS2 = new Set(["session.prompt", "session.title", "session.compaction", "session.generate", "tool.execute.before", "tool.execute.after"])
const ZERO_TOKENS = { input: 0, output: 0, reasoning: 0, cache: { read: 0, write: 0 } }

// eventStream is ctx.event.subscribe(): push resolves once the plugin has handled the
// event (it asks for the next one only then), so hooks and events keep the order the test
// gives them.
function eventStream() {
  const queue = []
  const handled = []
  let wake = null
  let subscribed = false
  let ended = false
  const settle = () => {
    for (const done of handled.splice(0)) done()
  }
  return {
    subscribe() {
      subscribed = true
      return {
        [Symbol.asyncIterator]() {
          return {
            next() {
              settle()
              if (queue.length > 0) {
                const { value, done } = queue.shift()
                handled.push(done)
                return Promise.resolve({ value, done: false })
              }
              if (ended) return Promise.resolve({ value: undefined, done: true })
              return new Promise((r) => {
                wake = r
              })
            },
            return() {
              ended = true
              settle()
              return Promise.resolve({ value: undefined, done: true })
            },
          }
        },
      }
    },
    push(value) {
      if (!subscribed || ended) return Promise.resolve()
      return new Promise((done) => {
        if (wake) {
          const w = wake
          wake = null
          handled.push(done)
          w({ value, done: false })
        } else {
          queue.push({ value, done })
        }
      })
    },
    end() {
      ended = true
      settle()
      if (wake) {
        const w = wake
        wake = null
        w({ value: undefined, done: true })
      }
    },
  }
}

// fakeCtx2 is what OpenCode 2 hands setup: its hooks are kept by name ("session.prompt").
function fakeCtx2({ version = "2.4.1", directory = "/repo", get } = {}) {
  const events = eventStream()
  const hooks = new Map()
  const domain = (name) => ({
    hook: async (hook, fn) => {
      hooks.set(name + "." + hook, fn)
    },
  })
  return {
    ctx: {
      app: { name: "cli", version, channel: "latest" },
      location: { directory },
      event: { subscribe: () => events.subscribe() },
      session: { ...domain("session"), get: async ({ sessionID }) => (get ? get(sessionID) : null) },
      tool: domain("tool"),
      permission: domain("permission"),
    },
    hooks,
    event: (e) => events.push(e),
    hook: async (name, input) => hooks.get(name)?.(input),
    end: () => events.end(),
  }
}

// Every console.error the plugin writes while fn runs: on OpenCode 2 that is its log.
async function errorsDuring(fn) {
  const errors = []
  const saved = console.error
  console.error = (...a) => errors.push(a.join(" "))
  try {
    return { result: await fn(), errors }
  } finally {
    console.error = saved
  }
}

function readCase2(file) {
  return readFileSync(file, "utf8")
    .split("\n")
    .filter(Boolean)
    .map((l) => JSON.parse(l))
}

const sessionOf2 = (rec) => (rec.kind === "event" ? rec.data.data?.sessionID : rec.data?.sessionID)

// answers2 answers ctx.session.get as OpenCode did at a point of the case. A scripted case
// has OpenCode's real answers: the latest at or before that point, else the next. A real
// case has none, so the answer is built in the measured shape from the case itself.
function answers2(records, scripted) {
  if (scripted) {
    return (id, cursor) => {
      const isMine = (r) => r.kind === "lookup" && r.data.sessionID === id && r.data.rec
      for (let i = cursor; i >= 0; i--) if (isMine(records[i])) return structuredClone(records[i].data.rec)
      for (let i = cursor + 1; i < records.length; i++) if (isMine(records[i])) return structuredClone(records[i].data.rec)
      return null
    }
  }
  return (id, cursor) => {
    const created = records.find((r) => r.kind === "event" && r.name === "session.created" && r.data.data.sessionID === id)
    const setup = records.find((r) => r.kind === "probe" && r.pid === records[cursor].pid)
    let title
    let usage
    for (let i = 0; i < cursor; i++) {
      const r = records[i]
      if (r.kind !== "event" || r.data.data?.sessionID !== id) continue
      if (r.name === "session.renamed") title = r.data.data.title
      if (r.name === "session.usage.updated") usage = r.data.data
    }
    const rec = {
      id,
      projectID: "prj",
      cost: usage?.cost ?? 0,
      tokens: structuredClone(usage?.tokens ?? ZERO_TOKENS),
      time: { created: created?.data.created ?? 0, updated: 0 },
      location: { directory: created?.data.data.location?.directory ?? setup.data.directory },
    }
    if (created?.data.data.parentID) {
      rec.parentID = created.data.data.parentID
      rec.agent = created.data.data.agent
    }
    if (title) rec.title = title
    return rec
  }
}

// replayProcess2 runs one OpenCode 2 process of a case: one copy of the plugin per setup
// call (as OpenCode evaluates one per directory), the k-th delivery of an event to the k-th
// copy, a hook to the copy of its session's directory.
// How long after a prompt a paced replay keeps the session's own pace.
const PACED_MS = 150

async function replayProcess2(r, items, answer, { paced = false } = {}) {
  let cursor = 0
  const copies = []
  const delivered = new Map()
  const before = r.terma.calls().length
  // The plugin's clock is the capture's: each record arrives at the moment the probe saw it.
  // The plugin's own times (when a prompt was asked, when a claim was done) are then the
  // session's, beside OpenCode's. Two ways to keep it:
  // - by default the clock stands still between records and the plugin finishes with each
  //   before the next comes, so terma costs no time and every time out is OpenCode's own;
  // - paced, the records after a prompt arrive as far apart as they did in OpenCode and the
  //   clock runs between them, so a claim that takes a while takes it in the session's time.
  const realNow = Date.now
  let anchor = 0
  let anchoredAt = realNow()
  let pacedUntil = -Infinity
  Date.now = () => anchor + (paced ? realNow() - anchoredAt : 0)
  try {
    for (const { index, rec } of items) {
      const at = typeof rec.at === "number" ? rec.at : Date.parse(rec.at)
      if (Number.isFinite(at)) {
        if (paced && at <= pacedUntil) while (Date.now() < at) await new Promise((done) => setTimeout(done, 1))
        // Past that window the plugin finishes what it has, in the session's time, before the
        // clock jumps to the next record.
        else if (paced) await quiet2()
        anchor = Math.max(at, Date.now())
        anchoredAt = realNow()
        if (rec.kind === "hook" && rec.name === "session.prompt") pacedUntil = at + PACED_MS
      }
      await replayRecord2(r, rec, index, answer, copies, delivered, (i) => (cursor = i), () => cursor)
      if (!paced) await quiet2()
    }
    // The process closes.
    for (const c of copies) await c.cleanup()
    for (const c of copies) c.oc.end()
  } finally {
    Date.now = realNow
  }
  return { pid: items[0].rec.pid, hooks: r.terma.calls().slice(before) }
}

// quiet2 waits until the plugin has nothing of any session queued or running.
async function quiet2() {
  const st = globalThis[KEY]
  for (let i = 0; st && i < 200; i++) {
    const busy = [...st.sessions.values()].filter((s) => s.busy > 0)
    const lanes = [...st.hooks.running]
    if (busy.length === 0 && lanes.length === 0) return
    await Promise.all([...busy.map((s) => s.queue), ...lanes.map((l) => l.hookQueue)])
  }
}

async function replayRecord2(r, rec, index, answer, copies, delivered, setCursor, cursorNow) {
  {
    setCursor(index)
    const cursor = index
    if (rec.kind === "probe") {
      const mod = await load(r.config, { keep: copies.length > 0 })
      const oc = fakeCtx2({ version: rec.data.app.version, directory: rec.data.directory, get: (id) => answer(id, cursorNow()) })
      copies.push({ directory: rec.data.directory, oc, cleanup: await mod.default.setup(oc.ctx) })
    } else if (copies.length === 0) {
      return
    } else if (rec.kind === "event") {
      const k = delivered.get(rec.data.id) ?? 0
      delivered.set(rec.data.id, k + 1)
      await copies[Math.min(k, copies.length - 1)].oc.event(structuredClone(rec.data))
    } else if (rec.kind === "hook") {
      const dir = answer(rec.data.sessionID, cursor)?.location?.directory
      await (copies.find((c) => c.directory === dir) ?? copies[0]).oc.hook(rec.name, structuredClone(rec.data))
    }
  }
}

async function replay2(c, { paced = false, terma } = {}) {
  const records = readCase2(c.file)
  const answer = answers2(records, c.scripted)
  const r = rig({}, terma)
  const byPid = new Map()
  records.forEach((rec, index) => {
    if (!byPid.has(rec.pid)) byPid.set(rec.pid, [])
    byPid.get(rec.pid).push({ index, rec })
  })
  const processes = []
  const { errors } = await errorsDuring(async () => {
    for (const items of byPid.values()) {
      const spansBefore = r.spans().length
      const logsBefore = r.logs().length
      const p = await replayProcess2(r, items, answer, { paced })
      processes.push({ ...p, items, spans: r.spans().slice(spansBefore), logs: r.logs().slice(logsBefore) })
    }
  })
  return { records, answer, processes, spans: r.spans(), logs: r.logs(), hooks: r.terma.calls(), warnings: errors }
}

const replays2 = new Map()
function replayed2(label) {
  const c = ALL2.find((x) => x.label === label)
  if (!replays2.has(label)) replays2.set(label, replay2(c))
  return replays2.get(label)
}

// Facts of a case, read from its records.
const events2 = (records, ...types) => records.filter((r) => r.kind === "event" && types.includes(r.name))
const hooks2 = (records, name) => records.filter((r) => r.kind === "hook" && r.name === name)
const distinct = (records) => [...new Map(records.map((r) => [r.data.id, r])).values()]

// The model calls a process can report: steps that ended there and whose start it saw. The
// start is the step's last attempt.
function stepCalls(items) {
  const out = new Map()
  const starts = new Map()
  for (const { rec } of items) {
    if (rec.kind !== "event") continue
    const d = rec.data.data
    if (rec.name === "session.step.started") starts.set(d.assistantMessageID, rec.data)
    if ((rec.name === "session.step.ended" || rec.name === "session.step.failed") && !out.has(d.assistantMessageID) && starts.has(d.assistantMessageID)) {
      out.set(d.assistantMessageID, { start: starts.get(d.assistantMessageID), end: rec.data, failed: rec.name === "session.step.failed" })
    }
  }
  return out
}
const sessionsWithParent = (records) => {
  const out = new Map()
  for (const r of events2(records, "session.created")) if (r.data.data.parentID) out.set(r.data.data.sessionID, r.data.data.parentID)
  return out
}

describe("every 2.x case", () => {
  for (const c of ALL2) {
    test(
      c.label,
      async () => {
        const res = await replayed2(c.label)
        expect(res.spans.length + res.logs.length).toBeGreaterThan(0)
        const versions = new Map(res.processes.map((p) => [p.pid, p.items.find((x) => x.rec.kind === "probe").rec.data.app.version]))
        for (const p of res.processes) {
          for (const item of [...p.spans, ...p.logs]) {
            expect(item.scope).toEqual({ name: "terma-opencode", version: "2" })
            const a = A(item)
            expect(a["session.id"]).toMatch(/^ses_/)
            expect(a["opencode.version"]).toBe(versions.get(p.pid))
            for (const key of RETIRED_KEYS) expect(a[key]).toBeUndefined()
            expect(RETIRED_EVENTS).not.toContain(a["event.name"])
          }
        }
        const ids = res.spans.map((s) => s.spanId)
        expect(new Set(ids).size).toBe(ids.length)

        // Each process's first hook for a session is its claim, from the session's own directory.
        for (const p of res.processes) {
          const firstSeen = new Map()
          for (const { index, rec } of p.items) {
            const sid = sessionOf2(rec)
            const handled = (rec.kind === "event" && rec.name !== "session.deleted") || (rec.kind === "hook" && HOOKS2.has(rec.name))
            if (sid && handled && !firstSeen.has(sid)) firstSeen.set(sid, index)
          }
          const first = new Map()
          for (const h of p.hooks) if (!first.has(h.payload.session_id)) first.set(h.payload.session_id, h)
          for (const [sid, h] of first) {
            expect(h.event).toBe("opencode-session-start")
            expect(h.payload.cwd).toBe(res.answer(sid, firstSeen.get(sid)).location.directory)
          }
        }
        // A move claims the session in its new directory with a prompt hook of its own.
        const moves = distinct(events2(res.records, "session.moved")).filter((m) => m.data.data.location?.directory && m.data.data.location.directory !== m.data.location.directory)
        expect(res.hooks.filter((h) => h.event === "opencode-prompt").length).toBe(hooks2(res.records, "session.prompt").length + moves.length)
        expect(named(res.logs, "opencode.user_prompt").length).toBe(hooks2(res.records, "session.prompt").length)
        const ends = distinct(events2(res.records, "session.execution.succeeded", "session.execution.failed", "session.execution.interrupted"))
        expect(res.hooks.filter((h) => h.event === "opencode-idle").length).toBe(ends.length)
        expect(res.warnings).toEqual([])

        // Calls: one chat span per step, with OpenCode's own identity, usage and times.
        const calls = chats(res.spans).filter((s) => !kindOf(s))
        const expected = new Map(res.processes.flatMap((p) => [...stepCalls(p.items)]))
        expect(calls.map((s) => A(s)["opencode.call.id"]).sort()).toEqual([...expected.keys()].sort())
        for (const span of calls) {
          const a = A(span)
          const { start, end, failed } = expected.get(a["opencode.call.id"])
          expect(span.startTimeUnixNano).toBe(nanosOf(start.data.started ?? start.created))
          expect(span.endTimeUnixNano).toBe(nanosOf(end.created))
          expect(span.status).toEqual(failed ? { code: 2 } : { code: 1 })
          const t = end.data.tokens
          if (failed) {
            expect(a["error.type"]).toBe(end.data.error.type)
            if (!t) for (const key of Object.keys(a)) expect(key).not.toMatch(/usage/)
          } else {
            expect(a["gen_ai.usage.input_tokens"]).toBe(t.input)
            expect(a["gen_ai.usage.output_tokens"]).toBe(t.output)
            expect(a["opencode.usage.reasoning_tokens"]).toBe(t.reasoning)
            expect(a["gen_ai.usage.cache_read.input_tokens"]).toBe(t.cache.read)
            expect(a["gen_ai.usage.cache_write.input_tokens"]).toBe(t.cache.write)
            expect(a["gen_ai.usage.total_cost"]).toBe(end.data.cost)
            expect(a["gen_ai.response.finish_reasons"]).toBe(end.data.finish)
          }
        }

        // A reader that orders a session by time finds every call under the prompt that asked
        // for it: no span of a turn, and no title call a prompt started, begins before the
        // prompt's own time. The plugin's clock here is the capture's (replayProcess2).
        const ms = (ns) => Number(BigInt(ns) / 1000000n)
        for (const record of named(res.logs, "opencode.user_prompt")) {
          const id = A(record)["opencode.message.id"]
          const mine = res.spans.filter((sp) => sessionOf(sp) === sessionOf(record) && (A(sp)["opencode.parent_message.id"] === id || A(sp)["opencode.call.id"] === "title:" + id))
          for (const sp of mine) expect(ms(sp.startTimeUnixNano)).toBeGreaterThanOrEqual(ms(record.timeUnixNano))
          // And the prompt carries the moment it was asked: the probe saw the hook then.
          const hook = hooks2(res.records, "session.prompt").find((h) => h.data.messageID === id)
          const asked = typeof hook.at === "number" ? hook.at : Date.parse(hook.at)
          if (!hook.data.prompt.files?.length) expect(ms(record.timeUnixNano) - asked).toBeLessThan(50)
        }

        const again = await replay2(c)
        expect(identity(again)).toEqual(identity(res))
      },
      REPLAY_TIMEOUT,
    )
  }
})

// The fault this guards against was seen in a real session: terma's claim took about 20 ms,
// OpenCode started the first step inside that time, and the prompt, stamped once the claim
// was done, came out later than the call it had started.
for (const label of ["2.4.1/01-plain", "2.4.1/02-tools", "2.4.1/09-subagent", "scripted 2.0.0/sub"]) {
  test(
    `${label}, at the session's own pace with a claim that takes a while: every prompt comes before the calls it starts`,
    async () => {
      const c = ALL2.find((x) => x.label === label)
      const res = await replay2(c, { paced: true, terma: { delayed: { "opencode-session-start": 0.04 } } })
      const ms = (ns) => Number(BigInt(ns) / 1000000n)
      const prompts = named(res.logs, "opencode.user_prompt")
      expect(prompts.length).toBeGreaterThan(0)
      let held = 0
      for (const record of prompts) {
        const id = A(record)["opencode.message.id"]
        const mine = res.spans.filter((sp) => sessionOf(sp) === sessionOf(record) && (A(sp)["opencode.parent_message.id"] === id || A(sp)["opencode.call.id"] === "title:" + id))
        expect(mine.length).toBeGreaterThan(0)
        for (const sp of mine) {
          expect(ms(sp.startTimeUnixNano)).toBeGreaterThanOrEqual(ms(record.timeUnixNano))
          expect(ms(sp.endTimeUnixNano)).toBeGreaterThanOrEqual(ms(sp.startTimeUnixNano))
        }
        // The claim did hold this prompt back: it is later than the moment it was asked.
        const hook = hooks2(res.records, "session.prompt").find((h) => h.data.messageID === id)
        if (ms(record.timeUnixNano) - (typeof hook.at === "number" ? hook.at : Date.parse(hook.at)) >= 40) held++
      }
      expect(held).toBeGreaterThan(0)
      // Usage is OpenCode's own, as in the unpaced replay, and so is every end, except that of
      // a call that ended while its session was still being claimed: that one is held to its
      // prompt. Which calls those are depends on how long the claim took on this machine (the
      // subagent's call in 2.0.0/sub ends 50 ms after its prompt), so the rule is asserted
      // whichever way it fell.
      const plain = await replayed2(label)
      const stamps = new Map(prompts.map((r) => [sessionOf(r) + " " + A(r)["opencode.message.id"], BigInt(r.timeUnixNano)]))
      const idOf = (sp) => A(sp)["opencode.call.id"] ?? A(sp)["gen_ai.tool.call.id"]
      const facts = (spans, end) => spans.map((sp) => [idOf(sp), end(sp), A(sp)["gen_ai.usage.input_tokens"], A(sp)["gen_ai.usage.total_cost"]]).filter((f) => !String(f[0]).startsWith("title:")).sort()
      const heldEnd = (sp) => {
        const stamp = stamps.get(sessionOf(sp) + " " + A(sp)["opencode.parent_message.id"])
        return stamp !== undefined && BigInt(sp.endTimeUnixNano) < stamp ? String(stamp) : sp.endTimeUnixNano
      }
      expect(facts(res.spans, (sp) => sp.endTimeUnixNano)).toEqual(facts(plain.spans, heldEnd))
    },
    REPLAY_TIMEOUT,
  )
}

// --- per 2.x case -------------------------------------------------------------------

test("2.4.1 01-plain: the prompt, the title, and the title call with exactly its own usage", async () => {
  const res = await replayed2("2.4.1/01-plain")
  const [p] = hooks2(res.records, "session.prompt")
  expect(named(res.logs, "opencode.user_prompt").map((r) => A(r)["opencode.message.id"])).toEqual([p.data.messageID])
  const [renamed] = events2(res.records, "session.renamed")
  const titles = named(res.logs, "opencode.session.title")
  expect(titles.map((r) => r.body.stringValue)).toEqual([renamed.data.data.title])
  expect(titles[0].timeUnixNano).toBe(nanosOf(renamed.data.created))
  const titleCalls = chats(res.spans).filter((s) => kindOf(s) === "title")
  expect(titleCalls.length).toBe(1)
  const t = A(titleCalls[0])
  expect(t["opencode.call.id"]).toBe("title:" + p.data.messageID)
  expect(t["opencode.agent"]).toBe("title")
  expect(t["opencode.parent_message.id"]).toBeUndefined()
  expect(t["gen_ai.usage.input_tokens"]).toBe(4)
  expect(t["gen_ai.usage.cache_write.input_tokens"]).toBe(626)
  expect(t["gen_ai.usage.output_tokens"]).toBe(9)
  expect(t["gen_ai.usage.total_cost"]).toBe(0.00008315)
  expect(titleCalls[0].endTimeUnixNano).toBe(nanosOf(renamed.data.created))
  const [call] = chats(res.spans).filter((s) => !kindOf(s))
  expect(A(call)["opencode.parent_message.id"]).toBe(p.data.messageID)
  expect(A(call)["gen_ai.completion"]).toBe("PLAIN2-OK")
}, REPLAY_TIMEOUT)

test("2.4.1: every title call costs what the provider billed for it", async () => {
  let checked = 0
  for (const name of CASES2) {
    const res = await replayed2("2.4.1/" + name)
    for (const span of chats(res.spans).filter((s) => kindOf(s) === "title")) {
      const billed = TITLE_BILLED[sessionOf(span)]
      if (billed === undefined) continue
      expect([name, A(span)["gen_ai.usage.total_cost"]]).toEqual([name, billed])
      checked++
    }
  }
  expect(checked).toBe(Object.keys(TITLE_BILLED).length)
}, REPLAY_TIMEOUT * 4)

test("2.4.1 11-api-error: one failed call with no usage; its title call is sent at close, without usage", async () => {
  const res = await replayed2("2.4.1/11-api-error")
  const calls = chats(res.spans).filter((s) => !kindOf(s))
  expect(calls.length).toBe(1)
  const a = A(calls[0])
  expect(calls[0].status).toEqual({ code: 2 })
  expect(a["error.type"]).toBe("provider.invalid-request")
  expect(a["opencode.error.message"]).toContain("is not a valid model ID")
  for (const key of Object.keys(a)) expect(key).not.toMatch(/usage|completion|finish/)
  const titleCalls = chats(res.spans).filter((s) => kindOf(s) === "title")
  expect(titleCalls.length).toBe(1)
  for (const key of Object.keys(A(titleCalls[0]))) expect(key).not.toMatch(/usage/)
  expect(named(res.logs, "opencode.session.title")).toEqual([])
}, REPLAY_TIMEOUT)

test("scripted 2.4.1 err: a title whose usage comes right after a failed step still gets it", async () => {
  const res = await replayed2("scripted 2.4.1/err")
  const failed = events2(res.records, "session.step.failed")
  expect(failed.length).toBe(2)
  const titleCalls = chats(res.spans).filter((s) => kindOf(s) === "title")
  expect(titleCalls.length).toBe(1)
  const t = A(titleCalls[0])
  expect(t["gen_ai.usage.input_tokens"]).toBe(30)
  expect(t["gen_ai.usage.output_tokens"]).toBe(5)
  expect(chats(res.spans).filter((s) => !kindOf(s)).every((s) => s.status.code === 2)).toBe(true)
}, REPLAY_TIMEOUT)

for (const v of ["2.0.0", "2.4.1"]) {
  test(`scripted ${v} retry and always: a retried step is one call`, async () => {
    const retry = await replayed2(`scripted ${v}/retry`)
    const starts = new Map()
    for (const r of events2(retry.records, "session.step.started")) starts.set(r.data.data.assistantMessageID, (starts.get(r.data.data.assistantMessageID) ?? 0) + 1)
    expect([...starts.values()].filter((n) => n > 1).length).toBe(2)
    const calls = chats(retry.spans).filter((s) => !kindOf(s))
    expect(calls.map((s) => A(s)["opencode.call.id"]).sort()).toEqual([...starts.keys()].sort())

    const always = await replayed2(`scripted ${v}/always`)
    const failed = chats(always.spans).filter((s) => !kindOf(s))
    // 2.0.0 gave up after five attempts; the 2.4.1 capture ended while it still retried.
    expect(failed.length).toBe(v === "2.0.0" ? 1 : 0)
    if (failed.length > 0) {
      expect(failed[0].status).toEqual({ code: 2 })
      expect(A(failed[0])["error.type"]).toBe("provider.rate-limit")
    }
  }, REPLAY_TIMEOUT)
}

test("2.4.1 03-failed-tool: exit 3 is a completed call with its exit code", async () => {
  const res = await replayed2("2.4.1/03-failed-tool")
  const shell = tools(res.spans).filter((s) => A(s)["opencode.tool.exit_code"] === 3)
  expect(shell.length).toBe(1)
  expect(A(shell[0])["opencode.tool.status"]).toBe("completed")
  expect(A(shell[0])["gen_ai.tool.name"]).toBe("shell")
}, REPLAY_TIMEOUT)

for (const [name, denial] of [["04a-denied-rule", "rule"], ["04b-denied-ask", "ask"]]) {
  test(`2.4.1 ${name}: a call that never ran is one denied span`, async () => {
    const res = await replayed2("2.4.1/" + name)
    const [failed] = events2(res.records, "session.tool.failed")
    const spans = tools(res.spans)
    expect(spans.length).toBe(1)
    const a = A(spans[0])
    expect(a["gen_ai.tool.call.id"]).toBe(failed.data.data.assistantMessageID + ":" + failed.data.data.id)
    expect(a["opencode.tool.status"]).toBe("denied")
    expect(a["opencode.tool.denial"]).toBe(denial)
    expect(a["gen_ai.tool.call.result"]).toBe(failed.data.data.error.message)
    expect(spans[0].status).toEqual({ code: 2 })
  }, REPLAY_TIMEOUT)
}

for (const [name, tool, expectedFiles] of [
  ["06-write-edit", null, ["/home/user/work/oc-files/new.txt", "/home/user/work/oc-files/notes.txt"]],
  ["07-patch", "patch", ["/home/user/work/oc-patch/notes.txt"]],
]) {
  test(`2.4.1 ${name}: absolute files, and one file-edit hook per completed edit`, async () => {
    const res = await replayed2("2.4.1/" + name)
    const edits = tools(res.spans).filter((s) => ["write", "edit", "patch"].includes(A(s)["gen_ai.tool.name"]))
    if (tool) expect(edits.map((s) => A(s)["gen_ai.tool.name"])).toEqual([tool])
    expect(edits.flatMap((s) => A(s)["opencode.tool.files"]).sort()).toEqual(expectedFiles)
    const hooks = res.hooks.filter((h) => h.event === "opencode-file-edit")
    expect(hooks.length).toBe(edits.length)
    for (const span of edits) {
      const a = A(span)
      const hook = hooks.find((h) => h.payload.tool_call_id === a["gen_ai.tool.call.id"])
      expect(hook.payload.tool).toBe(a["gen_ai.tool.name"])
      expect(hook.payload.files).toEqual(a["opencode.tool.files"])
    }
  }, REPLAY_TIMEOUT)
}

for (const v of ["2.0.0", "2.4.1"]) {
  test(`scripted ${v} rel: relative paths resolve in the subdirectory the session runs in`, async () => {
    const res = await replayed2(`scripted ${v}/rel`)
    const dir = "/home/user/work/rel/pkg/sub"
    for (const h of res.hooks) expect(h.payload.cwd).toBe(dir)
    const spans = tools(res.spans)
    const files = spans.flatMap((s) => A(s)["opencode.tool.files"] ?? [])
    expect(files.length).toBeGreaterThan(0)
    for (const f of files) expect(f.startsWith(dir + "/")).toBe(true)
    const write = spans.find((s) => A(s)["gen_ai.tool.name"] === "write")
    expect(A(write)["opencode.tool.files"]).toEqual([dir + "/rel-new.txt"])
    // Only the edits that completed report a file edit.
    const completedEdits = spans.filter((s) => ["write", "edit", "patch"].includes(A(s)["gen_ai.tool.name"]) && A(s)["opencode.tool.status"] === "completed")
    const failedEdits = spans.filter((s) => ["write", "edit", "patch"].includes(A(s)["gen_ai.tool.name"]) && A(s)["opencode.tool.status"] !== "completed")
    expect(failedEdits.length).toBeGreaterThan(0)
    const hooks = res.hooks.filter((h) => h.event === "opencode-file-edit")
    expect(hooks.map((h) => h.payload.tool_call_id).sort()).toEqual(completedEdits.map((s) => A(s)["gen_ai.tool.call.id"]).sort())
  }, REPLAY_TIMEOUT)
}

test("2.4.1 05-mcp: the execute call from the events, each MCP call in it from the hook", async () => {
  const res = await replayed2("2.4.1/05-mcp")
  const byName = Object.fromEntries(tools(res.spans).map((s) => [A(s)["gen_ai.tool.name"], s]))
  expect(Object.keys(byName).sort()).toEqual(["capture_echo", "capture_fail", "execute"])
  expect(A(byName.execute)["opencode.mcp.server"]).toBeUndefined()
  const echo = A(byName.capture_echo)
  expect(echo["opencode.mcp.server"]).toBe("capture")
  expect(echo["opencode.mcp.tool"]).toBe("echo")
  expect(echo["gen_ai.tool.call.result"]).toBe("MCP-CANARY")
  expect(echo["opencode.tool.status"]).toBe("completed")
  const fail = A(byName.capture_fail)
  expect(fail["opencode.mcp.server"]).toBe("capture")
  expect(fail["opencode.mcp.tool"]).toBe("fail")
  expect(fail["opencode.tool.status"]).toBe("error")
  expect(fail["gen_ai.tool.call.result"]).toBe("capture MCP failure on purpose")
  expect(byName.capture_fail.status).toEqual({ code: 2 })
  // Each is in the step's turn.
  const [p] = hooks2(res.records, "session.prompt")
  for (const s of Object.values(byName)) expect(A(s)["opencode.parent_message.id"]).toBe(p.data.messageID)
}, REPLAY_TIMEOUT)

test("scripted 2.4.1 mcp and codemcp: a repaired call is one span with its MCP names", async () => {
  const res = await replayed2("scripted 2.4.1/mcp")
  const spans = tools(res.spans)
  expect(spans.length).toBe(1)
  const a = A(spans[0])
  expect(a["gen_ai.tool.name"]).toBe("capture_echo")
  expect(a["opencode.mcp.server"]).toBe("capture")
  expect(a["opencode.mcp.tool"]).toBe("echo")
  expect(a["opencode.tool.status"]).toBe("completed")

  const code = await replayed2("scripted 2.4.1/codemcp")
  const names = tools(code.spans).map((s) => [A(s)["gen_ai.tool.name"], A(s)["opencode.mcp.server"], A(s)["opencode.mcp.tool"]])
  expect(names.sort()).toEqual([["capture_echo", "capture", "echo"], ["capture_fail", "capture", "fail"], ["execute", undefined, undefined]])
}, REPLAY_TIMEOUT)

test("scripted 2.0.0 mcp and codemcp: spans from the events, and no MCP names", async () => {
  for (const name of ["mcp", "codemcp"]) {
    const res = await replayed2("scripted 2.0.0/" + name)
    const spans = tools(res.spans)
    expect(spans.length).toBe(1)
    for (const s of spans) {
      expect(A(s)["opencode.mcp.server"]).toBeUndefined()
      expect(A(s)["opencode.mcp.tool"]).toBeUndefined()
    }
  }
}, REPLAY_TIMEOUT)

for (const label of ["2.4.1/09-subagent", "scripted 2.0.0/sub", "scripted 2.4.1/sub"]) {
  test(`${label}: the child names its parent everywhere`, async () => {
    const res = await replayed2(label)
    const [[child, parent]] = [...sessionsWithParent(res.records)]
    const childItems = [...res.spans, ...res.logs].filter((x) => sessionOf(x) === child)
    expect(childItems.length).toBeGreaterThan(2)
    for (const x of childItems) expect(A(x)["opencode.parent_session.id"]).toBe(parent)
    for (const x of [...res.spans, ...res.logs].filter((x) => sessionOf(x) === parent)) expect(A(x)["opencode.parent_session.id"]).toBeUndefined()
    const launch = tools(res.spans).find((s) => A(s)["gen_ai.tool.name"] === "subagent")
    expect(A(launch)["opencode.tool.subagent_session.id"]).toBe(child)
    expect(named(res.logs, "opencode.session.title").filter((r) => sessionOf(r) === child)).toEqual([])
    for (const h of res.hooks.filter((h) => h.payload.session_id === child)) {
      expect(h.payload.parent_session_id).toBe(parent)
    }
    expect(res.hooks.find((h) => h.payload.session_id === child).event).toBe("opencode-session-start")
    if (res.processes.length > 1) {
      // The child resumed alone in a new process still names its parent, and the parent,
      // first seen there through an event, is claimed from its own record.
      const second = res.processes[1]
      const childCalls = chats(second.spans).filter((s) => sessionOf(s) === child)
      expect(childCalls.length).toBeGreaterThan(0)
      for (const s of childCalls) expect(A(s)["opencode.parent_session.id"]).toBe(parent)
      const claims = second.hooks.filter((h) => h.event === "opencode-session-start")
      expect(claims.find((h) => h.payload.session_id === child)?.payload).toEqual({ session_id: child, cwd: "/home/user/work/sub", parent_session_id: parent })
      // 2.4.1 tells the parent the child finished; 2.0.0 does not, and the parent stays unseen.
      const parentSeen = second.items.some(({ rec }) => sessionOf2(rec) === parent && rec.kind === "event")
      expect(parentSeen).toBe(label.includes("2.4.1"))
      if (parentSeen) expect(claims.find((h) => h.payload.session_id === parent)?.payload).toEqual({ session_id: parent, cwd: "/home/user/work/sub" })
    }
  }, REPLAY_TIMEOUT)
}

test("2.4.1 12-background-service: events delivered to every directory count once; each session claims its own", async () => {
  const res = await replayed2("2.4.1/12-background-service")
  const copies = new Map()
  for (const r of res.records.filter((r) => r.kind === "event")) copies.set(r.data.id, (copies.get(r.data.id) ?? 0) + 1)
  expect([...copies.values()].some((n) => n === 3)).toBe(true)
  expect(named(res.logs, "opencode.session.title").length).toBe(distinct(events2(res.records, "session.renamed")).length)
  expect(tools(res.spans).length).toBe(distinct(events2(res.records, "session.tool.success", "session.tool.failed")).length)
  expect(res.hooks.filter((h) => h.event === "opencode-file-edit").length).toBe(2)
  const claims = res.hooks.filter((h) => h.event === "opencode-session-start")
  expect(claims.length).toBe(new Set(res.hooks.map((h) => h.payload.session_id)).size)
  const dirOf = new Map(claims.map((h) => [h.payload.session_id, h.payload.cwd]))
  for (const h of res.hooks) expect(h.payload.cwd).toBe(dirOf.get(h.payload.session_id))
  expect(new Set(dirOf.values())).toEqual(new Set(["/home/user/work/oc-svc-a", "/home/user/work/oc-svc-b", "/home/user/work/oc-tui"]))
}, REPLAY_TIMEOUT)

test("2.4.1 14-serve-compact-rename: one compaction span, a rename by hand, and usage given to nothing else", async () => {
  const res = await replayed2("2.4.1/14-serve-compact-rename")
  const [started] = events2(res.records, "session.compaction.started")
  const [ended] = events2(res.records, "session.compaction.ended")
  const compactions = chats(res.spans).filter((s) => kindOf(s) === "compaction")
  expect(compactions.length).toBe(1)
  const c = A(compactions[0])
  expect(c["opencode.call.id"]).toBe("compaction:" + started.data.data.inputID)
  expect(c["opencode.compaction.reason"]).toBe("manual")
  expect(c["gen_ai.usage.total_cost"]).toBe(0.000342805)
  expect(c["gen_ai.usage.input_tokens"]).toBe(ended.data.data.tokens.input)
  expect(c["gen_ai.usage.output_tokens"]).toBe(ended.data.data.tokens.output)
  expect(c["gen_ai.usage.cache_read.input_tokens"]).toBe(ended.data.data.tokens.cache.read)
  expect(c["gen_ai.usage.cache_write.input_tokens"]).toBe(ended.data.data.tokens.cache.write)
  expect(c["gen_ai.completion"]).toBe(ended.data.data.text)
  expect(c["gen_ai.completion"]).toStartWith("## Objective")
  expect(c["opencode.parent_message.id"]).toBeUndefined()
  expect(compactions[0].startTimeUnixNano).toBe(nanosOf(started.data.created))
  expect(compactions[0].endTimeUnixNano).toBe(nanosOf(ended.data.created))
  expect(named(res.logs, "opencode.session.title").map((r) => r.body.stringValue)).toEqual(["notes.txt first line", "Renamed by hand RENAME2-CANARY"])
  const titleCalls = chats(res.spans).filter((s) => kindOf(s) === "title")
  expect(titleCalls.length).toBe(1)
  expect(A(titleCalls[0])["gen_ai.usage.total_cost"]).toBe(1.667e-5)
}, REPLAY_TIMEOUT)

test("scripted 2.4.1 compact: a failed compaction is one failed span that keeps what it was billed", async () => {
  const res = await replayed2("scripted 2.4.1/compact")
  const [failed] = events2(res.records, "session.compaction.failed")
  const compactions = chats(res.spans).filter((s) => kindOf(s) === "compaction")
  expect(compactions.length).toBe(1)
  expect(compactions[0].status).toEqual({ code: 2 })
  const c = A(compactions[0])
  expect(c["error.type"]).toBe("compaction.failed")
  expect(c["opencode.compaction.reason"]).toBe("manual")
  expect(c["gen_ai.request.model"]).toBe("m")
  const t = failed.data.data.tokens
  expect(t.input).toBeGreaterThan(0)
  expect(c["gen_ai.usage.input_tokens"]).toBe(t.input)
  expect(c["gen_ai.usage.output_tokens"]).toBe(t.output)
  // Zeros are not a usage.
  expect(failed.data.data.cost).toBe(0)
  expect(c["gen_ai.usage.total_cost"]).toBeUndefined()
  // The title never arrived: its call is sent at close, without usage.
  const titleCalls = chats(res.spans).filter((s) => kindOf(s) === "title")
  expect(titleCalls.length).toBe(1)
  for (const key of Object.keys(A(titleCalls[0]))) expect(key).not.toMatch(/usage/)
}, REPLAY_TIMEOUT)

test("2.4.1 10-resume-fork: each process claims its session and exports only its own steps", async () => {
  const res = await replayed2("2.4.1/10-resume-fork")
  expect(res.processes.length).toBe(4)
  for (const p of res.processes) {
    const [prompt] = p.items.filter((x) => x.rec.kind === "hook" && x.rec.name === "session.prompt")
    expect(p.hooks[0]).toMatchObject({ event: "opencode-session-start", payload: { session_id: prompt.rec.data.sessionID } })
    const own = [...stepCalls(p.items).keys()]
    expect(own.length).toBeGreaterThan(0)
    expect(chats(p.spans).filter((s) => !kindOf(s)).map((s) => A(s)["opencode.call.id"]).sort()).toEqual(own.sort())
  }
  const ids = res.spans.map((s) => s.spanId)
  expect(new Set(ids).size).toBe(ids.length)
  // The resumed processes title the session from its record, once each.
  for (const p of res.processes.slice(1, 3)) expect(named(p.logs, "opencode.session.title").map((r) => r.body.stringValue)).toEqual(["Kiwi word request"])
}, REPLAY_TIMEOUT)

test("scripted 2.4.1 rm: deleting a session ends it", async () => {
  const res = await replayed2("scripted 2.4.1/rm")
  const [deleted] = events2(res.records, "session.deleted")
  const sid = deleted.data.data.sessionID
  const mine = res.hooks.filter((h) => h.payload.session_id === sid).map((h) => h.event)
  expect(mine[0]).toBe("opencode-session-start")
  expect(mine.at(-1)).toBe("opencode-session-end")
  expect(res.hooks.find((h) => h.event === "opencode-session-end").payload).toEqual({ session_id: sid, cwd: "/home/user/work/rm", reason: "deleted" })
}, REPLAY_TIMEOUT)

test("scripted 2.4.1 svc: a queued prompt waits for its delivery; a moved session claims its new directory", async () => {
  const res = await replayed2("scripted 2.4.1/svc")
  const prompts = hooks2(res.records, "session.prompt").map((r) => r.data.messageID)
  const calls = chats(res.spans).filter((s) => !kindOf(s))
  // The second prompt was asked while the first step ran: that step stays in the first turn.
  const [firstStep] = events2(res.records, "session.step.started")
  const first = calls.find((s) => A(s)["opencode.call.id"] === firstStep.data.data.assistantMessageID)
  expect(A(first)["opencode.parent_message.id"]).toBe(prompts[0])
  const toolSpan = tools(res.spans).find((s) => A(s)["gen_ai.tool.call.id"].startsWith(firstStep.data.data.assistantMessageID + ":"))
  expect(A(toolSpan)["opencode.parent_message.id"]).toBe(prompts[0])
  expect(new Set(calls.map((s) => A(s)["opencode.parent_message.id"])).has(prompts[1])).toBe(true)
  // The move: the hooks after it run in the new directory.
  const [moved] = events2(res.records, "session.moved")
  const to = moved.data.data.location.directory
  expect(moved.data.location.directory).not.toBe(to)
  const promptHooks = res.hooks.filter((h) => h.event === "opencode-prompt")
  expect(promptHooks.at(0).payload.cwd).toBe(moved.data.location.directory)
  expect(promptHooks.at(-1).payload.cwd).toBe(to)
  // The rename by hand is a title like any other.
  expect(named(res.logs, "opencode.session.title").map((r) => r.body.stringValue)).toContain("Renamed by hand")
}, REPLAY_TIMEOUT)

// --- 2.x unit -----------------------------------------------------------------------

let evt2 = 0
const sid2 = "ses_two"
// One OpenCode 2 event for the session, at OpenCode time `created`.
const e2 = (type, created, data = {}, directory = "/repo") => ({ id: "evt_unit" + ++evt2, created, type, location: { directory }, data: { sessionID: sid2, ...data } })
const record2 = (extra = {}) => ({ id: sid2, projectID: "prj", cost: 0, tokens: structuredClone(ZERO_TOKENS), time: { created: 1000, updated: 1000 }, location: { directory: "/repo" }, ...extra })

async function start2(overrides, { terma, get = (id) => (id === sid2 ? record2() : null), version = "2.4.1", directory = "/repo", source } = {}) {
  const r = rig(overrides, terma)
  const mod = await load(r.config, source ? { source } : {})
  const oc = fakeCtx2({ version, directory, get })
  const cleanup = await mod.default.setup(oc.ctx)
  return {
    r,
    oc,
    close: async () => {
      await cleanup()
      oc.end()
    },
  }
}

const step2 = (oc, id, { start, end, turnText, tokens = { input: 1, output: 2, reasoning: 0, cache: { read: 0, write: 0 } }, cost = 0.001 } = {}) => [
  () => oc.event(e2("session.step.started", start, { assistantMessageID: id, agent: "build", model: { id: "m1", providerID: "p1" }, started: start })),
  ...(turnText ? [() => oc.event(e2("session.text.ended", end - 1, { assistantMessageID: id, ordinal: 0, text: turnText }))] : []),
  () => oc.event(e2("session.step.ended", end, { assistantMessageID: id, finish: "stop", cost, tokens })),
]
async function each(fns) {
  for (const fn of fns) await fn()
}

test("setup registers its hooks, returns a cleanup, and a build without one hook keeps the rest", async () => {
  const r = rig()
  const mod = await load(r.config)
  const oc = fakeCtx2({ get: (id) => record2({ id }) })
  oc.ctx.tool.hook = async () => {
    throw new Error("no tool hooks in this build")
  }
  const cleanup = await mod.default.setup(oc.ctx)
  expect(typeof cleanup).toBe("function")
  expect([...oc.hooks.keys()].sort()).toEqual(["session.compaction", "session.generate", "session.prompt", "session.title"])
  expect(await oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_u", prompt: { text: "hi" } })).toBeUndefined()
  await cleanup()
  oc.end()
  expect(named(r.logs(), "opencode.user_prompt").length).toBe(1)
})

test("a prompt queued while a step runs: that step stays in its own turn", async () => {
  const { r, oc, close } = await start2()
  await oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_A", prompt: { text: "first" } })
  await oc.event(e2("session.inbox.delivered", 1100, { inboxID: "msg_A" }))
  await oc.event(e2("session.step.started", 1200, { assistantMessageID: "msg_s1", agent: "build", model: { id: "m1", providerID: "p1" }, started: 1200 }))
  // Prompt B is asked now and delivered only after step 1 ended.
  await oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_B", prompt: { text: "second" } })
  await oc.event(e2("session.tool.input.started", 1300, { assistantMessageID: "msg_s1", id: "c1", name: "glob" }))
  await oc.event(e2("session.tool.called", 1310, { assistantMessageID: "msg_s1", id: "c1", input: { pattern: "*.txt" } }))
  await oc.event(e2("session.tool.success", 1320, { assistantMessageID: "msg_s1", id: "c1", content: [{ type: "text", text: "a.txt" }] }))
  await oc.event(e2("session.step.ended", 1400, { assistantMessageID: "msg_s1", finish: "tool-calls", cost: 0.001, tokens: ZERO_TOKENS }))
  await oc.event(e2("session.inbox.delivered", 1500, { inboxID: "msg_B" }))
  await each(step2(oc, "msg_s2", { start: 1600, end: 1700 }))
  await close()
  const turn = Object.fromEntries([...chats(r.spans()), ...tools(r.spans())].map((s) => [A(s)["opencode.call.id"] ?? A(s)["gen_ai.tool.call.id"], A(s)["opencode.parent_message.id"]]))
  expect(turn).toEqual({ msg_s1: "msg_A", "msg_s1:c1": "msg_A", msg_s2: "msg_B" })
})

test("a turn the harness starts itself stays in the last prompt's turn", async () => {
  const { r, oc, close } = await start2()
  await oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_A", prompt: { text: "first" } })
  await oc.event(e2("session.inbox.delivered", 1100, { inboxID: "msg_A" }))
  await each(step2(oc, "msg_s1", { start: 1200, end: 1300 }))
  // A background result is delivered: it is no prompt.
  await oc.event(e2("session.inbox.delivered", 1400, { inboxID: "msg_background" }))
  await each(step2(oc, "msg_s2", { start: 1500, end: 1600 }))
  await close()
  expect(chats(r.spans()).map((s) => A(s)["opencode.parent_message.id"])).toEqual(["msg_A", "msg_A"])
})

describe("a rise in usage is the title call's only when nothing else explains it", () => {
  const usage = (created, input, cost) => e2("session.usage.updated", created, { cost, tokens: { input, output: 0, reasoning: 0, cache: { read: 0, write: 0 } } })
  async function titled(between) {
    const { r, oc, close } = await start2()
    await oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_A", prompt: { text: "hi" } })
    await oc.hook("session.title", { sessionID: sid2, model: { id: "m-title", providerID: "p1" } })
    await between(oc)
    await oc.event(e2("session.renamed", 2000, { title: "A real title" }))
    await close()
    const calls = chats(r.spans()).filter((s) => kindOf(s) === "title")
    expect(calls.length).toBe(1)
    expect(A(calls[0])["opencode.call.id"]).toBe("title:msg_A")
    return A(calls[0])
  }
  test("its own update, directly before the rename", async () => {
    const t = await titled((oc) => oc.event(usage(1900, 7, 0.0003)))
    expect(t["gen_ai.usage.input_tokens"]).toBe(7)
    expect(t["gen_ai.usage.total_cost"]).toBe(0.0003)
  })
  test("not a compaction's", async () => {
    const t = await titled(async (oc) => {
      await oc.event(e2("session.compaction.started", 1800, { reason: "auto", inputID: "msg_c" }))
      await oc.event(usage(1900, 7, 0.0003))
    })
    for (const key of Object.keys(t)) expect(key).not.toMatch(/usage/)
  })
  test("not a step's: the rise that is exactly what an ended step used", async () => {
    const t = await titled(async (oc) => {
      await oc.event(e2("session.inbox.delivered", 1100, { inboxID: "msg_A" }))
      await each(step2(oc, "msg_s1", { start: 1200, end: 1800, tokens: { input: 7, output: 0, reasoning: 0, cache: { read: 0, write: 0 } }, cost: 0.0003 }))
      await oc.event(usage(1900, 7, 0.0003))
    })
    for (const key of Object.keys(t)) expect(key).not.toMatch(/usage/)
  })
  test("not a step's, when the title's own update came between that step's end and its update", async () => {
    const t = await titled(async (oc) => {
      await oc.event(e2("session.inbox.delivered", 1100, { inboxID: "msg_A" }))
      await each(step2(oc, "msg_s1", { start: 1200, end: 1800, tokens: { input: 100, output: 0, reasoning: 0, cache: { read: 0, write: 0 } }, cost: 0.01 }))
      await oc.event(usage(1801, 7, 0.0003))
      await oc.event(usage(1802, 107, 0.0103))
    })
    for (const key of Object.keys(t)) expect(key).not.toMatch(/usage/)
  })
  test("its own update between a step's end and that step's update, when the rename follows it", async () => {
    const { r, oc, close } = await start2()
    await oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_A", prompt: { text: "hi" } })
    await oc.hook("session.title", { sessionID: sid2, model: { id: "m-title", providerID: "p1" } })
    await oc.event(e2("session.inbox.delivered", 1100, { inboxID: "msg_A" }))
    await each(step2(oc, "msg_s1", { start: 1200, end: 1800, tokens: { input: 100, output: 0, reasoning: 0, cache: { read: 0, write: 0 } }, cost: 0.01 }))
    await oc.event(usage(1801, 7, 0.0003))
    await oc.event(e2("session.renamed", 1802, { title: "A real title" }))
    await oc.event(usage(1803, 107, 0.0103))
    await close()
    const t = A(chats(r.spans()).find((sp) => kindOf(sp) === "title"))
    expect([t["gen_ai.usage.input_tokens"], t["gen_ai.usage.total_cost"]]).toEqual([7, 0.0003])
  })
  test("not one with another event of the session after it", async () => {
    const t = await titled(async (oc) => {
      await oc.event(usage(1900, 7, 0.0003))
      await oc.event(e2("session.execution.succeeded", 1950))
    })
    for (const key of Object.keys(t)) expect(key).not.toMatch(/usage/)
  })
})

test("a resumed session's title call takes only its own rise, over the record's totals", async () => {
  const totals = { cost: 0.5, tokens: { input: 100, output: 50, reasoning: 5, cache: { read: 1000, write: 200 } } }
  const { r, oc, close } = await start2({}, { get: (id) => record2({ id, title: "", ...totals }) })
  await oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_A", prompt: { text: "again" } })
  await oc.hook("session.title", { sessionID: sid2, model: { id: "m-title", providerID: "p1" } })
  await oc.event(e2("session.usage.updated", 1900, { cost: 0.5001, tokens: { input: 104, output: 59, reasoning: 5, cache: { read: 1000, write: 226 } } }))
  await oc.event(e2("session.renamed", 2000, { title: "A real title" }))
  await close()
  const t = A(chats(r.spans()).find((s) => kindOf(s) === "title"))
  expect([t["gen_ai.usage.input_tokens"], t["gen_ai.usage.output_tokens"], t["opencode.usage.reasoning_tokens"], t["gen_ai.usage.cache_read.input_tokens"], t["gen_ai.usage.cache_write.input_tokens"], t["gen_ai.usage.total_cost"]]).toEqual([4, 9, 0, 0, 26, 0.0001])
})

test("a session OpenCode cannot describe, and whose creation was not seen, exports nothing and claims nothing", async () => {
  const { r, oc, close } = await start2({}, {
    get: (id) => {
      if (id === "ses_throws") throw new Error("gone")
      return null
    },
  })
  for (const id of ["ses_throws", "ses_null"]) {
    await oc.hook("session.prompt", { sessionID: id, messageID: "msg_" + id, prompt: { text: "hi" } })
    await oc.event({ ...e2("session.step.started", 1200, { assistantMessageID: "msg_a_" + id, started: 1200 }), data: { sessionID: id, assistantMessageID: "msg_a_" + id, started: 1200 } })
    await oc.event({ ...e2("session.step.ended", 1300), data: { sessionID: id, assistantMessageID: "msg_a_" + id, cost: 1, tokens: ZERO_TOKENS } })
    await oc.event({ ...e2("session.execution.succeeded", 1400), data: { sessionID: id } })
  }
  await close()
  expect(r.requests()).toEqual([])
  expect(r.terma.calls()).toEqual([])
}, 15_000)

test("a session that moves claims its new directory; an event's own location moves nothing", async () => {
  const { r, oc, close } = await start2({}, { get: (id) => record2({ id, location: { directory: "/repo-a" } }) })
  await oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_A", prompt: { text: "one" } })
  // The moved event's own location is still the old directory.
  await oc.event({ ...e2("session.moved", 1500, { location: { directory: "/repo-b" }, subpath: "" }, "/repo-a") })
  await oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_B", prompt: { text: "two" } })
  await oc.event(e2("session.inbox.delivered", 1600, { inboxID: "msg_B" }, "/repo-b"))
  await oc.event(e2("session.step.started", 1700, { assistantMessageID: "msg_s1", started: 1700 }, "/repo-b"))
  await oc.event(e2("session.tool.input.started", 1710, { assistantMessageID: "msg_s1", id: "c1", name: "write" }, "/repo-b"))
  await oc.event(e2("session.tool.called", 1720, { assistantMessageID: "msg_s1", id: "c1", input: { path: "x.txt", content: "x" } }, "/repo-b"))
  await oc.event(e2("session.tool.success", 1730, { assistantMessageID: "msg_s1", id: "c1", content: [{ type: "text", text: "ok" }] }, "/repo-b"))
  // An event that merely carries another directory.
  await oc.event(e2("session.text.ended", 1740, { assistantMessageID: "msg_s1", ordinal: 0, text: "done" }, "/elsewhere"))
  await oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_C", prompt: { text: "three" } })
  await close()
  const calls = r.terma.calls().map((c) => [c.event, c.payload.cwd])
  expect(calls).toEqual([
    ["opencode-session-start", "/repo-a"],
    ["opencode-prompt", "/repo-a"],
    // The move claims the session where it went, before anything more of it leaves.
    ["opencode-prompt", "/repo-b"],
    ["opencode-prompt", "/repo-b"],
    ["opencode-file-edit", "/repo-b"],
    ["opencode-prompt", "/repo-b"],
  ])
  expect(r.terma.calls().find((c) => c.event === "opencode-file-edit").payload.files).toEqual(["/repo-b/x.txt"])
  expect(A(tools(r.spans())[0])["opencode.tool.files"]).toEqual(["/repo-b/x.txt"])
})

test("deleting a session this process never saw only ends it, from the event's directory", async () => {
  const { r, oc, close } = await start2()
  await oc.event({ ...e2("session.deleted", 1500, {}, "/old"), data: { sessionID: "ses_old" } })
  await close()
  expect(r.terma.calls()).toEqual([{ event: "opencode-session-end", payload: { session_id: "ses_old", cwd: "/old", reason: "deleted" } }])
  expect(r.requests()).toEqual([])
  expect(globalThis[KEY].sessions.has("ses_old")).toBe(false)
})

test("an event OpenCode 2 sends to every directory is handled once; one with no id every time", async () => {
  const { r, oc, close } = await start2()
  const once = e2("session.execution.succeeded", 1500)
  await oc.event(once)
  await oc.event(structuredClone(once))
  await oc.event({ ...e2("session.execution.succeeded", 1600), id: undefined })
  await oc.event({ ...e2("session.execution.succeeded", 1700), id: undefined })
  await close()
  expect(r.terma.calls().filter((c) => c.event === "opencode-idle").length).toBe(3)
})

describe("content switches on OpenCode 2", () => {
  async function scenario(overrides) {
    const { r, oc, close } = await start2(overrides)
    await oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_user", prompt: { text: "delete everything" } })
    await oc.hook("session.title", { sessionID: sid2, model: { id: "m-title", providerID: "p1" } })
    await oc.event(e2("session.usage.updated", 1090, { cost: 0.0001, tokens: { input: 3, output: 4, reasoning: 0, cache: { read: 0, write: 0 } } }))
    await oc.event(e2("session.renamed", 1095, { title: "A real title" }))
    await oc.event(e2("session.inbox.delivered", 1100, { inboxID: "msg_user" }))
    await oc.event(e2("session.step.started", 1200, { assistantMessageID: "msg_a1", agent: "build", model: { id: "m1", providerID: "p1" }, started: 1200 }))
    const toolCall = async (id, name, input, ok, text, metadata = {}) => {
      await oc.event(e2("session.tool.input.started", 1300, { assistantMessageID: "msg_a1", id, name }))
      await oc.event(e2("session.tool.called", 1310, { assistantMessageID: "msg_a1", id, input }))
      await oc.hook("tool.execute.before", { tool: name, codemode: false, sessionID: sid2, messageID: "msg_a1", id, input })
      await oc.event(e2(ok ? "session.tool.success" : "session.tool.failed", 1320, { assistantMessageID: "msg_a1", id, content: [{ type: "text", text }], metadata, ...(ok ? {} : { error: { type: "tool.execution", message: text } }) }))
    }
    await toolCall("c1", "shell", { command: "rm -rf /" }, true, "secret output", { exit: 1 })
    await toolCall("c3", "read", { path: "/repo/a.txt" }, true, "file secret")
    await toolCall("c4", "execute", { code: "await tools.capture.echo({})" }, true, "code result")
    await oc.hook("tool.execute.before", { tool: "capture_echo", codemode: true, info: { id: "capture_echo", name: "echo" }, sessionID: sid2, messageID: "msg_a1", id: "c2", input: { text: "x" } })
    await oc.hook("tool.execute.after", { tool: "capture_echo", codemode: true, info: { id: "capture_echo", name: "echo" }, sessionID: sid2, messageID: "msg_a1", id: "c2", input: { text: "x" }, status: "completed", result: { output: "mcp secret" } })
    await oc.event(e2("session.text.ended", 1390, { assistantMessageID: "msg_a1", ordinal: 0, text: "the reply" }))
    await oc.event(e2("session.step.ended", 1400, { assistantMessageID: "msg_a1", finish: "stop", cost: 0.01, tokens: { input: 1, output: 2, reasoning: 3, cache: { read: 4, write: 5 } } }))
    await oc.event(e2("session.step.started", 1500, { assistantMessageID: "msg_a2", agent: "build", model: { id: "m1", providerID: "p1" }, started: 1500 }))
    await oc.event(e2("session.step.failed", 1600, { assistantMessageID: "msg_a2", error: { type: "provider.invalid-request", message: "provider said: delete everything" } }))
    await close()
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
    expect(everything).toContain("secret output")
    const failed = A(chats(r.spans()).find((s) => A(s)["opencode.call.id"] === "msg_a2"))
    expect(failed["error.type"]).toBe("provider.invalid-request")
    expect(failed["opencode.error.message"]).toBeUndefined()
  })

  test("tool content off: no arguments, results or files, but status, exit code and MCP names", async () => {
    const r = await scenario({ includeToolContent: false })
    const everything = JSON.stringify(r.requests())
    for (const secret of ["rm -rf", "secret output", "mcp secret", "file secret", "/repo/a.txt", "code result"]) expect(everything).not.toContain(secret)
    const byName = Object.fromEntries(tools(r.spans()).map((s) => [A(s)["gen_ai.tool.name"], A(s)]))
    expect(byName.shell["opencode.tool.status"]).toBe("completed")
    expect(byName.shell["opencode.tool.exit_code"]).toBe(1)
    expect(byName.shell["opencode.tool.output_bytes"]).toBe(13)
    expect(byName.capture_echo["opencode.mcp.server"]).toBe("capture")
    expect(byName.capture_echo["opencode.mcp.tool"]).toBe("echo")
    for (const a of Object.values(byName)) {
      for (const key of ["gen_ai.tool.call.arguments", "gen_ai.tool.call.result", "opencode.tool.files", "opencode.tool.title"]) expect(a[key]).toBeUndefined()
    }
    const failed = A(chats(r.spans()).find((s) => A(s)["opencode.call.id"] === "msg_a2"))
    expect(failed["opencode.error.message"]).toBeUndefined()
    expect(everything).toContain("delete everything")
  })

  test("both on: the error message and every content ship", async () => {
    const r = await scenario({})
    const failed = A(chats(r.spans()).find((s) => A(s)["opencode.call.id"] === "msg_a2"))
    expect(failed["opencode.error.message"]).toBe("provider said: delete everything")
    const byName = Object.fromEntries(tools(r.spans()).map((s) => [A(s)["gen_ai.tool.name"], A(s)]))
    expect(byName.read["opencode.tool.files"]).toEqual(["/repo/a.txt"])
    expect(byName.capture_echo["gen_ai.tool.call.result"]).toBe("mcp secret")
    expect(byName.shell["gen_ai.tool.call.arguments"]).toBe(JSON.stringify({ command: "rm -rf /" }))
  })
})

// --- files a prompt attaches, a moved file, a person's own shell command --------------

for (const v of ["2.0.0", "2.4.1"]) {
  test(`scripted ${v} svc2: a prompt with an attached file is the person's own text; a moved file names both paths; the person's shell command is no call`, async () => {
    const res = await replayed2(`scripted ${v}/svc2`)
    const attached = hooks2(res.records, "session.prompt").filter((r) => r.data.prompt.files?.length)
    expect(attached.length).toBe(2)
    // The prompt is the person's own text: the file's content is in no record.
    for (const r of [...res.logs, ...res.spans]) expect(JSON.stringify(r)).not.toContain("ATTACHED-FILE-CANARY")
    expect(named(res.logs, "opencode.user_prompt").map((r) => r.body.stringValue)).toContain("Summarize @attach.txt please.")
    expect(res.hooks.filter((h) => h.event === "opencode-tool-reach")).toEqual([])

    // The patch that moved notes.txt to moved.txt names both; the one that added and deleted, each.
    const dir = res.answer(sessionOf2(attached[0]), 0).location.directory
    const patches = tools(res.spans).filter((sp) => sp.name === "execute_tool patch").map((sp) => A(sp)["opencode.tool.files"].slice().sort())
    expect(patches).toEqual([[join(dir, "moved.txt"), join(dir, "notes.txt")], [join(dir, "added.txt"), join(dir, "calc.py")]])
    const edits = res.hooks.filter((h) => h.event === "opencode-file-edit").map((h) => h.payload.files.slice().sort())
    expect(edits).toEqual(patches)

    // The shell command the person ran made no model call or tool span.
    expect(distinct(events2(res.records, "session.shell.started")).length).toBe(1)
    expect(tools(res.spans).every((sp) => sp.name === "execute_tool patch")).toBe(true)
  }, REPLAY_TIMEOUT)
}

test("an image pasted from the clipboard is the person's own input: the session is recorded and no reach is checked", async () => {
  const png = "data:image/png;base64,iVBORw0KGgo="
  const { r, oc, close } = await start2()
  await oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_A", prompt: { text: "What is in the screenshot?", files: [{ uri: png, name: "clipboard" }] } })
  await oc.event(e2("session.inbox.delivered", 1100, { inboxID: "msg_A" }))
  await each(step2(oc, "msg_s1", { start: 1200, end: 1300, turnText: "a diagram" }))
  await close()
  expect(named(r.logs(), "opencode.user_prompt").map((l) => l.body.stringValue)).toEqual(["What is in the screenshot?"])
  expect(chats(r.spans()).length).toBe(1)
  expect(r.terma.calls().map((c) => c.event)).toEqual(["opencode-session-start", "opencode-prompt"])
  // Nothing of the image leaves.
  expect(JSON.stringify(r.requests())).not.toContain("iVBORw0KGgo")
})

test("after a move every prompt carries its own time, later than the move", async () => {
  const { r, oc, close } = await start2({}, { get: (id) => record2({ id, location: { directory: "/repo-a" } }) })
  await oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_A", prompt: { text: "one" } })
  await oc.event(e2("session.moved", 1500, { location: { directory: "/repo-b" }, subpath: "" }, "/repo-a"))
  await oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_B", prompt: { text: "two" } })
  await Bun.sleep(300)
  const asked = Date.now()
  await Bun.sleep(50)
  await oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_C", prompt: { text: "three" } })
  await close()
  const at = Object.fromEntries(named(r.logs(), "opencode.user_prompt").map((l) => [A(l)["opencode.message.id"], Number(BigInt(l.timeUnixNano) / 1000000n)]))
  expect(Object.keys(at)).toEqual(["msg_A", "msg_B", "msg_C"])
  // The third prompt is not stamped at the move: it was asked well after it.
  expect(at.msg_C).toBeGreaterThan(asked)
  expect(at.msg_C).toBeGreaterThan(at.msg_B)
  // Two prompts asked apart never share the move's time.
  expect(new Set(Object.values(at)).size).toBe(3)
})

test("a move there and back, the first arriving before the session's record, leaves the session where it ended", async () => {
  const quiet = mkdtempSync(join(root, "quiet-"))
  mkdirSync(join(quiet, ".git"))
  mkdirSync(join(quiet, ".opencode"))
  writeFileSync(join(quiet, ".opencode", "terma.json"), JSON.stringify({ includePrompts: false }))
  // The claim takes a while, so the move back arrives while the first move still waits.
  const { r, oc, close } = await start2({}, { get: (id) => record2({ id, location: { directory: quiet } }), terma: { delayed: { "opencode-session-start": 0.2 } } })
  const first = oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_A", prompt: { text: "PROMPT-ONE" } })
  await oc.event(e2("session.moved", 1500, { location: { directory: "/repo-open" }, subpath: "" }, quiet))
  await oc.event(e2("session.moved", 1600, { location: { directory: quiet }, subpath: "" }, "/repo-open"))
  const second = oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_B", prompt: { text: "PROMPT-TWO" } })
  await Promise.all([first, second])
  await close()
  // Asked in the withholding repository before the moves, and in it again after them.
  expect(named(r.logs(), "opencode.user_prompt").map((l) => l.body.stringValue)).toEqual(["", ""])
  // Each move claims the session where it went, in turn.
  expect(r.terma.calls().filter((c) => c.event === "opencode-prompt").map((c) => c.payload.cwd)).toEqual([quiet, "/repo-open", quiet, quiet])
}, 15_000)

test("a move that arrives before the session's record: what was asked before it leaves under the repository it was asked in", async () => {
  const quiet = mkdtempSync(join(root, "quiet-"))
  mkdirSync(join(quiet, ".git"))
  mkdirSync(join(quiet, ".opencode"))
  writeFileSync(join(quiet, ".opencode", "terma.json"), JSON.stringify({ includePrompts: false }))
  // OpenCode answers the lookup after the move: its record already names the new directory.
  let release
  const answered = new Promise((done) => {
    release = done
  })
  const { r, oc, close } = await start2({}, { get: async (id) => (await answered, record2({ id, location: { directory: "/repo-open" } })) })
  const first = oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_A", prompt: { text: "PROMPT-ONE" } })
  await oc.event(e2("session.moved", 1500, { location: { directory: "/repo-open" }, subpath: "" }, quiet))
  release()
  const second = oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_B", prompt: { text: "PROMPT-TWO" } })
  await Promise.all([first, second])
  await close()
  expect(named(r.logs(), "opencode.user_prompt").map((l) => l.body.stringValue)).toEqual(["", "PROMPT-TWO"])
  expect(r.terma.calls().filter((c) => c.event === "opencode-prompt").map((c) => c.payload.cwd)).toEqual([quiet, "/repo-open", "/repo-open"])
})

test("a prompt is stamped no earlier than its session's claim, and the calls it starts no earlier than it; a later prompt carries the moment it was asked", async () => {
  const { r, oc, close } = await start2({}, { terma: { delayed: { "opencode-session-start": 0.2 } } })
  const ms = (ns) => Number(BigInt(ns) / 1000000n)
  const asked = Date.now()
  await oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_A", prompt: { text: "hi" } })
  await oc.hook("session.title", { sessionID: sid2, model: { id: "m-title", providerID: "p1" } })
  await oc.event(e2("session.inbox.delivered", asked + 5, { inboxID: "msg_A" }))
  // OpenCode started the step while terma was still claiming the session.
  await each(step2(oc, "msg_s1", { start: asked + 20, end: asked + 400 }))
  await oc.event(e2("session.renamed", asked + 410, { title: "A real title" }))
  await quiet2()
  // With the claim made and nothing ahead of it, a prompt carries the moment it was asked,
  // and the step it starts keeps OpenCode's own time.
  const again = Date.now()
  await oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_B", prompt: { text: "more" } })
  await oc.event(e2("session.inbox.delivered", again + 5, { inboxID: "msg_B" }))
  await each(step2(oc, "msg_s2", { start: again + 20, end: again + 60 }))
  await close()

  const claimed = Math.floor(finishedAt(r.dir, "opencode-session-start"))
  const prompts = named(r.logs(), "opencode.user_prompt")
  const first = prompts.find((l) => A(l)["opencode.message.id"] === "msg_A")
  // The relay files a record under the claim in force at its time: the prompt is not earlier.
  expect(ms(first.timeUnixNano)).toBeGreaterThanOrEqual(asked + 200)
  expect(ms(first.timeUnixNano)).toBeGreaterThanOrEqual(claimed)
  const turn1 = r.spans().filter((sp) => A(sp)["opencode.parent_message.id"] === "msg_A" || kindOf(sp) === "title")
  expect(turn1.map((sp) => sp.name).sort()).toEqual(["chat m-title", "chat m1"])
  for (const sp of turn1) expect(ms(sp.startTimeUnixNano)).toBeGreaterThanOrEqual(ms(first.timeUnixNano))
  // Only the start moved, and no further than the prompt: the step ends when OpenCode said
  // (or at its prompt, on a machine so slow that the claim outlasted the whole step).
  const call1 = chats(turn1).find((sp) => !kindOf(sp))
  expect(ms(call1.startTimeUnixNano)).toBe(ms(first.timeUnixNano))
  expect(ms(call1.endTimeUnixNano)).toBe(Math.max(asked + 400, ms(first.timeUnixNano)))

  const second = prompts.find((l) => A(l)["opencode.message.id"] === "msg_B")
  expect(ms(second.timeUnixNano) - again).toBeGreaterThanOrEqual(0)
  expect(ms(second.timeUnixNano) - again).toBeLessThan(15)
  const call2 = chats(r.spans()).find((sp) => A(sp)["opencode.parent_message.id"] === "msg_B")
  expect(ms(call2.startTimeUnixNano)).toBe(again + 20)
})

// Seen on 2.4.1 in a resumed run: OpenCode does not wait for the claim, and a fast call
// can run and end while terma is still claiming the session.
test("a call that ends while its session is still being claimed is held to its prompt, start and end", async () => {
  const { r, oc, close } = await start2({}, { terma: { delayed: { "opencode-session-start": 0.2 } } })
  const ms = (ns) => Number(BigInt(ns) / 1000000n)
  const asked = Date.now()
  await oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_A", prompt: { text: "hi" } })
  await oc.hook("session.title", { sessionID: sid2, model: { id: "m-title", providerID: "p1" } })
  await oc.event(e2("session.inbox.delivered", asked + 5, { inboxID: "msg_A" }))
  await each(step2(oc, "msg_s1", { start: asked + 20, end: asked + 100 }))
  // The title call is as fast: its title arrives during the claim too.
  await oc.event(e2("session.renamed", asked + 60, { title: "A real title" }))
  await close()

  const [p] = named(r.logs(), "opencode.user_prompt")
  const at = ms(p.timeUnixNano)
  expect(at).toBeGreaterThanOrEqual(asked + 200)
  const spans = chats(r.spans())
  expect(spans.map((sp) => sp.name).sort()).toEqual(["chat m-title", "chat m1"])
  for (const sp of spans) {
    expect(ms(sp.startTimeUnixNano)).toBe(at)
    expect(ms(sp.endTimeUnixNano)).toBe(at)
  }
  const [t] = named(r.logs(), "opencode.session.title")
  expect(ms(t.timeUnixNano)).toBeGreaterThanOrEqual(at)
})

// A resumed session's title comes with its record, read before the claim. It keeps the time
// it was seen: the relay judges it by that. The prompt is held to the claim.
test("a resumed session's title read before its claim keeps its time; its prompt is stamped after the claim; a later rename keeps its time", async () => {
  const { r, oc, close } = await start2({}, { get: (id) => (id === sid2 ? record2({ title: "Resumed title" }) : null), terma: { delayed: { "opencode-session-start": 0.2 } } })
  const ms = (ns) => Number(BigInt(ns) / 1000000n)
  const asked = Date.now()
  await oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_A", prompt: { text: "hi" } })
  await oc.event(e2("session.inbox.delivered", asked + 5, { inboxID: "msg_A" }))
  await each(step2(oc, "msg_s1", { start: asked + 20, end: asked + 400 }))
  await quiet2()
  // Once claimed: the rename's own time.
  const renamed = Date.now() + 50
  await oc.event(e2("session.renamed", renamed, { title: "A new title" }))
  await close()

  const claimed = Math.floor(finishedAt(r.dir, "opencode-session-start"))
  const titles = Object.fromEntries(named(r.logs(), "opencode.session.title").map((l) => [l.body.stringValue, ms(l.timeUnixNano)]))
  expect(Object.keys(titles).sort()).toEqual(["A new title", "Resumed title"])
  expect(titles["Resumed title"]).toBeLessThan(claimed)
  expect(titles["A new title"]).toBe(renamed)
  const [p] = named(r.logs(), "opencode.user_prompt")
  expect(ms(p.timeUnixNano)).toBeGreaterThanOrEqual(claimed)
})

test("a title call never ends before it began, whatever time OpenCode gave the rename", async () => {
  const { r, oc, close } = await start2()
  const ms = (ns) => Number(BigInt(ns) / 1000000n)
  await oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_A", prompt: { text: "hi" } })
  await quiet2()
  await oc.hook("session.title", { sessionID: sid2, model: { id: "m-title", providerID: "p1" } })
  await oc.event(e2("session.renamed", Date.now() - 30, { title: "A real title" }))
  await close()
  const [sp] = chats(r.spans()).filter((x) => kindOf(x) === "title")
  expect(ms(sp.endTimeUnixNano)).toBeGreaterThanOrEqual(ms(sp.startTimeUnixNano))
})

test("a subagent seen through keeps its parent recording, and its call is sent", async () => {
  const child = "ses_known_child"
  const { r, oc, close } = await start2({}, { get: (id) => (id === sid2 ? record2() : id === child ? record2({ id: child, parentID: sid2 }) : null) })
  await oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_A", prompt: { text: "Delegate it." } })
  await oc.event(e2("session.inbox.delivered", 1100, { inboxID: "msg_A" }))
  await oc.event(e2("session.step.started", 1200, { assistantMessageID: "msg_s1", started: 1200 }))
  const input = { agent: "general", description: "child", prompt: "Say hi." }
  await oc.event(e2("session.tool.input.started", 1210, { assistantMessageID: "msg_s1", id: "c1", name: "subagent" }))
  await oc.event(e2("session.tool.called", 1220, { assistantMessageID: "msg_s1", id: "c1", input }))
  await oc.hook("session.prompt", { sessionID: child, messageID: "msg_c", prompt: { text: "Say hi." } })
  await oc.event(e2("session.tool.success", 1300, { assistantMessageID: "msg_s1", id: "c1", content: [{ type: "text", text: "CHILD-SAID-HI" }], metadata: { sessionID: child } }))
  await close()
  expect(tools(r.spans()).map((sp) => sp.name)).toEqual(["execute_tool subagent"])
})

test("a rename right behind a move is stamped after the move's claim, not at its own earlier time", async () => {
  const { r, oc, close } = await start2({}, { get: (id) => record2({ id, location: { directory: "/repo-a" } }), terma: { delayed: { "opencode-prompt": 0.2 } } })
  await oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_A", prompt: { text: "one" } })
  const before = Date.now()
  await oc.event(e2("session.moved", before, { location: { directory: "/repo-b" }, subpath: "" }, "/repo-a"))
  // OpenCode's own time for the rename is before the claim in the new directory finished.
  await oc.event(e2("session.renamed", before + 1, { title: "Renamed after the move" }, "/repo-b"))
  await close()
  const [t] = named(r.logs(), "opencode.session.title")
  expect(t.body.stringValue).toBe("Renamed after the move")
  expect(Number(BigInt(t.timeUnixNano) / 1000000n) - before).toBeGreaterThanOrEqual(200)
})

test("a failed session.moved claim darkens the session so later prompts do not export under the old claim", async () => {
  const { r, oc, close } = await start2({}, { directory: "/repo-a", get: (id) => record2({ id, location: { directory: "/repo-a" } }), terma: { fail: ["opencode-prompt"] } })
  await oc.event(e2("session.created", 1000, { location: { directory: "/repo-a" }, version: "2.4.1" }, "/repo-a"))
  await oc.event(e2("session.moved", 1200, { location: { directory: "/repo-b" }, subpath: "" }, "/repo-a"))
  await oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_after_fail", prompt: { text: "org-b secret" } })
  await close()
  expect(r.terma.calls()).toEqual([
    { event: "opencode-session-start", payload: { session_id: sid2, cwd: "/repo-a" } },
    { event: "opencode-prompt", payload: { session_id: sid2, cwd: "/repo-b" } },
  ])
  expect(named(r.logs(), "opencode.user_prompt")).toEqual([])
  const s = globalThis[KEY].sessions.get(sid2)
  expect(s.dark).toBe(true)
  expect(s.movedAt).toBe(0)
})

test("a turn that is interrupted ends like any other: the idle hook runs", async () => {
  const { r, oc, close } = await start2()
  await oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_A", prompt: { text: "hi" } })
  await oc.event(e2("session.execution.interrupted", 1500, { reason: "user" }))
  await close()
  expect(r.terma.calls().map((c) => c.event)).toEqual(["opencode-session-start", "opencode-prompt", "opencode-idle"])
})

test("a session that moves takes its new repository's own policy, from the move on and not before", async () => {
  const quiet = mkdtempSync(join(root, "quiet-"))
  mkdirSync(join(quiet, ".git"))
  mkdirSync(join(quiet, ".opencode"))
  writeFileSync(join(quiet, ".opencode", "terma.json"), JSON.stringify({ includePrompts: false }))
  // Into a repository that withholds prompts, and out of one: the prompt asked before the
  // move leaves under the old repository's rule, the one after under the new.
  // Twice each: with the session's record already there when the move arrives, and with the
  // move arriving while the first prompt, and the record it asked for, are still on their way.
  for (const known of [true, false]) {
    for (const [from, to, want] of [
      ["/repo-a", quiet, [["PROMPT-ONE", 10], ["", 10]]],
      [quiet, "/repo-a", [["", 10], ["PROMPT-TWO", 10]]],
    ]) {
      // OpenCode 2 sets a copy up for the directory its sessions run in.
      const { r, oc, close } = await start2({}, { directory: from, get: (id) => record2({ id, location: { directory: from } }) })
      if (known) await oc.event(e2("session.inbox.delivered", 1000, { inboxID: "msg_none" }, from))
      const first = oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_A", prompt: { text: "PROMPT-ONE" } })
      // The plugin has the move, and has not applied it yet, when the next prompt is asked.
      await oc.event(e2("session.moved", 1500, { location: { directory: to }, subpath: "" }, from))
      const second = oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_B", prompt: { text: "PROMPT-TWO" } })
      await Promise.all([first, second])
      await close()
      expect(named(r.logs(), "opencode.user_prompt").map((l) => [l.body.stringValue, A(l)["opencode.prompt.length"]])).toEqual(want)
      expect(r.terma.calls().filter((c) => c.event === "opencode-prompt").map((c) => c.payload.cwd)).toEqual([from, to, to])
    }
  }
})

test("the title asked again for the same prompt, with another model, is one call", async () => {
  const { r, oc, close } = await start2()
  await oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_A", prompt: { text: "hi" } })
  await oc.hook("session.title", { sessionID: sid2, model: { id: "m-small", providerID: "p1" } })
  await oc.hook("session.title", { sessionID: sid2, model: { id: "m-main", providerID: "p2" } })
  await oc.event(e2("session.usage.updated", 1900, { cost: 0.0003, tokens: { input: 7, output: 1, reasoning: 0, cache: { read: 0, write: 0 } } }))
  await oc.event(e2("session.renamed", 2000, { title: "A real title" }))
  await close()
  const calls = chats(r.spans()).filter((sp) => kindOf(sp) === "title")
  expect(calls.map((sp) => [A(sp)["opencode.call.id"], A(sp)["gen_ai.request.model"], A(sp)["gen_ai.provider.name"], A(sp)["gen_ai.usage.input_tokens"]])).toEqual([["title:msg_A", "m-main", "p2", 7]])
})

describe("a rise in usage that another call explains is not the title's", () => {
  const usage = (created, input, cost) => e2("session.usage.updated", created, { cost, tokens: { input, output: 0, reasoning: 0, cache: { read: 0, write: 0 } } })
  async function titled(between) {
    const { r, oc, close } = await start2()
    await oc.hook("session.prompt", { sessionID: sid2, messageID: "msg_A", prompt: { text: "hi" } })
    await oc.hook("session.title", { sessionID: sid2, model: { id: "m-title", providerID: "p1" } })
    await between(oc)
    await oc.event(e2("session.renamed", 2000, { title: "A real title" }))
    await close()
    return A(chats(r.spans()).find((sp) => kindOf(sp) === "title"))
  }
  const failedStep = (oc, extra) => [
    () => oc.event(e2("session.inbox.delivered", 1100, { inboxID: "msg_A" })),
    () => oc.event(e2("session.step.started", 1200, { assistantMessageID: "msg_s1", agent: "build", model: { id: "m1", providerID: "p1" }, started: 1200 })),
    () => oc.event(e2("session.step.failed", 1800, { assistantMessageID: "msg_s1", error: { type: "provider.content-filter", message: "no" }, ...extra })),
  ]
  test("a failed step that was billed", async () => {
    const t = await titled(async (oc) => {
      await each(failedStep(oc, { cost: 0.0003, tokens: { input: 7, output: 0, reasoning: 0, cache: { read: 0, write: 0 } } }))
      await oc.event(usage(1900, 7, 0.0003))
    })
    for (const key of Object.keys(t)) expect(key).not.toMatch(/usage/)
  })
  test("but a failed step that used nothing explains none", async () => {
    const t = await titled(async (oc) => {
      await each(failedStep(oc, {}))
      await oc.event(usage(1900, 7, 0.0003))
    })
    expect(t["gen_ai.usage.input_tokens"]).toBe(7)
  })
  test("a side question, which the plugin does not report", async () => {
    const t = await titled(async (oc) => {
      await oc.hook("session.generate", { sessionID: sid2, model: { id: "m1", providerID: "p1" } })
      await oc.event(usage(1900, 7, 0.0003))
    })
    for (const key of Object.keys(t)) expect(key).not.toMatch(/usage/)
  })
  test("a side question whose usage comes after a step's own update", async () => {
    const t = await titled(async (oc) => {
      await oc.hook("session.generate", { sessionID: sid2, model: { id: "m1", providerID: "p1" } })
      await oc.event(e2("session.inbox.delivered", 1100, { inboxID: "msg_A" }))
      await each(step2(oc, "msg_s1", { start: 1200, end: 1800, tokens: { input: 100, output: 0, reasoning: 0, cache: { read: 0, write: 0 } }, cost: 0.01 }))
      await oc.event(usage(1801, 100, 0.01))
      await oc.event(usage(1900, 107, 0.0103))
    })
    for (const key of Object.keys(t)) expect(key).not.toMatch(/usage/)
  })
})

test("a directory's copy that reloads closes its own sessions' pending title calls and no other directory's", async () => {
  const r = rig()
  const get = (id) => record2({ id, location: { directory: "/b" } })
  const a = fakeCtx2({ directory: "/a", get })
  const b = fakeCtx2({ directory: "/b", get })
  const cleanupA = await (await load(r.config)).default.setup(a.ctx)
  const cleanupB = await (await load(r.config, { keep: true })).default.setup(b.ctx)
  // The session runs in /b; /a's subscription is the first to see it.
  await a.event(e2("session.created", 1000, { location: { directory: "/b" }, version: "2.4.1" }, "/b"))
  await b.hook("session.prompt", { sessionID: sid2, messageID: "msg_A", prompt: { text: "hi" } })
  await b.hook("session.title", { sessionID: sid2, model: { id: "m-title", providerID: "p1" } })
  await cleanupA()
  a.end()
  expect(chats(r.spans()).filter((sp) => kindOf(sp) === "title")).toEqual([])
  await b.event(e2("session.usage.updated", 1900, { cost: 0.0003, tokens: { input: 7, output: 0, reasoning: 0, cache: { read: 0, write: 0 } } }, "/b"))
  await b.event(e2("session.renamed", 2000, { title: "A real title" }, "/b"))
  await cleanupB()
  b.end()
  const calls = chats(r.spans()).filter((sp) => kindOf(sp) === "title")
  expect(calls.map((sp) => A(sp)["gen_ai.usage.input_tokens"])).toEqual([7])
})

// --- one scenario through both entries ----------------------------------------------
//
// The same session written once as plain data, rendered as OpenCode 1 hooks and events
// for `server` and as OpenCode 2 ones for `setup`, with the same ids, times, tokens and
// cost. The wire and the hook calls must be the same but for the differences each major
// cannot avoid, named one by one below.

const SAME = {
  id: "ses_same",
  directory: "/repo",
  created: 1000,
  prompt: { id: "msg_user", text: "Read a.txt, then fix b.txt.", time: 1500 },
  title: { text: "Fix b.txt", model: "m-title", provider: "p1", end: 4600, tokens: { input: 3, output: 7, reasoning: 0, cache: { read: 0, write: 40 } }, cost: 0.00002 },
  steps: [
    {
      id: "msg_a1",
      agent: "build",
      model: "m1",
      provider: "p1",
      start: 2000,
      end: 3000,
      finish: "tool-calls",
      tokens: { input: 10, output: 20, reasoning: 5, cache: { read: 100, write: 50 } },
      cost: 0.0125,
      texts: ["Reading a.txt first.", "Then the fix."],
      tools: [
        { id: "call_1", name: "read", path: "/repo/a.txt", args: {}, output: "alpha", start: 2100, end: 2200 },
        { id: "call_2", name: "edit", path: "/repo/b.txt", args: { oldString: "beta", newString: "BETA" }, output: "Edited b.txt", start: 2300, end: 2400 },
      ],
    },
    {
      id: "msg_a2",
      agent: "build",
      model: "m1",
      provider: "p1",
      start: 3100,
      end: 4000,
      finish: "stop",
      tokens: { input: 2, output: 4, reasoning: 0, cache: { read: 300, write: 10 } },
      cost: 0.002,
      texts: ["Done."],
      tools: [],
    },
  ],
}

async function sameThrough1() {
  const r = rig()
  const mod = await load(r.config)
  const h = await mod.default.server({ client: fakeClient(null), directory: SAME.directory, worktree: SAME.directory })
  const info = { id: SAME.id, directory: SAME.directory, version: "1.19.6", title: "New session - 2026-11-25T00:00:00.000Z", time: { created: SAME.created } }
  // The prompt is stamped no earlier than the session's claim, which in this scenario is
  // done at the prompt's moment, as on OpenCode 2.
  const realNow = Date.now
  Date.now = () => SAME.prompt.time
  try {
    await h.event(ev("session.created", { info }))
    await h["chat.message"](
      { sessionID: SAME.id },
      { message: { id: SAME.prompt.id, agent: "build", model: { modelID: "m1", providerID: "p1" }, time: { created: SAME.prompt.time } }, parts: [{ type: "text", text: SAME.prompt.text }] },
    )
  } finally {
    Date.now = realNow
  }
  await h["chat.params"]({ sessionID: SAME.id, agent: "title", model: { id: SAME.title.model, providerID: SAME.title.provider }, message: { id: SAME.prompt.id } }, {})
  for (const step of SAME.steps) {
    const msg = { id: step.id, sessionID: SAME.id, role: "assistant", parentID: SAME.prompt.id, modelID: step.model, providerID: step.provider, mode: step.agent, time: { created: step.start } }
    await h.event(ev("message.updated", { info: msg }))
    for (const t of step.tools) {
      const input = { filePath: t.path, ...t.args }
      await h.event(toolPart(SAME.id, step.id, t.id, t.name, { status: "completed", input, output: t.output, title: t.path.slice(SAME.directory.length + 1), metadata: {}, time: { start: t.start, end: t.end } }))
    }
    for (const [i, text] of step.texts.entries()) await h.event(textPart(SAME.id, step.id, "prt_" + step.id + "_" + i, text))
    await h.event(ev("message.updated", { info: { ...msg, cost: step.cost, tokens: step.tokens, finish: step.finish, time: { created: step.start, completed: step.end } } }))
  }
  await h.event(ev("session.idle", { sessionID: SAME.id }))
  await h.event(ev("session.updated", { info: { ...info, title: SAME.title.text } }))
  await h.dispose()
  return r
}

async function sameThrough2() {
  const r = rig()
  const mod = await load(r.config)
  const record = { id: SAME.id, projectID: "prj", cost: 0, tokens: structuredClone(ZERO_TOKENS), time: { created: SAME.created, updated: SAME.created }, location: { directory: SAME.directory } }
  const oc = fakeCtx2({ version: "2.4.1", directory: SAME.directory, get: (id) => (id === SAME.id ? structuredClone(record) : null) })
  const cleanup = await mod.default.setup(oc.ctx)
  let n = 0
  const e = (type, created, data) => oc.event({ id: "evt_same" + ++n, created, type, location: { directory: SAME.directory }, data: { sessionID: SAME.id, ...data } })
  const totals = { cost: 0, input: 0, output: 0, reasoning: 0, read: 0, write: 0 }
  const used = (created, { cost, tokens }) => {
    totals.cost += cost
    for (const k of ["input", "output", "reasoning"]) totals[k] += tokens[k]
    totals.read += tokens.cache.read
    totals.write += tokens.cache.write
    const t = { input: totals.input, output: totals.output, reasoning: totals.reasoning, cache: { read: totals.read, write: totals.write } }
    return e("session.usage.updated", created, { cost: totals.cost, tokens: t })
  }
  // OpenCode 2 gives a prompt no time: the plugin takes it from its own clock, as the hook
  // fires and as the session's claim is done, which in this scenario is one moment.
  const realNow = Date.now
  Date.now = () => SAME.prompt.time
  try {
    await oc.hook("session.prompt", { sessionID: SAME.id, messageID: SAME.prompt.id, prompt: { text: SAME.prompt.text } })
    await quiet2()
  } finally {
    Date.now = realNow
  }
  await oc.hook("session.title", { sessionID: SAME.id, model: { id: SAME.title.model, providerID: SAME.title.provider } })
  await e("session.inbox.delivered", SAME.prompt.time + 1, { inboxID: SAME.prompt.id })
  for (const step of SAME.steps) {
    await e("session.step.started", step.start + 1, { assistantMessageID: step.id, agent: step.agent, model: { id: step.model, providerID: step.provider }, started: step.start })
    for (const t of step.tools) {
      const input = { path: t.path, ...t.args }
      await e("session.tool.input.started", t.start - 1, { assistantMessageID: step.id, id: t.id, name: t.name })
      await e("session.tool.called", t.start, { assistantMessageID: step.id, id: t.id, input })
      await oc.hook("tool.execute.before", { tool: t.name, codemode: false, sessionID: SAME.id, messageID: step.id, id: t.id, input })
      await e("session.tool.success", t.end, { assistantMessageID: step.id, id: t.id, content: [{ type: "text", text: t.output }], metadata: {} })
    }
    for (const [ordinal, text] of step.texts.entries()) await e("session.text.ended", step.end - 1, { assistantMessageID: step.id, ordinal, text })
    await e("session.step.ended", step.end, { assistantMessageID: step.id, finish: step.finish, cost: step.cost, tokens: step.tokens })
    await used(step.end + 1, step)
  }
  await e("session.execution.succeeded", 4100, {})
  await used(SAME.title.end - 1, SAME.title)
  await e("session.renamed", SAME.title.end, { title: SAME.title.text })
  await cleanup()
  oc.end()
  return r
}

// wireOf is what left, in a comparable form: spans by id, records by name and message,
// hook calls in the order each session made them.
function wireOf(r) {
  const spans = r
    .spans()
    .map((s) => ({ name: s.name, kind: s.kind, traceId: s.traceId, spanId: s.spanId, start: s.startTimeUnixNano, end: s.endTimeUnixNano, status: s.status, attributes: A(s) }))
    .sort((a, b) => (a.spanId < b.spanId ? -1 : 1))
  const logs = r
    .logs()
    .map((l) => ({ traceId: l.traceId, time: l.timeUnixNano, body: l.body.stringValue, attributes: A(l) }))
    .sort((a, b) => (a.attributes["event.name"] < b.attributes["event.name"] ? -1 : 1))
  return { spans, logs, hooks: r.terma.calls() }
}

// The differences the two majors cannot avoid, each removed by name.
function withoutAllowed(w) {
  for (const x of [...w.spans, ...w.logs]) {
    // Each major names its own version.
    delete x.attributes["opencode.version"]
  }
  for (const l of w.logs) {
    const name = l.attributes["event.name"]
    if (name === "opencode.user_prompt") {
      // OpenCode 2's prompt hook names no agent, model or provider. Its time is the same:
      // the moment the prompt was asked.
      for (const k of ["opencode.agent", "gen_ai.request.model", "gen_ai.provider.name"]) delete l.attributes[k]
    }
    // An OpenCode 1 title change carries no time.
    if (name === "opencode.session.title") delete l.time
  }
  for (const s of w.spans) {
    // OpenCode 1 cannot read the title call's usage.
    if (s.attributes["opencode.call.kind"] === "title") {
      for (const k of Object.keys(s.attributes)) if (/usage/.test(k)) delete s.attributes[k]
    }
    // OpenCode 2 gives a tool call no title.
    delete s.attributes["opencode.tool.title"]
  }
  return w
}

test("one scenario through both entries gives the same wire and the same hook calls", async () => {
  const began = Date.now()
  const one = withoutAllowed(wireOf(await sameThrough1()))
  const two = withoutAllowed(wireOf(await sameThrough2()))
  const ended = Date.now()
  expect(one.spans.length).toBe(5)
  expect(two.logs.length).toBe(2)

  // Two more differences, asserted rather than removed. Each major names a file argument its
  // own way (filePath, path), so the arguments differ in that name and nothing else.
  for (const [a, b] of one.spans.map((s, i) => [s.attributes, two.spans[i].attributes])) {
    if (a["gen_ai.tool.call.arguments"] === undefined) continue
    const { filePath, ...rest } = JSON.parse(a["gen_ai.tool.call.arguments"])
    expect(JSON.parse(b["gen_ai.tool.call.arguments"])).toEqual({ path: filePath, ...rest })
    delete a["gen_ai.tool.call.arguments"]
    delete b["gen_ai.tool.call.arguments"]
  }
  // The title call starts when the plugin sees the request, on its own clock in both; it ends
  // at the rename's time on OpenCode 2 (never before it began) and on the plugin's clock on
  // OpenCode 1.
  const titleCall = (w) => w.spans.find((s) => s.attributes["opencode.call.kind"] === "title")
  const clock = (ns) => Number(BigInt(ns) / 1000000n)
  for (const w of [one, two]) {
    const t = titleCall(w)
    expect(clock(t.start)).toBeGreaterThanOrEqual(began)
    expect(clock(t.start)).toBeLessThanOrEqual(ended)
    if (w === two) expect(clock(t.end)).toBe(Math.max(SAME.title.end, clock(t.start)))
    else expect(clock(t.end)).toBeGreaterThanOrEqual(clock(t.start))
    delete t.start
    delete t.end
  }
  expect(two).toEqual(one)
})
