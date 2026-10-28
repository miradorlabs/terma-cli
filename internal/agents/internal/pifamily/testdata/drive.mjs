// Drives the rendered extension (argv[2]) with a Pi-shaped, an omp-shaped and a countless
// assistant message and prints each chat span it would export as a JSON line: {name, attrs}.
// The export is captured, never sent.
const sent = []
globalThis.fetch = async (url, init) => {
  sent.push(JSON.parse(init.body))
  return {}
}
const extension = await import(process.argv[2])
const on = {}
extension.default({ on: (event, fn) => { on[event] = fn } })
const ctx = { sessionManager: { getSessionId: () => "s1" }, cwd: "/tmp" }
const message = (m) => on["message_end"]({ message: { role: "assistant", model: "requested", provider: "p", stopReason: "stop", ...m } }, ctx)

message({ responseModel: "pi-served", responseId: "r-pi", usage: { input: 1, output: 9, reasoning: 4 } })
message({ upstreamModel: "omp-served", responseId: "r-omp", usage: { input: 1, output: 9, reasoningTokens: 0 } })
message({ responseId: "r-none", usage: { input: 1, output: 9 } })
await new Promise((resolve) => setTimeout(resolve, 1300))

for (const body of sent) for (const rs of body.resourceSpans || []) for (const ss of rs.scopeSpans) for (const sp of ss.spans) {
  const attrs = Object.fromEntries(sp.attributes.map((a) => [a.key, Object.values(a.value)[0]]))
  console.log(JSON.stringify({ name: sp.name, attrs }))
}
process.exit(0)
