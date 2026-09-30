package harness

// The optional interfaces: what some harnesses can do and others cannot. A command asks
// for one with a type assertion and carries on without it, so a method that drifted —
// a renamed parameter, a changed result — would not fail to compile. It would stop
// matching, and the capability would switch itself off: key reuse silently minting a
// new key on every connect, for one. The assertions below turn that into a build
// error. Scoped is declared with the scope it describes.

// Noter is a harness with something to say before the user confirms a connect: a side
// effect of its own, or a limit of what its switches can do.
type Noter interface {
	ConnectNotes(e Exporter) []string
}

// Credentialed is a harness that can read back the key it already exports with, for
// one endpoint and project, so a reconnect reuses it instead of minting another. Reuse
// is per harness on purpose: each agent holds its own key, so one can be revoked
// without cutting the other off.
type Credentialed interface {
	CurrentCredential(endpoint, projectID string) (key string, ok bool)
}

// Backuper is a harness whose configuration is a single file it can snapshot before a
// connect rewrites it. One whose config is not a file it owns has nothing to back up.
type Backuper interface {
	Backup(endpoint string) (path string, err error)
}

// StatusLiner is a harness whose user-level status line can capture plan usage.
// Commands invoke it for machine-level capture, not repository policy.
type StatusLiner interface {
	InstallStatusLine() (bool, error)
	StatusLineState(cwd string) (StatusLineState, error)
	RemoveStatusLine() (bool, error)
}

// TurnNotifier captures funding evidence at the end of a turn while preserving
// the developer's existing notifier.
type TurnNotifier interface {
	NotifierInstalled() (bool, error)
	InstallNotifier() (bool, error)
	RemoveNotifier() (bool, error)
}

// EmissionChecker is a harness whose export switches combine several settings files, so
// what a repository exports is none of them alone.
type EmissionChecker interface {
	// EmissionStatus is the combined export for the repository at root.
	EmissionStatus(root string) (Status, error)
	// TelemetrySwitch is the setting that turns the whole export on.
	TelemetrySwitch() string
}

var (
	_ Noter = Codex{}

	_ Credentialed = Codex{}

	_ Backuper = Codex{}

	_ TurnNotifier = Codex{}
)

// StatusLineState is what a config's status line looks like to Terma.
type StatusLineState struct {
	ConfigPath string
	// Installed: the file holds Terma's command.
	Installed bool
	// Renderer is the previous command Terma passes through ("" when none).
	Renderer string
	// Replaced: Terma wrapped this file once, and the file now holds a status
	// line that is not Terma's. Capture has stopped; the user's entry stands.
	Replaced bool
	// Overrides lists settings files that outrank this one and define their own
	// statusLine, so Terma's never runs there.
	Overrides []string
}
