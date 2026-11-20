package cli

import (
	"context"
	"errors"
	"os"
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
	// account caches Account while the files it was read from are unchanged: the relay asks
	// at every part it accepts, and another process (setup, a sign-in or sign-out) may
	// change either file at any moment.
	account      string
	accountFiles [2]os.FileInfo
}

func (app *App) signInSource(cfg *config.Config) *signInSource {
	return &signInSource{cfg: cfg, version: app.version}
}

// Account is the account key of the profile's sign-in for its environment; "" when it is
// signed out, or set up with a server key, which is no one's.
func (s *signInSource) Account() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	files := [2]os.FileInfo{statOrNil(config.Path(s.cfg.Dir)), statOrNil(config.CredentialsPath(s.cfg.Dir))}
	if unchanged(s.accountFiles[0], files[0]) && unchanged(s.accountFiles[1], files[1]) {
		return s.account
	}
	s.account, s.accountFiles = "", files
	if file, err := config.LoadFile(s.cfg.Dir); err == nil {
		if p := file.Profiles[s.cfg.ProfileName]; p != nil && p.ServerKeySignIn {
			return ""
		}
	}
	s.account = auth.Account(s.cfg.Dir, s.cfg.ProfileName, s.cfg.AuthURL)
	return s.account
}

func statOrNil(path string) os.FileInfo {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	// On Windows the file's identity is read at the first SameFile, not at Stat: read it
	// now, or a later comparison would read the file that replaced this one.
	os.SameFile(info, info)
	return info
}

// unchanged reports whether a file read before as was is as it is now: both absent, or
// the same file (each write replaces it by rename) with the same time and size.
func unchanged(was, now os.FileInfo) bool {
	if was == nil || now == nil {
		return was == nil && now == nil
	}
	return os.SameFile(was, now) && was.ModTime().Equal(now.ModTime()) && was.Size() == now.Size()
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
	legacy := cachedLegacyRecord(app.stateDir)
	engine.Legacy = func() (string, time.Time) {
		rec := legacy()
		return rec.Account, rec.RetiredAt
	}
	engine.LegacySessionIDs = func() []string {
		if rec := legacy(); rec.Recent(time.Now()) {
			return rec.SessionIDs
		}
		return nil
	}
}

// cachedLegacyRecord reads the legacy record at most once a second: the relay asks for it
// for every queued entry from before user tokens. A read that fails keeps the last record
// read, or with none, names no one (auth.UnreadableLegacyRecord), never no record.
func cachedLegacyRecord(stateDir string) func() auth.LegacyRecord {
	var mu sync.Mutex
	rec := auth.UnreadableLegacyRecord(stateDir)
	var at time.Time
	return func() auth.LegacyRecord {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(at) > time.Second {
			if read, err := auth.ReadLegacyRecord(stateDir); err == nil {
				rec = read
			}
			at = time.Now()
		}
		return rec
	}
}
