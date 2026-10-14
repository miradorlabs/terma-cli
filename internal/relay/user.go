package relay

import "github.com/miradorlabs/terma-cli/internal/semconv"

// userSafeKeys name the user the relay stamps on an anonymous agent's part, by Mirador id: it
// leaves under any policy, as the heartbeat names the same user, on the resource the relay
// stamps alone (withhold). The email beside it is personal and leaves only with content.
var userSafeKeys = []string{semconv.TermaUserIDKey}

// nameUser makes the user a part names the relay's: no agent's part keeps a terma.user.id of
// its own, and an anonymous agent's (Policy.Anonymous) loses its user.email too, then names
// pol.User, its email only where the policy sends both kinds of content, since a policy
// withholding either withholds user.email, as it does a hook event's
// (delivery.Router.Outgoing). Run after the content policy.
func nameUser(p *part, pol Policy) {
	unstamp(p, semconv.TermaUserIDKey)
	if !pol.Anonymous {
		return
	}
	unstamp(p, semconv.UserEmailKey)
	if pol.User.ID == "" {
		return
	}
	stamp(p, semconv.TermaUserIDKey, pol.User.ID)
	if pol.User.Email != "" && pol.IncludePrompts && pol.IncludeToolContent {
		stamp(p, semconv.UserEmailKey, pol.User.Email)
	}
}
