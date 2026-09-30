package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/spool"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

const policyTestKey = "ter_srv_0123456789abcdef01234567"

func TestPolicyRefreshFiltersAlreadyQueuedReplies(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("TERMA_POLICY_STUB", "")
	var revision atomic.Int64
	revision.Store(1)
	arrived := make(chan []byte, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/policy" {
			if r.Header.Get("Authorization") != "Bearer developer-policy-token" {
				t.Error("policy did not use developer login")
			}
			fmt.Fprintf(w, `{"policy":{"version":"1.0","terma":{"per_repository":{"members_can_add_repositories":true},"capture":{"exclude_paths":[],"exclude_prompts":%t,"exclude_tool_content":false,"signals":["logs"]}}},"revision":%d,"updated_at":"2026-09-30T12:27:05Z"}`, revision.Load() > 1, revision.Load())
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+policyTestKey {
			t.Error("export did not use team key")
		}
		b, _ := io.ReadAll(r.Body)
		arrived <- b
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	t.Setenv("TERMA_AUTH_URL", srv.URL)
	t.Setenv("TERMA_API_URL", srv.URL)
	t.Setenv("TERMA_OTLP_URL", srv.URL)
	if err := keystore.Set("team", policyTestKey, keystore.Hosts{OTLP: srv.URL}); err != nil {
		t.Fatal(err)
	}
	if err := routing.SaveRecord(routing.Record{ProjectID: "team", Signals: []string{"logs"}, IncludePrompts: true, IncludeToolContent: true, Harnesses: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	seedPolicyLogin(t, srv.URL)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.ProjectID = "team"
	if err := refreshCollectionPolicy(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	s := spoolForTest(t)
	for _, name := range []string{hookrun.EventAssistantMessage, hookrun.EventSessionTitle, "terma.commit"} {
		attrs := map[string]any{hookrun.AttrProjectID: "team"}
		if name != "terma.commit" {
			attrs["text"] = "PRIVATE_CONTENT"
		}
		if err := s.Append(spool.Event{Time: time.Now(), Name: name, Attrs: attrs}); err != nil {
			t.Fatal(err)
		}
	}
	revision.Store(2)
	if err := refreshCollectionPolicy(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	res, err := flushSpool(t.Context(), true, 0)
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

func TestRelayRespectsSignalSelection(t *testing.T) {
	for _, signals := range [][]string{{"metrics"}, {"logs"}, {}} {
		t.Run(fmt.Sprint(signals), func(t *testing.T) {
			t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
			if err := keystore.Set("team", policyTestKey, keystore.Hosts{}); err != nil {
				t.Fatal(err)
			}
			if err := routing.SaveRecord(routing.Record{ProjectID: "team", Signals: signals, IncludePrompts: true, IncludeToolContent: true, Harnesses: []string{"codex"}}); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{Policy: config.DefaultPolicy(), OTLPURL: "http://127.0.0.1:1"}
			r := newTestRelay(relay.Options{Token: "token", Dir: t.TempDir(), Resolve: relayResolver(cfg, nil), Lookup: func(string, time.Time) (claim.Claim, bool) {
				return claim.Claim{ProjectID: "team", Tool: "codex"}, true
			}})
			m := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{Name: "chat", Attributes: []*commonpb.KeyValue{{Key: "session.id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "session"}}}}}}}}}}}
			body, err := proto.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer token")
			req.Header.Set("Content-Type", "application/x-protobuf")
			w := httptest.NewRecorder()
			r.Handler().ServeHTTP(w, req)
			if w.Code != 200 || r.Stats().Snapshot().Counters["dropped.policy_signal.traces"] != 1 {
				t.Fatalf("trace bypassed signal selection: %d %+v", w.Code, r.Stats().Snapshot())
			}
		})
	}
}

// An exporter configured by another repository must not bypass this repository's
// saved harness selection, even when its committed hooks claim a session here.
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
			t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
			arrived := make(chan []byte, 1)
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				body, _ := io.ReadAll(req.Body)
				arrived <- body
				w.WriteHeader(http.StatusOK)
			}))
			defer host.Close()
			if err := keystore.Set("team", policyTestKey, keystore.Hosts{OTLP: host.URL}); err != nil {
				t.Fatal(err)
			}
			rec := routing.Record{ProjectID: "team", Signals: []string{"traces"}, IncludePrompts: true, IncludeToolContent: true, Harnesses: []string{test.harness}}
			if err := routing.SaveRecord(rec); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{Policy: config.DefaultPolicy(), OTLPURL: host.URL}
			r := newTestRelay(relay.Options{Token: "test-token", Dir: t.TempDir(), Resolve: relayResolver(cfg, nil), Lookup: func(string, time.Time) (claim.Claim, bool) {
				return claim.Claim{ProjectID: "team", Tool: test.tool}, true
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
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
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
	if err := keystore.Set("team", policyTestKey, keystore.Hosts{OTLP: host.URL}); err != nil {
		t.Fatal(err)
	}
	rec := routing.Record{ProjectID: "team", Signals: []string{"traces"}, IncludePrompts: true, IncludeToolContent: true, Harnesses: []string{"codex"}}
	if err := routing.SaveRecord(rec); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := &config.Config{Policy: config.DefaultPolicy(), OTLPURL: host.URL}
	r := newTestRelay(relay.Options{Token: "test-token", Dir: dir, Resolve: relayResolver(cfg, nil), Lookup: func(string, time.Time) (claim.Claim, bool) {
		return claim.Claim{ProjectID: "team", Tool: "codex"}, true
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
	// The first delivery is still blocked. Remove Codex while its accepted part
	// remains on disk, then let the retry re-read the saved harness selection.
	rec.Harnesses = []string{"claude"}
	if err := routing.SaveRecord(rec); err != nil {
		t.Fatal(err)
	}
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

func TestRefreshMigratesExportersBeforeRemovingShim(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	sandboxMachine(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("TERMA_RELAY_SERVICE", "0")
	serveRelayForRefresh(t)
	shim := plantLegacyShim(t, "codex")
	rec := routing.Record{ProjectID: "team", Signals: []string{"metrics"}, IncludePrompts: false, IncludeToolContent: false, Harnesses: []string{"codex"}, CLI: true}
	if err := routing.SaveRecord(rec); err != nil {
		t.Fatal(err)
	}
	if _, err := refreshMachine(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(shim); !os.IsNotExist(err) {
		t.Fatalf("shim not removed: %v", err)
	}
	st, err := (harness.Codex{}).Status()
	if err != nil || !claim.Enabled() || !strings.HasPrefix(st.Endpoint, "http://127.0.0.1:") {
		t.Fatalf("replacement exporter missing: %+v %v", st, err)
	}
	after, ok, err := routing.LoadRecord("team")
	if err != nil || !ok || after.IncludePrompts || after.IncludeToolContent || len(after.Signals) != 1 || after.Signals[0] != "metrics" {
		t.Fatalf("upgrade changed consent: %+v %v", after, err)
	}
}

func TestReinstallKeepsSignalChoiceUnlessExplicit(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	sandboxMachine(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("TERMA_RELAY_SERVICE", "0")
	if err := keystore.Set("team", policyTestKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	if err := routing.SaveRecord(routing.Record{ProjectID: "team", Signals: []string{"metrics"}, Harnesses: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ProjectID: "team", OTLPURL: "http://127.0.0.1:1"}
	cmd := newInstallCommand()
	ui := newInstallUI(io.Discard, false)
	for _, explicit := range []bool{false, true} {
		f := installFlags{}
		if explicit {
			if err := cmd.Flags().Set("signals", "none"); err != nil {
				t.Fatal(err)
			}
			f.signals = "none"
		}
		if err := connectHarnessesForRepo(cmd, ui, cfg, []string{"codex"}, f); err != nil {
			t.Fatal(err)
		}
		r, _, err := routing.LoadRecord("team")
		if err != nil {
			t.Fatal(err)
		}
		if !explicit && (len(r.Signals) != 1 || r.Signals[0] != "metrics") || explicit && len(r.Signals) != 0 {
			t.Fatalf("wrong reinstalled signals: %v", r.Signals)
		}
	}
}

func seedPolicyLogin(t *testing.T, authURL string) {
	t.Helper()
	if _, err := auth.SaveCredential(config.DefaultProfile, &auth.Credential{AccessToken: "developer-policy-token", OrganizationID: "org-test", AuthURL: authURL, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateProfile(config.DefaultProfile, func(p *config.Profile) { p.OrganizationID = "org-test" }); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyRefreshOutageRetainsValidatedPolicy(t *testing.T) {
	for _, status := range []int{403, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
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
			cfg := &config.Config{ProfileName: config.DefaultProfile, OrganizationID: "org-test", AuthURL: srv.URL, APIURL: srv.URL, Policy: p}
			if err := saveCollectionPolicy(cfg, &p); err != nil {
				t.Fatal(err)
			}
			got, err := currentTeamPolicy(context.Background(), cfg, "team")
			if err != nil || got.IncludePrompts || got.Revision != 9 {
				t.Fatalf("outage widened capture: %+v %v", got, err)
			}
			cfg.Policy = config.DefaultPolicy()
			if _, err := currentTeamPolicy(context.Background(), cfg, "unknown"); err == nil {
				t.Fatal("unfetched policy defaulted to capture during rejection")
			}
			if keystore.Get("team") != "" {
				t.Fatal("policy fetch minted a server key")
			}
		})
	}
}

func TestPolicyCacheRejectsOlderRevisionAndOrganizationChanges(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	cfg := &config.Config{ProfileName: config.DefaultProfile, ProjectID: "team", AuthURL: "https://auth-dev.terma.ai"}
	p := config.DefaultPolicy()
	p.AuthURL, p.TeamID, p.Revision, p.FetchedAt = cfg.AuthURL, "team", 9, time.Now()
	if err := saveCollectionPolicy(cfg, &p); err != nil {
		t.Fatal(err)
	}
	p.Revision = 8
	if err := saveCollectionPolicy(cfg, &p); err == nil {
		t.Fatal("accepted older revision")
	}
	if err := config.UpdateProfile(cfg.ProfileName, func(profile *config.Profile) { profile.SelectOrganization("other", "Other") }); err != nil {
		t.Fatal(err)
	}
	if err := saveCollectionPolicy(cfg, &p); err == nil {
		t.Fatal("saved stale organization response")
	}
	file, err := config.LoadFile()
	if err != nil || file.Profiles[cfg.ProfileName].Policy != nil {
		t.Fatal("organization change retained a policy")
	}
}

func TestQueuedGlobalHookEventsAreWithheldAfterCoverageChange(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	e := spool.Event{Name: "terma.session.start", Global: true}
	if !spoolEventAllowed(config.Policy{Mode: config.ModeGlobal}, "global-project", e) {
		t.Fatal("global metadata withheld in global mode")
	}
	if spoolEventAllowed(config.DefaultPolicy(), "global-project", e) {
		t.Fatal("queued global event escaped after switch to repository mode")
	}
}

func TestPolicyGlobalDestinationIsTheRequestedTeam(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("TERMA_POLICY_STUB", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer developer-policy-token" || r.URL.Path != "/v1/policy" || r.URL.Query().Get("project_id") != "chosen" {
			t.Error("wrong developer policy request")
		}
		fmt.Fprint(w, `{"policy":{"version":"1.0","terma":{"global":{},"capture":{"exclude_paths":[],"exclude_prompts":true,"exclude_tool_content":false,"signals":["logs"]}}},"revision":1,"updated_at":"2026-09-30T12:27:05Z"}`)
	}))
	defer srv.Close()
	seedPolicyLogin(t, srv.URL)
	cfg := &config.Config{ProfileName: config.DefaultProfile, OrganizationID: "org-test", AuthURL: srv.URL, APIURL: srv.URL, ProjectID: "chosen", Policy: config.Policy{DefaultProjectID: "other"}}
	p, err := fetchPolicy(t.Context(), cfg)
	if err != nil || p.DefaultProjectID != "chosen" || p.TeamID != "chosen" {
		t.Fatalf("policy=%+v error=%v", p, err)
	}
	if keystore.Get("chosen") != "" {
		t.Fatal("reading policy created a telemetry key")
	}
}

func TestPolicyRefreshOtherTeamKeepsSelectedCoverage(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("TERMA_POLICY_STUB", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("project_id") != "other" || r.Header.Get("Authorization") != "Bearer developer-policy-token" {
			t.Error("wrong team policy request")
		}
		fmt.Fprint(w, `{"policy":{"version":"1.0","terma":{"per_repository":{"members_can_add_repositories":true},"capture":{"exclude_paths":[],"exclude_prompts":true,"exclude_tool_content":false,"signals":["logs"]}}},"revision":1,"updated_at":"2026-09-30T12:27:05Z"}`)
	}))
	defer srv.Close()
	seedPolicyLogin(t, srv.URL)
	selected := config.Policy{Mode: config.ModeGlobal, TeamID: "selected", OrganizationID: "org-test", AuthURL: srv.URL, DefaultProjectID: "selected", Revision: 9, FetchedAt: time.Now()}
	cfg := &config.Config{ProfileName: config.DefaultProfile, OrganizationID: "org-test", AuthURL: srv.URL, ProjectID: "selected", Policy: selected}
	if err := saveCollectionPolicy(cfg, &selected); err != nil {
		t.Fatal(err)
	}
	cfg.ProjectID = "other"
	if err := refreshCollectionPolicy(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	file, err := config.LoadFile()
	if err != nil {
		t.Fatal(err)
	}
	p := file.Profiles[config.DefaultProfile].Policy
	if !p.Global() || p.TeamID != "selected" || p.Revision != 9 {
		t.Fatalf("other team changed coverage: %+v", p)
	}
	other, ok, err := routing.LoadPolicy("other")
	if err != nil || !ok || other.Global() || other.IncludePrompts {
		t.Fatalf("other team's capture policy missing: %+v %v", other, err)
	}
}
