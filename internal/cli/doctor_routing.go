package cli

import (
	"cmp"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/migrate"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
)

// routingFacts are what doctor judges on a machine that routes by teams: the profile's
// routing state, and whether the move to routing by teams committed it; the teams' keys in
// keys.v2.json; and the runs that crossed organizations.
func (app *App) routingFacts(cfg *config.Config) *doctor.Routing {
	st, ok, err := routing.Load(app.stateDir, cfg.ProfileName)
	names := loadTeamNames(app.stateDir, cfg.ProfileName)
	r := &doctor.Routing{
		State: st, Err: err, Missing: err == nil && !ok,
		Teams: names.Teams, Orgs: names.Orgs,
		Key:           app.teamKey,
		MixedSessions: mixedSessions(app.stateDir, time.Now()),
	}
	r.MixedDropped, r.Blocked = relayBlocked(app.stateDir)
	if err == nil && ok && !st.Ready() {
		r.Unfinished = "its keys and sign-ins are still moving"
		if b, found := migrate.ReadBlock(app.stateDir); found && b.Detail != "" {
			r.Unfinished = b.Detail
		}
	}
	return r
}

// migrationBlockOf is why the last relay start did not move the machine under stateDir to
// routing by teams, as the migration recorded it (migrate.ReadBlock), in doctor's words;
// nil when nothing did. Only what the developer must mend fails: two installs, or hooks
// files terma cannot read. Anything else a later relay start may get past by itself.
func migrationBlockOf(_, stateDir string) *doctor.MigrationBlock {
	b, ok := migrate.ReadBlock(stateDir)
	if !ok {
		return nil
	}
	return blockWords(b)
}

// blockWords is b in doctor's words.
func blockWords(b migrate.Block) *doctor.MigrationBlock {
	switch b.Reason {
	case migrate.TwoInstalls:
		return &doctor.MigrationBlock{Fail: true,
			Detail: "the agents' hooks run " + strings.Join(b.Others, ", ") + ", not the relay's terma (" + b.Executable + ")",
			Fix:    "remove the other terma install, then run `terma teams`"}
	case migrate.Unchecked:
		return &doctor.MigrationBlock{Fail: true, Detail: b.Detail,
			Fix: "repair the hooks file it names (`terma setup` rewrites terma's entries), then run `terma teams`"}
	case migrate.OrgTypeUnknown:
		return &doctor.MigrationBlock{Detail: "whether a team's organization is your personal one could not be asked (" + b.Detail + ")",
			Fix: "sign in again with `terma setup` if you are signed out, then run `terma teams`"}
	case migrate.FlushBusy:
		return &doctor.MigrationBlock{Detail: "a spool flush held its lock for longer than the relay waits", Fix: "terma teams"}
	}
	return &doctor.MigrationBlock{Detail: cmp.Or(b.Detail, string(b.Reason)), Fix: "terma teams"}
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

// relayBlocked is what the relay kept from leaving, as its last stop recorded it in
// stats.json's blocked counts (relay.Snapshot.Blocked): the records it dropped from Mixed
// runs ("mixed:<session>", summed), and the rest by what blocked them, each pending
// binding ("pending:<binding>") and blocked team ("team:<enrollment>").
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
