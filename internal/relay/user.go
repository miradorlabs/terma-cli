package relay

import "github.com/miradorlabs/terma-cli/internal/semconv"

// nameUser names pol.User on every resource of the part as terma.user.id, the Mirador user
// terma is signed in as, which the heartbeat names too. It is the relay's to name, never the
// agent's: whatever the agent sent under the key goes first. An id, not a name, it leaves under
// any policy, on the resource alone (withhold). Run after the content policy.
func nameUser(p *part, pol Policy) {
	unstamp(p, semconv.TermaUserIDKey)
	if pol.User != "" {
		stamp(p, semconv.TermaUserIDKey, pol.User)
	}
}
