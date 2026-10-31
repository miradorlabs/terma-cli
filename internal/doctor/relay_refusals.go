package doctor

import (
	"fmt"
	"strings"
	"time"
)

// refusalsRecent is how long after the relay's last refusal doctor still takes refusals to
// be going on, rather than left in the counters by an exporter fixed since.
const refusalsRecent = time.Hour

// RefusalsCheck says whether the running relay refuses what agents export to it. A request
// with a token not the relay's is never counted as received, so the export check cannot see
// it, and nothing in it is forwarded. envNamed says the live agent environment check named
// the variables behind it.
func RefusalsCheck(r Refusals, now time.Time, envNamed bool) Check {
	since := " since " + r.Since.Local().Format("Jan 2 15:04")
	var what []string
	if r.Unauthorized > 0 {
		what = append(what, fmt.Sprintf("%d export requests for a token not its own", r.Unauthorized))
	}
	if r.Unreadable > 0 {
		what = append(what, fmt.Sprintf("%d it could not read", r.Unreadable))
	}
	if len(what) == 0 {
		return Check{Status: Pass, Detail: "the local relay has refused no export" + since}
	}
	detail := "the local relay has refused " + strings.Join(what, " and ") + since
	if !r.At.IsZero() {
		detail += ", the last at " + r.At.Local().Format("Jan 2 15:04")
		if now.Sub(r.At) >= refusalsRecent {
			return Check{Status: Pass, Detail: detail + "; none in the last hour"}
		}
	}
	if r.Unauthorized > 0 {
		detail += ": an agent process started with a stale or foreign exporter header or token, and nothing it exports reaches Terma"
	} else {
		detail += ": an exporter sends requests the relay cannot read, and nothing in them reaches Terma"
	}
	fix := "restart the agent sessions, and remove the exporter variable where it is set (the IDE or agent host that started them, or a shell profile)"
	if envNamed {
		fix += "; the live agent environment check above names the processes and variables"
	}
	return Check{Status: Warn, Detail: detail, Fix: fix}
}
