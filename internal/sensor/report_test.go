package sensor

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay"
)

type fakeBackend struct{ live []Flow }

func (f *fakeBackend) Run(context.Context, Sink) error { return nil }
func (f *fakeBackend) Roots([]string) error            { return nil }
func (f *fakeBackend) Watch(int) error                 { return nil }
func (f *fakeBackend) WatchCgroup(uint64, int) error   { return nil }
func (f *fakeBackend) Lend(int) error                  { return nil }
func (f *fakeBackend) Unlend(int) error                { return nil }
func (f *fakeBackend) Live() []Flow                    { return f.live }
func (f *fakeBackend) LiveFiles() []FileOp             { return nil }
func (f *fakeBackend) Enforce(Resolved) error          { return nil }

// The sensor sends what moved since the relay last placed a session's rollup: nothing
// twice, and a handed-back root's counts again with the next window.
func TestReportSendsEachByteOnce(t *testing.T) {
	var (
		mu       sync.Mutex
		got      [][]relay.SensorRollup
		unplaced = true
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rs []relay.SensorRollup
		_ = json.NewDecoder(r.Body).Decode(&rs)
		mu.Lock()
		got = append(got, rs)
		reply := relay.SensorReply{}
		for _, ro := range rs {
			if unplaced {
				reply.Unplaced = append(reply.Unplaced, ro.Root)
			} else {
				reply.Placed = append(reply.Placed, ro.Root)
			}
		}
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(reply)
	}))
	defer srv.Close()

	b := &fakeBackend{}
	st := NewState(b, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_ = st.Adopt(Proc{PID: 10, Exe: "agent", Start: time.Now()})
	remote := netip.MustParseAddrPort("1.2.3.4:443")
	st.FlowClosed(Flow{Root: 10, Remote: remote, TX: 100, RX: 1000, Closed: true})
	b.live = []Flow{{Root: 10, Remote: remote, TX: 5, RX: 50}}

	old := ReportEvery
	ReportEvery = 30 * time.Millisecond
	defer func() { ReportEvery = old }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Report(ctx, st, srv.URL, "t", slog.New(slog.NewTextHandler(io.Discard, nil))); close(done) }()

	wait := func(n int) {
		t.Helper()
		for i := 0; i < 200; i++ {
			mu.Lock()
			l := len(got)
			mu.Unlock()
			if l >= n {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("the relay got %d posts, want %d", len(got), n)
	}
	wait(1) // handed back
	mu.Lock()
	unplaced = false
	mu.Unlock()
	wait(2) // the same counts again, placed this time
	time.Sleep(4 * ReportEvery)
	b.live = []Flow{{Root: 10, Remote: remote, TX: 7, RX: 80}}
	wait(3)
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	sent := func(i int) (int64, int64) { n := got[i][0].Network[0]; return n.Sent, n.Received }
	if tx, rx := sent(0); tx != 105 || rx != 1050 {
		t.Errorf("first window sent %d/%d, want 105/1050", tx, rx)
	}
	if tx, rx := sent(1); tx != 105 || rx != 1050 {
		t.Errorf("after a hand-back the window sent %d/%d, want the same 105/1050 again", tx, rx)
	}
	if len(got) != 3 {
		t.Fatalf("the relay got %d posts, want 3: a window where nothing moved sends nothing", len(got))
	}
	if tx, rx := sent(2); tx != 2 || rx != 30 {
		t.Errorf("the next window sent %d/%d, want only what moved since: 2/30", tx, rx)
	}
}
