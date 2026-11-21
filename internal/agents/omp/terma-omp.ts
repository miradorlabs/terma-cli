// Terma extension for OMP.
//
// Written by `terma relay setup --harness omp` into ~/.omp/agent/extensions/terma-omp.ts; do
// not edit — the next setup rewrites it. CONFIG below is the only part that differs per
// machine (the raw template ships `CONFIG = null` and is inert).
//
// OMP exports natively, from the OTEL_* settings terma writes into its .env, so this
// extension exports nothing and holds no token: it hands session lifecycle, prompts and
// file edits to `terma hook omp-*`, which claims the session for the local relay and
// records commit attribution. Each edit names its tool call as OMP's own spans do, and a
// subagent's session start names the session that opened it.
//
// Dependency-free: it uses only node:fs, node:path and node:child_process.

const CONFIG: TermaConfig | null = null /* terma:config */

import { spawn } from "node:child_process"
import { closeSync, openSync, readSync } from "node:fs"
import { basename, dirname } from "node:path"

type TermaConfig = {
  version: 1
  hookCommand: string[]
}

// termaHook hands an event to `terma hook omp-*`, detached: attribution is best-effort
// and never delays the agent. It settles once the payload is written, which a handler that
// runs as the agent exits awaits: the agent's exit would otherwise cut the payload off.
function termaHook(event: string, payload: Record<string, unknown>): Promise<void> {
  if (!CONFIG) return Promise.resolve()
  return new Promise((resolve) => {
    try {
      const [cmd, ...args] = CONFIG!.hookCommand
      const child = spawn(cmd, [...args, event], { stdio: ["pipe", "ignore", "ignore"], detached: false })
      child.on("error", () => resolve())
      child.stdin.on("error", () => resolve())
      child.stdin.end(JSON.stringify(payload), () => resolve())
    } catch {
      resolve()
    }
  })
}

let session = ""

function sessionOf(ctx: any): string {
  const own = ctx && ctx.sessionManager && typeof ctx.sessionManager.getSessionId === "function" ? ctx.sessionManager.getSessionId() : ""
  return own ? String(own) : session
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

export default function (omp: any) {
  if (!CONFIG) return

  omp.on("session_start", (event: any, ctx: any) => {
    session = sessionOf(ctx)
    termaHook("omp-session-start", { session_id: session, cwd: ctx?.cwd, model: ctx?.model?.id, reason: event?.reason, parent_session_id: parentOf(ctx) || undefined })
  })

  omp.on("session_shutdown", async (_event: any, ctx: any) => {
    const s = sessionOf(ctx)
    if (s) await termaHook("omp-session-end", { session_id: s, cwd: ctx?.cwd })
  })

  // Every prompt is a claim too: a session whose start the extension missed, or a relay
  // that stopped between turns, is covered before the turn exports anything.
  omp.on("before_agent_start", (_event: any, ctx: any) => {
    termaHook("omp-prompt", { session_id: sessionOf(ctx), cwd: ctx?.cwd })
  })

  omp.on("tool_result", (event: any, ctx: any) => {
    const files = editedFiles(event)
    if (files.length > 0) {
      termaHook("omp-file-edit", { session_id: sessionOf(ctx), cwd: ctx?.cwd, tool: String(event?.toolName ?? ""), files, tool_call_id: event?.toolCallId })
    }
  })
}
