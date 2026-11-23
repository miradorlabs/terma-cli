package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/agentstest"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// An agent that reports through its hooks alone is recorded only while they are there:
// doctor fails a chosen one whose hooks file is gone, or names another terma, and says
// nothing of one the developer did not choose.
func TestAgentHooksCheck(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	fake := agentstest.UserHooked{Agent: agentstest.Agent{ID: "fake"}, Path: path}
	reg := agents.New(fake).Support(fake)
	command := hookmgr.UserHookCommand("/opt/terma/bin/terma")
	install := func(cmd func(string) string) {
		t.Helper()
		plan, err := fake.PlanUserHooks(dir, cmd, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := hookmgr.Apply(dir, plan); err != nil {
			t.Fatal(err)
		}
	}

	if _, ok := AgentHooksCheck(reg, nil, command); ok {
		t.Error("an agent the developer did not choose was checked")
	}
	if _, ok := AgentHooksCheck(reg, []string{"fake"}, nil); ok {
		t.Error("checked without knowing the command setup writes")
	}

	c, ok := AgentHooksCheck(reg, []string{"fake"}, command)
	if !ok || c.Status != Fail || c.Fix != "terma setup" || !strings.Contains(c.Detail, "fake") {
		t.Fatalf("no hooks file: %+v, %v", c, ok)
	}

	install(command)
	if c, ok := AgentHooksCheck(reg, []string{"fake"}, command); !ok || c.Status != Pass {
		t.Fatalf("hooks in place: %+v, %v", c, ok)
	}

	install(hookmgr.UserHookCommand("/old/terma"))
	if c, ok := AgentHooksCheck(reg, []string{"fake"}, command); !ok || c.Status != Fail {
		t.Fatalf("hooks naming another terma: %+v, %v", c, ok)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if c, ok := AgentHooksCheck(reg, []string{"fake"}, command); !ok || c.Status != Fail {
		t.Fatalf("hooks file deleted: %+v, %v", c, ok)
	}

	// An agent this build does not offer is not judged.
	if _, ok := AgentHooksCheck(agents.New(fake), []string{"fake"}, command); ok {
		t.Error("an unsupported agent was checked")
	}
}
