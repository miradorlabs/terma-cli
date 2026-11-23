package antigravity

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// Which checkout a turn's calls go under: each hook decides its own turn, and a call goes
// only where its turn was decided collected, or is held for a hook there.

// antigravityRoute names the checkout a collecting hook runs in: its path, its origin and
// the project it sends to; "" for a hook the policy does not collect.
func antigravityRoute(r *hookrun.Repo) string {
	if r == nil {
		return ""
	}
	return r.Root + "\x00" + r.Repository.Origin + "\x00" + r.ProjectID
}

// antigravityTurnInDoubt reports whether a call at step, read as turn's, may be another
// turn's: a decided prompt lies between turn's and the call, which a rewrite dropped.
func antigravityTurnInDoubt(turn string, step int64, turns map[string]string) bool {
	n := antigravityTurnNumber(turn)
	for t := range turns {
		if m := antigravityTurnNumber(t); m > n && m-1 < step {
			return true
		}
	}
	return false
}

// sendAntigravityHeld sends each held line whose turn was decided for route, read again
// at its offset and checked to be the same step, and returns what stays held: lines for
// another checkout, and those emit refused. A line whose turn is no longer kept, or that
// is no longer there, goes.
func sendAntigravityHeld(ctx context.Context, transcript string, held []antigravityHeld, turns map[string]string, route string, send func(antigravityStep, string) bool) []antigravityHeld {
	if len(held) == 0 {
		return nil
	}
	f, err := os.Open(transcript)
	if err != nil {
		return held
	}
	defer func() { _ = f.Close() }()
	var kept []antigravityHeld
	for _, h := range held {
		decided, ok := turns[h.Turn]
		if !ok {
			continue
		}
		if decided != route || ctx.Err() != nil || h.Length <= 0 || h.Length > antigravityLineLimit {
			kept = append(kept, h)
			continue
		}
		line := make([]byte, h.Length)
		var s antigravityStep
		if _, err := f.ReadAt(line, h.Offset); err != nil || json.Unmarshal(line, &s) != nil || s.StepIndex == nil || *s.StepIndex != h.Step {
			continue
		}
		s.subagentResult = h.Subagent
		if !send(s, h.Turn) {
			kept = append(kept, h)
		}
	}
	return kept
}

// decideAntigravityTurn records turn's decision, the route of the checkout a collecting
// hook ran it in (antigravityRoute) or "" for none, keeping the newest antigravityTurnsKept. A turn keeps its first
// decision: a later hook of it runs in the same checkout. A subagent's transcript holds no
// prompt, so its calls' turn is "", decided like any other.
func decideAntigravityTurn(turns map[string]string, turn, root string) map[string]string {
	out := make(map[string]string, len(turns)+1)
	for t, d := range turns {
		out[t] = d
	}
	if _, ok := out[turn]; !ok && !strings.HasPrefix(turn, antigravityUnknownTurn) {
		out[turn] = root
	}
	for len(out) > antigravityTurnsKept {
		oldest, n := "", int64(-1)
		for t := range out {
			if m := antigravityTurnNumber(t); n < 0 || m < n {
				oldest, n = t, m
			}
		}
		delete(out, oldest)
	}
	return out
}

// antigravityTurnNumber is the number in a turn id, -1 for none.
func antigravityTurnNumber(turn string) int64 {
	n, err := strconv.ParseInt(strings.TrimPrefix(turn, "turn-"), 10, 64)
	if err != nil || !strings.HasPrefix(turn, "turn-") {
		return -1
	}
	return n
}
