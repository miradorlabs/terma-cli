package harness

// Optional capabilities are asked for by type assertion, so a drifted method silently
// switches one off; each implementation asserts them with a `var _` line.

// Noter is an agent with something to say before the user confirms a connect.
type Noter interface {
	ConnectNotes(e Exporter) []string
}

// Credentialed is an agent that can read back its key for an endpoint and project, so a
// reconnect reuses it; keys are per agent so one can be revoked alone.
type Credentialed interface {
	CurrentCredential(endpoint, projectID string) (key string, ok bool)
}

// Backuper is an agent whose configuration is one file it can snapshot before a connect.
type Backuper interface {
	Backup(endpoint string) (path string, err error)
}
