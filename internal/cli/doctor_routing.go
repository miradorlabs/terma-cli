package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
)

// mixedRecent is how lately a Mixed run was active for doctor to name its session; older
// ones it counts.
const mixedRecent = 24 * time.Hour

// routingFacts gathers routing/<profile>.json, active-lane pairing, cached team/org names,
// Mixed runs, and relay blocked counts for doctor and status.
func (app *App) routingFacts(cfg *config.Config) *doctor.Routing {
	st, ok, err := routing.Load(app.stateDir, cfg.ProfileName)
	names := app.routeNames(cfg)
	r := &doctor.Routing{
		State:           st,
		Err:             err,
		Missing:         err == nil && !ok,
		ActiveAccount:   app.profileLane(cfg),
		ServerKey:       cfg.ServerKeySignIn,
		SingleTeamLabel: doctor.TeamLabel(cfg),
		Teams:           names.Teams,
		Orgs:            names.Orgs,
	}
	r.Mixed = runs.Mixed(app.stateDir, time.Now(), mixedRecent)
	r.MixedDropped, r.Blocked = relayBlocked(app.stateDir)
	return r
}

// routeNames loads cached team and organization names for cfg's profile and fills in the
// profile's own team and organization names from config.json when not yet cached.
func (app *App) routeNames(cfg *config.Config) teamNames {
	names := loadTeamNames(app.stateDir, cfg.ProfileName)
	if cfg.Team != "" && cfg.TeamName != "" && names.Teams[cfg.Team] == "" {
		names.Teams[cfg.Team] = cfg.TeamName
	}
	if cfg.OrganizationID != "" && cfg.OrganizationName != "" && names.Orgs[cfg.OrganizationID] == "" {
		names.Orgs[cfg.OrganizationID] = cfg.OrganizationName
	}
	return names
}

// relayBlocked reads what the relay kept from leaving from stats.json (relay.Snapshot.Blocked):
// records dropped from Mixed runs ("mixed:<session>", summed) and the rest by blocker
// ("pending:<binding>", "team:<enrollment>", "overflow").
func relayBlocked(stateDir string) (mixed int, blocked map[string]int) {
	data, err := os.ReadFile(filepath.Join(claim.Dir(stateDir), daemon.StatsFile))
	if err != nil {
		return 0, nil
	}
	var snap relay.Snapshot
	if json.Unmarshal(data, &snap) != nil {
		return 0, nil
	}
	for k, v := range snap.Blocked {
		if strings.HasPrefix(k, "mixed:") {
			mixed += v
			continue
		}
		if blocked == nil {
			blocked = map[string]int{}
		}
		blocked[k] += v
	}
	return mixed, blocked
}
