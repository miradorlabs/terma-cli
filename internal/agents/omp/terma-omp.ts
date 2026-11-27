// Terma extension for OMP.
//
// Written by `terma relay setup --harness omp` into ~/.omp/agent/extensions/terma-omp.ts; do
// not edit — the next setup rewrites it. CONFIG below is the only part that differs per
// machine (the raw template ships `CONFIG = null` and is inert).
//
// OMP exports natively, from the OTEL_* settings terma writes into its .env, so this
// extension exports nothing and holds no token, only the relay's address: it hands session
// lifecycle, prompts and file edits to `terma hook omp-*`, which claims the session for the
// local relay and records commit attribution. Each edit names its tool call as OMP's own spans do, and a
// subagent's session start names the session that opened it. The name an interactive OMP
// gives a session, which its telemetry never carries, goes to `terma hook omp-title`.
//
// Dependency-free: it uses only node:fs, node:path and node:child_process, and OMP's own
// @oh-my-pi/pi-utils where OMP provides it. Loaded by Pi, from an agent directory the two
// share, it does nothing.

const CONFIG: TermaConfig | null = null /* terma:config */

import { spawn } from "node:child_process"
import { closeSync, openSync, readSync } from "node:fs"
import { basename, dirname } from "node:path"

type TermaConfig = {
  version: 1
  hookCommand: string[]
  relay: string
}

// termaHook hands an event to `terma hook omp-*`, detached: attribution is best-effort
// and never delays the agent. It settles once the payload is written, which a handler that
// runs as the agent exits awaits: the agent's exit would otherwise cut the payload off; with
// untilExit, once the hook has exited, for a caller that orders its hooks. It reports whether
// the hook took the payload: with untilExit, whether it exited cleanly.
const HOOK_TIMEOUT_MS = 2000, HOOK_KILL_GRACE_MS = 250, MAX_HOOK_QUEUE = 64

function spawnHook(event: string, payload: Record<string, unknown>, untilExit = false, timeoutMs = 0): { result: Promise<boolean>; exit: Promise<void> } {
  if (!CONFIG) return { result: Promise.resolve(false), exit: Promise.resolve() }
  let resolveExit!: () => void
  const exit = new Promise<void>((r) => { resolveExit = r })
  const result = new Promise<boolean>((resolve) => {
    let settled = false, exited = false, child: ReturnType<typeof spawn> | null = null, timer: any, killTimer: any
    const finish = (ok: boolean) => { if (!settled) { settled = true; clearTimeout(timer); clearTimeout(killTimer); resolve(ok) } }
    const markExited = () => { if (!exited) { exited = true; clearTimeout(timer); clearTimeout(killTimer); resolveExit() } }
    const sendKill = (sig: NodeJS.Signals) => { try { child?.kill(sig) } catch (err: any) { if (err?.code === "ESRCH") markExited() } }
    try {
      const [cmd, ...args] = CONFIG!.hookCommand
      child = spawn(cmd, [...args, event], { stdio: ["pipe", "ignore", "ignore"], detached: false })
      child.on("error", (err: any) => { if (!child?.pid || err?.code === "ESRCH") markExited(); finish(false) })
      child.on("close", (code: number | null) => { markExited(); if (untilExit) finish(code === 0) })
      child.stdin.on("error", () => { if (!untilExit) finish(false) })
      if (untilExit && timeoutMs > 0) {
        timer = setTimeout(() => {
          if (!child) { markExited(); return finish(false) }
          sendKill("SIGTERM")
          killTimer = setTimeout(() => { sendKill("SIGKILL"); child?.unref?.(); finish(false) }, HOOK_KILL_GRACE_MS)
          killTimer?.unref?.()
        }, timeoutMs)
        timer?.unref?.()
      }
      child.stdin.end(JSON.stringify(payload), () => { if (!untilExit) finish(true) })
    } catch { markExited(); finish(false) }
  })
  return { result, exit }
}

function termaHook(event: string, payload: Record<string, unknown>, untilExit = false, timeoutMs = 0): Promise<boolean> {
  return spawnHook(event, payload, untilExit, timeoutMs).result
}

let session = ""

function sessionOf(ctx: any): string {
  const own = ctx && ctx.sessionManager && typeof ctx.sessionManager.getSessionId === "function" ? ctx.sessionManager.getSessionId() : ""
  return own ? String(own) : session
}

// titled is the name last handed over, or queued, for each session this process ran; a
// hook that fails gives it back, so the next look sends it again. titles hands them over one
// at a time, each hook done before the next starts, so a rename is never overtaken by the
// name it replaced.
const titled = new Map<string, string>()
let titles: Promise<void> = Promise.resolve()

