package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/spool"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

const policyTestKey = "ter_srv_0123456789abcdef01234567"

func TestPolicyRefreshFiltersAlreadyQueuedReplies(t *testing.T) {
	useConfigDir(t, t.TempDir())
	t.Setenv("TERMA_POLICY_STUB", "")
	var revision atomic.Int64
	revision.Store(1)
	arrived := make(chan []byte, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/policy" {
			if r.Header.Get("Authorization") != "Bearer developer-policy-token" {
				t.Error("policy did not use developer login")
			}
			fmt.Fprintf(w, `{"policy":{"version":"1.0","terma":{"per_repository":{"repositories":["github.com/acme/app"]},"capture":{"exclude_prompts":%t,"exclude_tool_content":false}}},"revision":%d,"updated_at":"2026-09-30T12:27:05Z"}`, revision.Load() > 1, revision.Load())
			return
		}
		if r.Header.Get("Authorization") != "Bearer developer-policy-token" || r.Header.Get(relay.ProjectHeader) != "team" {
			t.Error("export did not use the developer's sign-in, naming the team")
		}
		b, _ := io.ReadAll(r.Body)
		arrived <- b
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	t.Setenv("TERMA_AUTH_URL", srv.URL)
	t.Setenv("TERMA_API_URL", srv.URL)
	t.Setenv("TERMA_OTLP_URL", srv.URL)
	if err := keystore.Set(testApp.dir, "team", policyTestKey, keystore.Hosts{OTLP: srv.URL}); err != nil {
		t.Fatal(err)
	}
	seedPolicyLogin(t, srv.URL)
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	account := auth.AccountKey(srv.URL, "u-1")
	target := routing.Team{OrganizationID: cfg.OrganizationID, AuthURL: cfg.AuthURL, TeamID: "team"}
	pol1, err := testApp.policies().FetchTeam(t.Context(), cfg, account, target)
	if err != nil {
		t.Fatal(err)
	}
	target.Policy = pol1
	st, err := routing.Create(testApp.stateDir, cfg.ProfileName, account, func(s *routing.State) error {
		if _, err := s.Select(target); err != nil {
			return err
		}
		return s.Bind(appRepo.Origin, cfg.OrganizationID, false)
	})
	if err != nil {
		t.Fatal(err)
	}
	enrID := st.Teams[0].EnrollmentID
	s := spoolForTest(t)
	for _, name := range []string{semconv.TermaAssistantMessageEvent, semconv.TermaSessionTitleEvent, "terma.commit"} {
		attrs := map[string]any{hookrun.AttrProjectID: "team"}
		if name != "terma.commit" {
			attrs["text"] = "PRIVATE_CONTENT"
		}
		if err := s.Append(spool.Event{Time: time.Now(), Name: name, Repository: appRepo, Account: account, Profile: cfg.ProfileName, Enrollment: enrID, Attrs: attrs}); err != nil {
			t.Fatal(err)
		}
	}
	revision.Store(2)
	ref, _ := testApp.policies().RefreshTeam(t.Context(), cfg, account, st.Teams[0])
	if _, err := routing.Update(testApp.stateDir, cfg.ProfileName, st.Expect(), func(s *routing.State) error {
		_, err := s.ApplyRefresh(ref)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	res, err := testApp.flushSpool(t.Context(), true, 0)
	if err != nil || res.Err != nil || res.Sent != 1 || res.Withheld != 2 {
		t.Fatalf("flush: %+v, %v", res, err)
	}
	select {
	case body := <-arrived:
		if bytes.Contains(body, []byte("PRIVATE_CONTENT")) {
			t.Fatal("queued replies bypassed new policy")
		}
	case <-time.After(time.Second):
		t.Fatal("metadata was not delivered")
	}
	if n, _, err := s.Pending(); err != nil || n != 0 {
		t.Fatalf("withheld events remain queued: %d %v", n, err)
	}
}

// An exporter an earlier setup left pointing at the relay must not bypass the developer's
// agents, even when a hook claims a session.
func TestRelayRespectsHarnessSelection(t *testing.T) {
	for _, test := range []struct {
		name     string
		harness  string
		tool     string
		selected bool
	}{
		{"unselected codex", "claude", "codex", false},
		{"selected codex", "codex", "codex", true},
		{"claude hook label", "claude", "claude-code", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			useConfigDir(t, t.TempDir())
			arrived := make(chan []byte, 1)
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				body, _ := io.ReadAll(req.Body)
				arrived <- body
				w.WriteHeader(http.StatusOK)
			}))
			defer host.Close()
			if err := keystore.Set(testApp.dir, "team", policyTestKey, keystore.Hosts{OTLP: host.URL}); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{Dir: testApp.dir, StateDir: testApp.stateDir, Policy: config.DefaultPolicy(), OTLPURL: host.URL, Harnesses: []string{test.harness}, ServerKeySignIn: true}
			cfg.Policy.Repositories = []string{appRepo.Origin}
			r := newTestRelay(relay.Options{Token: "test-token", Dir: t.TempDir(), Resolve: testApp.relayDeps().Resolver(cfg), Lookup: func(string, time.Time) (claim.Claim, bool) {
				return claim.Claim{ProjectID: "team", Tool: test.tool, Repository: appRepo, Account: sendas.NoPersonLane}, true
			}})
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() { r.Run(ctx); close(done) }()
			defer func() { cancel(); <-done }()
			span := &tracepb.Span{Name: "chat", Attributes: []*commonpb.KeyValue{
				{Key: "session.id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "session"}}},
				{Key: "gen_ai.prompt", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "PRIVATE_CONTENT"}}},
			}}
			m := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{span}}}}}}

			body, err := proto.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer test-token")
			req.Header.Set("Content-Type", "application/x-protobuf")
			w := httptest.NewRecorder()
			r.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d", w.Code)
			}
			if !test.selected {
				if r.Stats().Snapshot().Counters["dropped.policy_signal.traces"] != 1 {
					t.Fatal("an unselected harness was admitted to the delivery queue")
				}
				return
			}
			select {
			case out := <-arrived:
				if !bytes.Contains(out, []byte("PRIVATE_CONTENT")) {
					t.Fatal("selected harness lost permitted content")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("selected harness was not delivered")
			}
		})
	}
}

