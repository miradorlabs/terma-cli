package e2e

import (
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Global mode — the organization collects every session and commit on the machine —
// set up the way a developer's machine gets it: the real `terma setup`, signed in to an
// account whose policy is global, which writes the agents' machine-wide hooks and git's
// global hooks path. Then real agents in four places, each of which must reach its
// configured global destination: the sandbox repository, another repository with a
// known remote, an unknown one, and a directory outside any repository. Commits in the repositories carry
// their session. Nothing is installed per repository.
//
// The sandbox's HOME, agent config and git global config are all its own, so this is
// safe on a developer's machine; `make machines` runs it on a fresh Linux one too.

const (
	globalDefault = "p-default"
	globalKnown   = "p-known"
)

func globalPolicy() string {
	data, _ := json.Marshal(map[string]any{
		"mode": "global", "include_prompts": true, "include_tool_content": true,
		"git_hooks": true, "default_project_id": globalDefault,
	})
	return string(data)
}

// UseGlobalSetup runs `terma setup` in global mode for agents, signed in to a fake
// account, with the relay on a port of the sandbox's own (started, logged).
func (sb *Sandbox) UseGlobalSetup(agents string) *Account {
	t := sb.T
	t.Helper()
	acct := sb.StartAccount()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sb.relayAddr = ln.Addr().String()
	_ = ln.Close()
	sb.relayed = true
	sb.ExtraEnv = append(sb.ExtraEnv, "TERMA_POLICY_STUB="+globalPolicy())
	out := sb.Setup(nil, "--harness", agents)
	if !strings.Contains(out, "every session on this machine") {
		t.Fatalf("setup did not enter global mode:\n%s", out)
	}
	// Setup ends with the machine's check-in, which the ingest received.
	if !strings.Contains(out, "this machine reported to your organization") {
		t.Errorf("setup did not report its check-in:\n%s", out)
	}
	checkedIn := len(sb.Receiver.WaitLogs(time.Second, func(l LogRecord) bool {
		return l.EventName == "terma.relay.heartbeat" && l.Attrs["terma.relay.heartbeat.reason"] == "setup"
	})) > 0
	if !checkedIn {
		t.Error("the ingest received no setup check-in")
	}
	// A relay that logs what it drops, in place of the one setup started.
	sb.StopRelay()
	sb.StartRelay()
	t.Cleanup(sb.StopRelay)
	return acct
}

// newRepo makes another repository with the given origin remote, trusted
// by the sandbox's Claude.
func (sb *Sandbox) newRepo(name, remote string) string {
	dir := filepath.Join(sb.Dir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		sb.T.Fatal(err)
	}
	sb.gitIn(dir, "init", "-q", "-b", "main")
	sb.gitIn(dir, "config", "user.email", "live@terma.test")
	sb.gitIn(dir, "config", "user.name", "Terma Live")
	sb.gitIn(dir, "config", "commit.gpgsign", "false")
	if remote != "" {
		sb.gitIn(dir, "remote", "add", "origin", remote)
	}
	return dir
}

// commitByHand commits the file the agent wrote in dir the way the developer's git would:
// with the sandbox's git config, through the hooks terma installed in the repository, and
// returns the message.
func (sb *Sandbox) commitByHand(dir, file string) string {
	t := sb.T
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
		t.Fatalf("the agent did not write %s: %v", file, err)
	}
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(sb.termaEnv(), "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	run("add", file)
	run("commit", "-q", "-m", "work in "+filepath.Base(dir))
	return run("log", "-1", "--format=%B")
}

// projectOfSession waits for the receiver to have the session's records and reports the
// projects they were filed under.
func projectOfSession(sb *Sandbox, sid string) map[string]int {
	projects := map[string]int{}
	agent := func(l LogRecord) bool {
		return l.Attrs["session.id"] == sid && l.Resource["service.name"] != "terma-cli"
	}
	sb.Receiver.WaitLogs(60*time.Second, agent)
	time.Sleep(2 * time.Second) // the rest of the session's export
	for _, l := range sb.Receiver.Logs() {
		if agent(l) {
			projects[l.Resource["mirador.project.id"]]++
		}
	}
	return projects
}

func TestGlobalModeClaude(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		ProvesAll(t, b, "global.placement", "global.commits")
		track(t)
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithClaude(b))
		acct := sb.UseGlobalSetup("claude")

		known := sb.newRepo("known", "git@github.com:org/known.git")
		unknown := sb.newRepo("unknown", "https://gitlab.example/me/side.git")
		scratch := filepath.Join(sb.Dir, "scratch")
		_ = os.MkdirAll(scratch, 0o700)

		for _, place := range []struct {
			name, dir, want string
			commit          bool
		}{
			{"bound", sb.Repo, globalDefault, true},
			{"known-remote", known, globalDefault, true},
			{"unknown-remote", unknown, globalDefault, true},
			{"outside-any-repository", scratch, globalDefault, false},
		} {
			t.Run(place.name, func(t *testing.T) {
				// The agent writes the file the commit then carries: a commit is stamped
				// with the session whose edits it contains.
				file := place.name + ".txt"
				var calls atomic.Int32
				provider := httptest.NewServer(claudeScriptedProvider(&calls, []map[string]any{
					{"name": "Write", "input": map[string]any{"file_path": filepath.Join(place.dir, file), "content": "written by the agent\n"}},
				}))
				defer provider.Close()
				sb.ClaudeBaseURL = provider.URL
				_, sid := sb.ClaudeHeadlessIn(place.dir, RouteAPIKey, "Write the file.", "--max-turns", "3", "--tools", "Write", "--allowedTools", "Write")
				got := projectOfSession(sb, sid)
				if len(got) != 1 || got[place.want] == 0 {
					t.Errorf("session %s in %s filed under %v, want only %s", sid, place.name, got, place.want)
				}
				if place.commit {
					msg := sb.commitByHand(place.dir, file)
					if !strings.Contains(msg, "Agent-Session-Id: "+sid) {
						t.Errorf("the commit in %s is not stamped with its session %s:\n%s", place.name, sid, msg)
					}
				}
				// Recorded once: only the machine-wide hooks ran.
				if starts := sb.Delivered("terma.session.start", sid, 30*time.Second); len(starts) != 1 {
					t.Errorf("session %s in %s was announced %d times, want once", sid, place.name, len(starts))
				} else if got := starts[0].Resource["mirador.project.id"] + starts[0].Attrs["mirador.project.id"]; !strings.Contains(got, place.want) {
					t.Logf("session start in %s: resource %v attrs %v", place.name, starts[0].Resource, starts[0].Attrs)
				}
			})
		}
		// All global records went with the selected team's key.
		for _, p := range []string{globalDefault} {
			if acct.KeyFor(p) == "" {
				t.Errorf("no key was minted for %s", p)
			}
		}
		sb.StopRelay()
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		if n := sum(c, "dropped.unclaimed"); n > 0 {
			t.Errorf("global mode dropped %d unclaimed records: %v", n, c)
		}
	})
}

