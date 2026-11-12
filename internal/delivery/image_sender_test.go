package delivery

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// startSender runs a sender for r until the test ends, and returns it once its first pass,
// at start, is done.
func startSender(t *testing.T, r Router, cfg *config.Config) (*ImageSender, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	s := NewImageSender(r, openSpool(t), func() (*config.Config, error) { return cfg, nil })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, "the sender's first pass", func() bool { return s.passes.Load() >= 1 })
	return s, cancel, done
}

// waitFor waits up to 3 s for cond.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The relay's start drains the queue.
func TestTheSenderDrainsAtStart(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	r, cfg, stateDir := imageFixture(t, rec, true)
	img := pixelN(1)
	store(t, stateDir, img)
	queue(t, r.Images, imageEvent(img))
	startSender(t, r, cfg)
	if len(rec.requests()) != 1 || len(queued(t, r.Images)) != 0 {
		t.Fatalf("%d requests after the first pass", len(rec.requests()))
	}
}

// An image queued after the sender's last pass waits for no poll: the next export the
// relay accepts (Nudge) sends it, and a nudge with nothing new runs no pass.
func TestAnExportWakesTheSender(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	r, cfg, stateDir := imageFixture(t, rec, true)
	s, _, _ := startSender(t, r, cfg)
	s.Nudge()
	time.Sleep(50 * time.Millisecond)
	if n := s.passes.Load(); n != 1 {
		t.Fatalf("a nudge with nothing new ran a pass: %d passes", n)
	}
	img := pixelN(1)
	store(t, stateDir, img)
	queue(t, r.Images, imageEvent(img))
	time.Sleep(100 * time.Millisecond)
	if len(rec.requests()) != 0 {
		t.Fatal("the sender found a new image with no wake")
	}
	s.Nudge()
	waitFor(t, "the image", func() bool { return len(rec.requests()) == 1 })
}

// A retry window's end wakes the sender, with no export and no poll.
func TestAWindowsEndWakesTheSender(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	r, cfg, stateDir := imageFixture(t, rec, true)
	img := pixelN(1)
	store(t, stateDir, img)
	queue(t, r.Images, imageEvent(img))
	// A window that ends in about a second.
	r.Images.DestinationFailed("p1", time.Now().Add(-29*time.Second))
	startSender(t, r, cfg)
	if len(rec.requests()) != 0 {
		t.Fatal("the sender sent into an open window")
	}
	waitFor(t, "the window's end", func() bool { return len(rec.requests()) == 1 })
}

// A stopping relay cuts off an upload in flight at once; its record stays queued, with no
// attempt counted, and the sender is no longer busy.
func TestStoppingCutsAnUploadOff(t *testing.T) {
	t.Parallel()
	arrived := make(chan struct{})
	var once sync.Once
	r, cfg, stateDir := imageFixture(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.ReadAll(req.Body) // the server notices a closed connection only once the body is read
		once.Do(func() { close(arrived) })
		<-req.Context().Done()
	}), true)
	img := pixelN(1)
	store(t, stateDir, img)
	queue(t, r.Images, imageEvent(img))
	s := NewImageSender(r, openSpool(t), func() (*config.Config, error) { return cfg, nil })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	<-arrived
	if !s.Busy() {
		t.Fatal("the sender is not busy while it uploads")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the sender did not return promptly once stopped")
	}
	if s.Busy() {
		t.Fatal("a stopped sender is still busy")
	}
	if left := queued(t, r.Images); len(left) != 1 || left[0].Attempts != 0 {
		t.Fatalf("image queue holds %+v", left)
	}
}

// The sender and a manual `--images` flush never send one record twice: the one that takes
// it second finds it settled.
func TestTheSenderAndAManualFlushSendEachImageOnce(t *testing.T) {
	t.Parallel()
	arrived, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	rec := &recorder{}
	r, cfg, stateDir := imageFixture(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		once.Do(func() {
			close(arrived)
			<-release
		})
		rec.ServeHTTP(w, req)
	}), true)
	img := pixelN(1)
	store(t, stateDir, img)
	queue(t, r.Images, imageEvent(img))
	s := NewImageSender(r, openSpool(t), func() (*config.Config, error) { return cfg, nil })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	<-arrived
	// The relay's sender is mid-upload; a manual flush reads the queue, still holding it.
	manual := make(chan ImageResult)
	go func() { manual <- r.FlushImages(t.Context(), openSpool(t), cfg, false) }()
	time.Sleep(200 * time.Millisecond)
	close(release)
	res := <-manual
	waitFor(t, "the sender's pass", func() bool { return s.passes.Load() >= 1 })
	sent := 0
	for _, body := range rec.requests() {
		sent += strings.Count(body, img.ID())
	}
	if sent != 1 || res.Sent != 0 {
		t.Fatalf("the image went %d times (the manual flush sent %d)", sent, res.Sent)
	}
}
