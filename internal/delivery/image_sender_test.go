package delivery

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
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
	if !s.uploading() || s.TryHalt() {
		t.Fatal("the sender is not uploading, or halts, mid-upload")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the sender did not return promptly once stopped")
	}
	if s.uploading() {
		t.Fatal("a stopped sender is still uploading")
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

// An image queued while the sender uploads another is new to it: the next export sends it.
func TestAnImageQueuedMidUploadGoesAtTheNextExport(t *testing.T) {
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
	first, second := pixelN(1), pixelN(2)
	store(t, stateDir, first, second)
	queue(t, r.Images, imageEvent(first))
	s := NewImageSender(r, openSpool(t), func() (*config.Config, error) { return cfg, nil })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	<-arrived
	queue(t, r.Images, imageEvent(second)) // a hook, while the first image is on its way
	close(release)
	waitFor(t, "the first pass", func() bool { return s.passes.Load() >= 1 })
	s.Nudge()
	waitFor(t, "the second image", func() bool {
		for _, body := range rec.requests() {
			if strings.Contains(body, second.ID()) {
				return true
			}
		}
		return false
	})
}

// Images held for a key have a deadline of their own: once the key is there, they go with
// no new image and no export.
func TestHeldImagesAreLookedAtAgain(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	r, cfg, stateDir := imageFixture(t, rec, true)
	img := pixelN(1)
	store(t, stateDir, img)
	e := imageEvent(img)
	e.Attrs[hookrun.AttrProjectID] = "keyless"
	queue(t, r.Images, e)
	s := NewImageSender(r, openSpool(t), func() (*config.Config, error) { return cfg, nil })
	s.heldMin = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, "the first pass", func() bool { return s.passes.Load() >= 1 })
	if len(rec.requests()) != 0 {
		t.Fatal("an image with no key went")
	}
	if err := keystore.Set(r.ConfigDir, "keyless", testKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the held image", func() bool { return len(rec.requests()) == 1 })
}

// The wait for held images grows only when its own look finds them still held, not on a
// pass a nudge started.
func TestTheHeldWaitGrowsOnlyOnItsOwnLook(t *testing.T) {
	t.Parallel()
	r, cfg, _ := imageFixture(t, &recorder{}, true)
	e := imageEvent(pixelN(1))
	e.Attrs[hookrun.AttrProjectID] = "keyless"
	queue(t, r.Images, e)
	s := NewImageSender(r, openSpool(t), func() (*config.Config, error) { return cfg, nil })
	s.pass(t.Context(), false)
	s.pass(t.Context(), false)
	if s.held != s.heldMin {
		t.Fatalf("after a nudged pass the held wait is %v, want %v", s.held, s.heldMin)
	}
	s.pass(t.Context(), true)
	if s.held != 2*s.heldMin {
		t.Fatalf("after its own look the held wait is %v, want %v", s.held, 2*s.heldMin)
	}
}

// A sender halts between images, not during one, and once halted starts no upload.
func TestTheSenderHaltsBetweenImages(t *testing.T) {
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
	var freed sync.Once
	free := func() { freed.Do(func() { close(release) }) }
	t.Cleanup(free) // a failing test still lets the server go
	first, second := pixelN(1), pixelN(2)
	store(t, stateDir, first, second)
	queue(t, r.Images, imageEvent(first))
	s := NewImageSender(r, openSpool(t), func() (*config.Config, error) { return cfg, nil })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	<-arrived
	if s.TryHalt() {
		t.Fatal("the sender halted mid-upload")
	}
	free()
	waitFor(t, "the first pass", func() bool { return s.passes.Load() >= 1 })
	if !s.TryHalt() {
		t.Fatal("an idle sender would not halt")
	}
	if !s.TryHalt() {
		t.Fatal("a halted sender says it is not")
	}
	queue(t, r.Images, imageEvent(second))
	s.Nudge()
	time.Sleep(100 * time.Millisecond)
	if n := len(rec.requests()); n != 1 {
		t.Fatalf("%d uploads; a halted sender started one", n)
	}
	if left := queued(t, r.Images); len(left) != 1 {
		t.Fatalf("image queue holds %+v", left)
	}
}

// A halted sender waits for its end, however its deadlines pass: it does not spin.
func TestAHaltedSenderDoesNotSpin(t *testing.T) {
	t.Parallel()
	r, cfg, _ := imageFixture(t, &recorder{}, true)
	e := imageEvent(pixelN(1))
	e.Attrs[hookrun.AttrProjectID] = "keyless" // held: a deadline 10 ms on
	queue(t, r.Images, e)
	s := NewImageSender(r, openSpool(t), func() (*config.Config, error) { return cfg, nil })
	s.heldMin = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, "the first pass", func() bool { return s.passes.Load() >= 1 })
	if !s.TryHalt() {
		t.Fatal("an idle sender would not halt")
	}
	time.Sleep(200 * time.Millisecond)
	if n := s.loops.Load(); n > 3 {
		t.Fatalf("a halted sender went round its loop %d times in 200 ms", n)
	}
}
