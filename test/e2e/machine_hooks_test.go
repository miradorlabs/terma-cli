package e2e

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// terma's hooks are the agents' user-level ones and git's global hooks path, so they run
// beside a repository's own, from any folder; the team policy's repository list decides
// where they record anything. Real agent builds with loopback providers: no credentials.

// claimFile is the relay claim terma's hooks write for a session.
type claimFile struct {
	ProjectID  string           `json:"project_id"`
	Repository claimRepository  `json:"repository"`
	Placements []claimPlacement `json:"placements"`
}

type claimRepository struct {
	Origin string `json:"origin"`
}

type claimPlacement struct {
	ProjectID  string          `json:"project_id"`
	Repository claimRepository `json:"repository"`
}

// claimOf is the claim for session, nil when no hook wrote one.
func (sb *Sandbox) claimOf(session string) *claimFile {
	c, _ := sb.claimFileOf(session)
	return c
}

// claimFileOf is claimOf and the file's bytes.
func (sb *Sandbox) claimFileOf(session string) (*claimFile, string) {
	data, err := os.ReadFile(filepath.Join(sb.TermaConfig, "relay", "claims", session+".json"))
	if err != nil {
		return nil, ""
	}
	var c claimFile
	if err := json.Unmarshal(data, &c); err != nil {
		sb.T.Fatalf("claim %s: %v\n%s", session, err, data)
	}
	return &c, string(data)
}

// marked reports whether c is a mark alone: every placement names no project and no repository.
func (c *claimFile) marked() bool {
	if c == nil || c.ProjectID != "" || len(c.Placements) == 0 {
		return false
	}
	for _, p := range c.Placements {
		if p.ProjectID != "" || p.Repository.Origin != "" {
			return false
		}
	}
	return true
}

// hookEventsOf counts what terma's hooks recorded for session: spooled, or delivered.
func (sb *Sandbox) hookEventsOf(session string) int {
	n := 0
	for _, e := range sb.Spool() {
		if e.SessionID == session {
			n++
		}
	}
	for _, l := range sb.Receiver.Logs() {
		if l.Resource["service.name"] == "terma-cli" && l.Attrs["session.id"] == session {
			n++
		}
	}
	return n
}

// claudeWrites runs a headless Claude session in dir that writes file there, through a
// scripted loopback provider, and returns its session id.
func (sb *Sandbox) claudeWrites(dir, file string) string {
	sb.T.Helper()
	var calls atomic.Int32
	provider := httptest.NewServer(claudeScriptedProvider(&calls, []map[string]any{
		{"name": "Write", "input": map[string]any{"file_path": filepath.Join(dir, file), "content": "written by the agent\n"}},
	}))
	defer provider.Close()
	sb.ClaudeBaseURL = provider.URL
	_, sid := sb.ClaudeHeadlessIn(dir, RouteAPIKey, "Write the file.", "--max-turns", "3", "--tools", "Write", "--allowedTools", "Write")
	return sid
}

// A repository's own Claude Code hooks and its clone's own git hook run beside terma's
// machine-wide ones: both fire, and the commit is still stamped.
func TestMachineHooksBesideRepositoryHooks(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithClaude(b))
		claudeMarker := filepath.Join(sb.Dir, "repo-claude-hook-ran")
		sb.write(".claude/settings.json", `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"touch '`+claudeMarker+`'"}]}]}}`)
		sb.Commit("the repository's own Claude Code hooks")
		gitMarker := filepath.Join(sb.Dir, "repo-git-hook-ran")
		hook := filepath.Join(sb.Repo, ".git", "hooks", "prepare-commit-msg")
		sb.writeAbs(hook, "#!/bin/sh\ntouch '"+gitMarker+"'\n")
		if err := os.Chmod(hook, 0o755); err != nil {
			t.Fatal(err)
		}

		sid := sb.claudeWrites(sb.Repo, "hello.txt")
		if _, err := os.Stat(claudeMarker); err != nil {
			t.Error("the repository's own SessionStart hook did not run beside terma's")
		}
		if len(sb.Delivered("terma.session.start", sid, 30*time.Second)) == 0 || sb.claimOf(sid) == nil {
			t.Error("terma's machine-wide hooks did not record the session beside the repository's")
		}
		msg := sb.commitWithGlobalHooks(sb.Repo, "hello.txt")
		if _, err := os.Stat(gitMarker); err != nil {
			t.Error("the clone's own prepare-commit-msg did not run after terma's")
		}
		if !strings.Contains(msg, "Agent-Session-Id: "+sid) {
			t.Errorf("commit not stamped with %s:\n%s", sid, msg)
		}
	})
}