// Codex in global mode, outside any repository and in an unknown one: its machine-wide
// hooks claim each thread for the default project, and what names no thread (its
// metrics) reaches that project too — attributed by its process, or caught by the
// default — instead of being dropped.
func TestGlobalModeCodex(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		ProvesAll(t, b, "global.placement")
		track(t)
		t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithCodex(b))
		sb.UseGlobalSetup("codex")
		unknown := sb.newRepo("unknown", "https://gitlab.example/me/side.git")
		scratch := filepath.Join(sb.Dir, "scratch")
		_ = os.MkdirAll(scratch, 0o700)
		for _, place := range []struct{ name, dir string }{{"unknown-remote", unknown}, {"outside-any-repository", scratch}} {
			t.Run(place.name, func(t *testing.T) {
				var calls atomic.Int32
				provider := httptest.NewServer(codexTelemetryProvider(t, &calls))
				defer provider.Close()
				sb.WorkDir = place.dir
				run := sb.CodexExec(RouteAPIKey, telemetryPrompt, fixtureCodexArgs(provider.URL)...)
				got := map[string]int{}
				agent := func(l LogRecord) bool { return l.Attrs["conversation.id"] == run.ThreadID }
				sb.Receiver.WaitLogs(60*time.Second, agent)
				time.Sleep(2 * time.Second)
				for _, l := range sb.Receiver.Logs() {
					if agent(l) {
						got[l.Resource["mirador.project.id"]]++
					}
				}
				if len(got) != 1 || got[globalDefault] == 0 {
					t.Errorf("thread %s in %s filed under %v, want only %s", run.ThreadID, place.name, got, globalDefault)
				}
			})
		}
		sb.StopRelay()
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		for _, k := range []string{"dropped.unclaimed_expired.metrics", "dropped.no_session_id.metrics", "dropped.no_session_process.metrics"} {
			if c[k] > 0 {
				t.Errorf("global mode dropped Codex metrics (%s): %v", k, c)
			}
		}
		if c["forwarded.metrics"] == 0 {
			t.Errorf("no Codex metric reached the default project: %v", c)
		}
	})
}

