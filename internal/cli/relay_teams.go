package cli

import (
	"context"
	"os"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// On a migrated machine the relay sends each team's records with that team's key, created
// with its organization's sign-in, never the profile's active one.

// createTeamKey mints team's key with its organization's sign-in, named for this machine
// and the team's enrollment, so the backend's reuse by name finds this machine's key for
// this enrollment and no other. The relay calls it through daemon.MintTeamKey, which
// serializes creation on the machine; `terma teams` mints through the same path.
func (app *App) createTeamKey(ctx context.Context, cfg *config.Config, team routing.Team) (string, error) {
	cred, err := auth.LoadCredentialFor(cfg.Dir, cfg.ProfileName, team.OrganizationID)
	if err != nil {
		return "", err
	}
	scoped := *cfg
	scoped.ProjectID, scoped.OrganizationID, scoped.AuthURL, scoped.APIKey = team.TeamID, team.OrganizationID, team.AuthURL, ""
	client, err := api.New(&scoped, api.Options{Version: app.version, ProjectID: team.TeamID, Credential: cred})
	if err != nil {
		return "", err
	}
	key, _, err := client.CreateServerKey(ctx, team.TeamID, teamKeyName(team),
		"Created by terma, for a team selected on this machine")
	return key, err
}

// teamKeyName names a team's key for this machine and the team's enrollment. The backend
// takes names of at most 128 bytes, so a long hostname is cut to leave the enrollment whole.
func teamKeyName(team routing.Team) string {
	host := hostname()
	if room := 128 - len("terma@/") - len(team.EnrollmentID); len(host) > room {
		host = host[:max(room, 0)]
	}
	return "terma@" + host + "/" + team.EnrollmentID
}

// selectedTeamUser is teamUser on a migrated machine: the Mirador user signed in to team's
// own organization, only for parts going to this profile's own ingest from a team of its
// environment; "" for a profile set up with a server key, which is no one's. It reads the
// keychain: the relay asks it through daemon.Teams, which keeps the answer a while.
func (app *App) selectedTeamUser(cfg *config.Config, team routing.Team, endpoint string) string {
	if cfg.ServerKeySignIn || team.AuthURL != cfg.AuthURL || !sameHost(endpoint, cfg.OTLPURL) {
		return ""
	}
	cred, err := auth.LoadCredentialFor(cfg.Dir, cfg.ProfileName, team.OrganizationID)
	if err != nil || cred.CheckEnvironment(team.AuthURL) != nil {
		return ""
	}
	return cred.UserID
}

// teamHeartbeatSend delivers the relay's heartbeat on a migrated machine to one selected,
// Active team of this environment with its own key: personal first, the developer's own
// organization, else the first in the team list. It names that team's signed-in user, and
// with no such team, nothing is sent. It mints no key: the relay does, for a team's records.
func (app *App) teamHeartbeatSend(ctx context.Context, cfg *config.Config, beat *logspb.LogsData) error {
	st, ok, err := routing.LoadReady(app.stateDir, cfg.ProfileName)
	if err != nil {
		return err
	}
	if !ok {
		return relay.ErrNoKey
	}
	order := make([]routing.Team, 0, len(st.Teams))
	if n := len(st.Teams); n > 0 && st.Teams[n-1].Personal {
		order = append(order, st.Teams[n-1])
		order = append(order, st.Teams[:n-1]...)
	} else {
		order = append(order, st.Teams...)
	}
	pinned := app.flags.otlpURL != "" || os.Getenv("TERMA_OTLP_URL") != ""
	now := time.Now()
	for _, t := range order {
		if t.StateAt(now) != routing.Active || t.AuthURL != cfg.AuthURL {
			continue
		}
		key, hosts, err := keystore.GetTeam(app.dir, t.EnrollmentID)
		if err != nil {
			return err // every key sits in the one keychain: asking for the next would wait as long
		}
		if key == "" {
			continue
		}
		endpoint := cfg.OTLPURL
		if !pinned && hosts.OTLP != "" {
			endpoint = hosts.OTLP
		}
		relay.NameHeartbeatUser(beat, app.selectedTeamUser(cfg, t, endpoint))
		return sendHeartbeat(ctx, endpoint, key, beat)
	}
	return relay.ErrNoKey
}
