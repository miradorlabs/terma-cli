package shim

import (
	"os"
	"path/filepath"
)

// Prepare answers a PATH shim an earlier terma installed. The launcher asks for the
// arguments to place ahead of the agent's own and reads them back from directory; there
// are none now — the agent's telemetry is configured machine-wide — so it writes the
// empty plan, and the launcher starts the real agent unchanged. The protocol is
// versioned (`terma-args-v1:<count>`), and a launcher that finds anything else starts the
// agent unchanged too, so this never has to succeed for the agent to start.
func Prepare(_, directory string, _ []string) error {
	return os.WriteFile(filepath.Join(directory, "count"), []byte("terma-args-v1:0\n"), 0o600)
}
