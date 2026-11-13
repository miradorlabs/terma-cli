package relay

import (
	"sort"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// Terma Desktop's system governance reports what the operating system saw an agent session's
// processes do: connections, files, denials (terma.capture.surface governance). It reports on
// every process of every session it sees, none of which a claim names, so the relay trusts the
// app itself, by the code signature the kernel holds for it, for those records and nothing
// else: a governance part goes where its session's claim placed the session at the record's
// time (claim.During). The session must still be claimed, its repository collected, and the
// team's content policy applies as to any record. The app is judged once per connection, when
// its sender is found (senderOf), so a pid used again later inherits nothing.
const (
	desktopTeamID     = "YR8HVF742J" // Mirador Labs Inc.
	desktopIdentifier = "ai.terma.desktop"
)

// DesktopSender reports whether pid is the Terma app as Mirador Labs signed it, with a
// signature that still holds.
func DesktopSender(pid int) bool {
	s, ok := procinfo.Signer(pid)
	return ok && s.Valid && !s.Platform && s.TeamID == desktopTeamID && s.Identifier == desktopIdentifier
}

// governanceOnly reports whether every record of a log part is a governance record: the app's
// trust covers no other kind.
func governanceOnly(p *part) bool {
	logs, ok := p.msg.(*logspb.LogsData)
	if !ok {
		return false
	}
	n := 0
	for _, rl := range logs.GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				if attrString(lr.GetAttributes(), semconv.TermaCaptureSurfaceKey) != semconv.TermaCaptureSurfaceGovernance {
					return false
				}
				n++
			}
		}
	}
	return n > 0
}

// markGovernance marks p a governance part when the signed app sent it and it holds nothing
// else. Only its session's claim may send it (part.claimed): global mode's catch-all, which
// takes what no claim places, never takes it.
func markGovernance(p *part, desktop bool) {
	if !desktop || !governanceOnly(p) {
		return
	}
	p.governance, p.claimed = true, true
	p.until = nanos(lastLog(p.msg.(*logspb.LogsData)))
}

// byPlacement is p cut where its session moved to another placement within the time its
// records span, so each piece lies within one placement and goes where claim.During puts it;
// p alone for any other part, or when the session has no claim yet or did not move. A move
// the claim learns later makes the part ambiguous (decideClaimed): loss, never a guess.
func (r *Relay) byPlacement(p *part) []*part {
	if !p.governance {
		return []*part{p}
	}
	c, ok := r.lookup(p.session)
	if !ok {
		return []*part{p}
	}
	cuts := c.Moves(p.at, p.until)
	if len(cuts) == 0 {
		return []*part{p}
	}
	m := p.msg.(*logspb.LogsData)
	pieces := make([]*logspb.LogsData, len(cuts)+1)
	for _, rl := range m.GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			byPiece := make([][]*logspb.LogRecord, len(pieces))
			for _, lr := range sl.GetLogRecords() {
				t := nanos(logTime(lr))
				i := sort.Search(len(cuts), func(i int) bool { return cuts[i].After(t) })
				byPiece[i] = append(byPiece[i], lr)
			}
			for i, recs := range byPiece {
				if len(recs) == 0 {
					continue
				}
				if pieces[i] == nil {
					pieces[i] = &logspb.LogsData{}
				}
				pieces[i].ResourceLogs = append(pieces[i].ResourceLogs, &logspb.ResourceLogs{
					Resource: rl.GetResource(), SchemaUrl: rl.GetSchemaUrl(),
					ScopeLogs: []*logspb.ScopeLogs{{Scope: sl.GetScope(), SchemaUrl: sl.GetSchemaUrl(), LogRecords: recs}},
				})
			}
		}
	}
	var out []*part
	for _, m := range pieces {
		if m == nil {
			continue
		}
		q := *p
		q.msg, q.records, q.at = m, 0, earliest(m)
		q.until = nanos(lastLog(m))
		for _, rl := range m.ResourceLogs {
			q.records += len(rl.ScopeLogs[0].LogRecords)
		}
		out = append(out, &q)
	}
	return out
}

// logTime is when a record happened, else when it was observed, as earliest reads it.
func logTime(lr *logspb.LogRecord) uint64 {
	if t := lr.GetTimeUnixNano(); t != 0 {
		return t
	}
	return lr.GetObservedTimeUnixNano()
}

// lastLog is the latest record time in m, in nanoseconds since the Unix epoch.
func lastLog(m *logspb.LogsData) uint64 {
	var last uint64
	for _, rl := range m.GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				last = max(last, logTime(lr))
			}
		}
	}
	return last
}
