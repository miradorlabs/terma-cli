package muse

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Muse's hooks say what happened; its session protocol also says how long it took. Its
// host serves a live view only of the sessions it runs itself, so terma reads the others'
// stored views, once a turn or the session ends: the Stop or SessionEnd hook starts a
// detached `terma hook muse-feed`, which reads the session's view from where the last read
// left it through a `muse serve` of its own and spools what the hooks lack: each tool call's
// duration, folding into the hook's call by its id; each turn's duration and time to its
// first token; the session's name; and, at most every museQuotaEvery, the plan's usage.
// Model calls stay the hooks': theirs name every call, a reminder's too, by a request id the
// protocol's usage records lack.

const (
	// museFeedEvent is the detached reader, `terma hook muse-feed <session> <cwd> [turn]`.
	museFeedEvent = "muse-feed"
	// sourceMuseSession is terma.evidence.source for what Muse's session protocol reported.
	sourceMuseSession = "muse_session"
	// sourceMuseUsage is for what `muse usage` reported.
	sourceMuseUsage = "muse_usage"

	museQuotaEvery = 5 * time.Minute
	// museReadEvery spaces a session's reads: each starts a `muse serve` for a moment
	// (~0.2 s, ~125 MB), and what a turn's end skips the next read takes up from its cursor.
	// The session's end always reads.
	museReadEvery = 2 * time.Minute
	museViewPage  = 200
	// museTurnWait is how long the reader waits for the turn that ended to reach the view:
	// Muse records it after the Stop hook returns.
	museTurnWait = 10 * time.Second
)

// museFeedDir holds, per session, how far its view has been read, and when the plan's usage
// was last read; musePendingDir the sessions whose last turns no read has taken up yet.
var (
	museFeedDir    = hookrun.AgentStateDir(name, "feed")
	musePendingDir = hookrun.AgentStateDir(name, "pending-reads")
)

// StateDirs are the directories the reader keeps state in.
func (Agent) StateDirs() []string { return []string{museFeedDir, musePendingDir} }

// pendingRead is a session a turn's end did not read, for a later hook to.
type pendingRead struct {
	Session string `json:"session"`
	Cwd     string `json:"cwd"`
}

// viewCursor is how far a session's view has been read, and the tool calls it saw start
// and not yet finish, whose duration their finish completes.
type viewCursor struct {
	Cursor  string               `json:"cursor,omitempty"`
	Started map[string]time.Time `json:"started,omitempty"`
}

// readLater starts the detached reader for the root session, under the developer's consent:
// at a turn's end only once museReadEvery has passed since the session's last read, at its
// end always. Each read rewrites the session's cursor, so its time is the last read's. A
// turn's end it skips leaves the session pending, so a session closed with no end of its own
// (a terminal closed under it: Muse then runs no SessionEnd) is read by a later hook.
func readLater(env hookrun.Env, root, cwd, turn string, final bool) {
	if env.Detach == nil || !consented(env.Consent()) {
		return
	}
	defer catchUp(env, root)
	cursor := filepath.Join(env.StateDir, museFeedDir, hookrun.EvidenceID(root)+".json")
	if info, err := os.Stat(cursor); !final && err == nil && env.Time().Sub(info.ModTime()) < museReadEvery {
		markPending(env, root, cwd)
		return
	}
	env.Detach(museFeedEvent, root, cwd, turn)
}

// markPending records that the session has turns no read took up; its time is the first
// skipped turn's, which catchUp waits museReadEvery past.
func markPending(env hookrun.Env, root, cwd string) {
	dir := filepath.Join(env.StateDir, musePendingDir)
	path := filepath.Join(dir, hookrun.EvidenceID(root)+".json")
	if _, err := os.Stat(path); err == nil {
		return
	}
	b, err := json.Marshal(pendingRead{Session: root, Cwd: cwd})
	if err != nil || os.MkdirAll(dir, 0o700) != nil {
		return
	}
	_ = hookrun.WriteState(path, b)
}

// catchUp starts a read of each other session pending for museReadEvery, under the
// developer's consent, and touches its record, so a read that keeps failing is retried no
// more often than a turn's end would.
func catchUp(env hookrun.Env, except string) {
	if env.Detach == nil || !consented(env.Consent()) {
		return
	}
	dir := filepath.Join(env.StateDir, musePendingDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	now := env.Time()
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !strings.HasSuffix(e.Name(), ".json") || now.Sub(info.ModTime()) < museReadEvery {
			continue
		}
		path := filepath.Join(dir, e.Name())
		var p pendingRead
		if b, err := os.ReadFile(path); err != nil || json.Unmarshal(b, &p) != nil || p.Session == except || !session.ValidID(p.Session) {
			continue
		}
		_ = os.Chtimes(path, now, now)
		env.Detach(museFeedEvent, p.Session, p.Cwd, "")
	}
}

