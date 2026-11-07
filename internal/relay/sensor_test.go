package relay

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// A sensor rollup goes with its root's session, under that session's key; a root that has
// named no session yet is handed back, and nothing of it leaves.
func TestSensorRollupsGoWhereTheirRootsTelemetryWent(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newFixture()
	r, srv := f.relay(t, u, allPolicies(u))
	r.learnProcess(4242, "A") // the agent's own export named session A
	end := f.clock()
	body, _ := json.Marshal([]SensorRollup{
		{Root: 4242, End: end, Window: 60, FilesWrote: 2048, Processes: 3,
			Network: []SensorTraffic{{Host: "api.anthropic.com", Sent: 900, Received: 12000, Connections: 2}},
			Blocks:  []SensorBlock{{Kind: "connect", Rule: 7, Enforced: true, At: end}}},
		{Root: 99, End: end, Window: 60, Processes: 1},
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sensor", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var reply SensorReply
	_ = json.NewDecoder(resp.Body).Decode(&reply)
	_ = resp.Body.Close()
	if len(reply.Placed) != 1 || reply.Placed[0] != 4242 || len(reply.Unplaced) != 1 || reply.Unplaced[0] != 99 {
		t.Fatalf("reply %+v, want 4242 placed and 99 handed back", reply)
	}
	var recs map[string]string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		byAuth, _ := u.logs(t)
		recs = map[string]string{}
		for _, rec := range byAuth["Bearer key-p1"] {
			recs[rec.EventName] = attr(rec.Attributes, semconv.SessionIDKey)
		}
		if len(recs) == 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, ev := range []string{semconv.TermaSensorNetworkEvent, semconv.TermaSensorFilesEvent, semconv.TermaSensorBlockEvent} {
		if recs[ev] != "A" {
			t.Errorf("%s reached p1 with session %q, want A; got %v", ev, recs[ev], recs)
		}
	}
}
