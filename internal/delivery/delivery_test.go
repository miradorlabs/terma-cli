package delivery

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

const testKey = "ter_srv_minted0123456789abcdefghijklmnopqrstuv"

// A project's events go where its key works: a pinned host, else the key's own, else the
// profile's.
func TestEachProjectGoesToItsKeysOwnEnvironment(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	prod, err := config.EndpointsFor(config.EnvProd)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{OTLPURL: prod.OTLPURL, APIURL: prod.APIURL}
	if err := keystore.Set("keyed", testKey, keystore.Hosts{OTLP: "https://otlp.keyed", API: "https://api.keyed"}); err != nil {
		t.Fatal(err)
	}
	r := Router{}
	for _, tc := range []struct{ project, otlp, api string }{
		{"keyed", "https://otlp.keyed", "https://api.keyed"},
		{"unknown", prod.OTLPURL, prod.APIURL},
	} {
		if got := r.Endpoint(cfg, tc.project); got != tc.otlp {
			t.Errorf("Endpoint(%s) = %q, want %q", tc.project, got, tc.otlp)
		}
		if got := r.API(cfg, tc.project); got != tc.api {
			t.Errorf("API(%s) = %q, want %q", tc.project, got, tc.api)
		}
	}
	pinned := Router{OTLPPinned: true, APIPinned: true}
	if pinned.Endpoint(cfg, "keyed") != cfg.OTLPURL || pinned.API(cfg, "keyed") != cfg.APIURL {
		t.Fatal("a pinned host was not used for every project")
	}
}

// A reply or a thread's name leaves only when its agent consents; with no one to ask, or
// an agent that refuses, it is withheld.
func TestConversationContentNeedsItsAgentsConsent(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	pol := config.Policy{Mode: config.ModeRepo, IncludePrompts: true}
	reply := spool.Event{Name: hookrun.EventAssistantMessage, Attrs: map[string]any{hookrun.AttrTool: "fake"}}
	var asked []string
	for _, tc := range []struct {
		name    string
		consent func(string, hookrun.Consent) bool
		want    bool
	}{
		{"no one to ask", nil, false},
		{"refused", func(string, hookrun.Consent) bool { return false }, false},
		{"consented", func(tool string, _ hookrun.Consent) bool { asked = append(asked, tool); return true }, true},
	} {
		if got := (Router{Consent: tc.consent}).Allowed(pol, "p1", reply); got != tc.want {
			t.Errorf("%s: Allowed = %v", tc.name, got)
		}
	}
	if len(asked) != 1 || asked[0] != "fake" {
		t.Fatalf("asked %v", asked)
	}
	noPrompts := pol
	noPrompts.IncludePrompts = false
	if (Router{Consent: func(string, hookrun.Consent) bool { return true }}).Allowed(noPrompts, "p1", reply) {
		t.Fatal("a reply left under a policy that withholds prompts")
	}
}

// A queued event is held to the policy in force when it leaves: one whose coverage moved
// out of global mode sends nothing.
func TestQueuedEventsMeetTodaysPolicy(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	start := spool.Event{Name: hookrun.EventSessionStart}
	logs := config.Policy{Mode: config.ModeRepo}
	r := Router{}
	if !r.Allowed(logs, "p1", start) {
		t.Fatal("a project's event was withheld")
	}
	global := spool.Event{Name: hookrun.EventSessionStart, Global: true}
	if r.Allowed(logs, "p1", global) {
		t.Fatal("a global-mode event left after coverage moved back to repositories")
	}
}

// A project with no policy yet, or no key, keeps its events; one with neither problem
// delivers, and every counter says why.
func TestAProjectWithoutAPolicyOrAKeyKeepsItsEvents(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	s, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"unfetched", "keyless", ""} {
		if err := s.Append(spool.Event{Name: hookrun.EventSessionStart, Attrs: map[string]any{hookrun.AttrProjectID: id}}); err != nil {
			t.Fatal(err)
		}
	}
	r := Router{Policy: func(_ context.Context, _ *config.Config, team string) (config.Policy, error) {
		if team == "unfetched" {
			return config.Policy{}, errors.New("not fetched yet")
		}
		return config.Policy{Mode: config.ModeRepo}, nil
	}}
	res := r.Flush(t.Context(), s, &config.Config{}, true, 0)
	if res.Sent != 0 || res.Held != 2 || res.Unroutable != 1 || res.Err != nil {
		t.Fatalf("Flush = %+v", res)
	}
}

