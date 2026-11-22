package relay

import "github.com/miradorlabs/terma-cli/internal/semconv"

// userSafeKeys name the user the relay stamps (nameUser): an id, not a name, safe under any
// policy on the resource the relay stamps alone (withhold), as checkoutSafeKeys are.
var userSafeKeys = []string{semconv.TermaUserIDKey}

// nameUser names pol.User on every resource of the part as terma.user.id, the Mirador user
// terma is signed in as, which the heartbeat names too. It is the relay's to name, never the
// agent's: whatever the agent sent under the key goes first. An id, not a name, it leaves under
// any policy, on the resource alone (withhold). Run after the content policy.
//
// owner is the account the part will leave under (Relay.ownerAt, judged when it is enqueued): only that account's
// user is named. One no person collected, as under a server key, names no one, even when a
// person's sign-in is in place by the time it is placed.
func nameUser(p *part, pol Policy, owner string) {
	unstamp(p, semconv.TermaUserIDKey)
	if pol.User != "" && owner != "" && owner == pol.UserAccount {
		stamp(p, semconv.TermaUserIDKey, pol.User)
	}
}
