// Trims the probe's view of real OpenCode 1.19.6 sessions down to what the plugin's
// OpenCode 1 entry reads, so the replay tests stay small. Deterministic: the same input
// gives the same bytes.
//
//   node testdata/trim.mjs <fixtures-scrubbed>/1.19.6
//
// writes testdata/1.19.6/<case>/plugin-view.jsonl for each case below.
import { mkdirSync, readFileSync, writeFileSync } from "node:fs"
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
  "09-subagent",
  "10-resume-fork",
  "11-api-error",
  "12-serve",
  "20-custom-provider-marked",
]

// What the entry consumes, plus tool.execute.after and file.edited, which the tests use
// to prove they are ignored.
const EVENTS = new Set([
  "session.created",
  "session.updated",
  "session.deleted",
  "session.idle",
  "message.updated",
  "message.part.updated",
  "permission.asked",
  "permission.replied",
  "file.edited",
])
const HOOKS = new Set(["config", "chat.message", "chat.params", "tool.execute.before", "tool.execute.after", "dispose"])

// Long opaque provider signatures, and reasoning text, which the plugin never sends.
function scrub(value) {
  if (Array.isArray(value)) return value.map(scrub)
  if (!value || typeof value !== "object") return value
  const out = {}
  for (const [k, v] of Object.entries(value)) {
    if (k === "metadata" && v && typeof v === "object" && !Array.isArray(v) && "openrouter" in v) {
      const { openrouter, ...rest } = v
      out[k] = scrub(rest)
      continue
    }
    out[k] = scrub(v)
  }
  if (out.type === "reasoning" && typeof out.text === "string") out.text = ""
  return out
}

function trimRecord(r) {
  if (r.kind === "probe" && r.name === "plugin.input") {
    // Which directories the process served: one plugin instance each.
    return { ...r, data: { directory: r.data.directory, worktree: r.data.worktree } }
  }
  if (r.kind === "event") {
    if (!EVENTS.has(r.name)) return null
    return { ...r, data: scrub(r.data) }
  }
  if (r.kind === "hook") {
    if (!HOOKS.has(r.name)) return null
    if (r.name === "config") return { ...r, data: { keys: r.data.keys } }
    if (r.name === "chat.params") {
      // The model and provider records are large and the entry reads four fields of them.
      const i = r.data.input ?? {}
      const input = {
        sessionID: i.sessionID,
        agent: i.agent,
        model: { id: i.model?.id, providerID: i.model?.providerID, api: { npm: i.model?.api?.npm } },
        provider: { id: i.provider?.id },
        message: i.message,
      }
      return { ...r, data: { input, output: r.data.output } }
    }
    return { ...r, data: scrub(r.data) }
  }
  return null
}

const source = process.argv[2]
if (!source) {
  console.error("usage: node testdata/trim.mjs <fixtures-scrubbed>/1.19.6")
  process.exit(2)
}
const here = dirname(fileURLToPath(import.meta.url))
for (const name of CASES) {
  const lines = readFileSync(join(source, name, "plugin-view.jsonl"), "utf8").split("\n").filter(Boolean)
  const kept = lines.map((l) => trimRecord(JSON.parse(l))).filter(Boolean)
  const dest = join(here, "1.19.6", name)
  mkdirSync(dest, { recursive: true })
  writeFileSync(join(dest, "plugin-view.jsonl"), kept.map((r) => JSON.stringify(r)).join("\n") + "\n")
}
