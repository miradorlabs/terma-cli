package harness

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCodexRolloutSpawnReadsTheParentFromTheRolloutHead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	const child = "01a04490-7a2c-7b1e-8000-000000000002"
	write := func(id, source string) string {
		path := filepath.Join(home, "sessions", "2026", "09", "16", "rollout-2026-09-16T10-00-00-"+id+".jsonl")
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		body := `{"timestamp":"2026-09-16T10:00:00.000Z","type":"session_meta","payload":{"id":"` + id + `","source":` + source + `}}` + "\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	ctx := context.Background()

	path := write(child, `{"subagent":{"thread_spawn":{"agent_nickname":"Boyle","agent_path":"/root/architect","agent_role":null,"depth":1,"parent_thread_id":"01a04490-7a2c-7b1e-8000-000000000001"}}}`)
	spawn, status := CodexRolloutSpawn(ctx, child, path)
	if status != "present" || spawn.ParentThreadID != "01a04490-7a2c-7b1e-8000-000000000001" || spawn.Depth != 1 || spawn.AgentNickname != "Boyle" || spawn.AgentPath != "/root/architect" {
		t.Fatalf("spawn = %+v (%s)", spawn, status)
	}
	// The path is a hint: the same thread is found without it.
	if spawn, status = CodexRolloutSpawn(ctx, child, ""); status != "present" || spawn.Depth != 1 {
		t.Fatalf("search: %+v (%s)", spawn, status)
	}

	for _, source := range []string{`"cli"`, `"exec"`, `{"subagent":"review"}`, `{"subagent":{"other":1}}`, `{"subagent":{"thread_spawn":{"depth":1}}}`} {
		const root = "01a04490-7a2c-7b1e-8000-000000000003"
		path := write(root, source)
		if spawn, status := CodexRolloutSpawn(ctx, root, path); status != "root" || spawn != (CodexThreadSpawn{}) {
			t.Fatalf("source %s: %+v (%s)", source, spawn, status)
		}
	}
	if _, status := CodexRolloutSpawn(ctx, "01a04490-7a2c-7b1e-8000-000000000004", ""); status != "missing" {
		t.Fatalf("missing rollout: %s", status)
	}
	if _, status := CodexRolloutSpawn(ctx, "not/safe", ""); status != "invalid_session" {
		t.Fatalf("unsafe id: %s", status)
	}
}
