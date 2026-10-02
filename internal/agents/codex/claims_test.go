package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Codex Desktop's activity, spooled without Env.EmitFor, carries the repository's binding.
func TestCodexDesktopActivityCarriesTheProject(t *testing.T) {
	env := fundingEnv(t)
	connectCodexDesktop(t)
	path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "2026", "09", "19", "rollout-2026-09-19T12-18-12-"+replySession+".jsonl")
	hookruntest.WriteFile(t, filepath.Dir(path), filepath.Base(path), strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"` + replySession + `"}}`,
		`{"timestamp":"2026-09-19T18:18:39Z","type":"event_msg","payload":{"type":"task_started","turn_id":"` + replyTurn + `","trace_id":"` + replyTrace + `"}}`,
		`{"type":"turn_context","payload":{"turn_id":"` + replyTurn + `","model":"gpt-6-sol"}}`,
		`{"timestamp":"2026-09-19T18:18:42Z","type":"token_usage_record","payload":{"turn_id":"` + replyTurn + `","response_id":"resp_1","usage":{"input_tokens":100,"output_tokens":40}}}`,
		`{"timestamp":"2026-09-19T18:18:44Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"` + replyTurn + `","duration_ms":4400}}`,
	}, "\n")+"\n")
	b, _ := json.Marshal(map[string]any{"session_id": replySession, "cwd": env.Cwd, "transcript_path": path, "model": "gpt-6-sol"})
	env.Stdin = strings.NewReader(string(b))
	if err := stop(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	activity := 0
	for _, e := range hookruntest.Spooled(t, env.Spool) {
		if e.Name != hookrun.EventModelCall && e.Name != hookrun.EventTurnSummary {
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
// payload.
func TestCodexSubagentThreadIsClaimed(t *testing.T) {
	root := hookruntest.InitRepo(t)
	hookruntest.RelayOn(t)
	if err := project.Save(root, &project.File{Project: project.Project{ID: "project-a"}}); err != nil {
		t.Fatal(err)
	}
	sp, _ := spool.Open(t.TempDir())
	env := hookrun.Env{Now: time.Now(), Cwd: root, Spool: sp,
		Stdin: strings.NewReader(`{"session_id":"root-thread","agent_id":"child-thread","agent_type":"worker","cwd":"` + root + `"}`)}
	if err := subagentStart(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	payload := `{"session_id":"root-thread","agent_id":"child-thread-2","cwd":"` + root + `"}`
	hookrun.ClaimFromPayload(context.Background(), hookrun.Env{Now: time.Now(), Cwd: root}, payloadSession(t, payload), "codex")
	for _, id := range []string{"root-thread", "child-thread", "child-thread-2"} {
		if c, ok := claim.Read(id, time.Now()); !ok || c.ProjectID != "project-a" {
			t.Errorf("%s not claimed: %+v %v", id, c, ok)
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