func TestQueuedRelayExportsRespectHarnessDeselection(t *testing.T) {
	useConfigDir(t, t.TempDir())
	attempted := make(chan []byte, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var requests atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		body, _ := io.ReadAll(req.Body)
		select {
		case attempted <- body:
		default:
		}
		select {
		case <-release:
		case <-req.Context().Done():
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer host.Close()
	if err := keystore.Set(testApp.dir, "team", policyTestKey, keystore.Hosts{OTLP: host.URL}); err != nil {
		t.Fatal(err)
	}
	choose := func(agents ...string) {
		t.Helper()
		pol := config.DefaultPolicy()
		pol.TeamID, pol.FetchedAt, pol.Repositories = "team", time.Now(), []string{appRepo.Origin}
		if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.Harnesses, p.ServerKeySignIn = agents, true }); err != nil {
			t.Fatal(err)
		}
		selectPolicy(t, pol)
	}
	choose("codex")
	dir := t.TempDir()
	cfg := &config.Config{Dir: testApp.dir, StateDir: testApp.stateDir, ProfileName: config.DefaultProfile, Policy: config.DefaultPolicy(), OTLPURL: host.URL}
	r := newTestRelay(relay.Options{Token: "test-token", Dir: dir, Resolve: testApp.relayDeps().Resolver(cfg), Lookup: func(string, time.Time) (claim.Claim, bool) {
		return claim.Claim{ProjectID: "team", Tool: "codex", Repository: appRepo, Account: sendas.NoPersonLane}, true
	}})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() { unblock(); cancel(); <-done }()
	span := &tracepb.Span{Name: "chat", Attributes: []*commonpb.KeyValue{
		{Key: "session.id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "session"}}},
		{Key: "gen_ai.prompt", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "PRIVATE_CONTENT"}}},
	}}
	body, err := proto.Marshal(&tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{span}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/x-protobuf")
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	select {
	case sent := <-attempted:
		if !bytes.Contains(sent, []byte("PRIVATE_CONTENT")) {
			t.Fatal("selected harness was not admitted with its permitted content")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the selected harness never attempted delivery")
	}
	// Deselect the harness while its accepted part is on disk; the retry must re-read
	// the selection.
	choose("claude")
	unblock()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		entries, err := filepath.Glob(filepath.Join(dir, "team", "codex", "*.pb"))
		if err != nil {
			t.Fatal(err)
		}
		if r.Stats().Snapshot().Counters["dropped.policy_signal_or_content.traces"] == 1 && len(entries) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("deselected harness remained queued: %v %+v", entries, r.Stats().Snapshot())
		}
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("deselected harness retried delivery: %d requests", n)
	}
}

