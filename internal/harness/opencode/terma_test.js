// Runs the plugin the way OpenCode would — CONFIG spliced in, hooks called with the
// events OpenCode emits — against a local OTLP receiver, and checks what arrives.
// `bun test internal/harness/opencode`.
import { test, expect, beforeAll, afterAll } from "bun:test"
import { mkdtempSync, readFileSync, writeFileSync, mkdirSync, chmodSync, readdirSync, rmSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"

let server
let received = [] // { path, headers, body }
let dir
let helperPath
let hookLog

beforeAll(() => {
  dir = mkdtempSync(join(tmpdir(), "terma-opencode-"))
  server = Bun.serve({
    port: 0,
    async fetch(req) {
      received.push({ path: new URL(req.url).pathname, headers: Object.fromEntries(req.headers), body: await req.json() })
      return new Response("{}", { status: 200 })
    },
  })
  // The helper script: prints the Authorization header, exactly as terma writes it.
  helperPath = join(dir, "helper")
  writeFileSync(helperPath, `#!/bin/sh\necho '{"Authorization": "Bearer ter_srv_test"}'\n`)
  chmodSync(helperPath, 0o700)
  // A fake `terma hook`: one file per invocation (they run concurrently and detached,
  // so a shared log would interleave), holding the event name, a tab, and stdin.
  hookLog = join(dir, "hooks")
  mkdirSync(hookLog)
  const fakeTerma = join(dir, "terma")
  writeFileSync(fakeTerma, `#!/bin/sh\n{ printf '%s\\t' "$2"; cat; } > "${hookLog}/$$.$(date +%s%N)"\n`)
  chmodSync(fakeTerma, 0o700)
})
afterAll(() => server?.stop(true))

async function load(config, worktree) {
  const src = readFileSync(join(import.meta.dir, "terma.js"), "utf8")
  const line = `const CONFIG = ${JSON.stringify(config)}`
  const spliced = src.replace(/^const CONFIG = null \/\* terma:config \*\/$/m, line)
  expect(spliced).not.toBe(src)
  const path = join(dir, `plugin-${Math.random().toString(36).slice(2)}.js`)
  writeFileSync(path, spliced)
  const mod = await import(path)
  const exportsList = Object.keys(mod)
  // OpenCode's legacy loader treats every export as a plugin function.
  expect(exportsList).toEqual(["TermaPlugin"])
  return mod.TermaPlugin({ directory: worktree, worktree })
}

function baseConfig(overrides = {}) {
  return {
    version: 1,
    endpoint: `http://127.0.0.1:${server.port}`,
    headersHelper: helperPath,
    signals: ["traces", "logs", "metrics"],
    includePrompts: true,
    includeToolContent: true,
    resourceAttributes: { "mirador.project.id": "proj-1", "enduser.id": "dev@example.com" },
    hookCommand: [join(dir, "terma"), "hook"],
    ...overrides,
  }
}

const assistant = {
  id: "msg_a1",
  sessionID: "ses_1",
  role: "assistant",
  parentID: "msg_u1",
  modelID: "claude-opus-5",
  providerID: "anthropic",
  mode: "build",
  path: { cwd: "/repo", root: "/repo" },
  cost: 0.0421,
  tokens: { input: 1200, output: 340, reasoning: 20, cache: { read: 800, write: 100 } },
  finish: "stop",
  time: { created: 1_757_400_000_000, completed: 1_757_400_004_500 },
}

test("model calls become spans with tokens, cost and the session as trace", async () => {
  received = []
  const hooks = await load(baseConfig(), dir)
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_1", projectID: "p", directory: "/repo", title: "fix the build", version: "1.2.3" } } } })
  await hooks.event({ event: { type: "message.part.updated", properties: { part: { type: "text", messageID: "msg_a1", sessionID: "ses_1", text: "I fixed it." } } } })
  await hooks.event({ event: { type: "message.updated", properties: { info: assistant } } })
  // A second update of the same completed message must not double count.
  await hooks.event({ event: { type: "message.updated", properties: { info: assistant } } })
  await hooks.dispose()

  const traces = received.filter((r) => r.path === "/v1/traces")
  expect(traces.length).toBe(1)
  expect(traces[0].headers["authorization"]).toBe("Bearer ter_srv_test")
  expect(traces[0].headers["content-type"]).toBe("application/json")
  const rs = traces[0].body.resourceSpans[0]
  const res = Object.fromEntries(rs.resource.attributes.map((a) => [a.key, a.value.stringValue]))
  expect(res["service.name"]).toBe("opencode")
  expect(res["mirador.project.id"]).toBe("proj-1")
  expect(rs.scopeSpans[0].scope.name).toBe("terma-opencode")
  const spans = rs.scopeSpans[0].spans
  expect(spans.length).toBe(1)
  const s = spans[0]
  expect(s.name).toBe("chat claude-opus-5")
  expect(s.kind).toBe(3)
  expect(s.traceId).toMatch(/^[0-9a-f]{32}$/)
  expect(s.spanId).toMatch(/^[0-9a-f]{16}$/)
  expect(s.startTimeUnixNano).toBe("1757400000000000000")
  const a = Object.fromEntries(s.attributes.map((x) => [x.key, x.value]))
  expect(a["gen_ai.usage.input_tokens"]).toEqual({ intValue: "1200" })
  expect(a["gen_ai.usage.output_tokens"]).toEqual({ intValue: "340" })
  expect(a["gen_ai.usage.cache_read.input_tokens"]).toEqual({ intValue: "800" })
  expect(a["gen_ai.usage.total_cost"]).toEqual({ doubleValue: 0.0421 })
  expect(a["gen_ai.request.model"]).toEqual({ stringValue: "claude-opus-5" })
  expect(a["gen_ai.provider.name"]).toEqual({ stringValue: "anthropic" })
  expect(a["session.id"]).toEqual({ stringValue: "ses_1" })
  expect(a["gen_ai.completion"]).toEqual({ stringValue: "I fixed it." })

  const logs = received.filter((r) => r.path === "/v1/logs")
  expect(logs.length).toBe(1)
  const recs = logs[0].body.resourceLogs[0].scopeLogs[0].logRecords
  const created = recs.find((r) => r.attributes.some((x) => x.key === "event.name" && x.value.stringValue === "opencode.session.created"))
  expect(created).toBeDefined()
  expect(created.body.stringValue).toBe("fix the build")
  expect(created.traceId).toBe(s.traceId)
})

test("prompts and tool content stay home when excluded", async () => {
  received = []
  const hooks = await load(baseConfig({ includePrompts: false, includeToolContent: false }), dir)
  await hooks["chat.message"]({ sessionID: "ses_2", agent: "build", model: { providerID: "openai", modelID: "gpt-5" }, messageID: "msg_u2" }, { message: { id: "msg_u2" }, parts: [{ type: "text", text: "delete everything" }] })
  await hooks["tool.execute.before"]({ tool: "bash", sessionID: "ses_2", callID: "call_1" }, { args: { command: "rm -rf /" } })
  await hooks["tool.execute.after"]({ tool: "bash", sessionID: "ses_2", callID: "call_1", args: { command: "rm -rf /" } }, { title: "rm", output: "secret output", metadata: {} })
  await hooks.event({ event: { type: "message.part.updated", properties: { part: { type: "text", messageID: "msg_a2", sessionID: "ses_2", text: "done" } } } })
  await hooks.event({ event: { type: "message.updated", properties: { info: { ...assistant, id: "msg_a2", sessionID: "ses_2" } } } })
  await hooks.dispose()

  const everything = JSON.stringify(received)
  expect(everything).not.toContain("delete everything")
  expect(everything).not.toContain("rm -rf")
  expect(everything).not.toContain("secret output")
  expect(everything).not.toContain('"done"')
  // The shape survives: the prompt event and tool span are there, with lengths only.
  const recs = received.find((r) => r.path === "/v1/logs").body.resourceLogs[0].scopeLogs[0].logRecords
  const prompt = recs.find((r) => r.attributes.some((x) => x.value.stringValue === "opencode.user_prompt"))
  expect(prompt.body.stringValue).toBe("")
  expect(prompt.attributes.find((x) => x.key === "opencode.prompt.length").value).toEqual({ intValue: "17" })
  const spans = received.find((r) => r.path === "/v1/traces").body.resourceSpans[0].scopeSpans[0].spans
  const tool = spans.find((s) => s.name === "execute_tool bash")
  expect(tool).toBeDefined()
  expect(tool.attributes.find((x) => x.key === "opencode.tool.output_bytes").value).toEqual({ intValue: "13" })
  expect(tool.attributes.some((x) => x.key === "gen_ai.tool.call.result")).toBe(false)
})

test("signals off means nothing is sent, and a repository policy overrides the global one", async () => {
  received = []
  const wt = mkdtempSync(join(tmpdir(), "terma-wt-"))
  mkdirSync(join(wt, ".opencode"))
  writeFileSync(join(wt, ".opencode", "terma.json"), JSON.stringify({ signals: [], includePrompts: false }))
  const hooks = await load(baseConfig(), wt)
  await hooks.event({ event: { type: "message.updated", properties: { info: { ...assistant, id: "msg_a3" } } } })
  await hooks.dispose()
  expect(received.length).toBe(0)
})

test("session and file events reach terma hook with the session attached", async () => {
  received = []
  for (const f of readdirSync(hookLog)) rmSync(join(hookLog, f))
  const hooks = await load(baseConfig(), dir)
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_9", directory: "/repo" } } } })
  await hooks["tool.execute.before"]({ tool: "edit", sessionID: "ses_9", callID: "c9" }, { args: { filePath: "/repo/src/a.go" } })
  await hooks["tool.execute.after"]({ tool: "edit", sessionID: "ses_9", callID: "c9", args: { filePath: "/repo/src/a.go" } }, { title: "a.go", output: "ok", metadata: {} })
  await hooks.event({ event: { type: "file.edited", properties: { file: "/repo/src/b.go" } } })
  await hooks.event({ event: { type: "session.deleted", properties: { info: { id: "ses_9", directory: "/repo" } } } })
  await hooks.dispose()
  // The hook processes are detached; give them a moment.
  await new Promise((r) => setTimeout(r, 500))
  const calls = readdirSync(hookLog).map((f) => {
    const [event, payload] = readFileSync(join(hookLog, f), "utf8").split("\t")
    return { event, payload: JSON.parse(payload) }
  })
  expect(calls.length).toBe(4)
  expect(calls).toContainEqual({ event: "opencode-session-start", payload: { session_id: "ses_9", cwd: "/repo" } })
  expect(calls).toContainEqual({ event: "opencode-session-end", payload: { session_id: "ses_9", cwd: "/repo", reason: "deleted" } })
  const edits = calls.filter((c) => c.event === "opencode-file-edit").map((c) => c.payload)
  expect(edits.length).toBe(2)
  expect(edits).toContainEqual({ session_id: "ses_9", cwd: dir, file: "/repo/src/a.go", tool: "edit" })
  expect(edits).toContainEqual({ session_id: "ses_9", cwd: dir, file: "/repo/src/b.go" })
})

test("a session the task tool opened names its parent", async () => {
  received = []
  for (const f of readdirSync(hookLog)) rmSync(join(hookLog, f))
  const hooks = await load(baseConfig(), dir)
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_child", parentID: "ses_parent", directory: "/repo" } } } })
  await hooks.dispose()
  await new Promise((r) => setTimeout(r, 500))
  const calls = readdirSync(hookLog).map((f) => {
    const [event, payload] = readFileSync(join(hookLog, f), "utf8").split("\t")
    return { event, payload: JSON.parse(payload) }
  })
  expect(calls).toContainEqual({ event: "opencode-session-start", payload: { session_id: "ses_child", cwd: "/repo", parent_session_id: "ses_parent" } })
  const recs = received.filter((r) => r.path === "/v1/logs")[0].body.resourceLogs[0].scopeLogs[0].logRecords
  const created = recs.find((r) => r.attributes.some((x) => x.key === "event.name" && x.value.stringValue === "opencode.session.created"))
  const a = Object.fromEntries(created.attributes.map((x) => [x.key, x.value]))
  expect(a["opencode.parent_session.id"]).toEqual({ stringValue: "ses_parent" })
})

test("a child session's spans name its parent, so the link survives a traces-only policy", async () => {
  received = []
  const hooks = await load(baseConfig(), dir)
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_child2", parentID: "ses_parent2", directory: "/repo" } } } })
  await hooks["tool.execute.before"]({ tool: "bash", sessionID: "ses_child2", callID: "call_c" }, { args: { command: "ls" } })
  await hooks["tool.execute.after"]({ tool: "bash", sessionID: "ses_child2", callID: "call_c", args: { command: "ls" } }, { title: "ls", output: "a", metadata: {} })
  await hooks.event({ event: { type: "message.updated", properties: { info: { ...assistant, id: "msg_c1", sessionID: "ses_child2" } } } })
  // A session with no parent says nothing about one.
  await hooks["tool.execute.before"]({ tool: "bash", sessionID: "ses_root2", callID: "call_r" }, { args: { command: "ls" } })
  await hooks["tool.execute.after"]({ tool: "bash", sessionID: "ses_root2", callID: "call_r", args: { command: "ls" } }, { title: "ls", output: "a", metadata: {} })
  await hooks.dispose()

  const spans = received.filter((r) => r.path === "/v1/traces").flatMap((r) => r.body.resourceSpans[0].scopeSpans[0].spans)
  const attr = (span, key) => span.attributes.find((x) => x.key === key)?.value
  const sessionOf = (span) => attr(span, "session.id")?.stringValue
  const child = spans.filter((s) => sessionOf(s) === "ses_child2")
  expect(child.length).toBeGreaterThanOrEqual(2)
  for (const span of child) expect(attr(span, "opencode.parent_session.id")).toEqual({ stringValue: "ses_parent2" })
  for (const span of spans.filter((s) => sessionOf(s) === "ses_root2")) expect(attr(span, "opencode.parent_session.id")).toBeUndefined()
})

test("a null CONFIG makes the plugin inert", async () => {
  const src = readFileSync(join(import.meta.dir, "terma.js"), "utf8")
  const path = join(dir, "raw.js")
  writeFileSync(path, src)
  const mod = await import(path)
  const hooks = await mod.TermaPlugin({ directory: dir, worktree: dir })
  expect(Object.keys(hooks)).toEqual([])
})

// Per-repo routing: one global plugin, each session pointed at whatever project its
// repository is bound to, with that project's own key.
function perRepoConfig(overrides = {}) {
  const cfg = baseConfig({
    perRepo: true,
    helpersDir: dir,
    helperPrefix: "opencode-otel-",
    projectAttribute: "mirador.project.id",
    resourceAttributes: { "enduser.id": "dev@example.com" },
    ...overrides,
  })
  delete cfg.headersHelper // resolved per repository, not fixed
  return cfg
}

test("per-repo mode resolves the project and key from the repository binding", async () => {
  received = []
  const wt = mkdtempSync(join(tmpdir(), "terma-wt-bound-"))
  mkdirSync(join(wt, ".terma"), { recursive: true })
  writeFileSync(join(wt, ".terma", "settings.json"), JSON.stringify({ project: { id: "proj-42" } }))
  const perHelper = join(dir, "opencode-otel-proj-42")
  writeFileSync(perHelper, `#!/bin/sh\necho '{"Authorization": "Bearer ter_srv_proj42"}'\n`)
  chmodSync(perHelper, 0o700)

  const hooks = await load(perRepoConfig(), wt)
  await hooks.event({ event: { type: "message.updated", properties: { info: assistant } } })
  await hooks.dispose()

  const traces = received.filter((r) => r.path === "/v1/traces")
  expect(traces.length).toBe(1)
  expect(traces[0].headers["authorization"]).toBe("Bearer ter_srv_proj42")
  const res = Object.fromEntries(traces[0].body.resourceSpans[0].resource.attributes.map((a) => [a.key, a.value.stringValue]))
  expect(res["mirador.project.id"]).toBe("proj-42")
  expect(res["service.name"]).toBe("opencode")
})

// A new git worktree does not get a gitignored binding. It is the same repository, so it
// reports to its main checkout's project — through git's own link, never nesting.
test("per-repo mode resolves a linked worktree through its main checkout", async () => {
  received = []
  const base = mkdtempSync(join(tmpdir(), "terma-wt-linked-"))
  const main = join(base, "main")
  mkdirSync(join(main, ".git", "worktrees", "feature"), { recursive: true })
  mkdirSync(join(main, ".terma"), { recursive: true })
  writeFileSync(join(main, ".terma", "settings.json"), JSON.stringify({ project: { id: "proj-77" } }))
  writeFileSync(join(main, ".git", "worktrees", "feature", "commondir"), "../..\n")
  const wt = join(base, "feature")
  mkdirSync(wt)
  writeFileSync(join(wt, ".git"), `gitdir: ${join(main, ".git", "worktrees", "feature")}\n`)
  const perHelper = join(dir, "opencode-otel-proj-77")
  writeFileSync(perHelper, `#!/bin/sh\necho '{"Authorization": "Bearer ter_srv_proj77"}'\n`)
  chmodSync(perHelper, 0o700)

  const hooks = await load(perRepoConfig(), wt)
  await hooks.event({ event: { type: "message.updated", properties: { info: assistant } } })
  await hooks.dispose()

  const traces = received.filter((r) => r.path === "/v1/traces")
  expect(traces.length).toBe(1)
  expect(traces[0].headers["authorization"]).toBe("Bearer ter_srv_proj77")
  const res = Object.fromEntries(traces[0].body.resourceSpans[0].resource.attributes.map((a) => [a.key, a.value.stringValue]))
  expect(res["mirador.project.id"]).toBe("proj-77")
})

test("per-repo mode stays inert in a repository with no terma binding", async () => {
  received = []
  const wt = mkdtempSync(join(tmpdir(), "terma-wt-unbound-"))
  const hooks = await load(perRepoConfig(), wt)
  expect(Object.keys(hooks)).toEqual([])
  expect(received.length).toBe(0)
})

// The binding is committed, so its id is whatever the repository's author wrote — and it
// names the helper the plugin executes. One that escapes the helpers directory must not
// be followed.
test("per-repo mode refuses a project id that escapes the helpers directory", async () => {
  received = []
  const wt = mkdtempSync(join(tmpdir(), "terma-wt-hostile-"))
  const marker = join(wt, "ran")
  const evil = join(wt, "evil.sh")
  writeFileSync(evil, `#!/bin/sh\ntouch '${marker}'\necho '{"Authorization": "Bearer stolen"}'\n`)
  chmodSync(evil, 0o700)
  mkdirSync(join(wt, ".terma"), { recursive: true })
  // Resolves, through join(), from the helpers directory to the script in the repository.
  const id = "x/" + "../".repeat(40) + evil.slice(1)
  writeFileSync(join(wt, ".terma", "settings.json"), JSON.stringify({ project: { id } }))

  const hooks = await load(perRepoConfig(), wt)
  expect(Object.keys(hooks)).toEqual([])
  expect(received.length).toBe(0)
  expect(readdirSync(wt)).not.toContain("ran")
})

test("per-repo mode ignores an unsupported TOML binding", async () => {
  received = []
  const wt = mkdtempSync(join(tmpdir(), "terma-wt-toml-"))
  writeFileSync(join(wt, ".terma.toml"), "[project]\nid = 'unselected'\n")
  const hooks = await load(perRepoConfig(), wt)
  expect(Object.keys(hooks)).toEqual([])
  expect(received.length).toBe(0)
})
