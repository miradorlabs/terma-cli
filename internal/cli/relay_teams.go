package cli

import (
	"context"
	"errors"
	"net/http"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

const heartbeatOrgNotAccessible = "ORG_NOT_ACCESSIBLE"

// selectedTeamUser returns the signed-in Mirador user ID for team when sending to cfg's
// own environment and ingest host ("" on a server-key profile or foreign host), reading
// user-credentials.json alone so it never prompts the keychain.
func (app *App) selectedTeamUser(cfg *config.Config, team routing.Team, endpoint string) string {
	if cfg.ServerKeySignIn || team.AuthURL != cfg.AuthURL || !sameHost(endpoint, cfg.OTLPURL) {
		return ""
	}
	cred, err := auth.LoadIdentity(cfg.Dir, cfg.ProfileName)
	if err != nil || cred.CheckEnvironment(team.AuthURL) != nil {
		return ""
	}
	return cred.UserID
}

func (app *App) activeProfileName() string {
	f, err := config.LoadFile(app.dir)
	if err != nil {
		return ""
	}
	return f.ActiveProfile
}

func (app *App) loadProfileConfig(name string) (*config.Config, error) {
	overrides := app.configOverrides()
	overrides.Profile = name
	return config.Load(app.dir, app.stateDir, overrides)
}

// teamHeartbeatSend delivers a heartbeat to the first Active team of cfg's environment
// (Personal first, then selected org teams) from routing/<profile>.json. Missing, corrupt,
// or wrong-account routing state fails closed without falling back to Profile.Team.
func (app *App) teamHeartbeatSend(ctx context.Context, src *signInSource, cfg *config.Config, lane string, beat *logspb.LogsData) error {
	if app.stateDir == "" || cfg.ProfileName == "" {
		return relay.ErrNoKey
	}
	st, found, err := routing.Load(app.stateDir, cfg.ProfileName)
	if err != nil {
		return err
	}
	if !found || lane == "" || lane == sendas.AccountUnreadable || st.Account != lane {
		return relay.ErrNoKey
	}
	order := make([]routing.Team, 0, len(st.Teams))
	if n := len(st.Teams); n > 0 && st.Teams[n-1].Personal {
		order = append(order, st.Teams[n-1])
		order = append(order, st.Teams[:n-1]...)
	} else {
		order = append(order, st.Teams...)
	}
	var token string
	var tokenLoaded bool
	var lastErr error
	now := time.Now()
	for _, t := range order {
		if t.StateAt(now) != routing.Active || t.AuthURL != cfg.AuthURL {
			continue
		}
		endpoint := app.delivery().Endpoint(cfg, t.TeamID)
		b := proto.CloneOf(beat)
		if lane == sendas.NoPersonLane {
			if !cfg.ServerKeySignIn || t.TeamID != cfg.Team || cfg.AuthURL != cfg.ServerKeyAuthURL {
				continue
			}
			key, kerr := keystore.Get(app.dir, t.TeamID)
			if kerr != nil {
				return kerr
			}
			if key == "" {
				continue
			}
			return sendHeartbeat(ctx, endpoint, key, "", b)
		}
		if !auth.ValidAccountKey(lane) || lane != cfg.Account || src == nil || src.cfg == nil || !sameHost(cfg.AuthURL, src.cfg.AuthURL) {
			return relay.ErrNoKey
		}
		if !tokenLoaded {
			tok, terr := src.Token(ctx, lane)
			if terr != nil {
				return terr
			}
			if tok == "" {
				return relay.ErrNoKey
			}
			token, tokenLoaded = tok, true
		}
		if u := app.selectedTeamUser(cfg, t, endpoint); u != "" && auth.AccountKey(cfg.AuthURL, u) == lane {
			relay.NameHeartbeatUser(b, u)
		}
		serr := sendHeartbeat(ctx, endpoint, token, t.TeamID, b)
		if serr == nil {
			return nil
		}
		if refusal, ok := errors.AsType[*heartbeatError](serr); ok {
			switch {
			case refusal.reason == sendas.ReasonSignInRetired:
				src.Retired(token)
				return serr
			case refusal.status == http.StatusUnauthorized:
				src.Refused(ctx, token)
				return serr
			case refusal.status == http.StatusForbidden && refusal.reason == heartbeatOrgNotAccessible:
				lastErr = serr
				continue
			}
		}
		return serr
	}
	if lastErr != nil {
		return lastErr
	}
	return relay.ErrNoKey
}
