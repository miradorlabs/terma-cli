package daemon

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// An exporter the relay accepted just before it stopped is answered, even though its
// request arrives after the stop began: it would never resend a dropped one.
func TestAStoppingRelayAnswersWhatItAccepted(t *testing.T) {
	stateDir, _, _ := setUpRelay(t)
	addr := freeAddr(t)
	c := runConfig(stateDir, time.Hour, nil)
	c.Addr = addr
	accepted := make(chan string, 8)
	hook := func(nc net.Conn) {
		select {
		case accepted <- nc.RemoteAddr().String():
		default:
		}
	}
	onFreshNew.Store(&hook)
	t.Cleanup(func() { onFreshNew.Store(nil) })

	r := startRun(t, c)
	r.await(t, "the relay")
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	// Wait until the relay's server has actually accepted conn (StateNew), not merely queued
	// in the OS TCP backlog.
	want := conn.LocalAddr().String()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case got := <-accepted:
			if got == want {
				goto isAccepted
			}
		case <-deadline:
			t.Fatal("the relay never accepted the dialed connection")
		}
	}
isAccepted:
	r.cancel()
	// Wait until the listener is closed (new dials fail), proving shutdown is underway before
	// we send the request, without burning a fixed sleep out of newRequestWait.
	for stopDeadline := time.Now().Add(5 * time.Second); time.Now().Before(stopDeadline); time.Sleep(5 * time.Millisecond) {
		probe, perr := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if perr != nil {
			break
		}
		_ = probe.Close()
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	req := "POST /v1/metrics HTTP/1.1\r\nHost: relay\r\nAuthorization: Bearer tok\r\n" +
		"Content-Type: application/x-protobuf\r\nContent-Length: 0\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("the accepted export was dropped: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the accepted export = %s", resp.Status)
	}
	<-r.done
}

// StateActive fires in net/http before Server.shuttingDown is checked: fresh must keep
// waiting across StateActive until the wrapped handler is entered (or the conn closes).
func TestFreshHoldsAcrossStateActiveUntilHandlerEntry(t *testing.T) {
	t.Parallel()
	var f fresh
	c1, c2 := net.Pipe()
	defer func() { _ = c1.Close(); _ = c2.Close() }()

	f.track(c1, http.StateNew)
	f.track(c1, http.StateActive)

	done := make(chan struct{})
	go func() {
		f.await(time.Second)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("fresh.await returned on StateActive before the handler ran")
	case <-time.After(30 * time.Millisecond):
	}

	ctx := f.connContext(nil)(context.Background(), c1)
	req := httptest.NewRequest(http.MethodPost, "/v1/metrics", nil).WithContext(ctx)
	f.handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(httptest.NewRecorder(), req)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("fresh.await did not return after handler entry")
	}
}

// A service relay stopped after teardown removed its token is not restarted, however it
// was stopped.
func TestAServiceRelayStoppedWithoutItsTokenStaysStopped(t *testing.T) {
	t.Parallel()
	stateDir, _, token := setUpRelay(t)
	c := runConfig(stateDir, 0, nil)
	r := startRun(t, c)
	r.await(t, "the relay")
	if err := os.Remove(token); err != nil {
		t.Fatal(err)
	}
	r.cancel()
	<-r.done
	if !r.res.SetupGone || r.res.Restart() {
		t.Fatalf("Run = %+v", r.res)
	}
}
