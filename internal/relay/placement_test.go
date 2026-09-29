package relay

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func stampedLog(name, session string, t time.Time, extra ...map[string]any) map[string]any {
	r := logRecord(name, "", append([]map[string]any{strAttr("session.id", session)}, extra...)...)
	r["timeUnixNano"] = fmt.Sprint(t.UnixNano())
	return r
}

// A session resumed in a personal directory keeps its id. Each record goes where the
// session was when it happened: the first run's to its repository's project — even when
// they arrive after the resume — and the resumed run's to the machine project.
func TestAResumedSessionIsPlacedWhereEachRunRan(t *testing.T) {
	h := newTestRelay(t)
	h.bindings[repoPath("a")] = "proj-a"
	h.keys["proj-a"] = "Bearer key-a"
	base := time.Now().Add(-time.Minute)
	resumed := base.Add(30 * time.Second)
	h.recordPlacement(sessionA, repoPath("a"), base)
	h.recordPlacement(sessionA, repoPath("personal"), resumed)
	h.start()
	h.post(sigLogs, logsBody(t,
		stampedLog("first-run", sessionA, base.Add(time.Second)),
		stampedLog("resumed-run", sessionA, resumed.Add(time.Second), strAttr("prompt", "PERSONAL")),
		stampedLog("first-run-late", sessionA, resumed.Add(-time.Second)),
	))
	eventually(t, "both runs placed", func() bool {
		return sameSet(h.gw.received("Bearer key-a"), []string{"first-run", "first-run-late"}) &&
			sameSet(h.gw.received("Bearer key-machine"), []string{"resumed-run"})
	})
	if strings.Contains(h.gw.bodiesFor("Bearer key-a"), "PERSONAL") {
		t.Fatal("the resumed personal run's prompt reached the repository's project")
	}
	// Both decisions are kept, so a restart places the same way.
	h.stop()
	h.start()
	h.post(sigLogs, logsBody(t,
		stampedLog("first-run-after-restart", sessionA, base.Add(2*time.Second)),
		stampedLog("resumed-after-restart", sessionA, resumed.Add(2*time.Second))))
	eventually(t, "the same placement after a restart", func() bool {
		return sameSet(h.gw.received("Bearer key-a"), []string{"first-run", "first-run-late", "first-run-after-restart"}) &&
			sameSet(h.gw.received("Bearer key-machine"), []string{"resumed-run", "resumed-after-restart"})
	})
}

// Codex has no user-level hooks; a turn resumed elsewhere is seen in its rollout's
// turn_context, which the relay reads as a placement.
func TestACodexTurnResumedElsewhereIsPlacedByItsRollout(t *testing.T) {
	h := newTestRelay(t)
	h.bindings[repoPath("codex-repo")] = "proj-codex"
	h.keys["proj-codex"] = "Bearer key-codex"
	resumed := time.Now().Add(-10 * time.Second)
	h.agentDirs[codexID] = []Placement{{Since: resumed, Dir: repoPath("elsewhere")}}
	h.start()
	logOf := func(name string, at time.Time) map[string]any {
		r := logRecord(name, "", strAttr("conversation.id", codexID))
		r["timeUnixNano"] = fmt.Sprint(at.UnixNano())
		return r
	}
	h.post(sigLogs, logsBody(t, logOf("codex.first_turn", resumed.Add(-time.Minute)), logOf("codex.resumed_turn", resumed.Add(time.Second))))
	eventually(t, "each turn where it ran", func() bool {
		return sameSet(h.gw.received("Bearer key-codex"), []string{"codex.first_turn"}) &&
			sameSet(h.gw.received("Bearer key-machine"), []string{"codex.resumed_turn"})
	})
}

func (h *testRelay) recordPlacement(session, dir string, at time.Time) {
	h.t.Helper()
	path := filepath.Join(h.dir, sessionsDir, session)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		h.t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		h.t.Fatal(err)
	}
	defer f.Close()
	fmt.Fprintf(f, "%d\t%s\n", at.UnixNano(), dir)
}

func TestRecordSessionKeepsAPlacementHistory(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("TERMA_CONFIG_DIR", cfg)
	if _, err := Ensure(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg, dirName)
	t0 := time.Unix(1790700000, 0)
	for i, d := range []string{repoPath("a"), repoPath("a"), repoPath("personal")} {
		if err := recordPlacement(sessionA, d, t0.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	ps := readPlacements(dir, sessionA)
	if len(ps) != 2 || ps[0].Dir != repoPath("a") || !ps[0].Since.Equal(t0) || ps[1].Dir != repoPath("personal") {
		t.Fatalf("placements %+v: want repo a since t0 (the repeat collapsed), then personal", ps)
	}
	for i := range maxPlacements + 5 {
		_ = recordPlacement(sessionA, repoPath(fmt.Sprint("d", i)), t0.Add(time.Hour+time.Duration(i)*time.Second))
	}
	if n := len(readPlacements(dir, sessionA)); n != maxPlacements {
		t.Fatalf("history holds %d placements, want the %d newest", n, maxPlacements)
	}
}

// The repository's hook and the global one both record a session's start; concurrent
// writers lose no placement.
func TestConcurrentPlacementsAreAllKept(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("TERMA_CONFIG_DIR", cfg)
	if _, err := Ensure(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = recordPlacement(sessionB, repoPath(fmt.Sprint("w", i)), time.Unix(1790700000+int64(i), 0))
		}()
	}
	wg.Wait()
	if n := len(readPlacements(filepath.Join(cfg, dirName), sessionB)); n != 12 {
		t.Fatalf("kept %d of 12 concurrent placements", n)
	}
}

// Files an earlier build wrote — one directory, one route, no time — still place a
// session, since the session began.
func TestEarlierOneLineFilesStillPlace(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{sessionsDir, routesDir} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, sessionsDir, sessionA), []byte(repoPath("a")+"\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, routesDir, sessionB), []byte("proj-b\n"), 0o600)
	if ps := readPlacements(dir, sessionA); len(ps) != 1 || ps[0].Dir != repoPath("a") || !ps[0].Since.IsZero() {
		t.Fatalf("one-line session file: %+v", ps)
	}
	r := newResolver(dir, func(string) (string, error) { return "", nil }, nil, t.Logf)
	if route, final := r.decide(context.Background(), sessionB, "claude", time.Now(), time.Now(), false); !final || route != "proj-b" {
		t.Fatalf("one-line route file: %q %v", route, final)
	}
}
