package relay

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
)

// The relay asks for the sender by the connection's client end, then its own: two ends of
// one type, which a swap would still compile.
func TestTheSenderIsAskedForByBothEnds(t *testing.T) {
	t.Parallel()
	var asked atomic.Value
	f := newFixture()
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
		PeerPID: func(client, server netip.AddrPort) (int, bool) {
			asked.Store([2]netip.AddrPort{client, server})
			return 0, false
		}})
	srv, _ := startRelay(t, r)
	var client atomic.Value
	transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err == nil {
			client.Store(c.LocalAddr().(*net.TCPAddr).AddrPort())
		}
		return c, err
	}}
	defer transport.CloseIdleConnections()
	body, _ := proto.Marshal(mixedLogs())
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/logs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	want := [2]netip.AddrPort{client.Load().(netip.AddrPort), srv.Listener.Addr().(*net.TCPAddr).AddrPort()}
	if got, _ := asked.Load().([2]netip.AddrPort); got != want {
		t.Errorf("PeerPID asked with %v, want the client's end then the relay's, %v", got, want)
	}
}
