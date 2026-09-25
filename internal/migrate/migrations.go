package migrate

import "github.com/miradorlabs/terma-cli/internal/shim"

// migrations is every migration, in ID order. Append only; see the package comment for
// the rules each one keeps.
var migrations = []Migration{
	{ID: 1, Name: "route Codex CLI from routing records written before the cli field", Run: shim.MigrateCodexCLIRoutes},
}