// sendTitle hands over the session's name when OMP has named it, or renamed it, since the
// last one sent: OMP names an interactive session after its first reply, in the background,
// and fires no event for it, so each turn's start and end and the shutdown look again.
function sendTitle(ctx: any): Promise<void> {
  try {
    const sm = ctx && ctx.sessionManager
    const name = sm && typeof sm.getSessionName === "function" ? sm.getSessionName() : undefined
    const s = sessionOf(ctx)
    // A name already queued may still wait behind an earlier one: the queue is what a
    // caller about to leave the session waits on.
    if (typeof name !== "string" || !name.trim() || !s || titled.get(s) === name) return titles
    titled.set(s, name)
    const payload = { session_id: s, cwd: ctx?.cwd, title: name }
    titles = titles.then(async () => {
      if (!(await termaHook("omp-title", payload, true)) && titled.get(s) === name) titled.delete(s)
    })
    return titles
  } catch {
    return Promise.resolve()
  }
}

// sendTitleBefore sends the title as OMP leaves a session, waiting at most ms: OMP gives a
// shutdown handler 2 s, and the hooks ahead of it in the queue may still be running.
// Accepted risk: a name still queued when OMP exits is never sent; a later run of the
// session (resume) sends it, and a hook that reads OMP's transcript is the upgrade path.
function sendTitleBefore(ctx: any, ms: number): Promise<void> {
  return Promise.race([
    sendTitle(ctx),
    new Promise<void>((resolve) => {
      const t: any = setTimeout(resolve, ms)
      t?.unref?.()
    }),
  ])
}

// parentOf is the session that opened this one for a subagent: its header's parentSession
// is the parent's transcript, and a subagent's own transcript sits in the folder of that
// name. A top-level parent's is <dir>/<time>_<id>.jsonl; a subagent's, opening one of its
// own, is <agent>.jsonl (an agent id may hold "_"), whose header names its id. A fork or a continuation names a parent
// too, but elsewhere.
function parentOf(ctx: any): string {
  try {
    const sm = ctx && ctx.sessionManager
    const parent = sm && typeof sm.getHeader === "function" ? sm.getHeader()?.parentSession : undefined
    const file = sm && typeof sm.getSessionFile === "function" ? sm.getSessionFile() : undefined
    if (typeof parent !== "string" || typeof file !== "string" || dirname(file) + ".jsonl" !== parent) return ""
    const m = /^\d{4}-\d\d-\d\dT\d\d-\d\d-\d\d-\d{3}Z_(.+)\.jsonl$/.exec(basename(parent))
    if (m) return m[1]
    const head = Buffer.alloc(16 * 1024)
    const fd = openSync(parent, "r")
    const n = readSync(fd, head, 0, head.length, 0)
    closeSync(fd)
    for (const line of head.subarray(0, n).toString("utf8").split("\n")) {
      if (!line.includes('"type":"session"')) continue
      const id = JSON.parse(line).id
      return typeof id === "string" ? id : ""
    }
    return ""
  } catch {
    return ""
  }
}

