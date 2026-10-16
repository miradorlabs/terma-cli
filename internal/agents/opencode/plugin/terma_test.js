// Runs the plugin the way OpenCode would — CONFIG spliced in, hooks called with the
// events OpenCode emits — against a local OTLP receiver, and checks what arrives.
// `bun test internal/agents/opencode/plugin`.
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

async function importPlugin(config) {
  const src = readFileSync(join(import.meta.dir, "terma.js"), "utf8")
  const line = `const CONFIG = ${JSON.stringify(config)}`
  const spliced = src.replace(/^const CONFIG = null \/\* terma:config \*\/$/m, line)
  expect(spliced).not.toBe(src)
  // Bun caches directory listings during module resolution. Give each generated
  // module a fresh directory so later imports see files created after the first.
  const path = join(mkdtempSync(join(dir, "module-")), "plugin.js")
  writeFileSync(path, spliced)
  const mod = await import(path)
  // OpenCode 2 reads the default export alone; OpenCode 1 treats every export as a plugin.
  expect(Object.keys(mod)).toEqual(["default"])
  expect(mod.default.id).toBe("terma")
  return mod.default
}

// load runs the plugin as OpenCode 1 does: server() and the hooks it returns.
async function load(config, worktree) {
  return (await importPlugin(config)).server({ directory: worktree, worktree })
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

test("a subagent's OpenRouter request names its parent and agent in the trace field", async () => {
  const hooks = await load(baseConfig(), dir)
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "ses_child3", parentID: "ses_parent3", directory: "/repo" } } } })
  const openrouter = { api: { npm: "@openrouter/ai-sdk-provider" } }
  const params = async (sessionID, model, provider = { options: {} }, options = { usage: { include: true } }) => {
    const output = { temperature: 0, topP: 1, topK: 0, maxOutputTokens: undefined, options }
    await hooks["chat.params"]({ sessionID, agent: "explore", model, provider, message: {} }, output)
    return output.options
  }

  expect(await params("ses_child3", openrouter)).toEqual({ usage: { include: true }, trace: { parent_session_id: "ses_parent3", agent: "explore" } })
  // A trace the developer configured, on the provider's extraBody or the model's options, is kept.
  expect(
    await params("ses_child3", openrouter, { options: { extraBody: { trace: { trace_id: "t1", environment: "ci" } } } }, { trace: { environment: "staging" } }),
  ).toEqual({ trace: { trace_id: "t1", environment: "staging", parent_session_id: "ses_parent3", agent: "explore" } })
  // A session with no parent, or a provider other than OpenRouter, sends its request unchanged.
  expect(await params("ses_root3", openrouter)).toEqual({ usage: { include: true } })
  expect(await params("ses_child3", { api: { npm: "@ai-sdk/anthropic" } })).toEqual({ usage: { include: true } })
  await hooks.dispose()
})

