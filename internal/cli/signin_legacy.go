package cli

import (
	"context"
	"path/filepath"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// retireOlderSignIns ends and deletes what an older terma signed this machine in with:
// its per-organization sessions, revoked best-effort with their own tokens, and the team
// keys it minted with them. A server key a profile was set up with (TERMA_API_KEY) stays.
func (app *App) retireOlderSignIns(ctx context.Context, authURL string) {
	_ = app.retireLegacySessions(ctx, authURL, false)
	_ = keystore.RetainOnly(app.dir, serverKeyTeams(app.dir))
}

// retireLegacySessions revokes, best-effort, and deletes the per-organization sessions an
// older terma signed in: all at once, within one deadline, so an unreachable auth host
// delays a sign-in by seconds, not seconds per organization. A token goes only to authURL,
// the auth host this profile is configured for: credentials.json names hosts, and a
// tampered one must not lead a keychain-held token elsewhere; another environment's
// sessions are forgotten unrevoked, to expire. Their ids are kept for the heartbeat, and
// whose what was queued under them is, decided once (auth.LegacyOwner): no person's when
// serverKey, the profile being set up with a server key. Unless that record can't be
// written, when they stay for the next sign-in to retire.
//
// With no older sign-ins left but what an older terma queued still here (it signed out
// before the upgrade, say), that backlog names no one: it is recorded ambiguous, or no
// person's under serverKey, so it is taken in and ages out counted.
//
// It reports only an older terma that held their lock too long, which left them all to the
// next sign-in; every other failure leaves them for it too, as the record says.
func (app *App) retireLegacySessions(ctx context.Context, authURL string, serverKey bool) error {
	if !auth.HasLegacySignIns(app.dir) {
		if app.hasLegacyBacklog() {
			_ = auth.RecordLegacy(app.stateDir, nil, auth.LegacyOwner(nil, true, serverKey), time.Now(), app.legacyStages()...)
		}
		return nil
	}
	// One snapshot, read, recorded and forgotten under the lock an older terma writes the
	// file under; one that holds it too long leaves them for the next sign-in.
	return auth.WithLegacySignIns(ctx, app.dir, func() {
		signIns, whole, err := app.legacySignIns(app.dir, true)
		if err != nil {
			// Not read, so not decided: they stay, for the next sign-in to retire. Deleted
			// now, what that terma queued would be left with no decision at all.
			return
		}
		// The decision is kept before they go, from the same read: without it, keep them,
		// and the next sign-in tries again. A file that names no sign-in, or doesn't parse
		// whole, names no one; what could be read of it is still revoked and forgotten below.
		owner := auth.LegacyOwner(signIns, whole, serverKey)
		if auth.RecordLegacy(app.stateDir, signIns, owner, time.Now(), app.legacyStages()...) != nil {
			return
		}
		revokeCtx, cancel := context.WithTimeout(ctx, legacyRevokeWait)
		var wg sync.WaitGroup
		for _, s := range signIns {
			if s.AccessToken != "" && sameHost(s.AuthURL, authURL) {
				wg.Go(func() { _ = api.RevokeLegacySession(revokeCtx, s.AuthURL, app.version, s.AccessToken) })
			}
		}
		wg.Wait()
		cancel()
		_ = auth.ForgetLegacy(app.dir)
	})
}

// legacyStages set aside what an older terma holds in its spool file and its relay's outbox,
// as the decision of whose it is is taken (auth.RecordLegacy).
func (app *App) legacyStages() []auth.LegacyStage {
	return []auth.LegacyStage{
		{Queue: auth.LegacySpool, Stage: func() error {
			s, err := spool.Open(filepath.Join(app.stateDir, spool.Dir))
			if err != nil {
				return err
			}
			return s.StageLegacy()
		}},
		{Queue: auth.LegacyOutbox, Stage: func() error {
			return relay.StageLegacyOutbox(filepath.Join(claim.Dir(app.stateDir), relay.LegacyOutboxDir))
		}},
	}
}

// hasLegacyBacklog reports whether an older terma left anything queued: in its spool file or
// its relay's outbox.
func (app *App) hasLegacyBacklog() bool {
	return spool.HasLegacy(filepath.Join(app.stateDir, spool.Dir)) ||
		relay.HasLegacyOutbox(filepath.Join(claim.Dir(app.stateDir), relay.LegacyOutboxDir))
}

// legacyRevokeWait bounds the best-effort revokes of an older terma's sessions.
const legacyRevokeWait = 5 * time.Second
