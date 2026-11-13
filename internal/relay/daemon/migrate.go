package daemon

import (
	"context"
	"os"

	"github.com/miradorlabs/terma-cli/internal/migrate"
)

// migrate migrates the machine to multi-org routing, or finishes a migration an earlier
// start left, when c asks for it (Config.Migration): only the relay migrates, while it
// holds its lock (internal/migrate). Run starts it once the relay listens, so asking each
// team's organization type and waiting out a spool flush never delays the relay: until
// the commit it routes by today's rules, and it asks for the machine's rules as it routes
// each record, so it moves to the new ones as the migration commits. A migration that did
// not happen leaves the machine as it was, and the relay runs on. One that stopped part
// way is the relay's error: the machine collects nothing until a start finishes it, so
// the relay stops, and whatever starts it again (the service manager, the next hook) is
// the next start.
func (c Config) migrate(ctx context.Context) error {
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