// feed is the detached reader: `terma hook muse-feed <session> <cwd> [turn]`.
func feed(ctx context.Context, env hookrun.Env) error {
	if len(env.Args) < 2 {
		return nil
	}
	root, cwd, turn := env.Args[0], env.Args[1], ""
	if len(env.Args) > 2 {
		turn = env.Args[2]
	}
	r, ok := env.Open(ctx, root, cwd)
	if !ok {
		return nil
	}
	dir := filepath.Join(env.StateDir, museFeedDir)
	if os.MkdirAll(dir, 0o700) != nil {
		return nil
	}
	path := filepath.Join(dir, hookrun.EvidenceID(root)+".json")
	unlock, err := hookrun.LockEvidence(path + ".lock")
	if err != nil {
		return nil
	}
	defer unlock()
	host, err := startMSPHost(ctx, env.Version)
	if err != nil {
		env.Logf("muse feed: %v", err)
		return nil
	}
	defer host.close()
	var cur viewCursor
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &cur)
	}
	if cur.Started == nil {
		cur.Started = map[string]time.Time{}
	}
	deadline := env.Time().Add(museTurnWait)
	for {
		seen := readView(ctx, host, root, &cur, func(rec spool.Event) { env.EmitFor(r, rec) }, env.Logf)
		if turn == "" || seen[turn] || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if b, err := json.Marshal(cur); err == nil {
		_ = hookrun.WriteState(path, b)
	}
	// Read: nothing of the session is pending any more.
	_ = os.Remove(filepath.Join(env.StateDir, musePendingDir, hookrun.EvidenceID(root)+".json"))
	readQuota(ctx, env, r, root, filepath.Join(dir, "quota"))
	return nil
}

// readView pages a session's view from cur, handing each record to emit, and returns the
// turns it saw finish.
func readView(ctx context.Context, host *mspHost, sessionID string, cur *viewCursor, emit func(spool.Event), logf func(string, ...any)) map[string]bool {
	finished := map[string]bool{}
	for range 50 {
		params := map[string]any{"sessionId": sessionID, "limit": museViewPage}
		if cur.Cursor != "" {
			params["cursor"] = cur.Cursor
		}
		var page struct {
			Events     []mspEvent `json:"events"`
			NextCursor string     `json:"nextCursor"`
		}
		if err := host.call(ctx, "view/page", params, &page); err != nil {
			logf("muse feed: view/page: %v", err)
			break
		}
		for _, ev := range page.Events {
			// An entry's own cursor is where the next read resumes, after it: the last page
			// names no next one.
			var at struct {
				ViewCursor string `json:"viewCursor"`
			}
			if json.Unmarshal(ev.Params, &at) == nil && at.ViewCursor != "" {
				cur.Cursor = at.ViewCursor
			}
			rec, ok := museRecord(sessionID, ev, cur.Started)
			if !ok {
				continue
			}
			if rec.Name == semconv.TermaTurnEvent {
				id, _ := rec.Attrs[semconv.TermaTurnIDKey].(string)
				finished[id] = true
			}
			emit(rec)
		}
		if page.NextCursor == "" || len(page.Events) == 0 {
			break
		}
		cur.Cursor = page.NextCursor
	}
	return finished
}

// mspEvent is one entry of a session's view: a notification as the host sent it.
type mspEvent struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// museRecord is what a view entry adds to the hooks' records, false for one that adds
// nothing. started holds the tool calls seen starting, by item id.
func museRecord(sessionID string, ev mspEvent, started map[string]time.Time) (spool.Event, bool) {
	var p struct {
		TurnID            string `json:"turnId"`
		Terminal          string `json:"terminal"`
		DurationMs        *int64 `json:"durationMs"`
		TimeToFirstTokenM *int64 `json:"timeToFirstTokenMs"`
		Name              string `json:"name"`
		Item              struct {
			ItemID     string    `json:"itemId"`
			Kind       string    `json:"kind"`
			TurnID     string    `json:"turnId"`
			Status     string    `json:"status"`
			RecordedAt time.Time `json:"recordedAt"`
			Tool       string    `json:"tool"`
			CallID     string    `json:"callId"`
		} `json:"item"`
	}
	if json.Unmarshal(ev.Params, &p) != nil {
		return spool.Event{}, false
	}
	attrs := map[string]any{semconv.GenAIMainAgentNameKey: museTool, semconv.TermaEvidenceSourceKey: sourceMuseSession}
	switch ev.Method {
	case "item/started":
		if p.Item.Kind == "toolCall" && p.Item.ItemID != "" && !p.Item.RecordedAt.IsZero() {
			started[p.Item.ItemID] = p.Item.RecordedAt
		}
		return spool.Event{}, false
	case "item/completed":
		if p.Item.Kind != "toolCall" || !hookrun.ShortLabel(p.Item.Tool) {
			return spool.Event{}, false
		}
		begun, ok := started[p.Item.ItemID]
		delete(started, p.Item.ItemID)
		attrs[semconv.GenAIToolNameKey] = p.Item.Tool
		hookrun.BoundedAttr(attrs, semconv.GenAIToolCallIDKey, p.Item.CallID)
		hookrun.BoundedAttr(attrs, semconv.TermaTurnIDKey, p.Item.TurnID)
		attrs[semconv.TermaOperationStatusKey] = "completed"
		if p.Item.Status != "completed" {
			attrs[semconv.TermaOperationStatusKey] = "error"
		}
		if ok && !p.Item.RecordedAt.Before(begun) {
			attrs[semconv.TermaOperationDurationMsKey] = p.Item.RecordedAt.Sub(begun).Milliseconds()
		}
		// A call with no id cannot fold into the hook's: it would count twice.
		if attrs[semconv.GenAIToolCallIDKey] == nil {
			return spool.Event{}, false
		}
		return spool.Event{Time: p.Item.RecordedAt, Name: semconv.TermaToolCallEvent, SessionID: sessionID, Attrs: attrs}, true
	case "turn/completed":
		if !hookrun.ShortLabel(p.TurnID) {
			return spool.Event{}, false
		}
		attrs[semconv.TermaTurnIDKey] = p.TurnID
		if hookrun.ShortLabel(p.Terminal) {
			attrs[semconv.TermaOperationStatusKey] = p.Terminal
		}
		if p.DurationMs != nil && *p.DurationMs >= 0 {
			attrs[semconv.TermaOperationDurationMsKey] = *p.DurationMs
		}
		if p.TimeToFirstTokenM != nil && *p.TimeToFirstTokenM >= 0 {
			attrs[semconv.TermaTurnTimeToFirstTokenMsKey] = *p.TimeToFirstTokenM
		}
		return spool.Event{Name: semconv.TermaTurnEvent, SessionID: sessionID, Attrs: attrs}, true
	case "session/nameChanged":
		if name := strings.TrimSpace(p.Name); name != "" && len(name) <= 256 {
			attrs[semconv.TermaSessionTitleKey] = name
			return spool.Event{Name: semconv.TermaSessionTitleEvent, SessionID: sessionID, Attrs: attrs}, true
		}
	}
	return spool.Event{}, false
}