test("a null CONFIG makes the plugin inert", async () => {
  const src = readFileSync(join(import.meta.dir, "terma.js"), "utf8")
  const path = join(mkdtempSync(join(dir, "module-")), "raw.js")
  writeFileSync(path, src)
  const mod = await import(path)
  const hooks = await mod.default.server({ directory: dir, worktree: dir })
  expect(Object.keys(hooks)).toEqual([])
  expect(await mod.default.setup(fakeContext(dir).ctx)).toBeUndefined()
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

// Same order as the Go side: a binding anywhere above is found before a linked worktree
// falls back to its main checkout's.
test("per-repo mode prefers a binding above the worktree over its main checkout's", async () => {
  received = []
  const base = mkdtempSync(join(tmpdir(), "terma-wt-order-"))
  const main = join(base, "main")
  mkdirSync(join(main, ".git", "worktrees", "feature"), { recursive: true })
  mkdirSync(join(main, ".terma"), { recursive: true })
  writeFileSync(join(main, ".terma", "settings.json"), JSON.stringify({ project: { id: "proj-main" } }))
  writeFileSync(join(main, ".git", "worktrees", "feature", "commondir"), "../..\n")
  const outer = join(base, "outer")
  mkdirSync(join(outer, ".terma"), { recursive: true })
  writeFileSync(join(outer, ".terma", "settings.json"), JSON.stringify({ project: { id: "proj-outer" } }))
  const wt = join(outer, "feature")
  mkdirSync(wt)
  writeFileSync(join(wt, ".git"), `gitdir: ${join(main, ".git", "worktrees", "feature")}\n`)
  const perHelper = join(dir, "opencode-otel-proj-outer")
  writeFileSync(perHelper, `#!/bin/sh\necho '{"Authorization": "Bearer ter_srv_outer"}'\n`)
  chmodSync(perHelper, 0o700)

  const hooks = await load(perRepoConfig(), wt)
  await hooks.event({ event: { type: "message.updated", properties: { info: assistant } } })
  await hooks.dispose()
  const traces = received.filter((r) => r.path === "/v1/traces")
  expect(traces.length).toBe(1)
  expect(traces[0].headers["authorization"]).toBe("Bearer ter_srv_outer")
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

// --- OpenCode 2 ---------------------------------------------------------------------

// fakeContext is the slice of OpenCode 2's plugin context the plugin uses, shaped as
// @opencode/plugin's types declare it: hooks registered by name, and one event stream the
// whole server shares, which emit feeds to every subscriber.
function fakeContext(directory, project = directory) {
  const hooks = {}
  const options = {}
  const subscribers = new Set()
  const domain = (name) => ({
    hook: async (hook, callback, opts) => {
      hooks[`${name}.${hook}`] = callback
      options[`${name}.${hook}`] = opts
      return { dispose: async () => {} }
    },
  })
  const ctx = {
    app: { name: "cli", version: "2.0.30", channel: "latest" },
    location: { directory, project: { id: "global", directory: project, canonical: project } },
    options: {},
    session: domain("session"),
    tool: domain("tool"),
    event: {
      subscribe({ signal } = {}) {
        const queue = []
        let wake = null
        const sub = {
          push(event) {
            queue.push(event)
            wake?.()
          },
        }
        subscribers.add(sub)
        return {
          async *[Symbol.asyncIterator]() {
            try {
              while (!signal?.aborted) {
                if (queue.length === 0) {
                  await new Promise((r) => {
                    wake = r
                    signal?.addEventListener("abort", r, { once: true })
                  })
                  wake = null
                  continue
                }
                yield queue.shift()
              }
            } finally {
              subscribers.delete(sub)
            }
          },
        }
      },
    },
  }
  return { ctx, hooks, options, subscribers }
}

// server is OpenCode 2's shared server: every location's plugin instance hears every event.
function fakeServer() {
  const contexts = []
  let seq = 0
  return {
    location(directory, project) {
      const c = fakeContext(directory, project)
      contexts.push(c)
      return c
    },
    async emit(type, data, directory) {
      const event = { id: `evt_${++seq}`, created: Date.now(), type, data }
      if (directory) event.location = { directory }
      for (const c of contexts) for (const s of c.subscribers) s.push(event)
      await new Promise((r) => setTimeout(r, 5))
    },
  }
}

async function setupPlugin(config, c) {
  const plugin = await importPlugin(config)
  const cleanup = await plugin.setup(c.ctx)
  // Let the event loop open its subscription.
  await new Promise((r) => setTimeout(r, 5))
  return cleanup
}

async function hookCalls() {
  // The hook processes are detached; give them a moment.
  await new Promise((r) => setTimeout(r, 500))
  return readdirSync(hookLog).map((f) => {
    const [event, payload] = readFileSync(join(hookLog, f), "utf8").split("\t")
    return { event, payload: JSON.parse(payload) }
  })
}

function clearHooks() {
  for (const f of readdirSync(hookLog)) rmSync(join(hookLog, f))
}

// What a session sent, with the parts that differ between two runs of it taken out:
// clock times, and the tool arguments, which are each OpenCode's own tool's.
function sent() {
  const strip = (attributes) =>
    attributes.filter((a) => a.key !== "gen_ai.tool.call.arguments").sort((a, b) => a.key.localeCompare(b.key))
  const spans = received
    .filter((r) => r.path === "/v1/traces")
    .flatMap((r) => r.body.resourceSpans.flatMap((rs) => rs.scopeSpans.flatMap((ss) => ss.spans)))
    .map(({ startTimeUnixNano, endTimeUnixNano, ...s }) => ({ ...s, attributes: strip(s.attributes) }))
    .sort((a, b) => a.spanId.localeCompare(b.spanId))
  const name = (r) => r.attributes.find((a) => a.key === "event.name").value.stringValue
  const records = received
    .filter((r) => r.path === "/v1/logs")
    .flatMap((r) => r.body.resourceLogs.flatMap((rl) => rl.scopeLogs.flatMap((sl) => sl.logRecords)))
    .map(({ timeUnixNano, ...r }) => ({ ...r, attributes: strip(r.attributes) }))
    .sort((a, b) => name(a).localeCompare(name(b)))
  const resources = received.map((r) => JSON.stringify((r.body.resourceSpans ?? r.body.resourceLogs)[0].resource))
  return { spans, records, resources: [...new Set(resources)] }
}

const sortCalls = (calls) => calls.map((c) => JSON.stringify(c)).sort()

const usage = { cost: 0.0421, tokens: { input: 1200, output: 340, reasoning: 20, cache: { read: 800, write: 100 } } }

test("an OpenCode 2 session sends what the same OpenCode 1 session sends", async () => {
  const file = join(dir, "src", "a.go")

  // OpenCode 1.
  received = []
  clearHooks()
  const v1 = await load(baseConfig(), dir)
  await v1.event({ event: { type: "session.created", properties: { info: { id: "ses_eq", projectID: "global", directory: dir, version: "2.0.30" } } } })
  await v1["chat.message"]({ sessionID: "ses_eq", agent: "build", model: { providerID: "anthropic", modelID: "claude-opus-5" }, messageID: "msg_u1" }, { message: { id: "msg_u1" }, parts: [{ type: "text", text: "fix the build" }] })
  await v1["tool.execute.before"]({ tool: "edit", sessionID: "ses_eq", callID: "call_1" }, { args: { filePath: file } })
  await v1["tool.execute.after"]({ tool: "edit", sessionID: "ses_eq", callID: "call_1", args: { filePath: file } }, { output: "Edited src/a.go", metadata: {} })
  await v1.event({ event: { type: "message.part.updated", properties: { part: { type: "text", messageID: "msg_a1", sessionID: "ses_eq", text: "I fixed it." } } } })
  await v1.event({ event: { type: "message.updated", properties: { info: { ...assistant, ...usage, sessionID: "ses_eq", parentID: "msg_u1" } } } })
  await v1.event({ event: { type: "session.idle", properties: { sessionID: "ses_eq" } } })
  await v1.event({ event: { type: "session.deleted", properties: { info: { id: "ses_eq", directory: dir } } } })
  await v1.dispose()
  const one = sent()
  const oneCalls = await hookCalls()

  // OpenCode 2.
  received = []
  clearHooks()
  const server = fakeServer()
  const c = server.location(dir)
  const cleanup = await setupPlugin(baseConfig(), c)
  await server.emit("session.created", { sessionID: "ses_eq", projectID: "global", location: { directory: dir }, subpath: "", slug: "s", version: "2.0.30" }, dir)
  await c.hooks["session.prompt"]({ sessionID: "ses_eq", messageID: "msg_u1", prompt: { text: "fix the build", files: [] }, delivery: "steer" })
  await server.emit("session.execution.started", { sessionID: "ses_eq" })
  await server.emit("session.step.started", { sessionID: "ses_eq", assistantMessageID: "msg_a1", agent: "build", model: { id: "claude-opus-5", providerID: "anthropic" }, started: Date.now() }, dir)
  await c.hooks["tool.execute.before"]({ tool: "edit", sessionID: "ses_eq", agent: "build", messageID: "msg_a1", id: "call_1", input: { path: "src/a.go", oldString: "a", newString: "b" } })
  await c.hooks["tool.execute.after"]({ tool: "edit", sessionID: "ses_eq", agent: "build", messageID: "msg_a1", id: "call_1", input: { path: "src/a.go", oldString: "a", newString: "b" }, status: "completed", result: { content: [{ type: "text", text: "Edited src/a.go" }] } })
  await server.emit("session.text.ended", { sessionID: "ses_eq", assistantMessageID: "msg_a1", ordinal: 0, text: "I fixed it." }, dir)
  await server.emit("session.step.ended", { sessionID: "ses_eq", assistantMessageID: "msg_a1", finish: "stop", ...usage, files: ["src/a.go"] }, dir)
  await server.emit("session.usage.updated", { sessionID: "ses_eq", ...usage })
  await server.emit("session.execution.succeeded", { sessionID: "ses_eq" })
  await server.emit("session.deleted", { sessionID: "ses_eq" })
  await cleanup()
  const two = sent()
  const twoCalls = await hookCalls()

  expect(one.spans.length).toBe(2)
  expect(one.records.map((r) => r.attributes.find((a) => a.key === "event.name").value.stringValue)).toEqual([
    "opencode.session.created",
    "opencode.session.deleted",
    "opencode.session.idle",
    "opencode.user_prompt",
  ])
  expect(two).toEqual(one)
  expect(oneCalls.length).toBe(3)
  expect(sortCalls(twoCalls)).toEqual(sortCalls(oneCalls))
  expect(twoCalls).toContainEqual({ event: "opencode-file-edit", payload: { session_id: "ses_eq", cwd: dir, file, tool: "edit" } })

  // The arguments are OpenCode 2's own.
  const tool = received
    .filter((r) => r.path === "/v1/traces")
    .flatMap((r) => r.body.resourceSpans[0].scopeSpans[0].spans)
    .find((s) => s.name === "execute_tool edit")
  const args = tool.attributes.find((a) => a.key === "gen_ai.tool.call.arguments").value.stringValue
  expect(JSON.parse(args)).toEqual({ path: "src/a.go", oldString: "a", newString: "b" })
})

test("an OpenCode 2 instance takes only its own location's sessions", async () => {
  received = []
  clearHooks()
  const server = fakeServer()
  const here = server.location(dir)
  const other = mkdtempSync(join(tmpdir(), "terma-other-"))
  server.location(other) // the other location's instance: no plugin loaded there
  const cleanup = await setupPlugin(baseConfig(), here)
  await server.emit("session.created", { sessionID: "ses_there", projectID: "global", location: { directory: other }, version: "2.0.30" }, other)
  await server.emit("session.step.started", { sessionID: "ses_there", assistantMessageID: "msg_x", agent: "build", model: { id: "m", providerID: "p" }, started: Date.now() }, other)
  await server.emit("session.step.ended", { sessionID: "ses_there", assistantMessageID: "msg_x", finish: "stop", ...usage }, other)
  await server.emit("session.execution.succeeded", { sessionID: "ses_there" })
  await server.emit("session.deleted", { sessionID: "ses_there" })
  // A session placed here only by an event that carries its location is still this one's.
  await server.emit("session.step.started", { sessionID: "ses_resumed", assistantMessageID: "msg_y", agent: "build", model: { id: "m", providerID: "p" }, started: Date.now() }, dir)
  await server.emit("session.step.ended", { sessionID: "ses_resumed", assistantMessageID: "msg_y", finish: "stop", ...usage }, dir)
  await cleanup()

  expect(JSON.stringify(received)).not.toContain("ses_there")
  const spans = received.filter((r) => r.path === "/v1/traces").flatMap((r) => r.body.resourceSpans[0].scopeSpans[0].spans)
  expect(spans.map((s) => s.name)).toEqual(["chat m"])
  expect(await hookCalls()).toEqual([])
})

test("an OpenCode 2 tool's path is the session's, and a written file's own path wins", async () => {
  received = []
  clearHooks()
  const server = fakeServer()
  const c = server.location(dir)
  const cleanup = await setupPlugin(baseConfig(), c)
  await server.emit("session.created", { sessionID: "ses_sub", projectID: "global", location: { directory: dir }, subpath: "pkg", version: "2.0.30" }, dir)
  const call = (id, tool, input, result, status = "completed") =>
    c.hooks["tool.execute.after"]({ tool, sessionID: "ses_sub", agent: "build", messageID: "msg_1", id, input, status, ...(status === "completed" ? { result } : { error: { message: "no such file" } }) })
  await call("c1", "edit", { path: "x.go", oldString: "a", newString: "b" }, { content: "ok" })
  await call("c2", "write", { path: "y.go", content: "z" }, { output: { operation: "write", target: "/elsewhere/y.go", resource: "y.go" }, content: [{ type: "text", text: "Created" }] })
  await call("c3", "write", { path: "/abs/z.go", content: "z" }, { content: "ok" })
  await call("c4", "edit", { path: "gone.go", oldString: "a", newString: "b" }, undefined, "error")
  await call("c5", "read", { path: "r.go" }, { content: "text" })
  await cleanup()

  const edits = (await hookCalls()).filter((h) => h.event === "opencode-file-edit").map((h) => h.payload)
  expect(edits.length).toBe(3)
  expect(edits).toContainEqual({ session_id: "ses_sub", cwd: dir, file: join(dir, "pkg", "x.go"), tool: "edit" })
  expect(edits).toContainEqual({ session_id: "ses_sub", cwd: dir, file: "/elsewhere/y.go", tool: "write" })
  expect(edits).toContainEqual({ session_id: "ses_sub", cwd: dir, file: "/abs/z.go", tool: "write" })
  // The failed call still has its span, its error as the output.
  const spans = received.filter((r) => r.path === "/v1/traces").flatMap((r) => r.body.resourceSpans[0].scopeSpans[0].spans)
  const failed = spans.find((s) => s.attributes.some((a) => a.key === "gen_ai.tool.call.id" && a.value.stringValue === "c4"))
  expect(failed.attributes.find((a) => a.key === "gen_ai.tool.call.result").value).toEqual({ stringValue: "no such file" })
})

test("an OpenCode 2 subagent's OpenRouter request names its parent and agent in the trace field", async () => {
  const server = fakeServer()
  const c = server.location(dir)
  const cleanup = await setupPlugin(baseConfig(), c)
  expect(c.options["session.context"]).toEqual({ providerID: "openrouter" })
  await server.emit("session.created", { sessionID: "ses_child4", parentID: "ses_parent4", projectID: "global", location: { directory: dir }, version: "2.0.30" }, dir)
  const context = async (sessionID, providerID, options) => {
    const input = { sessionID, agent: "explore", model: { id: "m", providerID }, system: [], messages: [], options, tools: {} }
    await c.hooks["session.context"](input)
    return input.options
  }
  expect(await context("ses_child4", "openrouter", { usage: { include: true } })).toEqual({ usage: { include: true }, trace: { parent_session_id: "ses_parent4", agent: "explore" } })
  expect(await context("ses_child4", "openrouter", { trace: { environment: "staging" } })).toEqual({ trace: { environment: "staging", parent_session_id: "ses_parent4", agent: "explore" } })
  expect(await context("ses_root4", "openrouter", { usage: { include: true } })).toEqual({ usage: { include: true } })
  expect(await context("ses_child4", "anthropic", { usage: { include: true } })).toEqual({ usage: { include: true } })
  await cleanup()
})

test("an OpenCode 2 failure is the model call's status and the session's error", async () => {
  received = []
  const server = fakeServer()
  const c = server.location(dir)
  const cleanup = await setupPlugin(baseConfig(), c)
  await server.emit("session.step.started", { sessionID: "ses_err", assistantMessageID: "msg_e", agent: "build", model: { id: "m", providerID: "p" }, started: Date.now() }, dir)
  await server.emit("session.step.failed", { sessionID: "ses_err", assistantMessageID: "msg_e", error: { type: "provider.auth", message: "bad key" } }, dir)
  await server.emit("session.execution.failed", { sessionID: "ses_err", error: { type: "provider.auth", message: "bad key" } })
  await cleanup()

  const span = received.find((r) => r.path === "/v1/traces").body.resourceSpans[0].scopeSpans[0].spans[0]
  expect(span.status).toEqual({ code: 2, message: "bad key" })
  expect(span.attributes.find((a) => a.key === "error.type").value).toEqual({ stringValue: "provider.auth" })
  const recs = received.find((r) => r.path === "/v1/logs").body.resourceLogs[0].scopeLogs[0].logRecords
  const names = recs.map((r) => r.attributes.find((a) => a.key === "event.name").value.stringValue)
  expect(names).toEqual(["opencode.session.error", "opencode.session.idle"])
  expect(recs[0].body.stringValue).toBe("bad key")
  expect(recs[0].severityNumber).toBe(17)
})

test("OpenCode 2 prompts and tool content stay home when excluded", async () => {
  received = []
  const server = fakeServer()
  const c = server.location(dir)
  const cleanup = await setupPlugin(baseConfig({ includePrompts: false, includeToolContent: false }), c)
  await c.hooks["session.prompt"]({ sessionID: "ses_q", messageID: "msg_q", prompt: { text: "delete everything", files: [] }, delivery: "steer" })
  await server.emit("session.step.started", { sessionID: "ses_q", assistantMessageID: "msg_qa", agent: "build", model: { id: "gpt-5", providerID: "openai" }, started: Date.now() }, dir)
  await c.hooks["tool.execute.after"]({ tool: "shell", sessionID: "ses_q", agent: "build", messageID: "msg_qa", id: "call_q", input: { command: "rm -rf /" }, status: "completed", result: { content: "secret output" } })
  await server.emit("session.text.ended", { sessionID: "ses_q", assistantMessageID: "msg_qa", ordinal: 0, text: "done" }, dir)
  await server.emit("session.step.ended", { sessionID: "ses_q", assistantMessageID: "msg_qa", finish: "stop", ...usage }, dir)
  await cleanup()

  const everything = JSON.stringify(received)
  expect(everything).not.toContain("delete everything")
  expect(everything).not.toContain("rm -rf")
  expect(everything).not.toContain("secret output")
  expect(everything).not.toContain('"done"')
  const recs = received.find((r) => r.path === "/v1/logs").body.resourceLogs[0].scopeLogs[0].logRecords
  const prompt = recs.find((r) => r.attributes.some((x) => x.value.stringValue === "opencode.user_prompt"))
  const a = Object.fromEntries(prompt.attributes.map((x) => [x.key, x.value]))
  expect(a["opencode.prompt.length"]).toEqual({ intValue: "17" })
  // The prompt names the model the step that answered it ran.
  expect(a["gen_ai.request.model"]).toEqual({ stringValue: "gpt-5" })
})
