package relay

import (
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
// team's content policy applies as to any record.
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
