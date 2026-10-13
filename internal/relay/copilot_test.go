package relay

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// testdata/copilot_cli_traces.json is one traces export as Copilot CLI 1.0.96 sent it
// (OTLP/JSON, content capture on), with what was said, its ids and its paths replaced: a
// turn of four requests, two apply_patch calls and a shell command that failed. Every span
// names the session its hooks claim in gen_ai.conversation.id.
const copilotConversation = "00000001-0000-4000-8000-000000000001"

// copilotRelay is a relay under pol with the Copilot session claimed or not, the export
// posted as the CLI and VS Code post it: JSON, to an endpoint whose path leads with the
// relay's token, and with no Authorization header.
func copilotRelay(t *testing.T, pol Policy, claimed bool) (*Relay, *upstream, *fixture) {
	t.Helper()
	u := newUpstream(t)
	f := newFixture()
	if claimed {
		f.claims[copilotConversation] = claim.Claim{ProjectID: "p1", Tool: "copilot"}
	}
	pol.Endpoint, pol.Key = u.srv.URL, "key-p1"
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
		Resolve: func(claim.Claim) (Policy, error) { return pol, nil }})
	runRelay(t, r)
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	body, err := os.ReadFile(filepath.Join("testdata", "copilot_cli_traces.json"))
	if err != nil {
		t.Fatal(err)
	}
	if code := post(t, srv, "/"+token+"/v1/traces", body, "application/json", "", false); code != http.StatusOK {
		t.Fatalf("the relay answered %d to an export by its path token", code)
	}
	return r, u, f
}

func (u *upstream) sent() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	var b strings.Builder
	for _, req := range u.requests {
		b.Write(req.body)
	}
	return b.String()
}

// A claimed Copilot session leaves whole under a policy that collects content.
func TestRelayForwardsAClaimedCopilotSession(t *testing.T) {
	t.Parallel()
	r, u, _ := copilotRelay(t, Policy{IncludePrompts: true, IncludeToolContent: true}, true)
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.traces"] == 8 })
	sent := u.sent()
	for _, want := range []string{copilotConversation, "PROMPT-TEXT", "REPLY-TEXT", "*** Begin Patch", "env | grep -c OTEL_EXPORTER", "gpt-6-luna"} {
		if !strings.Contains(sent, want) {
			t.Errorf("the forwarded session lacks %q", want)
		}
	}
}

// With content withheld, what was said and what its tools were given and returned stay
// here; the session, its models, token counts, tool names and a command's exit status leave.
func TestRelayWithholdsCopilotContent(t *testing.T) {
	t.Parallel()
	r, u, _ := copilotRelay(t, Policy{}, true)
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.traces"] == 8 })
	sent := u.sent()
	for _, content := range []string{"PROMPT-TEXT", "REPLY-TEXT", "SYSTEM-INSTRUCTIONS", "TOOL-DEFINITION", "TOOL-DESCRIPTION",
		"Begin Patch", "a.txt", "grep -c", "completed with exit code"} {
		if strings.Contains(sent, content) {
			t.Errorf("content left the machine: %q", content)
		}
	}
	// Every key the CLI sent is one its capture rules name: none was withheld unclassified.
	for name := range r.Stats().Snapshot().Counters {
		if strings.HasPrefix(name, "unclassified.") {
			t.Errorf("the relay withheld a key no rule names: %s", name)
		}
	}
	for _, kept := range []string{copilotConversation, "gen_ai.usage.input_tokens", "gen_ai.usage.cache_read.input_tokens", "gpt-6-luna",
		"apply_patch", "gen_ai.tool.call.id", "github.copilot.interaction_id", "github.copilot.nano_aiu", "process.exit.code", "invoke_agent"} {
		if !strings.Contains(sent, kept) {
			t.Errorf("the forwarded session lacks %q", kept)
		}
	}
}

// A Copilot session no hook claimed is held, then dropped: none of it leaves.
func TestRelayDropsAnUnclaimedCopilotSession(t *testing.T) {
	t.Parallel()
	r, u, f := copilotRelay(t, Policy{IncludePrompts: true, IncludeToolContent: true}, false)
	f.mu.Lock()
	f.now = f.now.Add(DefaultTraceHold + time.Minute)
	f.mu.Unlock()
	waitForCounters(t, r, func(c map[string]int) bool { return c["dropped.unclaimed_expired.traces"] == 8 })
	if sent := u.sent(); sent != "" {
		t.Fatalf("an unclaimed session left the machine: %d bytes", len(sent))
	}
}
