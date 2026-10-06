package daemon

import (
	"bufio"
	"net"
	"net/http"
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
	r := startRun(t, c)
	r.await(t, "the relay")
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	time.Sleep(100 * time.Millisecond) // accepted, its request not yet sent
	r.cancel()
	time.Sleep(200 * time.Millisecond)
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
