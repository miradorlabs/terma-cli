package daemon

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// freeAddr is a loopback address nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// exporter sends empty metric exports to addr, a new connection each, until stopped,
// counting what was answered and what failed: an agent never resends a failed export.
type exporter struct {
	ok, failed atomic.Int64
	firstErr   atomic.Value
	stop       chan struct{}
	done       sync.WaitGroup
}

func export(t *testing.T, addr string) *exporter {
	t.Helper()
	e := &exporter{stop: make(chan struct{})}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	e.done.Go(func() {
		for {
			select {
			case <-e.stop:
				return
			default:
			}
			req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/metrics", bytes.NewReader(nil))
			req.Header.Set("Authorization", "Bearer tok")
			req.Header.Set("Content-Type", "application/x-protobuf")
			resp, err := client.Do(req)
			if err == nil && resp.StatusCode != http.StatusOK {
				err = &net.AddrError{Err: resp.Status, Addr: addr}
			}
			if resp != nil {
				_ = resp.Body.Close()
			}
			if err != nil {
				e.failed.Add(1)
				e.firstErr.CompareAndSwap(nil, err)
			} else {
				e.ok.Add(1)
			}
			time.Sleep(2 * time.Millisecond)
		}
	})
	return e
}

func (e *exporter) finish(t *testing.T) {
	t.Helper()
	close(e.stop)
	e.done.Wait()
	if n := e.failed.Load(); n > 0 {
		t.Fatalf("%d of %d exports failed across the handoff; first: %v", n, n+e.ok.Load(), e.firstErr.Load())
	}
	if e.ok.Load() == 0 {
		t.Fatal("no export was answered")
	}
}

type relayRun struct {
	cancel context.CancelFunc
	up     chan struct{}
	// done is closed once Run returned res.
	done chan struct{}
	res  Result
}

func startRun(t *testing.T, c Config) *relayRun {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	r := &relayRun{cancel: cancel, up: make(chan struct{}), done: make(chan struct{})}
	c.Listening = func(net.Addr, time.Duration) { close(r.up) }
	go func() {
		defer close(r.done)
		var err error
		if r.res, err = Run(ctx, c); err != nil {
			t.Error(err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(20 * time.Second):
		}
	})
	return r
}

func (r *relayRun) await(t *testing.T, what string) {
	t.Helper()
	select {
	case <-r.up:
	case <-time.After(15 * time.Second):
		t.Fatalf("%s never listened", what)
	}
}

// The service's relay, stopped while an agent exports (launchctl bootout, an update's
// restart), hands its socket to the relay it starts: no export in between is refused.
func TestAStoppedRelayHandsItsSocketToItsSuccessor(t *testing.T) {
	if !handoffSupported {
		t.Skip("no socket handoff on this platform")
	}
	dir, _ := setUpRelay(t)
	addr := freeAddr(t)
	var successor *relayRun
	service := runConfig(dir, 0, nil)
	service.Addr = addr
	service.SpawnSuccessor = func() error {
		c := runConfig(dir, time.Hour, nil)
		c.Addr, c.Successor = addr, true
		successor = startRun(t, c)
		return nil
	}
	old := startRun(t, service)
	old.await(t, "the service's relay")
	e := export(t, addr)
	time.Sleep(200 * time.Millisecond)
	old.cancel()
	<-old.done
	if successor == nil {
		t.Fatal("the stopping relay started no successor")
	}
	successor.await(t, "the successor")
	time.Sleep(200 * time.Millisecond)
	e.finish(t)
}

// An on-demand relay steps aside for the service's relay waiting behind it, and hands it
// the socket, so the service's relay is the one that runs.
func TestAnOnDemandRelayHandsOverToTheWaitingService(t *testing.T) {
	if !handoffSupported {
		t.Skip("no socket handoff on this platform")
	}
	dir, _ := setUpRelay(t)
	addr := freeAddr(t)
	onDemand := runConfig(dir, time.Hour, nil)
	onDemand.Addr = addr
	spawned := false
	onDemand.SpawnSuccessor = func() error { spawned = true; return nil }
	old := startRun(t, onDemand)
	old.await(t, "the on-demand relay")
	e := export(t, addr)
	service := runConfig(dir, 0, nil)
	service.Addr = addr
	svc := startRun(t, service)
	select {
	case <-old.done:
		if old.res.Restart() {
			t.Errorf("the on-demand relay asks to be restarted: %+v", old.res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the on-demand relay never stepped aside")
	}
	svc.await(t, "the service's relay")
	time.Sleep(200 * time.Millisecond)
	e.finish(t)
	if spawned {
		t.Error("a successor was started though the service's relay was waiting")
	}
}

// A successor for another address listens there, and the old socket closes with its relay.
func TestASuccessorOnAnotherAddressListensAfresh(t *testing.T) {
	if !handoffSupported {
		t.Skip("no socket handoff on this platform")
	}
	dir, _ := setUpRelay(t)
	from, to := freeAddr(t), freeAddr(t)
	var successor *relayRun
	c := runConfig(dir, 0, nil)
	c.Addr = from
	c.SpawnSuccessor = func() error {
		next := runConfig(dir, time.Hour, nil)
		next.Addr, next.Successor = to, true
		successor = startRun(t, next)
		return nil
	}
	old := startRun(t, c)
	old.await(t, "the relay")
	old.cancel()
	<-old.done
	successor.await(t, "the successor")
	if conn, err := net.DialTimeout("tcp", to, time.Second); err != nil {
		t.Fatalf("the successor does not listen on its own address: %v", err)
	} else {
		_ = conn.Close()
	}
	if conn, err := net.DialTimeout("tcp", from, 200*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("the old address still accepts connections")
	}
}
