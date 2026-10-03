package relay

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// A claimed session whose repository the team policy no longer lists sends nothing more.
func TestAnUnadmittedClaimDropsItsRecords(t *testing.T) {
	u := newUpstream(t)
	r := newRelay(Options{Dir: t.TempDir(), Token: token,
		Lookup: func(string, time.Time) (claim.Claim, bool) { return claim.Claim{ProjectID: "p1"}, true },
		Resolve: func(claim.Claim) (Policy, error) {
			return Policy{Endpoint: u.srv.URL, Key: "key-p1", Unadmitted: true, IncludePrompts: true, IncludeToolContent: true}, nil
		}})
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	body, err := proto.Marshal(logsOf("session", 2))
	if err != nil {
		t.Fatal(err)
	}
	if code := post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false); code != http.StatusOK {
		t.Fatalf("export = %d", code)
	}
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["dropped.policy_repository.logs"] == 2 })
	if got, _ := u.logs(t); len(got) != 0 {
		t.Fatalf("forwarded %v", got)
	}
}
