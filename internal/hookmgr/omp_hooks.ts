// Terma commit-attribution hooks for omp.
//
// Installed by `terma install` into the repository's .omp/hooks/pre directory, where
// omp loads it at startup. Managed by `terma uninstall`; do not edit — the next install
// rewrites it.
//
// What it does: turns omp's own events into hook calls the terma binary turns into
// commit attribution. Telemetry export is not here: it lives in the user-scope
// extension `terma connect omp` writes into ~/.omp/agent/hooks/pre.
//
// What it runs: terma hook omp-session-start, terma hook omp-session-end and
// terma hook omp-file-edit (named in full so the wiring is greppable). All logic
// lives in the binary, so this file never changes when terma updates.
//
// Dependency-free on purpose: omp loads hooks without installing anything for them, so
// this file uses only what Node ships.

import { randomUUID } from "node:crypto"
import { spawn } from "node:child_process"

// A `terma hook` call that outlasts this is abandoned; attribution is best-effort.
const HOOK_TIMEOUT_MS = 500

const HOOK_COMMAND = ["terma", "hook"]

// The session id is stable for the process: omp fires session_start with no id, so
// this hook mints a UUID and reuses it for the session's whole life.
let sessionID
let model

// termaHook hands an event to the terma binary's hook subcommand, asynchronously: the
// hook is not on the agent loop's critical path, and a terma that hangs or is absent
// must never slow or fail the session. Errors are swallowed on purpose.
function termaHook(event, payload) {
  try {
    const child = spawn(HOOK_COMMAND[0], [...HOOK_COMMAND.slice(1), event], {
      stdio: ["pipe", "ignore", "ignore"],
    })
    const killer = setTimeout(() => { try { child.kill() } catch {} }, HOOK_TIMEOUT_MS)
    if (killer.unref) killer.unref()
    child.on("error", () => {})
    child.on("close", () => clearTimeout(killer))
    child.stdin.on("error", () => {})
    child.stdin.end(JSON.stringify(payload))
    if (child.unref) child.unref()
  } catch {
    // A hook that fails the agent uninstalls the product.
  }
}

// fileEditedFromInput pulls the path a write or edit touched out of its input, which
// for those tools always carries `path`. Anything else (bash, eval) reports no file,
// which matches what every other adapter can see.
function fileEditedFromInput(toolName, input) {
  if (!input || typeof input !== "object") return ""
  if (toolName === "write" || toolName === "edit") {
    return typeof input.path === "string" ? input.path : ""
  }
  return ""
}

export default function (pi) {
  pi.on("session_start", (_event, ctx) => {
    sessionID = randomUUID()
    model = ctx && ctx.model && ctx.model.id ? String(ctx.model.id) : undefined
    termaHook("omp-session-start", {
      session_id: sessionID,
      cwd: ctx && ctx.cwd ? ctx.cwd : process.cwd(),
      model,
    })
  })

  pi.on("session_shutdown", (_event, ctx) => {
    if (!sessionID) return
    termaHook("omp-session-end", {
      session_id: sessionID,
      cwd: ctx && ctx.cwd ? ctx.cwd : process.cwd(),
    })
    sessionID = undefined
  })

  pi.on("tool_result", (event, ctx) => {
    if (!sessionID) return
    const file = fileEditedFromInput(event.toolName, event.input)
    if (!file) return
    termaHook("omp-file-edit", {
      session_id: sessionID,
      cwd: ctx && ctx.cwd ? ctx.cwd : process.cwd(),
      model,
      tool: event.toolName,
      file,
    })
  })
}
