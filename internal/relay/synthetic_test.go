package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
)

// synthetic is an agent the engine has never heard of, known only by its declarations.
type synthetic struct{}

func (synthetic) Correlation() shape.Correlation {
	return shape.Correlation{SessionKeys: []shape.SessionKey{{Attr: "synthetic.session", Rank: 5}}}
}

func (synthetic) CaptureRules() shape.CaptureRules {
	return shape.CaptureRules{PromptFields: []string{"synthetic.prompt"}, ToolContentFields: []string{"synthetic.args"}, SafeKeys: []string{"synthetic.turn"}}
}

// The engine places, redacts and passes records by what an agent declares and nothing
// else: a session key no agent declares names no session, declared content is withheld
// under a policy that withholds it, a declared safe key passes, and an undeclared key is
// dropped and counted.
func TestRelayFollowsOnlyWhatAgentsDeclare(t *testing.T) {
	u := newUpstream(t)
	f := &fixture{now: time.Unix(1_800_000_000, 0), claims: map[string]claim.Claim{"S1": {ProjectID: "p2"}}}
	r := New(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
		Correlators: []shape.Correlator{synthetic{}}, Capturers: []shape.Capturer{synthetic{}},
		Resolve: func(c claim.Claim) (Policy, error) {
			if c.ProjectID != "p2" {
				return Policy{}, ErrNoKey
			}
			return Policy{Endpoint: u.srv.URL, Key: "key-p2", RequireClaim: true}, nil
		}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)

	logs := &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{
		{Attributes: []*commonpb.KeyValue{kv("event.name", "turn"), kv("synthetic.session", "S1"), kv("synthetic.prompt", "secret"),
			kv("synthetic.args", `{"path":"x"}`), kv("synthetic.turn", "t1"), kv("synthetic.unknown", "who knows")}},
		// Another agent's session key: unknown here, so the record names no session.
		{Attributes: []*commonpb.KeyValue{kv("event.name", "turn"), kv("session.id", "S1")}},
	}}}}}}
	body, err := proto.Marshal(logs)
	if err != nil {
		t.Fatal(err)
	}
	if code := post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false); code != http.StatusOK {
		t.Fatalf("export = %d", code)
	}
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 1 })
	got, _ := u.logs(t)
	recs := got["Bearer key-p2"]
	if len(recs) != 1 {
		t.Fatalf("forwarded %v", got)
	}
	a := recs[0].Attributes
	if v := attr(a, "synthetic.prompt"); v != "" && v != defaultMarker {
		t.Errorf("a declared prompt leaked: %q", v)
	}
	if attr(a, "synthetic.args") != "" || attr(a, "synthetic.unknown") != "" {
		t.Errorf("tool content or an undeclared key leaked: %v", a)
	}
	if attr(a, "synthetic.turn") != "t1" {
		t.Errorf("a declared safe key was dropped: %v", a)
	}
	c := r.Stats().Snapshot().Counters
	if c["dropped.no_session_id.logs"] != 1 || c["unclassified.synthetic.unknown"] == 0 {
		t.Fatalf("stats = %v", c)
	}
}
