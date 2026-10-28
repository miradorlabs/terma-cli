// Drives the rendered plugin (argv[2]) through dsh session events and prints each span it
// would export as a JSON line: {name, error, start (ms), attrs}. The export is captured, never
// sent.
const sent = []
globalThis.fetch = async (url, init) => {
  sent.push(JSON.parse(init.body))
  return {}
}
const plugin = await import(process.argv[2])
const on = {}
plugin.apply({ on: (event, fn) => { on[event] = fn } })

const session = { id: "session-1" }
const event = (type, data, time) => on["session/event"](session, { type, data, time })
const finish = (kind) => ({ type: "chunk", time: 5, chunk: { type: "finish", reason: { kind } } })
const usage = (u) => ({ type: "chunk", time: 4, chunk: { type: "usage", usage: u } })
async function stream(options, chunks) {
  async function* next() { yield* chunks }
  for await (const _ of on["llm/stream"](options, next)) { /* drain */ }
}

// Each of the agent loop's calls streams, then settles: a message, a failed retry and a
// cancelled one.
const loop = { sessionId: "session-1", model: "deepseek-v4", provider: "deepseek" }
event("step/start", {}, 1000)
await stream(loop, [])
event("assistant/message", {
  turn: 1, message: { id: "m1", source: { model: "deepseek-v4", provider: "deepseek" }, content: [] },
  stream: [finish("tool-calls")], usage: { inputTokens: 5, outputTokens: 9, reasoningTokens: 4 },
}, 2000)
await stream(loop, [])
event("assistant/attempt", { turn: 1, stream: [usage({ inputTokens: 5, outputTokens: 2 }), finish("error")] }, 2500)
await stream(loop, [])
event("assistant/attempt", { turn: 1, stream: [finish("aborted")] }, 2600)
// An auxiliary call streaming beside the agent loop's: its own span, and not the model the
// loop's failed call is charged to.
await stream(loop, [])
await stream({ sessionId: "session-1", purpose: "title", model: "deepseek-title", provider: "deepseek" },
  [{ type: "usage", usage: { inputTokens: 1, outputTokens: 1 } }, { type: "finish", reason: { kind: "stop" } }])
event("assistant/attempt", { turn: 2, stream: [finish("error")] }, 2700)
// A committed message settles its call: an attempt after it with no stream of its own takes
// neither that call's model nor its start, only the step's.
await stream(loop, [])
event("assistant/message", {
  turn: 2, message: { id: "m2", source: { model: "deepseek-v4", provider: "deepseek" }, content: [] }, stream: [finish("stop")],
}, 2800)
event("assistant/attempt", { turn: 2, stream: [finish("error")] }, 2900)
event("turn/end", {}, 3000)
await new Promise((resolve) => setTimeout(resolve, 50))

for (const body of sent) for (const rs of body.resourceSpans || []) for (const ss of rs.scopeSpans) for (const sp of ss.spans) {
  const attrs = Object.fromEntries(sp.attributes.map((a) => [a.key, Object.values(a.value)[0]]))
  console.log(JSON.stringify({ name: sp.name, error: sp.status.code === 2, start: Number(BigInt(sp.startTimeUnixNano) / 1000000n), attrs }))
}
process.exit(0)
