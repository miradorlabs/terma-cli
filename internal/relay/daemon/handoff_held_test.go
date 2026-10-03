package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay"
)

// What a relay holds when terma stops it reaches its successor exactly once: the old
// relay's last flush of the held store finishes before it releases the lock, and the
// successor reads the store only once it holds the lock.
func TestHeldRecordsSurviveTheHandoff(t *testing.T) {
	if !handoffSupported {
		t.Skip("no socket handoff on this platform")
	}
	dir, token := setUpRelay(t)
	addr := freeAddr(t)
	var successor *relayRun
	service := runConfig(dir, 0, nil)
	service.Addr = addr
	service.SpawnSuccessor = func() error {
		next := runConfig(dir, time.Hour, nil)
		next.Addr, next.Successor = addr, true
		successor = startRun(t, next)
		return nil
	}
	old := startRun(t, service)
	old.await(t, "the service's relay")
	postSessionlessSpans(t, addr, token, 5)
	old.stopAsTerma(t, dir)
	<-old.done
	if successor == nil {
		t.Fatal("the stopping relay started no successor")
	}
	successor.await(t, "the successor")
	successor.cancel() // signalled: it stops for good, holding what it took back
	<-successor.done

	before, after := counters(t, filepath.Join(dir, PrevStatsFile)), counters(t, filepath.Join(dir, StatsFile))
	if before["held_at_exit.traces"] != 5 {
		t.Fatalf("the stopping relay held %d spans at exit, want 5: %v", before["held_at_exit.traces"], before)
	}
	if after["recovered_held.traces"] != 5 || after["held_at_exit.traces"] != 5 {
		t.Fatalf("the successor took back %d spans and held %d at exit, want 5 and 5: %v",
			after["recovered_held.traces"], after["held_at_exit.traces"], after)
	}
}

func postSessionlessSpans(t *testing.T, addr, tokenPath string, n int) {
	t.Helper()
	tok, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	var spans []*tracepb.Span
	for range n {
		spans = append(spans, &tracepb.Span{Name: "fs.read_file", TraceId: []byte("0123456789abcdef")})
	}
	body, err := proto.Marshal(&tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}}}}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/traces", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+string(bytes.TrimSpace(tok)))
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export: %s", resp.Status)
	}
}

func counters(t *testing.T, path string) map[string]int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snap relay.Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	return snap.Counters
}
