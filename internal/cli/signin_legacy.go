package cli

import (
	"context"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
)

// retireOlderSignIns ends and deletes what an older terma signed this machine in with:
// its per-organization sessions, revoked best-effort with their own tokens, and the team
// keys it minted with them. A server key a profile was set up with (TERMA_API_KEY) stays.
func (app *App) retireOlderSignIns(ctx context.Context, authURL string) {
	app.retireLegacySessions(ctx, authURL)
	_ = keystore.RetainOnly(app.dir, serverKeyTeams(app.dir))
}

// retireLegacySessions revokes, best-effort, and deletes the per-organization sessions an
// older terma signed in: all at once, within one deadline, so an unreachable auth host
// delays a sign-in by seconds, not seconds per organization. A token goes only to authURL,
// the auth host this profile is configured for: credentials.json names hosts, and a
// tampered one must not lead a keychain-held token elsewhere; another environment's
// sessions are forgotten unrevoked, to expire. Their ids are kept for the heartbeat and
// their account for what was queued under them (auth.RecordLegacy); unless that record
// can't be written, when they stay for the next sign-in to retire.
func (app *App) retireLegacySessions(ctx context.Context, authURL string) {
	// None left, as on every sign-in once they are retired: the record, and the boundary
	// and the mark it keeps, stay as the retirement left them.
	if !auth.HasLegacySignIns(app.dir) {
		return
	}
	signIns, err := app.legacySignIns(app.dir, true)
	if err != nil {
		// Not read, so not recorded: they stay, for the next sign-in to retire. Deleted
		// now, what that terma queued would be left with no account at all.
		return
	}
	// The record is the only account what an older terma queued has once its sign-ins
	// are gone: without it, keep them, and the next sign-in tries again.
	// A file that names no sign-in, or doesn't read whole, names no one, and says so;
	// what could be read of it is still revoked and forgotten below.
	var record error
	recorded := time.Now()
	if auth.HasLegacySignIns(app.dir) && (len(signIns) == 0 || !auth.LegacyParsedWhole(app.dir)) {
		record = auth.RecordNoOne(app.stateDir, signIns, recorded)
	} else {
		record = auth.RecordLegacy(app.stateDir, signIns, recorded)
	}
	if record != nil {
		return
	}
	// The boundary moves to when the retirement is done (below): an older terma's hook
	// still running queues on that terma's side until its sign-ins are gone.
	defer func() { _ = auth.AdvanceLegacyBoundary(app.stateDir, recorded, time.Now()) }()
	revokeCtx, cancel := context.WithTimeout(ctx, legacyRevokeWait)
	var wg sync.WaitGroup
	for _, s := range signIns {
		if s.AccessToken != "" && sameHost(s.AuthURL, authURL) {
			wg.Go(func() { _ = api.RevokeLegacySession(revokeCtx, s.AuthURL, app.version, s.AccessToken) })
		}
	}
	wg.Wait()
	cancel()
	// What an older hook queued until the sign-ins are gone stays theirs in the spool,
	// this retirement the first or a retry: marked before they go, so a reader that
	// sees them gone sees at most the keychain deletes' moments unmarked, and again
	// once they are.
	if auth.MarkLegacyForgotten(app.stateDir, time.Now()) != nil {
		return
	}
	if auth.ForgetLegacy(app.dir) == nil {
		_ = auth.MarkLegacyForgotten(app.stateDir, time.Now())
	}
}

// legacyRevokeWait bounds the best-effort revokes of an older terma's sessions.
const legacyRevokeWait = 5 * time.Second
