package codex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/delivery"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// On a migrated machine what a rollout turn recorded routes by the directory the turn
// names, its own location, rather than by where the hook reading it runs: a thread that
// ran its first turn in a personal repository and was resumed in Acme's sends each turn's
// replies and quota to its own team. Routing by its own location exempts none of it from
// its run's Mixed state, since it is content (runs.Locational; see
// TestACodexReplyAfterItsRunCrossedIsDroppedWhileItsFileEventGoes). Its name, which any
// turn may have given it, is the thread's and goes with the session's run. Nothing names
// a project.
func TestRoutedCodexTurnsGoWhereTheyRan(t *testing.T) {
	st := hookruntest.TwoOrganizations(t)
	env := fundingEnv(t)
	routeCodex(t, &env)
	env.Policy, env.Team, env.Routing = config.NoPolicy("", ""), "", hookrun.NewRouting(hookruntest.RoutedProfile, st)
	side := hookruntest.RepoAt(t, "github.com/dawson/side")
	evs := stopResumed(t, env, side, env.Cwd)

	personal, acme := hookruntest.Enrollment(t, st, "personal"), hookruntest.Enrollment(t, st, "acme-platform")
	byTurn := map[any]string{resumedTurn1: personal, resumedTurn2: acme}
	counted := map[string]int{}
	for _, e := range evs {
		if _, ok := e.Attrs[hookrun.AttrProjectID]; ok || e.Profile != hookruntest.RoutedProfile {
			t.Errorf("%s: profile %q, attrs %v", e.Name, e.Profile, e.Attrs)
		}
		switch e.Name {
		case semconv.TermaAssistantMessageEvent, semconv.TermaSessionQuotaEvent:
			want, ok := byTurn[e.Attrs[semconv.TermaTurnIDKey]]
			if !ok {
				continue
			}
			if e.Enrollment != want || !e.OwnLocation || runs.Locational(e.Name) {
				t.Errorf("%s of turn %v: enrollment %q own %v, want %q by its own directory, obeying its run's Mixed state", e.Name, e.Attrs[semconv.TermaTurnIDKey], e.Enrollment, e.OwnLocation, want)
			}
			counted[e.Enrollment]++
		case semconv.TermaSessionTitleEvent:
			if e.Enrollment != acme || e.OwnLocation {
				t.Errorf("title: enrollment %q own %v, want the session's run's", e.Enrollment, e.OwnLocation)
			}
		}
	}
	if counted[personal] == 0 || counted[acme] == 0 {
		t.Fatalf("turn events by destination %v: each turn sends its own", counted)
	}
	if got := len(hookruntest.Named(evs, semconv.TermaSessionTitleEvent)); got != 1 {
		t.Errorf("%d titles, want the thread's one", got)
	}
}

// The hosted actions a turn ran route by that turn's directory too.
func TestRoutedCodexDesktopActivityGoesWhereItsTurnRan(t *testing.T) {
	st := hookruntest.TwoOrganizations(t)
	env := fundingEnv(t)
	routeCodex(t, &env)
	env.Policy, env.Team, env.Routing = config.NoPolicy("", ""), "", hookrun.NewRouting(hookruntest.RoutedProfile, st)
	side := hookruntest.RepoAt(t, "github.com/dawson/side")
	path := sequenceFixture(t, `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}`+"\n"+turnContext("turn-1", side)+
		`{"timestamp":"2026-10-03T22:01:40Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"turn-1","item":{"type":"Extension","id":"item_1","kind":"web.search"}}}`+"\n")
	r, ok := env.Open(context.Background(), testCodexID, env.Cwd)
	if !ok {
		t.Fatal("the hook's repository is not collected")
	}
	captureCodexDesktopActivity(context.Background(), env, r, &codexHookInput{SessionID: testCodexID, TranscriptPath: path})
	evs := hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaToolCallEvent)
	if len(evs) != 1 || evs[0].Enrollment != hookruntest.Enrollment(t, st, "personal") || !evs[0].OwnLocation || evs[0].Attrs[hookrun.AttrProjectID] != nil {
		t.Fatalf("hosted actions %+v, want one to personal by its turn's directory", evs)
	}
}

