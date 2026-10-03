package e2e

import (
	"fmt"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// codexDesktopSandbox is a sandbox with a Desktop-shaped app-server on a relay, and a
// personal directory beside the admitted repository.
func codexDesktopSandbox(t *testing.T, b Binary) (*Sandbox, *AppServer, string) {
	t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
	sb := New(t, Isolated, WithCodex(b))
	sb.UseRelay(RelayOptions{Start: true, Hold: 5 * time.Second, Content: true})
	var calls atomic.Int32
	provider := httptest.NewServer(replyingCodexProvider(&calls))
	t.Cleanup(provider.Close)
	personal := filepath.Join(sb.Dir, "personal")
	if err := os.MkdirAll(personal, 0o700); err != nil {
		t.Fatal(err)
	}
	return sb, sb.StartAppServer(b, "Codex Desktop", provider.URL), personal
}

// codexThreadReached reports whether anything of thread reached upstream, and whether
// all of it came with the project and its key.
func codexThreadReached(sb *Sandbox, thread string) (any, ok bool) {
	r := reached(sb.Receiver)[thread]
	return len(r) > 0, r["project "+sb.ProjectID] && r["key Bearer "+liveKey] && len(r) == 2
}

// Codex Desktop through the relay: one app-server process runs a thread in the bound
// repository and a personal one side by side, the first turn a while after the thread
// opened. The repository's thread reaches its
// project; the personal thread reaches nothing; and the process's metrics, which name
// no thread, are withheld — the process works for a personal thread too, and a
// counter cannot be split.
func TestRelayCodexDesktop(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		Proves(t, "codex-desktop", b.Version, "relay.desktop")
		track(t)
		sb, app, personal := codexDesktopSandbox(t, b)
		work := app.ThreadStart(sb.Repo, true)
		mine := app.ThreadStart(personal, false)
		// The developer opens the thread and types later: its start is exported at
		// thread/start, its first hook fires with the first turn, past the hold.
		time.Sleep(8 * time.Second)
		app.Turn(work, "Reply. TERMA_REPO_WORK")
		app.Turn(mine, "Reply. TERMA_PERSONAL_WORK")
		app.Turn(work, "Again. TERMA_REPO_WORK")
		app.Close()
		deadline := time.Now().Add(30 * time.Second)
		for any, _ := codexThreadReached(sb, work); !any && time.Now().Before(deadline); any, _ = codexThreadReached(sb, work) {
			time.Sleep(time.Second)
		}
		time.Sleep(8 * time.Second)
		if any, ok := codexThreadReached(sb, work); !any || !ok {
			t.Errorf("the repository's Desktop thread reached upstream as %v", reached(sb.Receiver)[work])
		}
		e := sb.Receiver.evidence()
		if any, _ := codexThreadReached(sb, mine); any || len(leakedFieldsOf(e, "TERMA_PERSONAL_WORK")) > 0 {
			t.Errorf("the personal Desktop thread reached upstream: %v %v", reached(sb.Receiver)[mine], leakedFieldsOf(e, "TERMA_PERSONAL_WORK"))
		}
		if len(e.metrics) > 0 {
			t.Errorf("%d metrics of a process that also worked for a personal thread reached upstream", len(e.metrics))
		}
		if c := sb.RelayStats(); sum(c, "received.metrics") == 0 {
			Note(t.Name(), "the app-server exported no metrics")
		}
		started := false
		for _, l := range e.logs {
			started = started || l.Attrs["event.name"] == "codex.conversation_starts" && l.Attrs["conversation.id"] == work
		}
		if !started {
			t.Errorf("the repository thread's conversation_starts, exported before its first turn, never reached upstream")
		}
		if len(sb.Events("terma.session.start", work)) == 0 {
			t.Errorf("the repository thread's hooks never ran under app-server")
		}
		sb.StopRelay()
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		failUnclassified(t, c)
		if sum(c, "received.metrics") == sum(c, "dropped.no_session_process.metrics") {
			// Only the TUI clients' own few; the app-server exported none (0.158, even on a
			// clean stop), so the metrics check above had nothing of the daemon's to judge.
			Note(t.Name(), "the app-server exported no metrics")
		}
		if sum(c, "dropped.unclaimed") == 0 {
			t.Errorf("the personal thread was never received and dropped: %v", c)
		}
	})
}

