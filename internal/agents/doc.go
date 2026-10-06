// Package agents is the contract a coding agent implements and the registry of the
// agents a build knows. Each agent is a package below this one, builtin registers them,
// and nothing else in terma names an agent (internal/boundary enforces it).
//
// Every agent implements Agent; the rest is optional capabilities, found with
// Registry.With and Registry.Find and asserted by each agent package (var _), so a
// drifted method fails the build instead of switching off.
//
// Hooks (hooks.go)
//
//	UserHooks       the machine-wide hooks setup writes
//	UserHooksTrust  the step the developer takes before those run
//	ManagedHooks    the same hooks as configuration an organization deploys
//	PayloadReader   a hook payload that names its session in keys of its own
//	Renderer        a hook that draws something (a status line)
//	OffSwitched     what an event still runs with hooks switched off
//
// Surfaces (surfaces.go)
//
//	Surfaced        more than one way to run the agent, chosen apart (a CLI, a desktop app)
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
//	Notifier         an end-of-turn notifier an earlier terma chained, which teardown restores
//	StateKeeper      hook state directories the relay ages out and teardown removes
//	EmissionChecker  an export that several settings files decide together
//
// How the relay places and redacts an agent's records is declared through
// internal/relay/shape (Correlator and Capturer); support levels are Covered (support.go).
package agents
