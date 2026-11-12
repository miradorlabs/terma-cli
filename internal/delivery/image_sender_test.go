package delivery

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/spool"
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
	waitWithin(t, 3*time.Second, what, cond)
}

// waitWithin waits up to d for cond.
func waitWithin(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
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
	loops := s.loops.Load()
	s.Nudge()
	// The loop has taken the nudge once it goes round again.
	waitFor(t, "the nudge", func() bool { return s.loops.Load() > loops })
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
	if !s.uploading() || s.TryHalt(false) {
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
	if s.TryHalt(false) {
		t.Fatal("the sender halted mid-upload")
	}
	free()
	waitFor(t, "the first pass", func() bool { return s.passes.Load() >= 1 })
	if !s.TryHalt(false) {
		t.Fatal("an idle sender would not halt")
	}
	if !s.TryHalt(false) {
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
	if !s.TryHalt(false) {
		t.Fatal("an idle sender would not halt")
	}
	loops := s.loops.Load()
	time.Sleep(200 * time.Millisecond)
	// At most the turn under way as it halted, which finds it halted and waits.
	if n := s.loops.Load() - loops; n > 1 {
		t.Fatalf("a halted sender went round its loop %d times in 200 ms", n)
	}
}

// A final halt asked mid-upload (a restart, making way) lets that image end and starts no
// other: the halt lands at the next image's boundary, inside the pass.
func TestAFinalHaltLandsAtTheNextImage(t *testing.T) {
	t.Parallel()
	for _, final := range []bool{true, false} {
		t.Run(fmt.Sprint("final=", final), func(t *testing.T) {
			t.Parallel()
			arrived, release := make(chan struct{}), make(chan struct{})
			var once, freed sync.Once
			free := func() { freed.Do(func() { close(release) }) }
			rec := &recorder{}
			r, cfg, stateDir := imageFixture(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				once.Do(func() {
					close(arrived)
					<-release
				})
				rec.ServeHTTP(w, req)
			}), true)
			t.Cleanup(free)
			first, second := pixelN(1), pixelN(2)
			store(t, stateDir, first, second)
			queue(t, r.Images, imageEvent(first), imageEvent(second))
			s, _, _ := startSenderNoWait(t, r, cfg)
			<-arrived
			if s.TryHalt(final) {
				t.Fatal("the sender halted mid-upload")
			}
			free()
			waitFor(t, "the pass", func() bool { return s.passes.Load() >= 1 })
			if final {
				if n := len(rec.requests()); n != 1 || !s.TryHalt(false) {
					t.Fatalf("%d uploads; halted %v", n, s.TryHalt(false))
				}
			} else if n := len(rec.requests()); n != 2 {
				t.Fatalf("an idle exit's halt kept the next image back: %d uploads", n)
			}
		})
	}
}

// startSenderNoWait runs a sender for r until the test ends, and returns it at once.
func startSenderNoWait(t *testing.T, r Router, cfg *config.Config) (*ImageSender, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	s := NewImageSender(r, openSpool(t), func() (*config.Config, error) { return cfg, nil })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return s, cancel, done
}

// Every window a pass learns of is a deadline: those it waited out, and those its failures
// and stalls opened; one a stopping sender left without an end is none.
func TestNextWindow(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		res  ImageResult
		want time.Time
	}{
		{"none", ImageResult{}, time.Time{}},
		{"waiting", ImageResult{Waiting: []Wait{{ProjectID: "p1", RetryAt: at}}}, at},
		{"failed", ImageResult{Failures: []Failure{{ProjectID: "p1", RetryAt: at}}}, at},
		{"stalled", ImageResult{Stalls: []Failure{{ProjectID: "p1", RetryAt: at}}}, at},
		{"cut short", ImageResult{Failures: []Failure{{ProjectID: "p1"}}}, time.Time{}},
		{"earliest", ImageResult{Waiting: []Wait{{RetryAt: at.Add(time.Hour)}}, Failures: []Failure{{}, {RetryAt: at.Add(time.Minute)}},
			Stalls: []Failure{{RetryAt: at}}}, at},
	} {
		if got := c.res.nextWindow(); !got.Equal(c.want) {
			t.Errorf("%s: nextWindow = %v, want %v", c.name, got, c.want)
		}
	}
}

// A queue whose lock cannot be taken is a failure with a window, not a skip: the image is
// tried again when the window ends.
func TestATakeThatFailsOpensAWindow(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	r, cfg, stateDir := imageFixture(t, rec, true)
	dir := t.TempDir()
	images, err := spool.OpenQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	r.Images = images
	img := pixelN(1)
	store(t, stateDir, img)
	queue(t, images, imageEvent(img))
	// The lock is a folder: it cannot be opened, let alone locked.
	if err := os.Mkdir(filepath.Join(dir, "delivery.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	res := r.FlushImages(t.Context(), openSpool(t), cfg, false)
	if res.Failed != 1 || len(res.Failures) != 1 || res.nextWindow().IsZero() || len(rec.requests()) != 0 {
		t.Fatalf("FlushImages = %+v, %d requests", res, len(rec.requests()))
	}
	if images.RetryAt("p1").IsZero() {
		t.Fatal("no window opened")
	}
}

// A key stored while images wait for it wakes them at the next export, not at their next
// look: a nudge looks at the keys as at the queue.
func TestANewKeyWakesHeldImagesAtTheNextExport(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	r, cfg, stateDir := imageFixture(t, rec, true)
	img := pixelN(1)
	store(t, stateDir, img)
	e := imageEvent(img)
	e.Attrs[hookrun.AttrProjectID] = "keyless"
	queue(t, r.Images, e)
	s := NewImageSender(r, openSpool(t), func() (*config.Config, error) { return cfg, nil })
	s.Watch(keystore.FilePath(r.ConfigDir))
	runSender(t, s)
	waitFor(t, "the first pass", func() bool { return s.passes.Load() >= 1 })
	if err := keystore.Set(r.ConfigDir, "keyless", testKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	s.Nudge()
	waitFor(t, "the held image", func() bool { return len(rec.requests()) == 1 })
}

// A final halt the relay then does not stop for (an export came) is resumed: the sender,
// stopped at the next image's boundary, sends again.
func TestAResumedSenderSendsAgain(t *testing.T) {
	t.Parallel()
	arrived, release := make(chan struct{}), make(chan struct{})
	var once, freed sync.Once
	free := func() { freed.Do(func() { close(release) }) }
	rec := &recorder{}
	r, cfg, stateDir := imageFixture(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		once.Do(func() {
			close(arrived)
			<-release
		})
		rec.ServeHTTP(w, req)
	}), true)
	t.Cleanup(free)
	first, second := pixelN(1), pixelN(2)
	store(t, stateDir, first, second)
	queue(t, r.Images, imageEvent(first), imageEvent(second))
	s, _, _ := startSenderNoWait(t, r, cfg)
	<-arrived
	if s.TryHalt(true) { // an update's quiesce, mid-upload
		t.Fatal("the sender halted mid-upload")
	}
	free()
	waitFor(t, "the halt at the boundary", func() bool { return s.passes.Load() >= 1 && s.TryHalt(false) })
	if n := len(rec.requests()); n != 1 {
		t.Fatalf("%d uploads before the resume", n)
	}
	s.Resume() // an export kept the relay
	waitFor(t, "the second image", func() bool { return len(rec.requests()) == 2 })
}
