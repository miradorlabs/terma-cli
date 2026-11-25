package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// signInSource is the profile's sign-in as the relay and the spool's delivery send with it:
// read again at every send, since the developer can sign in, out or as someone else while
// they run, and refreshed under the cross-process credential lock as every terma does.
type signInSource struct {
	cfg     *config.Config
	version string

	mu     sync.Mutex
	client *api.Client
	// sent are the access tokens Token handed out lately, each with its session: a refusal
	// that arrives after the session rotated its token still names that session.
	sent []sentToken
	// accountMu guards the account cache alone, apart from mu, which a client's creation
	// holds across a keychain read: an export must never wait on a keychain prompt.
	accountMu sync.Mutex
	// account caches Account while the files it was read from are unchanged: the relay asks
	// at every part it accepts, and another process (setup, a sign-in or sign-out) may
	// change either file at any moment.
	account      string
	generation   uint64 // the sign-in's generation, read with account
	accountFiles [2]os.FileInfo
	accountRead  bool // account was read, and may be kept while the files are unchanged
	// betweenReads, in tests, runs between Account's read of config.json and of the sign-in.
	betweenReads func()
}

type sentToken struct{ token, session string }

// maxSentTokens bounds the tokens remembered: a send's answer comes within its timeout,
// and a session rotates its token about hourly.
const maxSentTokens = 16

// handedOut remembers token as session's. s.mu is not held.
func (s *signInSource) handedOut(token, session string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.IndexFunc(s.sent, func(t sentToken) bool { return t.token == token }); i >= 0 {
		return
	}
	s.sent = append(s.sent, sentToken{token, session})
	if len(s.sent) > maxSentTokens {
		s.sent = s.sent[len(s.sent)-maxSentTokens:]
	}
}

func (app *App) signInSource(cfg *config.Config) *signInSource {
	return &signInSource{cfg: cfg, version: app.version}
}

// Account is the lane of the profile's sign-in: its account key, as auth.Account gives it
// (one for another environment keeps its own, which Token never has a token for);
// sendas.NoPersonLane when it is set up with a server key; "" when it is signed out;
// sendas.AccountUnreadable while config.json or the sign-in can't be read, or the two changed
// while they were read.
func (s *signInSource) Account() string {
	account, _ := s.Lane()
	return account
}

// Lane is Account with the generation of the sign-in it was read from
// (auth.Credential.Generation), taken from the same read of user-credentials.json.
func (s *signInSource) Lane() (account string, generation uint64) {
	s.accountMu.Lock()
	defer s.accountMu.Unlock()
	if s.accountRead && sameFiles(s.accountFiles, profileFiles(s.cfg.Dir)) {
		return s.account, s.generation
	}
	var gen uint64
	s.account, s.accountFiles = readLaneAsOne(s.cfg.Dir, func() (lane string) {
		lane, gen = s.readAccount()
		return lane
	})
	s.generation = gen
	// An unreadable one is not kept: a file made readable again (its permissions) need not
	// look changed.
	s.accountRead = s.account != sendas.AccountUnreadable
	return s.account, s.generation
}

// readLaneAsOne runs read, which reads config.json in dir and then the profile's sign-in,
// and is its lane only if neither file changed meanwhile: one replaced between the two
// reads (a sign-in as someone else, a switch to a server key) may pair one sign-in's
// profile with another's identity, so the lane is sendas.AccountUnreadable instead. files
// are the two as they were before read.
func readLaneAsOne(dir string, read func() string) (lane string, files [2]os.FileInfo) {
	files = profileFiles(dir)
	lane = read()
	if !sameFiles(files, profileFiles(dir)) {
		return sendas.AccountUnreadable, files
	}
	return lane, files
}

// profileFiles are config.json and the sign-in in dir, nil where one is absent.
func profileFiles(dir string) [2]os.FileInfo {
	return [2]os.FileInfo{statOrNil(config.Path(dir)), statOrNil(config.CredentialsPath(dir))}
}

func sameFiles(was, now [2]os.FileInfo) bool {
	return unchanged(was[0], now[0]) && unchanged(was[1], now[1])
}

// readAccount reads the profile's mode and pairing, then its sign-in, with the generation
// of the lane: the sign-in's (auth.Credential.Generation) plus the profile's switches
// between the server key and a person (config.Profile.ServerKeyGeneration). Each only
// grows, so the sum moves whenever either does: a switch to another person, to the key or
// back. A profile whose mode can't be read may be a server key's, which keeps an earlier
// person's sign-in on disk, or a person's: neither's, so the unreadable account. A sign-in
// or sign-out behind the key moves the generation all the same, so an export's retry that
// meets one is lost, though its lane is the key's throughout.
func (s *signInSource) readAccount() (string, uint64) {
	file, err := config.LoadFile(s.cfg.Dir)
	if err != nil {
		return sendas.AccountUnreadable, 0
	}
	p := file.Profiles[s.cfg.ProfileName]
	if p == nil {
		p = &config.Profile{}
	}
	if p.ServerKeySignIn {
		_, gen := auth.AccountAt(s.cfg.Dir, s.cfg.ProfileName)
		return sendas.NoPersonLane, gen + p.ServerKeyGeneration
	}
	if s.betweenReads != nil {
		s.betweenReads()
	}
	account, gen := auth.AccountAt(s.cfg.Dir, s.cfg.ProfileName)
	return pairedLane(account, p.Account), gen + p.ServerKeyGeneration
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
// retired), so what waits for it is held; an error for what a retry may fix (a locked
// keychain, an auth host out of reach), which the relay also waits out as keyless.
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
	if held.SessionID != ident.SessionID || auth.AccountKey(held.AuthURL, held.UserID) != account || !held.RetiredAt.IsZero() {
		return "", nil
	}
	s.handedOut(held.AccessToken, held.SessionID)
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

// Retired records that a server refused token's sign-in as retired: on the sign-in that
// sent it, while it is still the one stored. A sign-in made since, which another route's
// send may already hold, is not the one refused.
func (s *signInSource) Retired(token string) {
	s.mu.Lock()
	client := s.client
	session := ""
	if i := slices.IndexFunc(s.sent, func(t sentToken) bool { return t.token == token }); i >= 0 {
		session = s.sent[i].session
	}
	s.mu.Unlock()
	if client == nil {
		return
	}
	// The session that sent token, even if it has rotated since; never a sign-in made since.
	// Marked on the client too: a store that can't take the mark must not have Token hand
	// out a token of the retired session again.
	if held := client.Credential(); held.AccessToken == token || session != "" && held.SessionID == session {
		client.MarkRetired()
	}
}

// bindRelay has the relay send and heartbeat as the profile's sign-in, bind what it accepts
// to that sign-in's account, and name this installation on its heartbeat. The id is read as
// it is needed, at each heartbeat: a sign-in after the relay started writes it.
func (app *App) bindRelay(engine *relay.Options, cfg *config.Config) {
	src := app.signInSource(cfg)
	engine.SignIn = src
	engine.HeartbeatSend = app.relayHeartbeat(src)
	engine.Installation = func() string { return config.StoredInstallationID(app.dir) }
	engine.HookLosses = func() map[string]int {
		losses, _ := spool.TakeNewLosses(filepath.Join(app.stateDir, spool.Dir))
		return losses
	}
}
