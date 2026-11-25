// Runs the plugin the way OpenCode would (CONFIG spliced in, hooks and events delivered in
// the order OpenCode delivered them) against a local OTLP receiver and a fake `terma`, and
// checks the wire that arrives and the hooks that ran.
//
// The replay tests feed testdata/1.19.6/<case>/plugin-view.jsonl, what a plugin saw in real
// OpenCode 1.19.6 sessions (testdata/PROVENANCE.md), through the OpenCode 1 entry.
//
// `bun test` in this folder.
import { test, expect, beforeAll, afterAll, describe } from "bun:test"
import { mkdtempSync, readFileSync, writeFileSync, mkdirSync, chmodSync, existsSync, rmSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"

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

// A fake `terma hook`: hooks run one at a time, so one log keeps their order. Each line is
// the event, a tab, and the JSON it got on stdin.
function fakeTerma(dir, { fail = [] } = {}) {
  const log = join(dir, "hooks.log")
  const path = join(dir, "terma")
  const failing = fail.map((e) => `[ "$2" = "${e}" ] && exit 1`).join("\n")
  writeFileSync(path, `#!/bin/sh\n{ printf '%s\\t' "$2"; cat; printf '\\n'; } >> '${log}'\n${failing}\nexit 0\n`)
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
async function load(config, { keep = false } = {}) {
  if (!keep) delete globalThis[KEY]
  const src = config === null ? SOURCE : SOURCE.replace(PLACEHOLDER, `const CONFIG = ${JSON.stringify(config)}`)
  if (config !== null) expect(src).not.toBe(SOURCE)
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
  for (const { index, rec } of items) {
    cursor = index
    if (rec.kind === "event") {
      await route(eventSession(rec)).event({ event: structuredClone(rec.data) })
    } else if (rec.kind === "hook" && rec.name === "config") {
      const cfg = mcp ? { mcp: Object.fromEntries(mcp.map((n) => [n, { type: "local", command: ["mcp"] }])) } : {}
      await instances[Math.min(configs++, instances.length - 1)].hooks.config(cfg)
    } else if (rec.kind === "hook" && rec.name !== "dispose") {
      // tool.execute.after is not registered any more: calling nothing proves it.
      await route(eventSession(rec))[rec.name]?.(structuredClone(rec.data.input), structuredClone(rec.data.output))
    }
  }
  for (const inst of instances) await inst.hooks.dispose()
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
  return { spans, logs, hooks: result.hooks }
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

async function start(overrides, { terma, client, directory = "/repo" } = {}) {
  const r = rig(overrides, terma)
  const mod = await load(r.config)
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
  await hooks.dispose()
  expect(warnings.length).toBe(1)
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
  // Hooks run whatever the export policy says.
  expect(r.terma.calls().filter((c) => c.event === "opencode-session-start").map((c) => c.payload.session_id)).toEqual(["ses_q", "ses_p", "ses_o"])
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
