package daemon

import (
	"context"
	"os"

	"github.com/miradorlabs/terma-cli/internal/migrate"
)

// migrateAtStart migrates the machine to multi-org routing, or finishes a migration an
// earlier start left, when c asks for it (Config.Migration): only the relay migrates, at
// start, while it holds its lock (internal/migrate). A migration that did not happen
// leaves the machine as it was, and the relay runs as before. One that stopped part way
// is the relay's error: the machine collects nothing until a start finishes it, so the
// relay does not run, and whatever starts it again (the service manager, the next hook)
// is the next start.
func (c Config) migrateAtStart(ctx context.Context) error {
	if c.Migration == nil {
		return nil
	}
	pid := os.Getpid()
	out, err := migrate.Run(ctx, *c.Migration)
	switch {
	case out == migrate.Unfinished:
		c.Log.Printf("relay pid %d: the migration to multi-org routing stopped part way; nothing is collected until a relay start finishes it: %v", pid, err)
		return err
	case err != nil:
		c.Log.Printf("relay pid %d: did not migrate to multi-org routing: %v", pid, err)
	case out == migrate.Committed:
		c.Log.Printf("relay pid %d: migrated this machine to multi-org routing", pid)
	case out == migrate.NotMigrated:
		if b, ok := migrate.ReadBlock(c.Migration.StateDir); ok {
			c.Log.Printf("relay pid %d: did not migrate to multi-org routing (%s): %s %v", pid, b.Reason, b.Detail, b.Others)
		}
	}
	return nil
}
