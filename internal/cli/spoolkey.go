package cli

import (
	"context"
	"errors"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// spoolKey's fix, when set, is what the developer must do before events are delivered.
type spoolKey struct{ state, fix string }

// ensureSpoolKey never fails setup: held events wait up to the spool's MaxAge for a key.
func (app *App) ensureSpoolKey(ctx context.Context, cfg *config.Config) spoolKey {
	if keystore.Get(cfg.ProjectID) != "" {
		return spoolKey{state: "delivered with this team's key"}
	}
	const held = "held until this machine has a key for the team"
	if cfg.APIKey != "" {
		return spoolKey{held, "TERMA_API_KEY cannot mint a key for hook events: unset it and run `terma setup` again."}
	}
	client, err := app.newClient(cfg)
	var key string
	if err == nil {
		key, _, err = client.CreateServerKey(ctx, cfg.ProjectID, "terma-cli@"+hostname(),
			"Created by terma setup, for hook events")
	}
	switch {
	case errors.Is(err, auth.ErrNotLoggedIn):
		return spoolKey{held, "Sign in with `terma setup` again, so hook events from this machine are delivered."}
	case err != nil:
		return spoolKey{held, "Minting a key for hook events failed (" + err.Error() + "); run `terma setup` again."}
	}
	if err := keystore.Set(cfg.ProjectID, key, keystore.HostsOf(cfg)); err != nil {
		return spoolKey{held, "Storing the key for hook events failed (" + err.Error() + "); run `terma setup` again."}
	}
	return spoolKey{state: "Team key stored for this machine (" + keystore.Mask(key) + ")"}
}
