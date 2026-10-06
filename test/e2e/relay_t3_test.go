package e2e

import (
	"cmp"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// T3 Code through the relay. One T3 server runs a Codex thread and a Claude thread in
// the admitted repository, and one of each in a personal project beside it: the
// repository's reach its project — prompts included, content being allowed — and the
// personal ones reach nothing. T3 spawns one app-server per thread and Claude through
// the Agent SDK, each exporting from the process that runs its hooks, so nothing about
// T3 is special to the relay.
func TestRelayT3(t *testing.T) {
	forEachT3(t, func(t *testing.T, b Binary) {
		ProvesAll(t, b, "relay.telemetry")
		track(t)
		codexBuilds, claudeBuilds := CodexBinaries(t), ClaudeBinaries(t)
		if len(codexBuilds) == 0 || len(claudeBuilds) == 0 {
			t.Skip("T3 needs a Codex and a Claude Code build")
		}
		t.Setenv("OPENAI_API_KEY", "synthetic-telemetry-key")
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-telemetry-key")
		sb := New(t, Isolated, WithCodex(codexBuilds[len(codexBuilds)-1]), WithClaude(claudeBuilds[len(claudeBuilds)-1]))
		sb.UseRelay(RelayOptions{Start: true, Hold: 5 * time.Second, Content: true})
		personal := filepath.Join(sb.Dir, "personal")
		if err := os.MkdirAll(personal, 0o700); err != nil {
			t.Fatal(err)
		}
		codexProvider := httptest.NewServer(replyingCodexProvider(new(atomic.Int32)))
		defer codexProvider.Close()
		claudeProvider := httptest.NewServer(claudeWorkloadProvider(new(atomic.Int32), nil, false))
		defer claudeProvider.Close()

		// The developer trusted the repository's Codex hooks, and both folders.
		probe := sb.StartAppServer(sb.Codex, "terma-e2e", codexProvider.URL)
		if n := probe.TrustHooks(sb, sb.Repo); n == 0 {
			t.Fatal("Codex listed no hooks to trust in the repository")
		}
		probe.Close()
		sb.UseCodexConfigFile(codexProvider.URL)
		cfg := filepath.Join(sb.CodexHome, "config.toml")
		existing, _ := os.ReadFile(cfg)
		if err := os.WriteFile(cfg, append(existing, []byte("\n[projects."+tomlQuote(personal)+"]\ntrust_level = \"trusted\"\n")...), 0o600); err != nil {
			t.Fatal(err)
		}

		x := sb.StartT3(b, "ANTHROPIC_BASE_URL="+claudeProvider.URL, "ANTHROPIC_API_KEY=synthetic-telemetry-key")
		repo, mine := x.Project(sb.Repo), x.Project(personal)
		x.Turn(repo, T3Codex, "Reply. TERMA_REPO_CODEX")
		x.Turn(mine, T3Codex, "Reply. TERMA_PERSONAL_CODEX")
		x.Turn(repo, T3Claude, "Reply. TERMA_REPO_CLAUDE")
		x.Turn(mine, T3Claude, "Reply. TERMA_PERSONAL_CLAUDE")
		// T3 keeps its agents running; their exporters send on their own schedule
		// (Claude Code's logs every 5 s), so the repository's records are waited for
		// before T3 is stopped rather than lost to the stop.
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			e := sb.Receiver.evidence()
			if len(leakedFieldsOf(e, "TERMA_REPO_CODEX")) > 0 && len(leakedFieldsOf(e, "TERMA_REPO_CLAUDE")) > 0 {
				break
			}
			time.Sleep(time.Second)
		}
		x.Stop()
		time.Sleep(10 * time.Second) // past the hold
		e := sb.Receiver.evidence()
		seen := map[string]int{}
		for _, r := range e.logs {
			seen[r.Resource["service.name"]+" "+cmp.Or(r.EventName, r.Attrs["event.name"])]++
		}
		for _, m := range e.metrics {
			seen[m.Resource["service.name"]+" metric"]++
		}
		t.Logf("upstream by service and event: %v", seen)
		for _, marker := range []string{"TERMA_REPO_CODEX", "TERMA_REPO_CLAUDE"} {
			if len(leakedFieldsOf(e, marker)) == 0 {
				t.Errorf("the repository's %s thread never reached upstream", marker)
			}
		}
		for _, marker := range []string{"TERMA_PERSONAL_CODEX", "TERMA_PERSONAL_CLAUDE"} {
			if leaked := leakedFieldsOf(e, marker); len(leaked) > 0 {
				t.Errorf("the personal %s thread reached upstream in: %v", marker, leaked)
			}
		}
		for _, r := range e.logs {
			if !fromTerma(r) && r.Resource["mirador.project.id"] != sb.ProjectID {
				t.Errorf("a record reached upstream without the project: %v", r.Attrs)
			}
		}
		sb.StopRelay()
		c := sb.RelayStats()
		noteRelayStats(t.Name(), c)
		failUnclassified(t, c)
		if sum(c, "dropped.unclaimed")+sum(c, "dropped.not_collected") == 0 {
			t.Errorf("the personal threads were never received and dropped: %v", c)
		}
	})
}