// A repository's own .codex/hooks.json runs beside terma's machine-wide Codex hooks.
func TestMachineHooksBesideRepositoryHooksCodex(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithCodex(b))
		marker := filepath.Join(sb.Dir, "repo-codex-hook-ran")
		sb.write(".codex/hooks.json", `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"touch '`+marker+`'"}]}]}}`)
		var calls atomic.Int32
		provider := httptest.NewServer(codexTelemetryProvider(t, &calls))
		defer provider.Close()
		run := sb.CodexExec(RouteAPIKey, telemetryPrompt, fixtureCodexArgs(provider.URL)...)
		if _, err := os.Stat(marker); err != nil {
			t.Error("the repository's own Codex SessionStart hook did not run beside terma's")
		}
		if len(sb.Delivered("terma.session.start", run.ThreadID, 30*time.Second)) == 0 || sb.claimOf(run.ThreadID) == nil {
			t.Error("terma's machine-wide Codex hooks did not record the thread beside the repository's")
		}
	})
}

// Which working copies the team admits, wherever the agent starts: the listed repository
// by its origin from any folder, subdirectory or linked worktree and in any URL form, and
// nothing in a same-named folder with another origin, a fork, a repository with no origin,
// or a folder outside git, beyond a mark that the session is not collected, which names
// neither the team nor the place.
func TestMachineHooksAdmission(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithClaude(b), WithRepositories("github.com/acme/repo"))
		renamed := sb.newRepo("checkout-2", "git@github.com:acme/repo.git")
		https := sb.newRepo("https-clone", "https://GitHub.com/acme/repo/")
		worktree := filepath.Join(sb.Dir, "feature-wt")
		sb.git("worktree", "add", "-q", "-b", "feature", worktree)
		claudeWorktree := filepath.Join(sb.Repo, ".claude", "worktrees", "fix-1")
		sb.git("worktree", "add", "-q", "-b", "claude/fix-1", claudeWorktree)
		sameName := sb.newRepo(filepath.Join("elsewhere", "repo"), "git@github.com:acme/widget.git")
		fork := sb.newRepo("fork", "git@github.com:someone/repo.git")
		noOrigin := sb.newRepo("no-origin", "")
		outside := filepath.Join(sb.Dir, "notes")
		for _, d := range []string{filepath.Join(sb.Repo, "sub"), outside} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		for _, place := range []struct {
			name, dir, root, file string
			admitted              bool
		}{
			{"listed", sb.Repo, sb.Repo, "listed.txt", true},
			{"subdirectory", filepath.Join(sb.Repo, "sub"), sb.Repo, "sub/sub.txt", true},
			{"renamed-ssh-checkout", renamed, renamed, "renamed.txt", true},
			{"https-form", https, https, "https.txt", true},
			{"linked-worktree", worktree, worktree, "worktree.txt", true},
			{"claude-worktree", claudeWorktree, claudeWorktree, "claude-wt.txt", true},
			{"same-name-other-origin", sameName, sameName, "same-name.txt", false},
			{"fork", fork, fork, "fork.txt", false},
			{"no-origin", noOrigin, noOrigin, "no-origin.txt", false},
			{"outside-git", outside, "", "outside.txt", false},
		} {
			t.Run(place.name, func(t *testing.T) {
				track(t)
				sid := sb.claudeWrites(place.dir, filepath.Base(place.file))
				if !place.admitted {
					ran := slices.ContainsFunc(sb.HookPayloads("session-start"), func(p map[string]any) bool { return p["session_id"] == sid })
					if !ran {
						t.Fatal("terma's SessionStart hook never ran, so the control proves nothing")
					}
					time.Sleep(2 * time.Second) // any flush the hooks started
					c, raw := sb.claimFileOf(sid)
					if !c.marked() {
						t.Errorf("an unlisted working copy's session was not marked, or was claimed: %+v\n%s", c, raw)
					}
					for _, leak := range []string{sb.ProjectID, filepath.Base(place.dir), filepath.ToSlash(place.dir), "github.com"} {
						if strings.Contains(raw, leak) {
							t.Errorf("the mark holds %q: %s", leak, raw)
						}
					}
					if n := sb.hookEventsOf(sid); n != 0 {
						t.Errorf("hooks recorded %d events for an unlisted working copy's session", n)
					}
					if place.root == "" {
						return
					}
					if msg := sb.commitWithGlobalHooks(place.root, place.file); strings.Contains(msg, "Agent-Session-Id") {
						t.Errorf("an unlisted repository's commit was stamped:\n%s", msg)
					}
					return
				}
				c := sb.claimOf(sid)
				if c == nil || c.ProjectID != sb.ProjectID || !strings.EqualFold(c.Repository.Origin, "github.com/acme/repo") {
					t.Errorf("claim = %+v, want project %s with origin github.com/acme/repo", c, sb.ProjectID)
				}
				if len(sb.Delivered("terma.session.start", sid, 30*time.Second)) == 0 || len(sb.Delivered("terma.files.touched", sid, 30*time.Second)) == 0 {
					t.Errorf("the session's hook events were not delivered; spool: %+v", sb.Spool())
				}
				if msg := sb.commitWithGlobalHooks(place.root, place.file); !strings.Contains(msg, "Agent-Session-Id: "+sid) {
					t.Errorf("commit not stamped with %s:\n%s", sid, msg)
				}
			})
		}
	})
}