// museUsage is `muse usage --json`: the plan's tier and its two windows, as Meta last
// reported them.
type museUsage struct {
	Allowance *struct {
		Tier   string     `json:"tier"`
		Window *museLimit `json:"window"`
		Weekly *museLimit `json:"weekly"`
		AsOfMs int64      `json:"asOfMs"`
	} `json:"allowance"`
}

type museLimit struct {
	UsedPercent *int64 `json:"usedPercent"`
	ResetsAtMs  int64  `json:"resetsAtMs"`
}

// readQuota spools the plan's usage, from `muse usage`, on the session that just ended a
// turn, at most every museQuotaEvery: the windows are the account's, its sessions share them.
func readQuota(ctx context.Context, env hookrun.Env, r *hookrun.Repo, sessionID, marker string) {
	if info, err := os.Stat(marker); err == nil && env.Time().Sub(info.ModTime()) < museQuotaEvery {
		return
	}
	_ = hookrun.WriteState(marker, nil)
	muse, err := exec.LookPath("muse")
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, mspCallTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, muse, "usage", "--json")
	cmd.Env = append(os.Environ(), "MUSE_NO_AUTO_UPDATE=1")
	out, err := cmd.Output()
	if err != nil {
		env.Logf("muse feed: usage: %v", err)
		return
	}
	if rec, ok := museQuota(sessionID, out); ok {
		env.EmitFor(r, rec)
	}
}

// museQuota is the terma.session.quota of `muse usage --json`'s output: its tier as the
// plan, its current window as the primary limit and its weekly one as the secondary.
func museQuota(sessionID string, out []byte) (spool.Event, bool) {
	var u museUsage
	if json.Unmarshal(out, &u) != nil || u.Allowance == nil {
		return spool.Event{}, false
	}
	a := u.Allowance
	attrs := map[string]any{
		semconv.GenAIMainAgentNameKey: museTool, semconv.TermaEvidenceSourceKey: sourceMuseUsage,
		semconv.TermaEvidenceStatusKey: hookrun.StatusPresent,
	}
	hookrun.BoundedAttr(attrs, semconv.TermaAccountPlanTypeKey, a.Tier)
	limit := func(l *museLimit, used, resets string) {
		if l == nil || l.UsedPercent == nil || *l.UsedPercent < 0 {
			return
		}
		attrs[used] = *l.UsedPercent
		if l.ResetsAtMs > 0 {
			attrs[resets] = l.ResetsAtMs / 1000
		}
	}
	limit(a.Window, semconv.TermaRateLimitPrimaryUsedPercentKey, semconv.TermaRateLimitPrimaryResetsAtKey)
	limit(a.Weekly, semconv.TermaRateLimitSecondaryUsedPercentKey, semconv.TermaRateLimitSecondaryResetsAtKey)
	if a.Weekly != nil {
		attrs[semconv.TermaRateLimitSecondaryWindowMinutesKey] = int64(7 * 24 * 60)
	}
	ev := spool.Event{Name: semconv.TermaSessionQuotaEvent, SessionID: sessionID, Attrs: attrs}
	if a.AsOfMs > 0 {
		ev.Time = time.UnixMilli(a.AsOfMs)
	}
	return ev, true
}
