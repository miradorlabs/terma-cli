// Package agents is the contract a coding agent implements and the registry of the
// agents a build of terma knows. Each agent lives in its own package below this one,
// internal/agents/<name>; package builtin registers them, and code a few agents share
// is in internal/agents/internal. Nothing else in terma names an agent: what the core
// needs of one it asks through this package (internal/boundary enforces it).
//
// Every agent implements Agent: its name, its committed hooks file and the plan that
// writes it, and the `terma hook` events it handles. The rest is optional, and an
// agent implements only what it has; the registry finds each capability with
// Registry.With and Registry.Find, and each agent package asserts the ones it
// implements (var _), so a drifted method fails the build instead of switching off.
//
// Hooks (hooks.go)
//
//	Trusting        whether the agent will run the hooks a repository commits
//	UserHooks       machine-wide hooks, for global mode
//	UserHooksTrust  the step the developer takes before those run
//	ManagedHooks    the same hooks as configuration an organization deploys
//	PayloadReader   a hook payload that names its session in keys of its own
//	Renderer        a hook that draws something (a status line)
//	OffSwitched     what an event still runs with hooks switched off
//	Retrusting      the note a refresh prints when the agent must trust hooks again
//
// Surfaces (surfaces.go)
//
//	Surfaced        more than one way to run the agent, chosen apart (a CLI, a desktop app)
//	SurfaceChecker  a surface's own readiness check in a repository
//
// Telemetry (telemetry.go)
//
//	RelayExporter    points the agent's exporter at the local relay
//	RelayChecker     a condition that keeps its telemetry from the relay after setup
//	Exporting        an exporter terma configures in the agent's own settings (harness.Harness)
//	Labeled          a hook and trailer label other than its name
//	MachineRefresher home-directory files `terma update --refresh` rewrites
//	ContentConsent   the consent a spooled reply or title travels under
//	StatusLiner      the status line terma wraps to read plan usage
//	Notifier         the end-of-turn notifier terma chains in front of the developer's
//	EmissionChecker  an export that several settings files decide together
//
// How the relay places and redacts the agent's records is declared through
// internal/relay/shape (Correlator and Capturer), and how completely terma supports
// it through Covered (support.go).
package agents