// A reply Codex said after its run crossed organizations, read at Stop, is dropped with the
// run, though it routes by its turn's own directory: it is content. The file event the run
// spooled for the checkout it edited is not: it says only where the edit happened, and
// still goes to that checkout's own team, Acme. Before the crossing the reply goes too.
func TestACodexReplyAfterItsRunCrossedIsDroppedWhileItsFileEventGoes(t *testing.T) {
	for _, crossed := range []bool{false, true} {
		t.Run(map[bool]string{false: "the run never crossed", true: "the run crossed"}[crossed], func(t *testing.T) {
			st := hookruntest.TwoOrganizations(t)
			env := fundingEnv(t)
			routeCodex(t, &env)
			env.Cwd = hookruntest.RepoAt(t, "github.com/acme/api")
			env.Policy, env.Team, env.Routing = config.NoPolicy("", ""), "", hookrun.NewRouting(hookruntest.RoutedProfile, st)
			hookruntest.WriteRoutingState(t, env.StateDir, st)
			if err := os.WriteFile(config.CredentialsV2Path(env.ConfigDir), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var bodies []string
			host := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				mu.Lock()
				defer mu.Unlock()
				bodies = append(bodies, string(b))
			}))
			t.Cleanup(host.Close)
			acme := hookruntest.Enrollment(t, st, "acme-platform")
			if err := keystore.SetTeam(env.ConfigDir, acme, "acme-platform", "ter_srv_acme", keystore.Hosts{OTLP: host.URL}); err != nil {
				t.Fatal(err)
			}

			began := time.Now().Add(-time.Hour).UTC()
			if _, err := runs.Start(env.StateDir, replySession, began); err != nil {
				t.Fatal(err)
			}
			if crossed {
				if err := runs.MarkMixed(env.StateDir, replySession, began.Add(10*time.Minute), runs.Why{Reason: runs.Crossed}); err != nil {
					t.Fatal(err)
				}
			}
			hookruntest.WriteFile(t, env.Cwd, "NOTES.md", "acme\n")
			patch, _ := json.Marshal(map[string]any{"command": "apply_patch <<'PATCH'\n*** Begin Patch\n*** Update File: NOTES.md\n@@\n-acme\n+acme, edited\n*** End Patch\nPATCH"})
			call, _ := json.Marshal(map[string]any{"session_id": replySession, "hook_event_name": "PostToolUse", "cwd": env.Cwd, "model": "gpt-6",
				"tool_name": "apply_patch", "tool_use_id": "call_1", "tool_response": "ok", "tool_input": json.RawMessage(patch)})
			env.Stdin = strings.NewReader(string(call))
			if err := postToolUse(context.Background(), env); err != nil {
				t.Fatal(err)
			}
			said := began.Add(20 * time.Minute).Format(time.RFC3339Nano)
			path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "rollout-"+replySession+".jsonl")
			hookruntest.WriteFile(t, filepath.Dir(path), filepath.Base(path), strings.Join([]string{
				`{"type":"session_meta","payload":{"id":"` + replySession + `","cwd":` + strconv.Quote(env.Cwd) + `}}`,
				`{"timestamp":"` + said + `","type":"event_msg","payload":{"type":"task_started","turn_id":"` + replyTurn + `","trace_id":"` + replyTrace + `"}}`,
				strings.TrimSuffix(turnContext(replyTurn, env.Cwd), "\n"),
				`{"timestamp":"` + said + `","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg_a1","phase":"final_answer","content":[{"type":"output_text","text":"QUOTEDFROMACME the notes say so."}]}}`,
				`{"timestamp":"` + said + `","type":"event_msg","payload":{"type":"task_complete","turn_id":"` + replyTurn + `"}}`,
			}, "\n")+"\n")
			stopped, _ := json.Marshal(map[string]any{"session_id": replySession, "cwd": env.Cwd, "transcript_path": path, "model": "gpt-6"})
			env.Stdin = strings.NewReader(string(stopped))
			if err := stop(context.Background(), env); err != nil {
				t.Fatal(err)
			}

			router := delivery.Router{ConfigDir: env.ConfigDir, StateDir: env.StateDir, Consent: func(_ string, c hookrun.Consent) bool { return repliesConsented(c) }}
			res := router.Flush(context.Background(), env.Spool, &config.Config{ProfileName: config.DefaultProfile, OTLPURL: "http://127.0.0.1:1"}, true, 0)
			mu.Lock()
			body := strings.Join(bodies, "\n")
			mu.Unlock()
			if !strings.Contains(body, semconv.TermaFilesTouchedEvent) || !strings.Contains(body, "NOTES.md") {
				t.Errorf("the run's file event did not reach Acme (%+v):\n%s", res, body)
			}
			if sent := strings.Contains(body, "QUOTEDFROMACME"); sent == crossed {
				t.Errorf("reply sent %v, the run crossed %v (%+v):\n%s", sent, crossed, res, body)
			}
		})
	}
}
