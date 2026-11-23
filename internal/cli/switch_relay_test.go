package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// exportTo sends the relay one span of session, as an agent's exporter does.
func exportTo(t *testing.T, r *relay.Relay, session string) {
	t.Helper()
	str := func(s string) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
	}
	span := &tracepb.Span{Name: "chat", Attributes: []*commonpb.KeyValue{{Key: "session.id", Value: str(session)}}}
	body, err := proto.Marshal(&tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{span}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer relay-token")
	req.Header.Set("Content-Type", "application/x-protobuf")
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("export: status %d", w.Code)
	}
}

// A relay that resolved A's team under A's sign-in never sends B's part there: a switch to B
// within the policy cache's moment finds the policy resolved for A's lane, not B's.
func TestARelayPolicyCachedUnderOneSignInServesNoOther(t *testing.T) {
	f, ingest := pairingSandbox(t)
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.Harnesses = []string{"codex"} }); err != nil {
		t.Fatal(err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	r := newTestRelay(relay.Options{Token: "relay-token", Dir: t.TempDir(), Resolve: testApp.relayDeps().Resolver(cfg),
		SignIn: testApp.signInSource(cfg), PolicyCacheTTL: time.Minute, ClaimCacheTTL: time.Minute,
		Lookup: func(string, time.Time) (claim.Claim, bool) {
			return claim.Claim{ProjectID: acmeWeb, Tool: "codex", Repository: appRepo, Account: cfg.Account}, true
		}})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	tokenA, tokenB := "Bearer "+fakeUserToken("u-1"), "Bearer "+fakeUserToken("u-2")
	exportTo(t, r, "session-a")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if sent := ingest.take(); len(sent) > 0 {
			if sent[0].auth != tokenA || sent[0].project != acmeWeb {
				t.Fatalf("A's part went with %q to %q", sent[0].auth, sent[0].project)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("A's part never reached A's team: the pass proves nothing")
		}
	}

	if err := testApp.storeLogin(context.Background(), cfg, storedSessionOf(f, "u-2")); err != nil {
		t.Fatal(err)
	}
	exportTo(t, r, "session-b")
	time.Sleep(1500 * time.Millisecond)
	for _, s := range ingest.take() {
		if s.auth == tokenB || s.project == acmeWeb {
			t.Fatalf("after the switch a part went with %q to team %q", s.auth, s.project)
		}
	}
}

// A session A's hook claimed for A's team is not resolved by a relay restarted on B's
// sign-in, though B chose the same organization and team: the team in the claim was A's
// choice. B's own claim of it is.
func TestAClaimMadeUnderOneSignInResolvesForNoOther(t *testing.T) {
	f, _ := pairingSandbox(t)
	underA, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !claim.Write(testApp.stateDir, "session", claim.Claim{ProjectID: acmeWeb, Repository: appRepo, Account: underA.Account}, time.Now()) {
		t.Fatal("could not claim the session")
	}
	resolveNow := func(cfg *config.Config) error {
		t.Helper()
		c, ok := claim.Read(testApp.stateDir, "session", time.Now())
		if !ok {
			t.Fatal("the claim is gone")
		}
		if c, ok = c.At(0, time.Now()); !ok {
			t.Fatal("the claim covers nothing")
		}
		_, err := testApp.relayDeps().Resolver(cfg)(c)
		return err
	}
	if err := resolveNow(underA); err != nil {
		t.Fatalf("under A's own sign-in: %v", err)
	}
	// B signs in and keeps Acme and acmeWeb: the profile is B's, its team the same.
	if err := saveSignIn(testApp.dir, storedSessionOf(f, "u-2")); err != nil {
		t.Fatal(err)
	}
	underB, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if underB.Team != acmeWeb || underB.OrganizationID != orgA().ID {
		t.Fatalf("B's profile: organization %q team %q", underB.OrganizationID, underB.Team)
	}
	if err := resolveNow(underB); err == nil {
		t.Fatal("a relay on B's sign-in resolved the session A claimed")
	}
	claim.Write(testApp.stateDir, "session", claim.Claim{ProjectID: acmeWeb, Repository: appRepo, Account: underB.Account}, time.Now())
	if err := resolveNow(underB); err != nil {
		t.Fatalf("B's own claim: %v", err)
	}
}
