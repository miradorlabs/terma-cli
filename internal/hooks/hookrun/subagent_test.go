package hookrun_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// A launch is one marker per (session, agent), {}\n, under the agent's state directory; it
// proves that session's launch alone, ages out like the spool, and a hook with no spool
// records none.
func TestSubagentLaunchEvidence(t *testing.T) {
	stateDir := t.TempDir()
	dir := hookrun.AgentStateDir("someagent", "subagents")
	sp, _ := hookruntest.OpenSpool(t.TempDir())
	now := time.Now()
	env := hookrun.Env{StateDir: stateDir, Spool: sp, Now: now}

	path := hookrun.SubagentEvidencePath(stateDir, dir, "parent", "child")
	if want := filepath.Join(stateDir, dir, hookrun.EvidenceID("parent\x00child")+".json"); path != want {
		t.Fatalf("path %s, want %s", path, want)
	}
	env.RememberSubagent(dir, "parent", "child")
	if data, err := os.ReadFile(path); err != nil || string(data) != "{}\n" {
		t.Fatalf("marker %q, %v", data, err)
	}
	if !env.KnownSubagent(dir, "parent", "child") {
		t.Error("the launch is not known")
	}
	for _, other := range [][2]string{{"other", "child"}, {"parent", "other"}} {
		if env.KnownSubagent(dir, other[0], other[1]) {
			t.Errorf("%v is known", other)
		}
	}

	unspooled := hookrun.Env{StateDir: stateDir, Now: now}
	unspooled.RememberSubagent(dir, "parent", "unspooled")
	if env.KnownSubagent(dir, "parent", "unspooled") {
		t.Error("a hook with no spool recorded a launch")
	}

	old := now.Add(-spool.MaxAge - time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if env.KnownSubagent(dir, "parent", "child") {
		t.Error("an expired launch is known")
	}
	env.PruneSubagents(dir)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the expired marker was not pruned: %v", err)
	}
}
