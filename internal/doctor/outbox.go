package doctor

import (
	"fmt"
	"strings"
	"time"
)

// OutboxLossWindow is how long doctor reports a loss of the relay's outbox: as long as the
// outbox keeps a part.
const OutboxLossWindow = 14 * 24 * time.Hour

// OutboxCheck reports what the relay dropped of what it could not send: records past the
// outbox's room or age, which means sends failed for long, and claimed records a stopping
// relay dropped from memory while their team's policy could not be read.
func OutboxCheck(r Relay) Check {
	var lost []string
	if r.OutboxFull > 0 {
		lost = append(lost, fmt.Sprintf("%d because the outbox was full", r.OutboxFull))
	}
	if r.OutboxExpired > 0 {
		lost = append(lost, fmt.Sprintf("%d older than it keeps them", r.OutboxExpired))
	}
	if r.OutboxHeld > 0 {
		lost = append(lost, fmt.Sprintf("%d held while their team's policy could not be read, when the relay stopped", r.OutboxHeld))
	}
	detail := "the relay dropped records it could not send: " + strings.Join(lost, ", ")
	if !r.OutboxLossAt.IsZero() {
		detail += "; the last at " + r.OutboxLossAt.Local().Format("2006-01-02 15:04")
	}
	return Check{Status: Warn, Detail: detail, Fix: "terma setup"}
}

// relayFacts is the relay's state, the zero one when there is no probe.
func relayFacts(p Probes) Relay {
	if p.Relay == nil {
		return Relay{}
	}
	return p.Relay()
}
