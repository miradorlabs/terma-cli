package doctor

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// OutboxLossWindow is how long doctor reports a loss of the relay's outbox: as long as the
// outbox keeps a part.
const OutboxLossWindow = 14 * 24 * time.Hour

// outboxLosses say, by the relay's and the hooks' reason (relay.OutboxLosses,
// spool.ReadLosses), what each loss was, in the
// order doctor lists them.
var outboxLosses = []struct{ reason, what string }{
	{"outbox_full", "because the outbox was full"},
	{"outbox_expired", "older than it keeps them"},
	{"legacy_ambiguous", "an older terma queued that no one sign-in may send, past its age"},
	{"policy_timeout", "claimed for a team whose policy or key the relay couldn't read within 15 minutes"},
	{"signed_out", "collected while signed out"},
	{"unreadable", "collected while the sign-in could not be read"},
}

// OutboxCheck reports what the relay and hooks lost of what they collected, by reason:
// records past the outbox's room or age, which means sends failed for long; claimed records
// dropped from memory while they waited for a team's policy or key the relay couldn't read; and
// records collected with no sign-in to send them as. A reason it has no words for is
// listed by name.
func OutboxCheck(r Relay) Check {
	var lost []string
	known := map[string]bool{}
	for _, l := range outboxLosses {
		known[l.reason] = true
		if n := r.OutboxLosses[l.reason]; n > 0 {
			lost = append(lost, fmt.Sprintf("%d %s", n, l.what))
		}
	}
	for _, reason := range slices.Sorted(maps.Keys(r.OutboxLosses)) {
		if n := r.OutboxLosses[reason]; n > 0 && !known[reason] {
			lost = append(lost, fmt.Sprintf("%d lost as %s", n, reason))
		}
	}
	detail := "terma dropped records it could not send: " + strings.Join(lost, ", ")
	if !r.OutboxLossAt.IsZero() {
		detail += "; the last at " + r.OutboxLossAt.Local().Format("2006-01-02 15:04")
	}
	// Sends that failed for long, and a sign-out, are this machine's to fix; a policy or key
	// the relay couldn't read is not.
	fix := ""
	switch {
	case r.OutboxLosses["outbox_full"]+r.OutboxLosses["outbox_expired"] > 0:
		fix = "terma setup"
	case r.OutboxLosses["signed_out"] > 0:
		fix = "terma login"
	}
	return Check{Status: Warn, Detail: detail, Fix: fix}
}

// lostRecently reports whether the relay or a hook lost any record within OutboxLossWindow: an older
// loss is history.
func (r Relay) lostRecently() bool {
	for _, n := range r.OutboxLosses {
		if n > 0 {
			return time.Since(r.OutboxLossAt) < OutboxLossWindow
		}
	}
	return false
}

// relayFacts is the relay's state, the zero one when there is no probe.
func relayFacts(p Probes) Relay {
	if p.Relay == nil {
		return Relay{}
	}
	return p.Relay()
}