func seedPolicyLogin(t *testing.T, authURL string) {
	t.Helper()
	if err := saveSignIn(testApp.dir, &auth.Credential{AccessToken: "developer-policy-token", SessionID: "s-policy", UserID: "u-1", AuthURL: authURL, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.OrganizationID = "org-test" }); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyRefreshOutageRetainsValidatedPolicy(t *testing.T) {
	for _, status := range []int{403, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			useConfigDir(t, t.TempDir())
			t.Setenv("TERMA_POLICY_STUB", "")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer developer-policy-token" {
					t.Error("wrong policy credential")
				}
				w.WriteHeader(status)
			}))
			defer srv.Close()
			seedPolicyLogin(t, srv.URL)
			p := config.DefaultPolicy()
			p.OrganizationID, p.AuthURL, p.TeamID = "org-test", srv.URL, "team"
			p.Revision, p.IncludePrompts = 9, false
			p.FetchedAt = time.Now().Add(-time.Hour)
			cfg := &config.Config{Dir: testApp.dir, StateDir: testApp.stateDir, ProfileName: config.DefaultProfile, OrganizationID: "org-test", Account: auth.AccountKey(srv.URL, "u-1"), AuthURL: srv.URL, APIURL: srv.URL, Policy: p}
			if err := routing.StorePolicy(cfg, &p); err != nil {
				t.Fatal(err)
			}
			got, err := testApp.policies().Current(context.Background(), cfg, "team")
			if err != nil || got.IncludePrompts || got.Revision != 9 {
				t.Fatalf("outage widened capture: %+v %v", got, err)
			}
			cfg.Policy = config.DefaultPolicy()
			if _, err := testApp.policies().Current(context.Background(), cfg, "unknown"); err == nil {
				t.Fatal("unfetched policy defaulted to capture during rejection")
			}
			if k, _ := keystore.Get(testApp.dir, "team"); k != "" {
				t.Fatal("policy fetch minted a server key")
			}
		})
	}
}

func TestPolicyCacheRejectsOlderRevisionAndOrganizationChanges(t *testing.T) {
	useConfigDir(t, t.TempDir())
	cfg := &config.Config{Dir: testApp.dir, StateDir: testApp.stateDir, ProfileName: config.DefaultProfile, ProjectID: "team", AuthURL: "https://auth-dev.terma.ai"}
	p := config.DefaultPolicy()
	p.AuthURL, p.TeamID, p.Revision, p.FetchedAt = cfg.AuthURL, "team", 9, time.Now()
	if err := routing.StorePolicy(cfg, &p); err != nil {
		t.Fatal(err)
	}
	p.Revision = 8
	if err := routing.StorePolicy(cfg, &p); err == nil {
		t.Fatal("accepted older revision")
	}
	if err := config.UpdateProfile(testApp.dir, cfg.ProfileName, func(profile *config.Profile) { profile.SelectOrganization("other", "Other") }); err != nil {
		t.Fatal(err)
	}
	if err := routing.StorePolicy(cfg, &p); err == nil {
		t.Fatal("saved stale organization response")
	}
	if got, err := config.Load(testApp.dir, testApp.stateDir, config.Overrides{}); err != nil || got.Policy.Validated() {
		t.Fatalf("organization change retained a policy: %+v, %v", got, err)
	}
}

func TestQueuedGlobalHookEventsAreWithheldAfterCoverageChange(t *testing.T) {
	useConfigDir(t, t.TempDir())
	e := spool.Event{Name: "terma.session.start", Global: true}
	if !testApp.delivery().Allowed(config.Policy{Mode: config.ModeGlobal}, "global-project", e) {
		t.Fatal("global metadata withheld in global mode")
	}
	if testApp.delivery().Allowed(config.DefaultPolicy(), "global-project", e) {
		t.Fatal("queued global event escaped after switch to repository mode")
	}
}

func TestPolicyGlobalDestinationIsTheRequestedTeam(t *testing.T) {
	useConfigDir(t, t.TempDir())
	t.Setenv("TERMA_POLICY_STUB", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer developer-policy-token" || r.URL.Path != "/v1/policy" || r.URL.Query().Get("project_id") != "chosen" {
			t.Error("wrong developer policy request")
		}
		fmt.Fprint(w, `{"policy":{"version":"1.0","terma":{"global":{},"capture":{"exclude_prompts":true,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-09-30T12:27:05Z"}`)
	}))
	defer srv.Close()
	seedPolicyLogin(t, srv.URL)
	cfg := &config.Config{Dir: testApp.dir, StateDir: testApp.stateDir, ProfileName: config.DefaultProfile, OrganizationID: "org-test", AuthURL: srv.URL, APIURL: srv.URL, ProjectID: "chosen", Policy: config.Policy{DefaultProjectID: "other"}}
	p, err := testApp.policies().Fetch(t.Context(), cfg)
	if err != nil || p.DefaultProjectID != "chosen" || p.TeamID != "chosen" {
		t.Fatalf("policy=%+v error=%v", p, err)
	}
	if k, _ := keystore.Get(testApp.dir, "chosen"); k != "" {
		t.Fatal("reading policy created a telemetry key")
	}
}

