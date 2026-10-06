package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// listingApp is a repository-mode policy, content on, listing appRepo.
var listingApp = config.Policy{Mode: config.ModeRepo, Repositories: []string{appRepo.Origin}, IncludePrompts: true, IncludeToolContent: true}

// A queued reply or title leaves under the consent setup gives now: Codex among the
// developer's agents, through the relay.
func TestSpoolRepliesUseCurrentCodexConsent(t *testing.T) {
	useConfigDir(t, t.TempDir())
	reply := spool.Event{Repository: appRepo, Name: semconv.TermaAssistantMessageEvent, Attrs: map[string]any{semconv.GenAIMainAgentNameKey: "codex"}}
	if testApp.delivery().Allowed(listingApp, "team", reply) {
		t.Fatal("a reply left for a developer who did not choose Codex")
	}
	hookruntest.RelayOn(t, testApp.stateDir)
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.Harnesses = []string{"codex"} }); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{semconv.TermaAssistantMessageEvent, semconv.TermaSessionTitleEvent} {
		if !testApp.delivery().Allowed(listingApp, "team", spool.Event{Repository: appRepo, Name: name, Attrs: map[string]any{semconv.GenAIMainAgentNameKey: "codex"}}) {
			t.Fatalf("queued %s ignored the developer's consent", name)
		}
	}
	if testApp.delivery().Allowed(listingApp, "team", spool.Event{Repository: appRepo, Name: semconv.TermaAssistantMessageEvent}) {
		t.Fatal("a reply no agent's label vouches for was delivered")
	}
	// The team's policy, not the agent's config, decides whether prompts may leave.
	withheld := listingApp
	withheld.IncludePrompts = false
	for _, name := range []string{semconv.TermaAssistantMessageEvent, semconv.TermaSessionTitleEvent} {
		if testApp.delivery().Allowed(withheld, "team", spool.Event{Repository: appRepo, Name: name, Attrs: map[string]any{semconv.GenAIMainAgentNameKey: "codex"}}) {
			t.Fatalf("queued %s ignored the team's prompts-off", name)
		}
	}
}

// A queued Claude title is labelled claude-code, not the agent's name: delivery still finds
// the consent Claude gives.
func TestSpoolTitlesUseCurrentClaudeConsent(t *testing.T) {
	useConfigDir(t, t.TempDir())
	title := spool.Event{Repository: appRepo, Name: semconv.TermaSessionTitleEvent, Attrs: map[string]any{semconv.GenAIMainAgentNameKey: "claude-code"}}
	hookruntest.RelayOn(t, testApp.stateDir)
	if testApp.delivery().Allowed(listingApp, "team", title) {
		t.Fatal("a title left for a developer who did not choose Claude Code")
	}
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.Harnesses = []string{"claude"} }); err != nil {
		t.Fatal(err)
	}
	if !testApp.delivery().Allowed(listingApp, "team", title) {
		t.Fatal("a queued Claude title ignored the developer's consent")
	}
}

// CatchAll and the resolver each cache the policy for a second; once the team leaves global
// mode, neither may admit an unclaimed record: it is refused at ingress, or withheld.
func TestRelayAdmissionStopsGlobalCoverageWithStaleCatchAll(t *testing.T) {
	useConfigDir(t, t.TempDir())
	arrived := make(chan struct{}, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		arrived <- struct{}{}
		fmt.Fprint(w, `{}`)
	}))
	defer upstream.Close()
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.OrganizationID = "org-test"
	pol := config.DefaultPolicy()
	pol.Mode, pol.TeamID, pol.DefaultProjectID = config.ModeGlobal, "team", "team"
	pol.OrganizationID, pol.AuthURL = cfg.OrganizationID, cfg.AuthURL
	pol.Revision, pol.FetchedAt = 1, time.Now()
	save := func() {
		t.Helper()
		if err := config.UpdateProfile(testApp.dir, cfg.ProfileName, func(p *config.Profile) { p.OrganizationID = cfg.OrganizationID }); err != nil {
			t.Fatal(err)
		}
		selectPolicy(t, pol)
	}
	save()
	cfg.Policy = pol
	if err := keystore.Set(testApp.dir, "team", policyTestKey, keystore.Hosts{OTLP: upstream.URL}); err != nil {
		t.Fatal(err)
	}
	r := newTestRelay(relay.Options{Token: "token", Dir: t.TempDir(), Hold: 30 * time.Millisecond, TraceHold: 30 * time.Millisecond,
		CatchAll: testApp.relayDeps().CatchAll(), Resolve: testApp.relayDeps().Resolver(cfg, nil), PolicyCacheTTL: time.Second})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	send := func() {
		t.Helper()
		body, err := proto.Marshal(&logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{TimeUnixNano: uint64(time.Now().UnixNano())}}}}}}})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/x-protobuf")
		req.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		r.Handler().ServeHTTP(response, req)
		if response.Code != http.StatusOK {
			t.Fatalf("ingress: %d %s", response.Code, response.Body)
		}
	}
	send()
	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("global record was not forwarded")
	}
	pol.Mode, pol.Revision, pol.FetchedAt = config.ModeRepo, 2, time.Now()
	save()
	time.Sleep(1100 * time.Millisecond)
	send()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		counters := r.Stats().Snapshot().Counters
		if counters["dropped.no_session_process.logs"]+counters["dropped.no_session_id.logs"] > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fresh repo-mode record was not withheld: %v", counters)
		}
	}
	select {
	case <-arrived:
		t.Fatal("new unclaimed record bypassed repository coverage through stale catch-all")
	default:
	}
}
