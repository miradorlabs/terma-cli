package cmd

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/spool"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"
)

func TestSpoolRepliesUseCurrentNativeCodexConsent(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	exporter := harness.Exporter{Endpoint: "https://example.invalid", APIKey: policyTestKey, ProjectID: "team", Signals: []harness.Signal{harness.SignalLogs}, IncludePrompts: true}
	if err := (harness.Codex{}).Connect(exporter, false); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{hookrun.EventAssistantMessage, hookrun.EventSessionTitle} {
		if !spoolEventAllowed(config.DefaultPolicy(), "team", spool.Event{Name: name}) {
			t.Fatalf("queued %s ignored native consent without a routing record", name)
		}
	}
	exporter.IncludePrompts = false
	if err := (harness.Codex{}).Connect(exporter, false); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{hookrun.EventAssistantMessage, hookrun.EventSessionTitle} {
		if spoolEventAllowed(config.DefaultPolicy(), "team", spool.Event{Name: name}) {
			t.Fatalf("queued %s ignored native prompt opt-out", name)
		}
	}
}

// CatchAll deliberately still holds its 10-second global cache. The one-second
// resolver cache must nevertheless stop admitting new unclaimed records in repo mode.
func TestRelayAdmissionStopsGlobalCoverageWithStaleCatchAll(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	arrived := make(chan struct{}, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		arrived <- struct{}{}
		fmt.Fprint(w, `{}`)
	}))
	defer upstream.Close()
	cfg, err := loadConfig()
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
		if err := routing.SavePolicy(pol); err != nil {
			t.Fatal(err)
		}
		if err := config.UpdateProfile(cfg.ProfileName, func(p *config.Profile) {
			p.OrganizationID, p.Policy = cfg.OrganizationID, &pol
		}); err != nil {
			t.Fatal(err)
		}
	}
	save()
	cfg.Policy = pol
	if err := keystore.Set("team", policyTestKey, keystore.Hosts{OTLP: upstream.URL}); err != nil {
		t.Fatal(err)
	}
	r := relay.New(relay.Options{Token: "token", Dir: t.TempDir(), Hold: 30 * time.Millisecond, TraceHold: 30 * time.Millisecond,
		CatchAll: relayCatchAll(), Resolve: relayResolver(cfg, nil), PolicyCacheTTL: time.Second})
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
		if counters["dropped.no_session_process.logs"] > 0 {
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