// metricNewest is the latest data point time of m.
func metricNewest(m *metricspb.Metric) uint64 {
	var newest uint64
	note := func(t uint64) { newest = max(newest, t) }
	for _, p := range m.GetSum().GetDataPoints() {
		note(p.GetTimeUnixNano())
	}
	for _, p := range m.GetGauge().GetDataPoints() {
		note(p.GetTimeUnixNano())
	}
	for _, p := range m.GetHistogram().GetDataPoints() {
		note(p.GetTimeUnixNano())
	}
	return newest
}

// A Desktop thread started in the admitted repository, then — after Desktop (or the
// daemon) restarted, which unloads every thread — resumed from a personal directory
// (thread/resume takes a cwd; a thread still loaded keeps its own). What the resumed
// turn does must reach nothing.
func TestRelayCodexDesktopResumedElsewhere(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		Proves(t, "codex-desktop", b.Version, "relay.resumed_elsewhere")
		track(t)
		sb, app, personal := codexDesktopSandbox(t, b)
		provider := httptest.NewServer(replyingCodexProvider(new(atomic.Int32)))
		defer provider.Close()
		work := app.ThreadStart(sb.Repo, true)
		app.Turn(work, "Reply. TERMA_REPO_WORK")
		app.Close()
		time.Sleep(3 * time.Second) // the first turn's records leave before the resume
		again := sb.StartAppServer(b, "Codex Desktop", provider.URL)
		if got := again.ThreadResume(work, personal, false); got != personal {
			t.Logf("the resumed thread runs in %q, not the personal directory", got)
		}
		again.Turn(work, "Reply. TERMA_PERSONAL_WORK")
		again.Close()
		time.Sleep(10 * time.Second)
		e := sb.Receiver.evidence()
		if any, _ := codexThreadReached(sb, work); !any {
			t.Errorf("the thread's repository turn never reached upstream")
		}
		if leaked := leakedFieldsOf(e, "TERMA_PERSONAL_WORK"); len(leaked) > 0 {
			t.Errorf("the turn resumed in a personal directory reached upstream in: %v", leaked)
		}
		starts := 0
		for _, l := range e.logs {
			if l.Attrs["event.name"] == "codex.conversation_starts" && l.Attrs["conversation.id"] == work {
				starts++
			}
		}
		Note(t.Name(), fmt.Sprintf("conversation_starts forwarded for the thread: %d", starts))
		sb.StopRelay()
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		failUnclassified(t, c)
	})
}

// titleOf finds the title conversation among what reached upstream: its start has
// Codex's internal policies. It returns the conversation and the thread the relay
// attributed it to.
func titleOf(e telemetryEvidence) (title, thread, attribution string) {
	for _, r := range e.logs {
		if r.Attrs["event.name"] == "codex.conversation_starts" && r.Attrs["approval_policy"] == "never" && r.Attrs["sandbox_policy"] == "read-only" {
			return r.Attrs["conversation.id"], r.Resource["terma.relay.session.id"], r.Resource["terma.relay.attribution"]
		}
	}
	return "", "", ""
}