func TestPolicyRefreshOtherTeamKeepsSelectedCoverage(t *testing.T) {
	useConfigDir(t, t.TempDir())
	t.Setenv("TERMA_POLICY_STUB", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("project_id") != "other" || r.Header.Get("Authorization") != "Bearer developer-policy-token" {
			t.Error("wrong team policy request")
		}
		fmt.Fprint(w, `{"policy":{"version":"1.0","terma":{"per_repository":{"repositories":[]},"capture":{"exclude_prompts":true,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-09-30T12:27:05Z"}`)
	}))
	defer srv.Close()
	seedPolicyLogin(t, srv.URL)
	selected := config.Policy{Mode: config.ModeGlobal, TeamID: "selected", OrganizationID: "org-test", AuthURL: srv.URL, DefaultProjectID: "selected", Revision: 9, FetchedAt: time.Now()}
	cfg := &config.Config{Dir: testApp.dir, StateDir: testApp.stateDir, ProfileName: config.DefaultProfile, OrganizationID: "org-test", Account: auth.AccountKey(srv.URL, "u-1"), AuthURL: srv.URL, ProjectID: "selected", Policy: selected}
	if err := routing.StorePolicy(cfg, &selected); err != nil {
		t.Fatal(err)
	}
	cfg.ProjectID = "other"
	if err := testApp.policies().Refresh(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(testApp.dir, testApp.stateDir, config.Overrides{AuthURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if p := loaded.Policy; !p.Global() || p.TeamID != "selected" || p.Revision != 9 {
		t.Fatalf("other team changed coverage: %+v", p)
	}
	if other, ok, err := config.ReadPolicy(testApp.stateDir, "other"); err != nil || !ok || other.Global() || other.IncludePrompts {
		t.Fatalf("other team's capture policy missing: %+v %v", other, err)
	}
}

// The hooks, the relay, delivery and doctor read the team's stored policy, so one that no
// refresh has reached for over a week has expired for every one of them.
func TestEveryReaderSeesTheStoredPolicy(t *testing.T) {
	useConfigDir(t, t.TempDir())
	t.Setenv("TERMA_POLICY_STUB", "")
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.OrganizationID = "org-test"
	pol := listingApp
	pol.TeamID, pol.OrganizationID, pol.AuthURL, pol.Revision, pol.FetchedAt = "team", cfg.OrganizationID, cfg.AuthURL, 1, time.Now()
	if err := routing.StorePolicy(cfg, &pol); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateProfile(testApp.dir, cfg.ProfileName, func(p *config.Profile) {
		p.Harnesses = []string{"codex"}
		p.ServerKeySignIn = true
		p.ServerKeyAuthURL = cfg.AuthURL
	}); err != nil {
		t.Fatal(err)
	}
	pol.FetchedAt = time.Now().Add(-config.MaxPolicyAge - time.Hour)
	selectPolicy(t, pol)
	if err := keystore.Set(testApp.dir, "team", policyTestKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}

	hp := testApp.hookProfile()
	if !hp.Routing.Usable() {
		t.Fatal("the hooks lost usable routing state")
	}
	st, found, err := routing.Load(testApp.stateDir, cfg.ProfileName)
	if err != nil || !found {
		t.Fatalf("routing state load: found=%v err=%v", found, err)
	}
	if got := routing.NewRouter(st).Route(routing.Location{Kind: routing.Repository, Repository: appRepo.Origin}, time.Now()); got.Outcome != routing.Hold || got.Blocked != routing.Expired {
		t.Fatalf("the hooks routed expired team as %+v, want hold/expired", got)
	}
	rec := relay.Record{
		Placed: true,
		Placement: claim.Claim{
			Profile:    cfg.ProfileName,
			Account:    st.Account,
			Tool:       "codex",
			Repository: appRepo,
		},
		Account: st.Account,
		At:      time.Now(),
	}
	if got := testApp.relayDeps().Teams(cfg).Route(rec); got.Outcome == relay.Send {
		t.Fatalf("the relay granted %+v", got)
	}
	stored, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if testApp.delivery().Allowed(stored.Policy, "team", spool.Event{Repository: appRepo, Name: semconv.TermaSessionStartEvent}) {
		t.Fatal("delivery sent under an expired policy")
	}
	if rows := doctor.Context(doctor.Env{Config: stored, RepoErr: errors.New("outside git")}); !slices.ContainsFunc(rows, func(r doctor.Row) bool { return r.Label == "Capture" }) {
		t.Fatalf("doctor shows capture on: %v", rows)
	}
}
