// Trims the probe's view of real OpenCode 2 sessions down to what the plugin's OpenCode 2
// entry reads, so the replay tests stay small. Deterministic: the same input gives the same
// bytes.
//
//   node testdata/trim2.mjs <fixtures-scrubbed>/2.4.1 <scripted>
//
// writes testdata/2.4.1/<case>/plugin-view.jsonl from the first folder and
// testdata/2.x-scripted/<version>/<case>/plugin-view.jsonl from the second, whose
// <version>/<case>/plugin-view.jsonl are the scripted sessions.
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs"
import { dirname, join } from "node:path"
import { fileURLToPath } from "node:url"

const CASES = [
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
const SCRIPTED = { "2.0.0": ["sub", "retry", "always", "err", "mcp", "codemcp", "compact", "rel", "rm", "attach", "svc2"] }
SCRIPTED["2.4.1"] = [...SCRIPTED["2.0.0"], "svc"]

// What the entry consumes.
const EVENTS = new Set([
  "session.created",
  "session.moved",
  "session.deleted",
  "session.inbox.delivered",
  "session.step.started",
  "session.step.ended",
  "session.step.failed",
  "session.text.ended",
  "session.tool.input.started",
  "session.tool.called",
  "session.tool.progress",
  "session.tool.success",
  "session.tool.failed",
  "permission.asked",
  "session.renamed",
  "session.usage.updated",
  "session.compaction.started",
  "session.compaction.ended",
  "session.compaction.failed",
  "session.execution.succeeded",
  "session.execution.failed",
  "session.execution.interrupted",
  // Not consumed: a shell command the person ran, which the tests use to prove it is no call.
  "session.shell.started",
  "session.shell.ended",
])
// The hooks it registers, plus two it does not, which the tests use to prove they are ignored.
const HOOKS = new Set([
  "session.prompt",
  "session.title",
  "session.compaction",
  "session.generate",
  "tool.execute.before",
  "tool.execute.after",
  "session.experimental.idle",
  "session.retry",
])
const HOOK_DROP = new Set(["system", "messages", "tools", "options", "headers"])

function trimRecord(r) {
  if (r.kind === "probe" && (r.name === "setup.context" || r.name === "setup")) {
    // One setup call: the directory it was for, and the app OpenCode named.
    const directory = r.name === "setup" ? r.data.directory : r.data.location?.directory
    return { ...r, name: "setup", data: { directory, app: r.data.app } }
  }
  if (r.kind === "event") {
    if (!EVENTS.has(r.name)) return null
    // The durable envelope is OpenCode's own bookkeeping; the entry never reads it.
    const { durable, ...data } = r.data
    return { ...r, data }
  }
  if (r.kind === "hook") {
    if (!HOOKS.has(r.name)) return null
    return { ...r, data: Object.fromEntries(Object.entries(r.data).filter(([k]) => !HOOK_DROP.has(k))) }
  }
  // What ctx.session.get really answered, whole.
  if (r.kind === "lookup") return r
  return null
}

function trim(src, dest) {
  const lines = readFileSync(src, "utf8").split("\n").filter(Boolean)
  const kept = lines.map((l) => trimRecord(JSON.parse(l))).filter(Boolean)
  mkdirSync(dirname(dest), { recursive: true })
  writeFileSync(dest, kept.map((r) => JSON.stringify(r)).join("\n") + "\n")
}

const [real, scripted] = process.argv.slice(2)
if (!real || !scripted) {
  console.error("usage: node testdata/trim2.mjs <fixtures-scrubbed>/2.4.1 <scripted>")
  process.exit(2)
}
const here = dirname(fileURLToPath(import.meta.url))
for (const name of CASES) trim(join(real, name, "plugin-view.jsonl"), join(here, "2.4.1", name, "plugin-view.jsonl"))
for (const [version, cases] of Object.entries(SCRIPTED)) {
  for (const name of cases) {
    const src = join(scripted, version, name, "plugin-view.jsonl")
    if (!existsSync(src)) throw new Error("missing " + src)
    trim(src, join(here, "2.x-scripted", version, name, "plugin-view.jsonl"))
  }
}
