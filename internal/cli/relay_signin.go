package cli

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
)

// signInSource is the profile's sign-in as the relay and the spool's delivery send with it:
// read again at every send, since the developer can sign in, out or as someone else while
// they run, and refreshed under the cross-process credential lock as every terma does.
type signInSource struct {
	cfg     *config.Config
	version string

	mu     sync.Mutex
	client *api.Client
	// account caches Account for a second: the relay asks at every part it accepts.
	account   string
	accountAt time.Time
}

func (app *App) signInSource(cfg *config.Config) *signInSource {
	return &signInSource{cfg: cfg, version: app.version}
}

// Account is the account key of the profile's sign-in for its environment; "" when it is
// signed out, or set up with a server key, which is no one's.
func (s *signInSource) Account() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.accountAt) < time.Second {
		return s.account
	}
	s.account, s.accountAt = "", time.Now()
	if file, err := config.LoadFile(s.cfg.Dir); err == nil {
		if p := file.Profiles[s.cfg.ProfileName]; p != nil && p.ServerKeySignIn {
			return ""
		}
	}
	s.account = auth.Account(s.cfg.Dir, s.cfg.ProfileName, s.cfg.AuthURL)
	return s.account
}

// Token is a fresh access token of the sign-in whose account key is account: "" with no
// error when that sign-in cannot send at all now (signed out, another person's, or
// retired), so what waits for it is held; an error for what a retry may fix.
func (s *signInSource) Token(ctx context.Context, account string) (string, error) {
	ident, err := auth.LoadIdentity(s.cfg.Dir, s.cfg.ProfileName)
	switch {
	case errors.Is(err, auth.ErrNotLoggedIn):
		return "", nil
	case err != nil:
		return "", err
	case account == "" || auth.AccountKey(ident.AuthURL, ident.UserID) != account || ident.CheckEnvironment(s.cfg.AuthURL) != nil:
		return "", nil
	case !ident.RetiredAt.IsZero():
		return "", nil
	}
	client, err := s.clientFor(ident.SessionID)
	if errors.Is(err, auth.ErrNotLoggedIn) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if err := client.Refresh(ctx); errors.Is(err, auth.ErrNotLoggedIn) || errors.Is(err, auth.ErrSignInRetired) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	// The pair the client holds is the session account names, never one stored since.
	held := client.Credential()
	if held.SessionID != ident.SessionID || auth.AccountKey(held.AuthURL, held.UserID) != account {
		return "", nil
	}
	return held.AccessToken, nil
}

// clientFor is a client holding the stored sign-in of session, kept while that session is
// the stored one: a keychain read per send would prompt on some machines.
func (s *signInSource) clientFor(session string) (*api.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil && s.client.Credential().SessionID == session {
		return s.client, nil
	}
	plain := *s.cfg
	plain.APIKey = ""
	client, err := api.New(&plain, api.Options{Version: s.version})
	if err != nil {
		return nil, err
	}
	if client.Credential().SessionID != session {
		return nil, auth.ErrNotLoggedIn // signed in again since: the session asked for is over
	}
	s.client = client
	return client, nil
}

// Refused rotates token, which a server refused, unless another process already has.
func (s *signInSource) Refused(ctx context.Context, token string) {
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client != nil {
		_ = client.RefreshRefused(ctx, api.TokenSHA256(token))
	}
}

// Retired records that a server refused the sign-in as retired, on the sign-in that was
// sent: only while it is still the one stored.
func (s *signInSource) Retired() {
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client != nil {
		_ = auth.MarkRetired(s.cfg.Dir, s.cfg.ProfileName, client.Credential().RefreshToken)
	}
}

// bindRelay has the relay send as the profile's sign-in, bind what it accepts to that
// sign-in's account, count what an older terma queued as its retired sign-ins' account, and
// name this installation, and those sign-ins while recent, on its heartbeat. The record and
// the id are read as they are needed: a sign-in after the relay started writes them.
func (app *App) bindRelay(engine *relay.Options, cfg *config.Config) {
	engine.SignIn = app.signInSource(cfg)
	engine.Installation = func() string { return config.StoredInstallationID(app.dir) }
	engine.Legacy = func() (string, time.Time) {
		rec, _ := auth.ReadLegacyRecord(app.stateDir)
		return rec.Account, rec.RetiredAt
	}
	engine.LegacySessionIDs = func() []string {
		if rec, err := auth.ReadLegacyRecord(app.stateDir); err == nil && rec.Recent(time.Now()) {
			return rec.SessionIDs
		}
		return nil
	}
}