// The interactive TUI attached to Codex's daemon — what a bare `codex` does since
// 0.157 when a daemon runs. The daemon, not the TUI, runs the thread, its hooks and
// its export. A thread in the admitted repository reaches its project, and nothing else
// does: not its title conversation (unclaimed, and nothing proves it is Codex's own),
// and not a thread in a personal directory in the same daemon — neither its prompt
// nor its own title.
func TestRelayCodexDaemonTUI(t *testing.T) {
	forEachCodex(t, func(t *testing.T, b Binary, _ bool) {
		ProvesAll(t, b, "relay.daemon")
		if versionLess(b.Version, "0.157.0") {
			Record(t.Name(), "not run", "the TUI attaches to a daemon from 0.157")
			t.Skip("the TUI attaches to a daemon from 0.157")
		}
		track(t)
		t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithCodex(b))
		sb.UseShortCodexHome()
		sb.UseRelay(RelayOptions{Start: true, Hold: 10 * time.Second, Content: true})
		personal := filepath.Join(sb.Dir, "personal")
		if err := os.MkdirAll(personal, 0o700); err != nil {
			t.Fatal(err)
		}
		var calls atomic.Int32
		provider := httptest.NewServer(replyingCodexProvider(&calls))
		defer provider.Close()
		probe := sb.StartAppServer(b, "terma-e2e", provider.URL)
		if n := probe.TrustHooks(sb, sb.Repo); n == 0 {
			t.Fatal("Codex listed no hooks to trust in the repository")
		}
		probe.Close()
		sb.UseCodexConfigFile(provider.URL)
		// The developer trusts the folders they work in; the personal one has no hooks.
		cfg := filepath.Join(sb.CodexHome, "config.toml")
		existing, _ := os.ReadFile(cfg)
		if err := os.WriteFile(cfg, append(existing, []byte("\n[projects."+tomlQuote(personal)+"]\ntrust_level = \"trusted\"\n")...), 0o600); err != nil {
			t.Fatal(err)
		}
		daemon := sb.StartCodexDaemon(b)

		sb.CodexDaemonTUI(b, sb.Repo, "Reply. TERMA_REPO_WORK")
		time.Sleep(5 * time.Second)
		claims, _ := filepath.Glob(filepath.Join(sb.TermaConfig, "relay", "claims", "*.json"))
		attached := false
		for _, c := range claims {
			data, _ := os.ReadFile(c)
			attached = attached || strings.Contains(string(data), fmt.Sprintf(",%d,", daemon.PID())) || strings.Contains(string(data), fmt.Sprintf("[%d,", daemon.PID()))
		}
		if !attached {
			t.Fatalf("no claim names the daemon (pid %d): the TUI ran in-process, so this scenario proves nothing", daemon.PID())
		}
		e := sb.Receiver.evidence()
		title, thread, how := titleOf(e)
		switch {
		case agentRecords(e) == 0:
			t.Fatalf("nothing of the repository's daemon thread reached upstream: %v", sb.RelayStats())
		case calls.Load() < 2:
			Note(t.Name(), "no title conversation this build")
		case title != "":
			t.Errorf("the unclaimed title conversation %s reached upstream (as %q, for %q)", title, how, thread)
		}

		personalStart := time.Now()
		sb.CodexDaemonTUI(b, personal, "Reply. TERMA_PERSONAL_WORK")
		daemon.Stop()                // its exporters flush on the way out
		time.Sleep(15 * time.Second) // past the hold
		e = sb.Receiver.evidence()
		if leaked := leakedFieldsOf(e, "TERMA_PERSONAL_WORK"); len(leaked) > 0 {
			t.Errorf("the personal daemon thread reached upstream in: %v", leaked)
		}
		for _, r := range e.logs {
			if r.Resource["service.name"] != "terma-cli" && r.Resource["mirador.project.id"] != sb.ProjectID {
				t.Errorf("a record reached upstream without the project: %v", r.Attrs)
			}
		}
		// Once the daemon also works for the personal thread, a counter it exports is
		// partly personal: none may be attributed to the project by process.
		late := 0
		for _, m := range e.metrics {
			if metricNewest(m.Proto) > uint64(personalStart.Add(2*time.Second).UnixNano()) {
				late++
			}
		}
		if late > 0 {
			t.Errorf("%d metrics exported while the daemon ran a personal thread reached the project", late)
		}
		sb.StopRelay()
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		failUnclassified(t, c)
		if sum(c, "dropped.unclaimed") == 0 {
			t.Errorf("the personal thread was never received and dropped: %v", c)
		}
	})
}