// A Codex thread resumed between an admitted repository and an unlisted one, either way:
// the unlisted run is marked, a placement with no project and no origin, and the relay
// drops what it exports; the admitted run is collected, from the move on.
func TestMachineHooksCodexResumedUnlisted(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		for _, listedFirst := range []bool{true, false} {
			t.Run(map[bool]string{true: "listed-then-unlisted", false: "unlisted-then-listed"}[listedFirst], func(t *testing.T) {
				codexResumedAcross(t, b, listedFirst)
			})
		}
	})
}

func codexResumedAcross(t *testing.T, b Binary, listedFirst bool) {
	track(t)
	t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
	sb := New(t, Isolated, WithCodex(b))
	sb.UseRelay(RelayOptions{Start: true, Hold: 3 * time.Second, Content: true})
	var calls atomic.Int32
	provider := httptest.NewServer(codexTelemetryProvider(t, &calls))
	defer provider.Close()
	unlisted := sb.newRepo("unlisted", "")
	listed := sb.Repo
	firstDir, firstWork, thenDir, thenWork := listed, "TERMA_LISTED_WORK", unlisted, "TERMA_UNLISTED_WORK"
	if !listedFirst {
		firstDir, firstWork, thenDir, thenWork = unlisted, "TERMA_UNLISTED_WORK", listed, "TERMA_LISTED_WORK"
	}
	calls.Store(1) // plain replies, no tool call
	sb.WorkDir = firstDir
	first := sb.CodexExec(RouteAPIKey, firstWork+" please", fixtureCodexArgs(provider.URL)...)
	calls.Store(1)
	sb.WorkDir = thenDir
	resumed := sb.CodexExec(RouteAPIKey, thenWork+" please", append(fixtureCodexArgs(provider.URL), "resume", first.ThreadID)...)
	if resumed.ThreadID != first.ThreadID {
		Note(t.Name(), "Codex gave the resumed run a new thread id, so no placement follows it")
		return
	}
	c := sb.claimOf(first.ThreadID)
	if c == nil || len(c.Placements) != 2 {
		t.Fatalf("claim = %+v, want a placement for each run", c)
	}
	mark, claimed := c.Placements[1], c.Placements[0]
	if !listedFirst {
		mark, claimed = claimed, mark
	}
	if mark.ProjectID != "" || mark.Repository.Origin != "" || claimed.ProjectID != sb.ProjectID || claimed.Repository.Origin != "github.com/acme/repo" {
		t.Errorf("placements = %+v, want the repository's and a mark with no project or origin", c.Placements)
	}
	deadline := time.Now().Add(30 * time.Second)
	for len(leakedFieldsOf(sb.Receiver.evidence(), "TERMA_LISTED_WORK")) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Second)
	}
	time.Sleep(6 * time.Second) // past the hold
	sb.StopRelay()
	stats := sb.RelayStats()
	noteRelayStats(t.Name(), stats)
	e := sb.Receiver.evidence()
	if len(leakedFieldsOf(e, "TERMA_LISTED_WORK")) == 0 {
		t.Errorf("the listed run's prompt never reached the project: %v", stats)
	}
	if leaked := leakedFieldsOf(e, "TERMA_UNLISTED_WORK"); len(leaked) > 0 || sum(stats, "dropped.not_collected.") == 0 {
		t.Errorf("want the unlisted run's records dropped as not_collected, none leaked (%v): %v", leaked, stats)
	}
}
