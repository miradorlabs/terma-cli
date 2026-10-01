package cli

import (
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
)

// relayResolver turns a claim into its session's policy (daemon.Resolver).
func (app *App) relayResolver(cfg *config.Config, mint func(projectID string)) func(claim.Claim) (relay.Policy, error) {
	return daemon.Resolver(cfg, daemon.ResolverDeps{Mint: mint, AgentName: app.agents.NameForTool,
		Endpoint: func(projectID string) string { return app.projectEndpoint(cfg, projectID) }})
}

func relayCatchAll() func() (claim.Claim, bool) { return daemon.CatchAll(hookPolicy) }