// editedFiles are the files a successful tool call changed: OMP's write names its file in
// its details' resolvedPath, else its input, and none when it touches no file (a message to
// a peer) or goes to one of OMP's internal URLs (agent://, cfg://, …) that resolves to none;
// its edit names them in its details, one file or several, each with its own outcome.
// apply_patch is the edit tool's wire name in patch mode, with the same details.
function editedFiles(event: any): string[] {
  if (event?.isError === true) return []
  const name = String(event?.toolName ?? "")
  const files: string[] = []
  const add = (p: unknown) => {
    if (typeof p === "string" && p && !files.includes(p)) files.push(p)
  }
  if (name === "write" && event?.details?.touchesNoFiles !== true) {
    const path = event?.input?.path
    add(event?.details?.resolvedPath || (typeof path === "string" && /^[a-zA-Z][a-zA-Z0-9+.-]*:\/\//.test(path) ? "" : path))
  }
  if (name === "edit" || name === "apply_patch") {
    const d = event?.details
    add(d?.path)
    add(d?.move)
    if (Array.isArray(d?.perFileResults)) {
      for (const r of d.perFileResults) {
        if (r && r.isError !== true) {
          add(r.path)
          add(r.move)
        }
      }
    }
  }
  return files
}

// nonURLPath returns p when it is a filesystem path rather than an OMP internal URL.
function nonURLPath(p: unknown): string {
  return typeof p === "string" && p && !/^[a-zA-Z][a-zA-Z0-9+.-]*:\/\//.test(p) ? p : ""
}

// toolReach describes what a read, search, glob, list or shell tool call can reach.
function toolReach(event: any): Record<string, unknown> | null {
  const name = String(event?.toolName ?? "")
  const input = event?.input ?? {}
  const out: Record<string, unknown> = {}
  const paths: string[] = []
  const addPath = (p: unknown) => {
    const s = typeof p === "string" ? p : ""
    if (s && !paths.includes(s)) paths.push(s)
  }
  const addFile = (p: unknown) => {
    const s = nonURLPath(p)
    if (s) addPath(s)
  }
  // OMP search tools accept semicolon-separated multi-target paths (e.g. "src;../beta").
  const addSearchPaths = (p: unknown) => {
    if (typeof p === "string" && p) {
      if (p.includes(";")) {
        for (const seg of p.split(";")) addFile(seg.trim())
      } else {
        addFile(p)
      }
    }
    if (Array.isArray(input.paths)) for (const q of input.paths) addFile(q)
  }
  switch (name) {
    case "read":
      addPath(event?.details?.resolvedPath)
      addFile(input.path)
      if (Array.isArray(input.paths)) for (const p of input.paths) addFile(p)
      break
    case "grep":
    case "ast_grep": {
      addSearchPaths(input.path)
      const g = typeof input.glob === "string" ? input.glob : typeof input.include === "string" ? input.include : ""
      if (g) out.globs = [g]
      if (paths.length === 0) paths.push(".")
      break
    }
    case "find":
    case "glob": {
      addSearchPaths(input.path)
      const g = typeof input.pattern === "string" ? input.pattern : typeof input.glob === "string" ? input.glob : ""
      if (g) out.globs = [g]
      if (paths.length === 0) paths.push(".")
      break
    }
    case "ls": {
      const p = nonURLPath(input.path)
      if (p) out.lists = [p]
      break
    }
    case "bash":
      if (typeof input.command === "string" && input.command) out.command = input.command
      if (typeof input.cwd === "string" && input.cwd) out.dir = input.cwd
      break
    default:
      return null
  }
  if (paths.length > 0) out.paths = paths
  return Object.keys(out).length > 0 ? out : null
}

// askForContent asks OMP for full content, which the relay applies the team's policy to, as if
// OMP's agent .env asked: OMP keeps that from the tools it runs. It asks only where every
// signal goes to the relay as OMP resolves it, the shell and the launch directory's .env
// ahead of its own .env files, so no collector of the developer's gets content on terma's
// request; a capture setting of the developer's own stands. OMP reads capture once, at its
// first model call, after it binds this extension. An OMP without the module this needs gets
// no request, and its content stays home.
async function askForContent() {
  try {
    const omp: any = await import("@oh-my-pi/pi-utils")
    const to = (s: string) => String(omp.$otelEnv(`OTEL_EXPORTER_OTLP_${s}_ENDPOINT`) || omp.$otelEnv("OTEL_EXPORTER_OTLP_ENDPOINT") || "")
    if (["TRACES", "LOGS", "METRICS"].every((s) => to(s).startsWith(CONFIG!.relay + "/"))) {
      omp.claimOmpOtelEnvFileEntry("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", "full")
    }
  } catch {
    // Not OMP's runtime.
  }
}

// inPi reports whether Pi is the host. Pi and OMP each answer this name with their own
// package, whatever copy of it sits on disk (seen in Pi 1.5.0 and 1.6.1, OMP 18.18.2 and
// 19.1.1), and the package names its host's config folder: .pi in Pi, .omp in OMP. A host
// that has no such package, or gives any other answer, is not taken for Pi. The name stays
// written out in the import: through a variable, OMP looks for the package on disk instead.
async function inPi(): Promise<boolean> {
  try {
    const host: any = await import("@earendil-works/pi-coding-agent")
    return host.CONFIG_DIR_NAME === ".pi"
  } catch {
    return false
  }
}

export default async function (omp: any) {
  // Pi loads this file too from an agent directory it shares with OMP (PI_CODING_AGENT_DIR),
  // and its session is not OMP's to hand over: no handler is registered, so no hook runs.
  if (!CONFIG || (await inPi())) return
  await askForContent()
  // Serialize lifecycle and reach hooks so session-start always finishes (with confirmed
  // child exit) before tool-reach; titles keep their own queue.
  let lifecycle: Promise<void> = Promise.resolve()
  let pending = 0
  const queuedHook = (event: string, payload: Record<string, unknown>): Promise<boolean> => {
    if (pending >= MAX_HOOK_QUEUE) return Promise.resolve(false)
    pending++
    const slot = lifecycle.then(() => spawnHook(event, payload, true, HOOK_TIMEOUT_MS))
    lifecycle = slot.then((s) => s.exit, () => undefined).finally(() => { pending-- })
    return new Promise<boolean>((resolve) => {
      let done = false
      const settle = (ok: boolean) => {
        if (done) return
        done = true
        clearTimeout(wait)
        resolve(ok)
      }
      const wait: any = setTimeout(() => settle(false), HOOK_TIMEOUT_MS + HOOK_KILL_GRACE_MS + 500)
      wait?.unref?.()
      slot.then((s) => s.result, () => false).then(settle, () => settle(false))
    })
  }
  const awaitWithin = <T>(p: Promise<T>, ms: number, fallback: T): Promise<T> =>
    Promise.race([
      p,
      new Promise<T>((resolve) => {
        const t: any = setTimeout(() => resolve(fallback), ms)
        t?.unref?.()
      }),
    ])
  // A session whose reach hook failed is tainted: this extension hands nothing more of it
  // over, and asks terma to fence its run (mark it Mixed) so the relay drops what OMP exports
  // of it. A tool is never blocked or failed for it: with terma gone, signed out or not yet
  // set up, nothing claims the session and the relay forwards none of it anyway.
  const tainted = new Set<string>()
  const fenced = new Map<string, { gen: number; parent: string; pGen: number }>()
  const fencing = new Map<string, Promise<boolean>>()
  // unfenced are the runs whose fence failed once: a later tool call tries again without
  // waiting for it, so a terma that cannot answer delays the agent once, not at every call.
  const unfenced = new Set<string>()
  const runGen = new Map<string, number>()
  const isTainted = (s: string, parent = "") => !!(s && tainted.has(s)) || !!(parent && tainted.has(parent))
  const isFenced = (s: string, parent = "") => {
    const valid = (id: string) => {
      const f = fenced.get(id)
      return !!f && f.gen === (runGen.get(id) ?? 0) && (!f.parent || f.pGen === (runGen.get(f.parent) ?? 0))
    }
    return (!!s && valid(s)) || (!!parent && valid(parent))
  }
  const taint = (s: string, cwd: unknown, parent: string, gen = runGen.get(s) ?? 0, pGen = parent ? (runGen.get(parent) ?? 0) : 0): Promise<boolean> => {
    if (!s) return Promise.resolve(false)
    if ((runGen.get(s) ?? 0) === gen) tainted.add(s)
    if (parent && (runGen.get(parent) ?? 0) === pGen) tainted.add(parent)
    if (isFenced(s, parent)) return Promise.resolve(true)
    const key = parent || s
    const active = fencing.get(key)
    if (active) return unfenced.has(key) ? Promise.resolve(false) : active
    // Fail-closed fence: await lifecycle drain (bounded) and flip the run to Mixed via
    // an unresolvable shell reach, which the first tool call it is needed for waits for,
    // within the hook's bounds. If fencing fails too the tool still runs: the session stays
    // tainted, and every later tool call tries the fence again, in the background.
    const p = (async () => {
      const drained = await awaitWithin(lifecycle.then(() => true, () => true), HOOK_TIMEOUT_MS + HOOK_KILL_GRACE_MS, false)
      if (!drained) return false
      const run = spawnHook("omp-tool-reach", {
        session_id: s,
        cwd,
        tool: "bash",
        command: "eval $TERMA_REACH_FAILED",
        parent_session_id: parent || undefined,
      }, true, HOOK_TIMEOUT_MS)
      lifecycle = lifecycle.then(() => run.exit, () => run.exit)
      const ok = await awaitWithin(run.result, HOOK_TIMEOUT_MS + HOOK_KILL_GRACE_MS + 500, false)
      if (ok && (runGen.get(s) ?? 0) === gen && (!parent || (runGen.get(parent) ?? 0) === pGen)) {
        fenced.set(s, { gen, parent, pGen })
        if (parent) fenced.set(parent, { gen: pGen, parent: "", pGen: 0 })
      }
      return ok
    })().finally(() => { if (fencing.get(key) === p) fencing.delete(key) })
    fencing.set(key, p)
    const waited = unfenced.has(key) ? Promise.resolve(false) : p
    void p.then((ok) => {
      if ((runGen.get(s) ?? 0) !== gen) return
      if (ok) unfenced.delete(key)
      else unfenced.add(key)
    })
    return waited
  }
  const reached = new Map<string, Promise<boolean>>()
  const emitReach = (event: any, ctx: any): Promise<boolean> | null => {
    const reach = toolReach(event)
    if (!reach) return null
    const s = sessionOf(ctx)
    if (!s) return null
    const parent = parentOf(ctx)
    const gen = runGen.get(s) ?? 0
    const pGen = parent ? (runGen.get(parent) ?? 0) : 0
    if (isFenced(s, parent)) return Promise.resolve(true)
    if (isTainted(s, parent)) return taint(s, ctx?.cwd, parent, gen, pGen)
    return queuedHook("omp-tool-reach", {
      session_id: s,
      cwd: ctx?.cwd,
      tool: String(event?.toolName ?? ""),
      parent_session_id: parent || undefined,
      ...reach,
    }).then((ok) => (ok ? true : taint(s, ctx?.cwd, parent, gen, pGen)))
  }

  omp.on("session_start", (event: any, ctx: any) => {
    session = sessionOf(ctx)
    const parent = parentOf(ctx)
    if (session) {
      runGen.set(session, (runGen.get(session) ?? 0) + 1)
      fenced.delete(session)
      fencing.delete(session)
      unfenced.delete(session)
      if (!parent) tainted.delete(session)
    }
    return queuedHook("omp-session-start", { session_id: session, cwd: ctx?.cwd, model: ctx?.model?.id, reason: event?.reason, parent_session_id: parent || undefined }).then(() => undefined)
  })

  // The session's end is handed over first: it must land within OMP's 2 s, or the next
  // commit is credited to a session that has ended. A title that lands after the end's
  // flush is delivered by its own.
  omp.on("session_shutdown", async (_event: any, ctx: any) => {
    const s = sessionOf(ctx)
    const end = s ? queuedHook("omp-session-end", { session_id: s, cwd: ctx?.cwd }).then(() => undefined) : Promise.resolve()
    await Promise.all([end, isTainted(s, parentOf(ctx)) ? Promise.resolve() : sendTitleBefore(ctx, 1500)])
  })

  // /new, /resume and a fork leave the session without a shutdown: its name, given or
  // changed since the last turn, goes now. ctx is still the session being left. The handler
  // returns nothing, which lets the switch go ahead.
  omp.on("session_before_switch", async (_event: any, ctx: any) => {
    if (isTainted(sessionOf(ctx), parentOf(ctx))) return
    await sendTitleBefore(ctx, 1500)
  })

  // Every prompt is a claim too: a session whose start the extension missed, or a relay
  // that stopped between turns, is covered before the turn exports anything.
  omp.on("before_agent_start", (_event: any, ctx: any) => {
    const s = sessionOf(ctx)
    if (isTainted(s, parentOf(ctx))) return
    void queuedHook("omp-prompt", { session_id: s, cwd: ctx?.cwd })
    sendTitle(ctx)
  })

  omp.on("agent_end", (_event: any, ctx: any) => {
    if (isTainted(sessionOf(ctx), parentOf(ctx))) return
    sendTitle(ctx)
  })

  // The reach is judged, or the run fenced, before the tool runs where terma answers in
  // time. The handler returns nothing whatever came of it, which lets the tool run: a hook
  // never fails the agent, and a failed one only stops what terma collects of the session.
  omp.on("tool_call", async (event: any, ctx: any) => {
    const pendingReach = emitReach(event, ctx)
    if (!pendingReach) return
    if (event?.toolCallId) {
      reached.set(event.toolCallId, pendingReach)
      if (reached.size > 1024) reached.delete(reached.keys().next().value!)
    }
    await pendingReach.catch(() => false)
  })

  omp.on("tool_result", async (event: any, ctx: any) => {
    const s = sessionOf(ctx)
    const parent = parentOf(ctx)
    const id = event?.toolCallId
    const prior = id ? reached.get(id) : undefined
    if (id) reached.delete(id)
    if (prior && !(await prior)) return
    if ((!prior || event?.details?.resolvedPath) && !isFenced(s, parent)) {
      const extra = emitReach(event, ctx)
      if (extra && !(await extra)) return
    }
    if (isTainted(s, parent)) return
    const files = editedFiles(event)
    if (files.length > 0) {
      void queuedHook("omp-file-edit", {
        session_id: s,
        cwd: ctx?.cwd,
        tool: String(event?.toolName ?? ""),
        files,
        tool_call_id: event?.toolCallId,
        parent_session_id: parent || undefined,
      })
    }
  })
}
