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
// beside a repository's own, from any folder; the team policy's folder list decides
// where they record anything. Real agent builds with loopback providers: no credentials.

// claimFile is the relay claim terma's hooks write for a session.
type claimFile struct {
	ProjectID  string           `json:"project_id"`
	Repository claimRepository  `json:"repository"`
	Placements []claimPlacement `json:"placements"`
}

type claimRepository struct {
	Names []string `json:"names"`
	Path  string   `json:"path"`
}

type claimPlacement struct {
	ProjectID  string          `json:"project_id"`
	Repository claimRepository `json:"repository"`
}

// claimOf is the claim for session, nil when no hook wrote one.
func (sb *Sandbox) claimOf(session string) *claimFile {
	data, err := os.ReadFile(filepath.Join(sb.TermaConfig, "relay", "claims", session+".json"))
	if err != nil {
		return nil
	}
	var c claimFile
	if err := json.Unmarshal(data, &c); err != nil {
		sb.T.Fatalf("claim %s: %v\n%s", session, err, data)
	}
	return &c
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

// Which folders the team admits, wherever the agent starts: the repository from a
// subdirectory, a checkout by origin's repository name, a folder outside git by a
// parent's name, and nothing at all in a repository the list does not name.
func TestMachineHooksAdmission(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithClaude(b), WithFolders("repo", "widget", "notes-parent"))
		renamed := sb.newRepo("checkout-2", "https://github.com/acme/widget.git")
		unlisted := sb.newRepo("unlisted", "")
		outside := filepath.Join(sb.Dir, "notes-parent", "today")
		for _, d := range []string{filepath.Join(sb.Repo, "sub"), outside} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		for _, place := range []struct {
			name, dir, root, file, admittedAs string
		}{
			{"listed", sb.Repo, sb.Repo, "listed.txt", "repo"},
			{"subdirectory", filepath.Join(sb.Repo, "sub"), sb.Repo, "sub/sub.txt", "repo"},
			{"origin-name", renamed, renamed, "renamed.txt", "widget"},
			{"parent-folder", outside, "", "today.txt", "notes-parent"},
			{"unlisted", unlisted, unlisted, "unlisted.txt", ""},
		} {
			t.Run(place.name, func(t *testing.T) {
				track(t)
				sid := sb.claudeWrites(place.dir, filepath.Base(place.file))
				if place.admittedAs == "" {
					ran := slices.ContainsFunc(sb.HookPayloads("session-start"), func(p map[string]any) bool { return p["session_id"] == sid })
					if !ran {
						t.Fatal("terma's SessionStart hook never ran, so the control proves nothing")
					}
					time.Sleep(2 * time.Second) // any flush the hooks started
					if c := sb.claimOf(sid); c != nil {
						t.Errorf("an unlisted folder's session was claimed: %+v", *c)
					}
					if n := sb.hookEventsOf(sid); n != 0 {
						t.Errorf("hooks recorded %d events for an unlisted folder's session", n)
					}
					if msg := sb.commitWithGlobalHooks(place.root, place.file); strings.Contains(msg, "Agent-Session-Id") {
						t.Errorf("an unlisted folder's commit was stamped:\n%s", msg)
					}
					return
				}
				c := sb.claimOf(sid)
				if c == nil || c.ProjectID != sb.ProjectID || !slices.Contains(c.Repository.Names, place.admittedAs) {
					t.Errorf("claim = %+v, want project %s with the name %q", c, sb.ProjectID, place.admittedAs)
				}
				if len(sb.Delivered("terma.session.start", sid, 30*time.Second)) == 0 || len(sb.Delivered("terma.files.touched", sid, 30*time.Second)) == 0 {
					t.Errorf("the session's hook events were not delivered; spool: %+v", sb.Spool())
				}
				if place.root == "" {
					return
				}
				if msg := sb.commitWithGlobalHooks(place.root, place.file); !strings.Contains(msg, "Agent-Session-Id: "+sid) {
					t.Errorf("commit not stamped with %s:\n%s", sid, msg)
				}
			})
		}
	})
}

// A Codex thread from an admitted repository resumed in an unlisted one: the claim gains
// a placement there, and the relay drops what the resumed run exports.
func TestMachineHooksCodexResumedUnlisted(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		track(t)
		t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithCodex(b))
		sb.UseRelay(RelayOptions{Start: true, Hold: 3 * time.Second, Content: true})
		var calls atomic.Int32
		provider := httptest.NewServer(codexTelemetryProvider(t, &calls))
		defer provider.Close()
		first := sb.CodexExec(RouteAPIKey, telemetryPrompt, fixtureCodexArgs(provider.URL)...)
		unlisted := sb.newRepo("unlisted", "")
		calls.Store(1) // the resumed turn gets a plain reply, no tool call
		sb.WorkDir = unlisted
		resumed := sb.CodexExec(RouteAPIKey, "TERMA_UNLISTED_WORK please", append(fixtureCodexArgs(provider.URL), "resume", first.ThreadID)...)
		if resumed.ThreadID != first.ThreadID {
			Note(t.Name(), "Codex gave the resumed run a new thread id, so no claim follows it")
			if sb.claimOf(resumed.ThreadID) != nil {
				t.Error("the resumed thread in an unlisted folder was claimed")
			}
			return
		}
		c := sb.claimOf(first.ThreadID)
		if c == nil || len(c.Placements) < 2 {
			t.Fatalf("claim = %+v, want a placement for the unlisted folder", c)
		}
		last := c.Placements[len(c.Placements)-1]
		if !slices.Equal(last.Repository.Names, []string{"unlisted"}) || !slices.Contains(c.Placements[0].Repository.Names, "repo") {
			t.Errorf("placements = %+v, want the repository's, then the unlisted folder's", c.Placements)
		}
		time.Sleep(6 * time.Second) // past the hold
		sb.StopRelay()
		stats := sb.RelayStats()
		noteRelayStats(t.Name(), stats)
		if sum(stats, "dropped.policy_repository.") == 0 || len(leakedFieldsOf(sb.Receiver.evidence(), "TERMA_UNLISTED_WORK")) > 0 {
			t.Errorf("want the resumed run's records dropped as policy_repository, none leaked: %v", stats)
		}
	})
}
