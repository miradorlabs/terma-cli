package claude

import (
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/live"
)

// The status line's rate-limit windows warn while they are used faster than they reset
// (live.Project): a note under the developer's own status line for as long as the warning
// holds, and one desktop notification per window. For now only Claude Code warns, from its
// status line; codexQuota marks where Codex's windows would plug in.

// claudePaceStateDir records each window that has warned, so each notifies once.
var claudePaceStateDir = hookrun.AgentStateDir(name, "pace-warnings")

// paceWarnings are the payload's windows on course to run out before they reset, the
// soonest to run out first.
func paceWarnings(p *statusLinePayload, now time.Time) []live.Pace {
	var out []live.Pace
	for _, w := range quotaWindows {
		rl, ok := p.RateLimits[w.name]
		if !ok || w.length == 0 || rl.UsedPercentage == nil || rl.ResetsAt == nil {
			continue
		}
		window := live.Window{Name: w.name, Length: w.length, UsedPercent: *rl.UsedPercentage, ResetsAt: time.Unix(*rl.ResetsAt, 0)}
		if pace, ok := live.Project(window, now); ok {
			out = append(out, pace)
		}
	}
	slices.SortFunc(out, func(a, b live.Pace) int { return a.RunsOut.Compare(b.RunsOut) })
	return out
}

// notifyPace notifies the first warning of each window in paces.
func notifyPace(env hookrun.Env, paces []live.Pace) {
	if env.Notifier == nil || env.StateDir == "" {
		return
	}
	dir, now := filepath.Join(env.StateDir, claudePaceStateDir), env.Time()
	for _, p := range paces {
		if live.FirstWarning(dir, p, now) {
			env.Notifier.Notify(p.Message(displayName, now))
		}
	}
}

// withPaceNote adds the notes of paces as a row of their own under what was drawn. Nothing
// drawn stays nothing, as withIndicator leaves it, so a renderer can still hide the status line.
func withPaceNote(drawn []byte, paces []live.Pace, now time.Time) []byte {
	if len(paces) == 0 || len(drawn) == 0 {
		return drawn
	}
	notes := make([]string, len(paces))
	for i, p := range paces {
		notes[i] = p.Note(now)
	}
	note := strings.Join(notes, " · ")
	if drawn[len(drawn)-1] != '\n' {
		drawn = append(drawn, '\n')
	}
	return append(drawn, note...)
}
