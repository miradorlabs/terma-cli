package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
)

// routingFacts are what doctor judges on a machine that routes by teams: the profile's
// routing state, the teams' keys in keys.v2.json, and the runs that crossed organizations.
func (app *App) routingFacts(cfg *config.Config) *doctor.Routing {
	st, ok, err := routing.Load(app.stateDir, cfg.ProfileName)
	names := loadTeamNames(app.stateDir, cfg.ProfileName)
	return &doctor.Routing{
		State: st, Err: err, Missing: err == nil && !ok,
		Teams: names.Teams, Orgs: names.Orgs,
		Key:           app.teamKey,
		MixedSessions: mixedSessions(app.stateDir, time.Now()),
		MixedDropped:  mixedDropped(app.stateDir),
	}
}

// teamKey is an enrollment's key on this machine, masked; "" when it has none.
func (app *App) teamKey(enrollmentID string) string {
	key, _, err := keystore.GetTeam(app.dir, enrollmentID)
	switch {
	case err != nil:
		return "in the system keychain, which is locked or unavailable"
	case key == "":
		return ""
	}
	return keystore.Mask(key)
}

// routedKeys are doctor's Keys on a machine that routes by teams: each team's own key,
// by its enrollment; no agent's key is used there.
func (app *App) routedKeys(r *doctor.Routing) doctor.Keys {
	return func(agent, projectID string) string {
		if agent != "" {
			return ""
		}
		for _, t := range r.State.Teams {
			if t.TeamID == projectID {
				return app.teamKey(t.EnrollmentID)
			}
		}
		return ""
	}
}

// mixedSessions are the sessions whose current run crossed organizations, from the run
// ledger: they send no telemetry until they start a new run.
func mixedSessions(stateDir string, now time.Time) []string {
	entries, err := os.ReadDir(filepath.Join(stateDir, runs.Dir))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if e.IsDir() || !ok {
			continue
		}
		if _, mixed := runs.MixedAt(stateDir, id, now); mixed {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// mixedDropped counts what the relay dropped from Mixed runs, as its last stop recorded
// its counters (dropped.mixed.<signal>).
func mixedDropped(stateDir string) int {
	data, err := os.ReadFile(filepath.Join(claim.Dir(stateDir), daemon.StatsFile))
	if err != nil {
		return 0
	}
	var snap struct {
		Counters map[string]int `json:"counters"`
	}
	if json.Unmarshal(data, &snap) != nil {
		return 0
	}
	n := 0
	for k, v := range snap.Counters {
		if strings.HasPrefix(k, "dropped.mixed.") {
			n += v
		}
	}
	return n
}