// An event spooled while the team's policy collected content leaves under the policy in
// force when it is sent: once tightened, without its content, the rest of it still sent.
func TestContentSpooledUnderAnOlderPolicyLeavesWithoutIt(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
	}))
	defer srv.Close()
	if err := keystore.Set("p1", testKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	s, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []spool.Event{
		{Name: hookrun.EventUserPrompt, SessionID: "s1", Attrs: map[string]any{hookrun.AttrProjectID: "p1", hookrun.AttrTurnID: "turn-1", "prompt": "private prompt"}},
		{Name: hookrun.EventToolCall, SessionID: "s1", Attrs: map[string]any{hookrun.AttrProjectID: "p1", hookrun.AttrToolCallID: "call-1", "arguments": "private arguments", "output": "private output"}},
		{Name: hookrun.EventApprovalAsked, SessionID: "s1", Attrs: map[string]any{hookrun.AttrProjectID: "p1", hookrun.AttrReason: "private reason"}},
	} {
		if err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	tightened := config.Policy{Mode: config.ModeRepo, TeamID: "p1", FetchedAt: time.Now()}
	r := Router{OTLPPinned: true, Policy: func(context.Context, *config.Config, string) (config.Policy, error) { return tightened, nil }}
	res := r.Flush(t.Context(), s, &config.Config{OTLPURL: srv.URL}, true, 0)
	if res.Sent != 3 || res.Err != nil {
		t.Fatalf("Flush = %+v", res)
	}
	sent := string(bytes.Join(bodies, nil))
	if strings.Contains(sent, "private") {
		t.Fatalf("content the policy withholds left:\n%q", sent)
	}
	for _, kept := range []string{"turn-1", "call-1", hookrun.EventApprovalAsked} {
		if !strings.Contains(sent, kept) {
			t.Errorf("%s did not leave with its content withheld", kept)
		}
	}
}

// Outgoing withholds by kind and by switch, and leaves the spooled event as it was.
func TestOutgoingWithholdsOnlyWhatThePolicyDoes(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	call := spool.Event{Name: hookrun.EventToolCall, Attrs: map[string]any{"arguments": "a", "output": "o", hookrun.AttrToolName: "exec"}}
	prompt := spool.Event{Name: hookrun.EventUserPrompt, Attrs: map[string]any{"prompt": "p"}}
	promptsOnly := config.Policy{Mode: config.ModeRepo, IncludePrompts: true}
	if out, ok := (Router{}).Outgoing(promptsOnly, "p1", call); !ok || out.Attrs["arguments"] != nil || out.Attrs["output"] != nil || out.Attrs[hookrun.AttrToolName] != "exec" {
		t.Fatalf("tool call = %+v, %v", out, ok)
	}
	if call.Attrs["arguments"] != "a" {
		t.Fatal("Outgoing changed the spooled event")
	}
	if out, ok := (Router{}).Outgoing(promptsOnly, "p1", prompt); !ok || out.Attrs["prompt"] != "p" {
		t.Fatalf("prompt = %+v, %v", out, ok)
	}
	// Not validated: nothing leaves at all.
	if _, ok := (Router{}).Outgoing(config.NoPolicy("", ""), "p1", prompt); ok {
		t.Fatal("an event left under no policy")
	}
}

// Every event kind the hooks spool is classified, as content-bearing or content-free, so
// delivery never sends an unknown kind's content past a policy that withholds it.
func TestEveryHookEventKindIsClassified(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	file, err := parser.ParseFile(token.NewFileSet(), "../hooks/hookrun/events.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	kinds := 0
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range spec.Names {
			if !strings.HasPrefix(name.Name, "Event") || i >= len(spec.Values) {
				continue
			}
			lit, ok := spec.Values[i].(*ast.BasicLit)
			if !ok {
				continue
			}
			kind, _ := strconv.Unquote(lit.Value)
			kinds++
			if _, content := hookContent[kind]; content == contentFree[kind] {
				t.Errorf("%s (%s): classify it in exactly one of hookContent or contentFree", name.Name, kind)
			}
		}
		return true
	})
	if kinds < 20 {
		t.Fatalf("found %d event kinds in events.go; the parse missed them", kinds)
	}
	// An unclassified kind leaves only under a policy that withholds nothing.
	unknown := spool.Event{Name: "terma.something.new", Attrs: map[string]any{"text": "private"}}
	if _, ok := (Router{}).Outgoing(config.Policy{Mode: config.ModeRepo, IncludeToolContent: true}, "p1", unknown); ok {
		t.Error("an unclassified kind left under a policy withholding prompts")
	}
	if _, ok := (Router{}).Outgoing(config.Policy{Mode: config.ModeRepo, IncludePrompts: true, IncludeToolContent: true}, "p1", unknown); !ok {
		t.Error("an unclassified kind was withheld under a policy withholding nothing")
	}
}
