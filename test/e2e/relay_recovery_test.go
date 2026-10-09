package e2e

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The relay stopping in the middle of a turn: killed, as by a crash, or making way for a
// newer terma, as after an upgrade. The disruption lands at the session's second model
// request, after the tool call and its hook, and that request then takes midTurnPause, as
// a long generation would, so the agent's exporter sends before the session's next hook.
//
// Every run proves the upgrade: a hook-started relay making way is followed by the relay
// that takes its place at once, and the session arrives whole. TERMA_E2E_RELAY_COMPARE=1
// measures the rest: each disruption under each way the relay runs, the service managers'
// restarts included. A kill loses what the agent exports before something starts another
// relay, which for a hook-started relay is the session's next hook: those cells fail, and
// what they lost is the measurement.

// relayCompare runs the whole comparison rather than the upgrade alone.
var relayCompare = os.Getenv("TERMA_E2E_RELAY_COMPARE") == "1"

// midTurnPause is longer than Claude Code's 5-second log export interval.
const midTurnPause = 8 * time.Second

// relayDisruption stops the relay running as pid.
type relayDisruption struct {
	name string
	stop func(t *testing.T, sb *Sandbox, pid int)
}

var relayDisruptions = []relayDisruption{
	{"killed", func(t *testing.T, _ *Sandbox, pid int) {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Errorf("kill relay %d: %v", pid, err)
		}
	}},
	// What a newer terma's hook does (daemon.Supersede): it asks the relay to make way, and
	// starts the relay that follows it, unless the service manager starts the next. Done
	// here, since the sandbox's terma is a development build, which never supersedes a relay.
	{"replaced", func(t *testing.T, sb *Sandbox, pid int) {
		dir := filepath.Join(sb.TermaConfig, "relay")
		if err := os.WriteFile(filepath.Join(dir, "replace"), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
			t.Errorf("ask relay %d to make way: %v", pid, err)
			return
		}
		if sb.serviceRelay {
			return
		}
		log, err := os.OpenFile(filepath.Join(dir, "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Error(err)
			return
		}
		follower := exec.Command(sb.Terma, "relay", "run", "--quiet", "--launch", "hook", "--follow")
		follower.Env, follower.Dir = sb.termaEnv(), sb.Repo
		follower.Stdout, follower.Stderr = log, log
		if err := follower.Start(); err != nil {
			t.Errorf("start the follower: %v", err)
			_ = log.Close()
			return
		}
		go func() { _ = follower.Wait(); _ = log.Close() }()
		t.Cleanup(func() { _ = follower.Process.Kill() })
	}},
}

// disruptMidTurn wraps a model provider: at the second request isModel counts, it stops
// the relay running then and answers midTurnPause later. The pid it stopped is returned
// through the pointer, 0 until then.
func (sb *Sandbox) disruptMidTurn(h http.Handler, isModel func(*http.Request) bool, d relayDisruption) (http.Handler, *atomic.Int64) {
	t := sb.T
	var requests atomic.Int32
	var stopped atomic.Int64
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isModel(r) && requests.Add(1) == 2 {
			pid := recordedPID(filepath.Join(sb.TermaConfig, "relay"))
			if pid <= 0 || !sb.waitRelay(time.Second) {
				t.Errorf("no relay running at the second model request (pid %d)", pid)
			} else {
				d.stop(t, sb, pid)
				stopped.Store(int64(pid))
			}
			time.Sleep(midTurnPause)
		}
		h.ServeHTTP(w, r)
	}), &stopped
}

// checkRelayAfter fails unless the relay was stopped mid-turn and one runs now. A killed
// relay's successor is a later hook's; a relay asked to make way may still be serving,
// since it waits out what it holds.
func (sb *Sandbox) checkRelayAfter(stopped *atomic.Int64) {
	t := sb.T
	t.Helper()
	if stopped.Load() == 0 {
		t.Fatal("the relay was never stopped mid-turn")
	}
	if !sb.waitRelay(10 * time.Second) {
		t.Fatal("no relay runs after the one stopped mid-turn")
	}
}

// relayLaunches are the ways a developer's relay runs: started by the session's hooks, or
// kept running by the service manager from before the session.
var relayLaunches = []struct {
	name string
	opts RelayOptions
}{{"hook", RelayOptions{}}, {"launchd", RelayOptions{Service: Launchd}}, {"systemd", RelayOptions{Service: Systemd}}}

// midTurnCases are the launches and disruptions this run covers: the upgrade of a
// hook-started relay, or with relayCompare all of them.
func midTurnCases(t *testing.T, b Binary, run func(t *testing.T, opts RelayOptions, d relayDisruption)) {
	for _, l := range relayLaunches {
		for _, d := range relayDisruptions {
			proven := l.name == "hook" && d.name == "replaced"
			if !proven && !relayCompare {
				continue
			}
			t.Run(l.name+"/"+d.name, func(t *testing.T) {
				if proven {
					Proves(t, b.Harness, b.Version, "relay.replaced_mid_turn")
				}
				run(t, l.opts, d)
			})
		}
	}
}

func claudeModelRequest(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/v1/messages") }

func codexModelRequest(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/responses") }

func TestRelayClaudeMidTurn(t *testing.T) {
	forEachClaude(t, func(t *testing.T, b Binary, _ bool) {
		midTurnCases(t, b, func(t *testing.T, opts RelayOptions, d relayDisruption) {
			track(t)
			t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
			sb := New(t, Isolated, WithClaude(b))
			sb.UseRelay(opts)
			sb.logHookRelayOnFailure()
			var calls atomic.Int32
			h, stopped := sb.disruptMidTurn(claudeTelemetryProvider(&calls), claudeModelRequest, d)
			provider := httptest.NewServer(h)
			defer provider.Close()
			sb.ClaudeBaseURL = provider.URL
			_, sid := sb.ClaudeHeadless(RouteAPIKey, telemetryPrompt, "--max-turns", "3", "--tools", "Bash", "--allowedTools", "Bash")
			sb.checkRelayAfter(stopped)
			awaitTelemetry(t, sb, func(r contractReporter, e telemetryEvidence) {
				checkClaudeTelemetry(r, e, sid, sb.ProjectID, true)
			})
			sb.StopRelay()
			c := sb.RelayStats()
			noteRelayStats(t.Name(), c)
			failUnclassified(t, c)
		})
	})
}

func TestRelayCodexMidTurn(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		midTurnCases(t, b, func(t *testing.T, opts RelayOptions, d relayDisruption) {
			track(t)
			t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
			sb := New(t, Isolated, WithCodex(b))
			sb.UseRelay(opts)
			sb.logHookRelayOnFailure()
			var calls atomic.Int32
			h, stopped := sb.disruptMidTurn(codexTelemetryProvider(t, &calls), codexModelRequest, d)
			provider := httptest.NewServer(h)
			defer provider.Close()
			run := sb.CodexExec(RouteAPIKey, telemetryPrompt, fixtureCodexArgs(provider.URL)...)
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("Codex stdout:\n%s\nCodex stderr:\n%s", run.Stdout, run.Stderr)
				}
			})
			sb.checkRelayAfter(stopped)
			awaitTelemetry(t, sb, func(r contractReporter, e telemetryEvidence) {
				checkCodexTelemetry(r, e, run.ThreadID, sb.ProjectID, true, true)
			})
			sb.StopRelay()
			c := sb.RelayStats()
			noteRelayStats(t.Name(), c)
			failUnclassified(t, c)
		})
	})
}
