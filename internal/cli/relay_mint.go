package cli

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
)

// newRelayKeyMinter mints claimed projects' keys with the signed-in credential.
func (app *App) newRelayKeyMinter(ctx context.Context, cfg *config.Config) *daemon.KeyMinter {
	return daemon.NewKeyMinter(ctx, cfg, app.createProjectKey)
}

func (app *App) createProjectKey(ctx context.Context, cfg *config.Config, projectID string) (string, error) {
	scoped := *cfg
	scoped.ProjectID = projectID
	client, err := app.newClient(&scoped)
	if err != nil {
		return "", err
	}
	key, _, err := client.CreateServerKey(ctx, projectID, "terma-relay@"+hostname(),
		"Created by the terma relay, for a repository connected in Terma")
	return key, err
}
