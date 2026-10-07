package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Codex Desktop's activity, spooled without Env.EmitFor, carries the developer's team.
func TestCodexDesktopActivityCarriesTheProject(t *testing.T) {
	env := fundingEnv(t)
	routeCodex(t, &env)
	path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "2026", "09", "19", "rollout-2026-09-19T12-18-12-"+replySession+".jsonl")
	hookruntest.WriteFile(t, filepath.Dir(path), filepath.Base(path), strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"` + replySession + `"}}`,
		`{"timestamp":"2026-09-19T18:18:39Z","type":"event_msg","payload":{"type":"task_started","turn_id":"` + replyTurn + `","trace_id":"` + replyTrace + `"}}`,
		`{"type":"turn_context","payload":{"turn_id":"` + replyTurn + `","cwd":` + strconv.Quote(env.Cwd) + `,"model":"gpt-6-sol"}}`,
		`{"timestamp":"2026-09-19T18:18:41Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"` + replyTurn + `","item":{"type":"Extension","id":"item_web","kind":"web.search","query":"q"}}}`,
		`{"timestamp":"2026-09-19T18:18:43Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"` + replyTurn + `","item":{"type":"ContextCompaction","id":"item_compact"}}}`,
	}, "\n")+"\n")
	b, _ := json.Marshal(map[string]any{"session_id": replySession, "cwd": env.Cwd, "transcript_path": path, "model": "gpt-6-sol"})
	env.Stdin = strings.NewReader(string(b))
	if err := stop(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	activity := 0
	for _, e := range hookruntest.Spooled(t, env.Spool) {
		if e.Name != semconv.TermaToolCallEvent && e.Name != semconv.TermaCompactionEvent {
			continue
		}
		activity++
		if e.Attrs[hookrun.AttrProjectID] != "project-a" {
			t.Fatalf("%s spooled without its project: %v", e.Name, e.Attrs)
		}
	}
	if activity == 0 {
		t.Fatal("no Desktop activity was captured")
	}
}

// A subagent's own thread id (agent_id) is claimed too, from a spooled event or a bare
// payload, with the working tree.
func TestCodexSubagentThreadIsClaimed(t *testing.T) {
	t.Parallel()
	root := hookruntest.InitRepo(t)
	stateDir := t.TempDir()
	sp, _ := spool.Open(t.TempDir())
	env := hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Spool: sp, Team: "project-a",
		Stdin: strings.NewReader(`{"session_id":"root-thread","agent_id":"child-thread","agent_type":"worker","cwd":` + strconv.Quote(root) + `}`)}
	if err := subagentStart(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	payload := `{"session_id":"root-thread","agent_id":"child-thread-2","cwd":` + strconv.Quote(root) + `}`
	hookrun.ClaimFromPayload(context.Background(), hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Team: "project-a"}, payloadSession(t, payload), "codex")
	// Either way the claim names the working tree, which the relay stamps on Codex's own records.
	tree, _ := filepath.EvalSymlinks(root)
	for _, id := range []string{"root-thread", "child-thread", "child-thread-2"} {
		if c, ok := claim.Read(stateDir, id, time.Now()); !ok || c.ProjectID != "project-a" || c.Root != tree || c.Cwd != tree {
			t.Errorf("%s not claimed with its working tree and directory %s: %+v %v", id, tree, c, ok)
		}
	}
}

func payloadSession(t *testing.T, payload string) hookrun.PayloadSession {
	t.Helper()
	s, ok := hookrun.ReadPayloadSession([]byte(payload))
	if !ok {
		t.Fatalf("no session in %s", payload)
	}
	return s
}