// machineOnly skips a scenario that writes the machine's own configuration (/etc):
// it runs only on the throwaway machine `make machines` starts.
func machineOnly(t *testing.T) {
	t.Helper()
	if os.Getenv("TERMA_MACHINE") != "1" {
		t.Skip("writes the machine's system configuration: runs only under `make machines`")
	}
}

// deployManaged puts the files `terma setup --managed-config` writes where an
// organization's deployment would: Codex's system requirements and Claude Code's managed
// settings.
func (sb *Sandbox) deployManaged() {
	t := sb.T
	t.Helper()
	out := filepath.Join(sb.Dir, "managed")
	sb.terma(sb.Home, "setup", "--managed-config", out, "--managed-terma", sb.Terma)
	for from, to := range map[string]string{
		"codex-requirements.toml":      "/etc/codex/requirements.toml",
		"claude-managed-settings.json": "/etc/claude-code/managed-settings.json",
	} {
		data, err := os.ReadFile(filepath.Join(out, from))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to, data, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(to) })
	}
}

// Global mode deployed by the organization as managed configuration: setup writes no
// per-user hooks for Claude Code or Codex, and the managed ones claim every session —
// Codex's without anyone trusting them (its hook trust is not bypassed here).
func TestGlobalModeManagedConfig(t *testing.T) {
	machineOnly(t)
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		ProvesAll(t, b, "global.managed")
		track(t)
		t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithCodex(b))
		sb.deployManaged()
		sb.UseGlobalSetup("claude,codex")
		for _, f := range []string{filepath.Join(sb.CodexHome, "hooks.json"), filepath.Join(sb.ClaudeConfig, "settings.json")} {
			if data, _ := os.ReadFile(f); strings.Contains(string(data), "hook --user") {
				t.Errorf("setup wrote per-user hooks beside the managed ones: %s", f)
			}
		}
		scratch := filepath.Join(sb.Dir, "scratch")
		_ = os.MkdirAll(scratch, 0o700)
		var calls atomic.Int32
		provider := httptest.NewServer(codexTelemetryProvider(t, &calls))
		defer provider.Close()
		sb.WorkDir = scratch
		sb.CodexHooksUntrusted = true
		run := sb.CodexExec(RouteAPIKey, telemetryPrompt, fixtureCodexArgs(provider.URL)...)
		got := map[string]int{}
		agent := func(l LogRecord) bool { return l.Attrs["conversation.id"] == run.ThreadID }
		sb.Receiver.WaitLogs(60*time.Second, agent)
		time.Sleep(2 * time.Second)
		for _, l := range sb.Receiver.Logs() {
			if agent(l) {
				got[l.Resource["mirador.project.id"]]++
			}
		}
		if len(got) != 1 || got[globalDefault] == 0 {
			t.Errorf("thread %s filed under %v, want only %s: the managed hooks did not claim it", run.ThreadID, got, globalDefault)
		}
		if starts := sb.Delivered("terma.session.start", run.ThreadID, 30*time.Second); len(starts) != 1 {
			t.Errorf("thread announced %d times, want once", len(starts))
		}
	})
}
