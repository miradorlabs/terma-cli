package hookrun

import (
	"slices"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
	"github.com/miradorlabs/terma-cli/internal/trailer"
)

// A push's sessions: those its report lists, within its caps, and every one, which its lane
// is chosen from.

// pushedLists are the commit ids terma.push.commits lists, at most MaxPushCommits, and the
// sessions stamped into those commits alone, each agent's session once, at most
// maxPushSessions; all is every session stamped into any of the commits, each once, which
// the push's lane is chosen from (pushLane). shas is never nil: an empty list is still the
// string[] the registry declares.
func pushedLists(commits []gitx.PushedCommit) (shas []string, sessions, all []trailer.Trailer) {
	listed := min(len(commits), MaxPushCommits)
	shas = make([]string, 0, listed)
	var stamped []trailer.Trailer
	for i, c := range commits {
		if i < listed {
			shas = append(shas, c.SHA)
		}
		for _, t := range trailer.Parse(c.Trailers, "") {
			if session.ValidID(t.SessionID) {
				stamped = append(stamped, t)
				if i < listed {
					sessions = append(sessions, t)
				}
			}
		}
	}
	sessions = uniqueSessions(sessions)
	return shas, sessions[:min(len(sessions), maxPushSessions)], uniqueSessions(stamped)
}

// lane is the pre-push hook's lane, as its record's event names it: with none, the hook could
// not read its sign-in, in an agent's sandbox.
func (rec pushRecord) lane() spool.Lane {
	if rec.Event.Account == "" && !rec.Event.NoPerson {
		return spool.Lane{Lost: sendas.LostUnreadable}
	}
	return spool.Lane{Account: rec.Event.Account, NoPerson: rec.Event.NoPerson}
}

// pushLane is recordLane over the accounts of every session a push carries, read one
// manifest at a time and no further than the first that keeps the push from one lane:
// recordLane refuses a list only for an element it refuses beside the first, so that pair
// is checked as each is read.
func pushLane(own spool.Lane, store *session.Store, sessions []trailer.Trailer) (spool.Lane, bool) {
	var first []string
	for i, t := range sessions {
		account := accountsOf(store, []trailer.Trailer{t})
		if i == 0 {
			first = account
		}
		if _, ok := recordLane(own, append(slices.Clip(first), account...)); !ok {
			return spool.Lane{}, false
		}
	}
	return recordLane(own, first)
}
