package harness

// The optional interfaces: what some harnesses can do and others cannot. A command asks
// for one with a type assertion and carries on without it, so a method that drifted —
// a renamed parameter, a changed result — would not fail to compile. It would stop
// matching, and the capability would switch itself off: key reuse silently minting a
// new key on every connect, for one. The assertions below are what turns that into a
// build error. (Scoped, the fourth, is declared with the scope it describes.)

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

var (
	_ Noter = Codex{}
	_ Noter = OpenCode{}

	_ Credentialed = Claude{}
	_ Credentialed = Codex{}
	_ Credentialed = OpenCode{}

	_ Backuper = Claude{}
	_ Backuper = Codex{}

	_ Scoped = Claude{}
	_ Scoped = OpenCode{}
)
