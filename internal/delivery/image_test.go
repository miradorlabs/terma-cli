package delivery

import (
	"cmp"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/promptimage"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// pixel is a 1×1 PNG.
var pixel, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==")

// pixelN is another 1×1 PNG for each n, so each has an id of its own.
func pixelN(n int) promptimage.Image {
	data := append(append([]byte{}, pixel...), make([]byte, n)...)
	return promptimage.Image{Data: data, MIMEType: "image/png", Index: 1}
}

// largeImage is an image of the largest size a record carries, its own for each n: its
// body outgrows every buffer between client and server, so a cut leaves it half written.
func largeImage(n byte) promptimage.Image {
	data := make([]byte, promptimage.MaxBytes)
	copy(data, pixel)
	data[len(pixel)] = n
	return promptimage.Image{Data: data, MIMEType: "image/png", Index: 1}
}

// imageEvent is a spooled terma.prompt.image record of img, as an agent's hook writes it.
func imageEvent(img promptimage.Image) spool.Event {
	attrs := img.Attrs()
	attrs[hookrun.AttrProjectID] = "p1"
	attrs[semconv.GenAIMainAgentNameKey] = "fake"
	return spool.Event{Repository: app, Name: semconv.TermaPromptImageEvent, SessionID: "s1", Attrs: attrs}
}

// quotaEvent is an event of p1's that is no image.
func quotaEvent() spool.Event {
	return spool.Event{Repository: app, Name: semconv.TermaSessionQuotaEvent, SessionID: "s1", Attrs: map[string]any{hookrun.AttrProjectID: "p1"}}
}

type recorder struct {
	mu     sync.Mutex
	bodies []string
	refuse func(body string) bool
	// status is what a refusal answers; 500 when unset.
	status int
	// cut, when it holds for a body's first bodyHead bytes, closes the connection there,
	// unread and unanswered: an upload that did not get through, when the body is larger
	// than the buffers between the two.
	cut func(head string) bool
	// delay holds every answer back.
	delay time.Duration
}

// bodyHead is how much of a body recorder reads before it decides to cut it.
const bodyHead = 64 << 10

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	head := make([]byte, bodyHead)
	n, _ := io.ReadFull(r.Body, head)
	if rec.cut != nil && rec.cut(string(head[:n])) {
		rec.mu.Lock()
		rec.bodies = append(rec.bodies, string(head[:n]))
		rec.mu.Unlock()
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
		return
	}
	rest, _ := io.ReadAll(r.Body)
	b := append(head[:n], rest...)
	rec.mu.Lock()
	rec.bodies = append(rec.bodies, string(b))
	rec.mu.Unlock()
	time.Sleep(rec.delay)
	if rec.refuse != nil && rec.refuse(string(b)) {
		w.WriteHeader(cmp.Or(rec.status, http.StatusInternalServerError))
	}
}

func (rec *recorder) requests() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]string(nil), rec.bodies...)
}

// imageFixture is a router whose image lane (Router.Images) delivers to a server rec
// answers, for project p1, whose policy collects prompts when prompts says so.
func imageFixture(t *testing.T, rec http.Handler, prompts bool) (Router, *config.Config, string) {
	t.Helper()
	cfgDir, stateDir := t.TempDir(), t.TempDir()
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	if err := keystore.Set(cfgDir, "p1", testKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	pol := config.Policy{Mode: config.ModeRepo, Repositories: listed, TeamID: "p1", FetchedAt: time.Now(), IncludePrompts: prompts, IncludeToolContent: true}
	r := Router{ConfigDir: cfgDir, StateDir: stateDir, OTLPPinned: true, Images: openSpool(t),
		Policy: func(_ context.Context, _ *config.Config, team string) (config.Policy, error) {
			p := pol
			p.TeamID = team
			return p, nil
		},
		Consent: func(string, hookrun.Consent) bool { return true }}
	return r, &config.Config{OTLPURL: srv.URL}, stateDir
}

func openSpool(t *testing.T) *spool.Spool {
	t.Helper()
	s, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func store(t *testing.T, stateDir string, images ...promptimage.Image) {
	t.Helper()
	for _, img := range images {
		if err := promptimage.Store(stateDir, img); err != nil {
			t.Fatal(err)
		}
	}
}

func queue(t *testing.T, s *spool.Spool, events ...spool.Event) {
	t.Helper()
	if err := s.Append(events...); err != nil {
		t.Fatal(err)
	}
}

func queued(t *testing.T, s *spool.Spool) []spool.Event {
	t.Helper()
	left, err := s.Peek(100)
	if err != nil {
		t.Fatal(err)
	}
	return left
}

// Each image leaves in a request of its own with its bytes from the store, which forgets
// them once no waiting record names them; one whose bytes are gone says so, and one too
// large to carry leaves as it was spooled.
func TestImagesLeaveOneToARequest(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	r, cfg, stateDir := imageFixture(t, rec, true)
	stored := promptimage.Image{Data: pixel, MIMEType: "image/png", Label: "[Image #1]", Index: 1}
	gone := pixelN(1)
	big := promptimage.Image{Data: append(append([]byte{}, pixel...), make([]byte, promptimage.MaxBytes)...), MIMEType: "image/png", Index: 3}
	store(t, stateDir, stored)
	queue(t, r.Images, imageEvent(stored), imageEvent(gone), imageEvent(big))
	res := r.FlushImages(t.Context(), openSpool(t), cfg, false)
	if res.Sent != 3 || res.Err != nil {
		t.Fatalf("FlushImages = %+v", res)
	}
	bodies := rec.requests()
	if len(bodies) != 3 {
		t.Fatalf("%d requests; want one per image", len(bodies))
	}
	data := base64.StdEncoding.EncodeToString(pixel)
	for i, want := range []struct{ id, has, lacks string }{
		{stored.ID(), data, semconv.TermaImageOmittedKey},
		{gone.ID(), semconv.TermaImageOmittedUnavailable, semconv.TermaImageDataKey},
		{big.ID(), semconv.TermaImageOmittedTooLarge, semconv.TermaImageDataKey},
	} {
		body := bodies[i]
		if strings.Count(body, `"eventName":"`+semconv.TermaPromptImageEvent+`"`) != 1 || !strings.Contains(body, want.id) || !strings.Contains(body, want.has) || strings.Contains(body, want.lacks) {
			t.Errorf("image %d request = %.300s", i+1, body)
		}
	}
	if _, ok := promptimage.Load(stateDir, stored.ID()); ok {
		t.Error("a sent image stayed in the store")
	}
	if left := queued(t, r.Images); len(left) != 0 {
		t.Fatalf("image queue holds %+v", left)
	}
}

// Events never wait on an image: the events' pass sends none, and hands an image record an
// older build spooled with the events to the image lane, its bytes left for that lane.
func TestEventsNeverWaitOnImages(t *testing.T) {
	t.Parallel()
	// An image's upload never gets an answer.
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	rec := &recorder{}
	r, cfg, stateDir := imageFixture(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		if strings.Contains(string(b), semconv.TermaPromptImageEvent) {
			select {
			case <-block:
			case <-req.Context().Done():
			}
			return
		}
		rec.mu.Lock()
		rec.bodies = append(rec.bodies, string(b))
		rec.mu.Unlock()
	}), true)
	img := promptimage.Image{Data: pixel, MIMEType: "image/png", Index: 1}
	store(t, stateDir, img)
	events := openSpool(t)
	queue(t, events, imageEvent(img), quotaEvent())
	start := time.Now()
	res := r.Flush(t.Context(), events, cfg, true, 0)
	if res.Sent != 1 || res.Moved != 1 || res.Err != nil {
		t.Fatalf("Flush = %+v", res)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the events' pass took %v", took)
	}
	if bodies := rec.requests(); len(bodies) != 1 || !strings.Contains(bodies[0], semconv.TermaSessionQuotaEvent) {
		t.Fatalf("events' requests = %q", bodies)
	}
	if left := queued(t, events); len(left) != 0 {
		t.Fatalf("events' spool holds %+v", left)
	}
	if moved := queued(t, r.Images); len(moved) != 1 || !isImage(moved[0]) {
		t.Fatalf("image queue holds %+v", moved)
	}
	if _, ok := promptimage.Load(stateDir, img.ID()); !ok {
		t.Fatal("the events' pass forgot an image's bytes")
	}
}

// With nowhere to hand it, an image record stays in the events' spool: never lost, never sent.
func TestAnImageWithNoLaneStaysQueued(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	r, cfg, _ := imageFixture(t, rec, true)
	r.Images = nil
	events := openSpool(t)
	queue(t, events, imageEvent(pixelN(0)), quotaEvent())
	res := r.Flush(t.Context(), events, cfg, true, 0)
	if res.Sent != 1 || res.Failed != 1 || res.Err == nil {
		t.Fatalf("Flush = %+v", res)
	}
	if left := queued(t, events); len(left) != 1 || !isImage(left[0]) {
		t.Fatalf("events' spool holds %+v", left)
	}
}

// An image is what was said: a policy that withholds prompts withholds the whole record.
func TestImagesStayWhenPromptsDo(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	r, cfg, stateDir := imageFixture(t, rec, false)
	img := promptimage.Image{Data: pixel, MIMEType: "image/png", Index: 1}
	store(t, stateDir, img)
	queue(t, r.Images, imageEvent(img))
	res := r.FlushImages(t.Context(), openSpool(t), cfg, false)
	if res.Sent != 0 || res.Withheld != 1 || len(rec.requests()) != 0 {
		t.Fatalf("FlushImages = %+v, %d requests", res, len(rec.requests()))
	}
	if _, ok := promptimage.Load(stateDir, img.ID()); ok {
		t.Error("a withheld image's bytes stayed in the store")
	}
}

// A refused image goes back to its queue as it came, its bytes left in the store for the
// next pass; the host's refusal fails the pass and backs off the project's images.
func TestARefusedImageKeepsItsBytesOutOfTheSpool(t *testing.T) {
	t.Parallel()
	rec := &recorder{refuse: func(string) bool { return true }}
	r, cfg, stateDir := imageFixture(t, rec, true)
	img := promptimage.Image{Data: pixel, MIMEType: "image/png", Index: 1}
	store(t, stateDir, img)
	queue(t, r.Images, imageEvent(img))
	res := r.FlushImages(t.Context(), openSpool(t), cfg, false)
	if res.Sent != 0 || res.Failed != 1 || res.Err == nil || len(res.Failures) != 1 || len(res.Stalls) != 0 {
		t.Fatalf("FlushImages = %+v", res)
	}
	left := queued(t, r.Images)
	if len(left) != 1 || left[0].Attempts != 1 {
		t.Fatalf("image queue holds %+v", left)
	}
	if _, ok := left[0].Attrs[semconv.TermaImageDataKey]; ok {
		t.Fatal("the image's bytes went into the spool")
	}
	if _, ok := promptimage.Load(stateDir, img.ID()); !ok {
		t.Fatal("a refused image left the store")
	}
	if r.Images.RetryAt("p1").IsZero() || len(r.Images.StalledWindows(time.Now())) != 0 {
		t.Fatal("a refusal opened no window, or one that says it stalled")
	}
}

// An image record an older build left in the events' spool still names its bytes there,
// though the image lane sent the same image for another prompt.
func TestAnImageQueuedWithTheEventsKeepsItsBytes(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	r, cfg, stateDir := imageFixture(t, rec, true)
	img := promptimage.Image{Data: pixel, MIMEType: "image/png", Index: 1}
	store(t, stateDir, img)
	events := openSpool(t)
	older := imageEvent(img)
	older.Attrs[semconv.TermaMessageIDKey] = "m0"
	queue(t, events, older)
	queue(t, r.Images, imageEvent(img))
	if res := r.FlushImages(t.Context(), events, cfg, false); res.Sent != 1 {
		t.Fatalf("FlushImages = %+v", res)
	}
	if _, ok := promptimage.Load(stateDir, img.ID()); !ok {
		t.Fatal("the bytes a record in the events' spool names were forgotten")
	}
}

// An image a record still waiting names keeps its bytes, though another record carried it.
func TestAWaitingRecordKeepsItsImage(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	r, cfg, stateDir := imageFixture(t, rec, true)
	img := promptimage.Image{Data: pixel, MIMEType: "image/png", Index: 1}
	store(t, stateDir, img)
	// p2's policy is out of reach: its record is held for a later pass.
	policy := r.Policy
	r.Policy = func(ctx context.Context, cfg *config.Config, id string) (config.Policy, error) {
		if id == "p2" {
			return config.Policy{}, context.DeadlineExceeded
		}
		return policy(ctx, cfg, id)
	}
	held := imageEvent(img)
	held.Attrs[hookrun.AttrProjectID] = "p2"
	queue(t, r.Images, imageEvent(img), held)
	if res := r.FlushImages(t.Context(), openSpool(t), cfg, false); res.Sent != 1 || res.Held != 1 {
		t.Fatalf("FlushImages = %+v", res)
	}
	if _, ok := promptimage.Load(stateDir, img.ID()); !ok {
		t.Fatal("the bytes a held record needs were forgotten")
	}
}

// An image the platform refuses outright goes again without its bytes, saying so, and holds
// up nothing after it.
func TestAnImageRefusedOutrightLeavesWithoutItsBytes(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			data := base64.StdEncoding.EncodeToString(pixel)
			rec := &recorder{status: status, refuse: func(body string) bool { return strings.Contains(body, data) }}
			r, cfg, stateDir := imageFixture(t, rec, true)
			img := promptimage.Image{Data: pixel, MIMEType: "image/png", Index: 1}
			store(t, stateDir, img)
			queue(t, r.Images, imageEvent(img), imageEvent(pixelN(1)))
			res := r.FlushImages(t.Context(), openSpool(t), cfg, false)
			if res.Sent != 2 || res.Err != nil {
				t.Fatalf("FlushImages = %+v", res)
			}
			resent := rec.requests()[1]
			if strings.Contains(resent, data) || !strings.Contains(resent, semconv.TermaImageOmittedRefused) {
				t.Fatalf("resent image = %.300s", resent)
			}
			if left := queued(t, r.Images); len(left) != 0 {
				t.Fatalf("image queue holds %+v", left)
			}
		})
	}
}

// An image that does not get through in its time (a slow uplink) is no news about the host:
// it fails nothing, backs off only its project's images, and goes behind the rest of them,
// so the next pass sends the others first.
func TestAStalledImageGoesBehindTheRest(t *testing.T) {
	t.Parallel()
	first, second := largeImage(1), pixelN(2)
	rec := &recorder{cut: func(head string) bool { return strings.Contains(head, "s-first") }}
	r, cfg, stateDir := imageFixture(t, rec, true)
	store(t, stateDir, first, second)
	stalling := imageEvent(first)
	stalling.SessionID = "s-first"
	queue(t, r.Images, stalling, imageEvent(second))

	res := r.FlushImages(t.Context(), openSpool(t), cfg, false)
	if res.Sent != 0 || res.Err != nil || len(res.Failures) != 0 || len(res.Stalls) != 1 || res.Failed != 0 || res.Deferred != 1 {
		t.Fatalf("FlushImages = %+v", res)
	}
	if len(rec.requests()) != 1 {
		t.Fatalf("%d requests; want the stalled image and no more this pass", len(rec.requests()))
	}
	if _, stalled := r.Images.StalledWindows(time.Now())["p1"]; !stalled {
		t.Fatal("a stalled image opened no stalled window")
	}
	left := queued(t, r.Images)
	if len(left) != 2 || imageID(left[0]) != second.ID() || left[1].Attempts != 1 {
		t.Fatalf("image queue holds %+v; want the second image, then the stalled one", left)
	}

	// The window over (force stands in for it), the second goes before the one that stalled.
	res = r.FlushImages(t.Context(), openSpool(t), cfg, true)
	bodies := rec.requests()[1:]
	if res.Sent != 1 || len(bodies) != 2 || !strings.Contains(bodies[0], second.ID()) || !strings.Contains(bodies[1], "s-first") {
		t.Fatalf("FlushImages = %+v; second pass sent %d requests", res, len(bodies))
	}
	if strings.Contains(bodies[0], "stalls") {
		t.Fatalf("a record's stalls left the machine: %.300s", bodies[0])
	}
	if _, ok := promptimage.Load(stateDir, first.ID()); !ok {
		t.Fatal("a waiting image's bytes were forgotten")
	}
}

// An image whose upload stalls imageMaxAttempts times is sent without its bytes, saying so.
func TestAnImageThatKeepsStallingLeavesWithoutItsBytes(t *testing.T) {
	t.Parallel()
	rec := &recorder{cut: func(head string) bool { return strings.Contains(head, semconv.TermaImageDataKey) }}
	r, cfg, stateDir := imageFixture(t, rec, true)
	img := largeImage(1)
	store(t, stateDir, img)
	queue(t, r.Images, imageEvent(img))
	for pass := 1; pass <= imageMaxAttempts; pass++ {
		if res := r.FlushImages(t.Context(), openSpool(t), cfg, true); len(res.Stalls) != 1 {
			t.Fatalf("pass %d: FlushImages = %+v", pass, res)
		}
	}
	res := r.FlushImages(t.Context(), openSpool(t), cfg, true)
	if res.Sent != 1 || res.Err != nil {
		t.Fatalf("FlushImages = %+v", res)
	}
	bodies := rec.requests()
	if last := bodies[len(bodies)-1]; len(bodies) != imageMaxAttempts+1 || strings.Contains(last, semconv.TermaImageDataKey) ||
		!strings.Contains(last, `"stringValue":"`+semconv.TermaImageOmittedUnsent+`"`) {
		t.Fatalf("%d requests, the last %.300s", len(bodies), last)
	}
	if left := queued(t, r.Images); len(left) != 0 {
		t.Fatalf("image queue holds %+v", left)
	}
	if _, ok := promptimage.Load(stateDir, img.ID()); ok {
		t.Fatal("an image given up on stayed in the store")
	}
}

// A body all written that the host never answers is the host's, not the uplink's: it fails,
// not stalls, and counts an attempt like one.
func TestAnUnansweredImageIsNoStall(t *testing.T) {
	t.Parallel()
	r, cfg, stateDir := imageFixture(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.ReadAll(req.Body)
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	}), true)
	img := pixelN(1)
	store(t, stateDir, img)
	queue(t, r.Images, imageEvent(img))
	res := r.FlushImages(t.Context(), openSpool(t), cfg, false)
	if len(res.Stalls) != 0 || len(res.Failures) != 1 || res.Failed != 1 {
		t.Fatalf("FlushImages = %+v", res)
	}
	if left := queued(t, r.Images); len(left) != 1 || left[0].Attempts != 1 {
		t.Fatalf("image queue holds %+v", left)
	}
}

// A cancelled pass learned nothing: the image it cut off waits, with no window and no stall.
func TestACancelledPassIsNoStall(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	var once sync.Once
	r, cfg, stateDir := imageFixture(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.ReadAll(req.Body) // the server notices a closed connection only once the body is read
		once.Do(func() { close(started) })
		<-req.Context().Done()
	}), true)
	img := promptimage.Image{Data: pixel, MIMEType: "image/png", Index: 1}
	store(t, stateDir, img)
	queue(t, r.Images, imageEvent(img))
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-started
		cancel()
	}()
	res := r.FlushImages(ctx, openSpool(t), cfg, false)
	if len(res.Stalls) != 0 || len(res.Failures) != 0 || res.Deferred != 1 {
		t.Fatalf("FlushImages = %+v", res)
	}
	if !r.Images.RetryAt("p1").IsZero() {
		t.Fatal("a cancelled pass opened a window")
	}
	if left := queued(t, r.Images); len(left) != 1 || left[0].Attempts != 0 {
		t.Fatalf("image queue holds %+v", left)
	}
}

// The image lane's pass has room for the largest image on the slowest uplink a request is
// given time for, which the events' pass does not.
func TestTheImagePassHasRoomForTheLargestImage(t *testing.T) {
	t.Parallel()
	largest := timeoutFor(base64.StdEncoding.EncodedLen(promptimage.MaxBytes) + 4<<10)
	if spool.DefaultFlushTimeout >= largest {
		t.Fatalf("the events' pass (%v) has room for the largest image (%v): the image lane's own pass is moot", spool.DefaultFlushTimeout, largest)
	}
	if ImagePassTimeout < 2*largest {
		t.Fatalf("the image pass (%v) has no room past the largest image's request (%v)", ImagePassTimeout, largest)
	}
}

// An image is started only with all its request's time left: too little of the pass left
// leaves it for the next pass untried, opening no window. The image lane's own pass, long
// enough, sends the same image.
func TestAnImageWaitsForAPassWithRoomForIt(t *testing.T) {
	t.Parallel()
	rec := &recorder{delay: 300 * time.Millisecond}
	r, cfg, stateDir := imageFixture(t, rec, true)
	r.requestTimeout = func(int) time.Duration { return time.Second }
	img := promptimage.Image{Data: pixel, MIMEType: "image/png", Index: 1}
	store(t, stateDir, img)
	queue(t, r.Images, imageEvent(img))

	r.imagePass = 500 * time.Millisecond
	res := r.FlushImages(t.Context(), openSpool(t), cfg, false)
	if res.Sent != 0 || res.Deferred != 1 || res.Failed != 0 || len(res.Stalls) != 0 || len(rec.requests()) != 0 {
		t.Fatalf("short pass: FlushImages = %+v, %d requests", res, len(rec.requests()))
	}
	if !r.Images.RetryAt("p1").IsZero() {
		t.Fatal("an image never tried opened a window")
	}

	r.imagePass = 3 * time.Second
	if res := r.FlushImages(t.Context(), openSpool(t), cfg, false); res.Sent != 1 {
		t.Fatalf("long pass: FlushImages = %+v", res)
	}
}

// Only one image pass runs at a time: another finds the queue busy and leaves at once.
func TestOneImagePassAtATime(t *testing.T) {
	t.Parallel()
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r, cfg, stateDir := imageFixture(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.ReadAll(req.Body)
		once.Do(func() { close(started) })
		<-release
	}), true)
	img := promptimage.Image{Data: pixel, MIMEType: "image/png", Index: 1}
	store(t, stateDir, img)
	queue(t, r.Images, imageEvent(img))
	done := make(chan ImageResult)
	go func() { done <- r.FlushImages(t.Context(), openSpool(t), cfg, false) }()
	<-started
	second := r.FlushImages(t.Context(), openSpool(t), cfg, false)
	close(release)
	if !second.Skipped || second.Reason != spool.SkipBusy {
		t.Fatalf("second pass = %+v", second)
	}
	if first := <-done; first.Sent != 1 {
		t.Fatalf("first pass = %+v", first)
	}
}

// A pass prunes bytes older than any record the spool still keeps.
func TestFlushPrunesImagesNoRecordOutlives(t *testing.T) {
	t.Parallel()
	r, cfg, stateDir := imageFixture(t, &recorder{}, true)
	img := promptimage.Image{Data: pixel, MIMEType: "image/png", Index: 1}
	store(t, stateDir, img)
	old := time.Now().Add(-spool.MaxAge - time.Hour)
	if err := os.Chtimes(filepath.Join(promptimage.Dir(stateDir), img.ID()), old, old); err != nil {
		t.Fatal(err)
	}
	r.FlushImages(t.Context(), openSpool(t), cfg, false)
	if _, ok := promptimage.Load(stateDir, img.ID()); ok {
		t.Fatal("bytes older than the spool keeps records outlived a pass")
	}
}

// timeoutError is a net.Error that timed out, as a TLS handshake's does.
type timeoutError struct{}

func (timeoutError) Error() string   { return "net/http: TLS handshake timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// How a send went: delivered, deferred having learned nothing, stalled on the uplink (which
// backs off only the project's images and fails nothing), or failed on the host.
func TestJudgeImage(t *testing.T) {
	t.Parallel()
	live := t.Context()
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, cancelExpired := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancelExpired()
	reset := &url.Error{Op: "Post", Err: &net.OpError{Op: "write", Err: os.NewSyscallError("write", connectionBroken[0])}}
	deadline := &url.Error{Op: "Post", Err: context.DeadlineExceeded}
	for _, c := range []struct {
		name string
		pass context.Context
		err  error
		got  progress
		want imageOutcome
	}{
		{"delivered", live, nil, written, imageDelivered},
		{"no time left to start it", live, errNoTimeLeft, notReached, imageDeferred},
		{"pass cancelled mid-upload", cancelled, &url.Error{Op: "Post", Err: context.Canceled}, reached, imageDeferred},
		{"pass cancelled before the host", cancelled, &url.Error{Op: "Post", Err: context.Canceled}, notReached, imageDeferred},
		{"pass ran out mid-upload", expired, deadline, reached, imageStalled},
		{"pass ran out before the host", expired, deadline, notReached, imageDeferred},
		{"own deadline mid-upload", live, deadline, reached, imageStalled},
		{"reset mid-upload", live, reset, reached, imageStalled},
		{"cut off", live, &url.Error{Op: "Post", Err: io.EOF}, reached, imageStalled},
		{"cut off mid-answer", live, &url.Error{Op: "Post", Err: io.ErrUnexpectedEOF}, reached, imageStalled},
		{"closed under the write", live, &url.Error{Op: "Post", Err: &net.OpError{Op: "write", Err: net.ErrClosed}}, reached, imageStalled},
		{"no answer to a body all written", live, deadline, written, imageFailed},
		{"cut off after a body all written", live, &url.Error{Op: "Post", Err: io.EOF}, written, imageFailed},
		{"pass ran out after a body all written", expired, deadline, written, imageFailed},
		{"host refused", live, &IngestError{Status: http.StatusInternalServerError}, written, imageFailed},
		{"gateway timeout", live, &IngestError{Status: http.StatusGatewayTimeout}, written, imageFailed},
		{"unprocessable", live, &IngestError{Status: http.StatusUnprocessableEntity}, written, imageFailed},
		{"too many requests", live, &IngestError{Status: http.StatusTooManyRequests}, written, imageUnavailable},
		{"bad gateway", live, &IngestError{Status: http.StatusBadGateway}, written, imageUnavailable},
		{"between deploys", live, &IngestError{Status: http.StatusServiceUnavailable}, written, imageUnavailable},
		{"key refused", live, &IngestError{Status: http.StatusUnauthorized}, written, imageUnavailable},
		{"dial", live, &url.Error{Op: "Post", Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}, notReached, imageUnavailable},
		{"dns", live, &url.Error{Op: "Post", Err: &net.DNSError{Err: "no such host", Name: "x"}}, notReached, imageUnavailable},
		{"tls certificate", live, &url.Error{Op: "Post", Err: &tls.CertificateVerificationError{}}, notReached, imageUnavailable},
		{"tls handshake timeout", live, &url.Error{Op: "Post", Err: timeoutError{}}, notReached, imageUnavailable},
		{"own deadline before the host", live, deadline, notReached, imageUnavailable},
		{"something else", live, errors.New("odd"), reached, imageFailed},
	} {
		if got := judgeImage(c.pass, c.err, c.got); got != c.want {
			t.Errorf("%s: judgeImage = %v, want %v", c.name, got, c.want)
		}
	}
}

// A request that reached the host says so, and one whose body went out says that: an
// upload cut off says only it reached, the host's answer that its body went.
func TestPostSaysHowFarItGot(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		cut  bool
		want progress
	}{{"answered", false, written}, {"cut off", true, reached}} {
		rec := &recorder{cut: func(string) bool { return c.cut }}
		srv := httptest.NewServer(rec)
		// The body outgrows every buffer between the two, so a cut leaves it half written.
		events := []spool.Event{{Name: "x", Attrs: map[string]any{"pad": strings.Repeat("x", 16<<20)}}}
		got, _ := (&otlpSender{Endpoint: srv.URL, APIKey: testKey}).post(t.Context(), events)
		srv.Close()
		if got != c.want {
			t.Errorf("%s: progress = %v, want %v", c.name, got, c.want)
		}
	}
	unreachable := &otlpSender{Endpoint: "http://127.0.0.1:1", APIKey: testKey}
	if got, err := unreachable.post(t.Context(), []spool.Event{{Name: "x"}}); err == nil || got != notReached {
		t.Errorf("no host: progress = %v, %v", got, err)
	}
}

// A request retried on a new connection starts over: a reused connection that failed before
// the request was written says nothing of the new one, and a failed write is not a body sent.
func TestProgressStartsOverOnANewConnection(t *testing.T) {
	t.Parallel()
	var got atomic.Int32
	trace := progressTrace(&got)
	trace.GotConn(httptrace.GotConnInfo{Reused: true})
	trace.WroteRequest(httptrace.WroteRequestInfo{Err: io.EOF})
	if progress(got.Load()) != reached {
		t.Fatalf("a failed write = %v", progress(got.Load()))
	}
	trace.ConnectStart("tcp", "127.0.0.1:1")
	if progress(got.Load()) != notReached {
		t.Fatalf("a new connection's dial = %v", progress(got.Load()))
	}
	trace.GotConn(httptrace.GotConnInfo{})
	trace.WroteRequest(httptrace.WroteRequestInfo{})
	if progress(got.Load()) != written {
		t.Fatalf("a body written = %v", progress(got.Load()))
	}
}

// Each image is settled in its queue as soon as it went: a pass cancelled (or killed) on the
// next resends none of what it delivered.
func TestEachImageIsSettledAsItGoes(t *testing.T) {
	t.Parallel()
	first, second := pixelN(1), pixelN(2)
	reached := make(chan struct{})
	var once sync.Once
	r, cfg, stateDir := imageFixture(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		if strings.Contains(string(b), second.ID()) {
			once.Do(func() { close(reached) })
			<-req.Context().Done()
		}
	}), true)
	store(t, stateDir, first, second)
	queue(t, r.Images, imageEvent(first), imageEvent(second))
	ctx, cancel := context.WithCancel(t.Context())
	var during []spool.Event
	go func() {
		<-reached
		during = queued(t, r.Images)
		cancel()
	}()
	res := r.FlushImages(ctx, openSpool(t), cfg, false)
	if res.Sent != 1 || res.Deferred != 1 || len(res.Stalls) != 0 || len(res.Failures) != 0 {
		t.Fatalf("FlushImages = %+v", res)
	}
	if len(during) != 1 || imageID(during[0]) != second.ID() {
		t.Fatalf("while the second image went, the queue held %+v", during)
	}
	if left := queued(t, r.Images); len(left) != 1 || imageID(left[0]) != second.ID() || left[0].Attempts != 0 {
		t.Fatalf("after a cancelled pass the queue holds %+v", left)
	}
}

// A pass stops before its next image once terma is torn down.
func TestTeardownStopsAnImagePass(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	r, cfg, stateDir := imageFixture(t, rec, true)
	first, second := pixelN(1), pixelN(2)
	store(t, stateDir, first, second)
	queue(t, r.Images, imageEvent(first), imageEvent(second))
	r.SetUp = func() bool { return len(rec.requests()) == 0 }
	res := r.FlushImages(t.Context(), openSpool(t), cfg, false)
	if res.Sent != 1 || res.Deferred != 1 || len(rec.requests()) != 1 {
		t.Fatalf("FlushImages = %+v, %d requests", res, len(rec.requests()))
	}
}

// Whether an image fits what is left of the pass is judged from the size its record names,
// before its bytes are read: a large image whose bytes have gone waits, untried, rather than
// leave as a small record saying they are unavailable.
func TestAnImageIsJudgedByItsSizeBeforeItsBytesAreRead(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	r, cfg, _ := imageFixture(t, rec, true)
	r.requestTimeout = func(n int) time.Duration { return time.Duration(n) * time.Microsecond }
	r.imagePass = time.Second
	big := pixelN(2 << 20) // ~2.8 s at this rate, its bytes never stored
	big.Data = big.Data[:promptimage.MaxBytes]
	queue(t, r.Images, imageEvent(big))
	res := r.FlushImages(t.Context(), openSpool(t), cfg, false)
	if res.Sent != 0 || res.Deferred != 1 || len(rec.requests()) != 0 {
		t.Fatalf("FlushImages = %+v, %d requests", res, len(rec.requests()))
	}
}

// Projects take turns, an image each, so one team's backlog does not fill the pass.
func TestProjectsTakeTurns(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	r, cfg, stateDir := imageFixture(t, rec, true)
	if err := keystore.Set(r.ConfigDir, "p2", testKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	a, b, c, other := pixelN(1), pixelN(2), pixelN(3), pixelN(4)
	store(t, stateDir, a, b, c, other)
	theirs := imageEvent(other)
	theirs.Attrs[hookrun.AttrProjectID] = "p2"
	queue(t, r.Images, imageEvent(a), imageEvent(b), imageEvent(c), theirs)
	if res := r.FlushImages(t.Context(), openSpool(t), cfg, false); res.Sent != 4 {
		t.Fatalf("FlushImages = %+v", res)
	}
	var order []string
	for _, body := range rec.requests() {
		for _, img := range []promptimage.Image{a, b, c, other} {
			if strings.Contains(body, img.ID()) {
				order = append(order, img.ID()[:6])
			}
		}
	}
	want := []string{a.ID()[:6], other.ID()[:6], b.ID()[:6], c.ID()[:6]}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("sent %v, want %v", order, want)
	}
}

// A project whose images window is open waits it out before its key is looked up.
func TestAWaitingProjectNeedsNoKey(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	r, cfg, _ := imageFixture(t, rec, true)
	img := imageEvent(pixelN(1))
	img.Attrs[hookrun.AttrProjectID] = "keyless"
	queue(t, r.Images, img)
	r.Images.DestinationFailed("keyless", time.Now())
	res := r.FlushImages(t.Context(), openSpool(t), cfg, false)
	if res.Held != 0 || res.Deferred != 1 || len(res.Waiting) != 1 {
		t.Fatalf("FlushImages = %+v", res)
	}
}

// An image the host keeps refusing (a 504) goes behind the rest each time, so the project's
// other images still get through, and after imageMaxAttempts it leaves without its bytes.
func TestAnImageTheHostKeepsRefusingHoldsUpNoOther(t *testing.T) {
	t.Parallel()
	data := base64.StdEncoding.EncodeToString(pixelN(1).Data)
	rec := &recorder{status: http.StatusGatewayTimeout, refuse: func(body string) bool { return strings.Contains(body, data) }}
	r, cfg, stateDir := imageFixture(t, rec, true)
	refused, other := pixelN(1), pixelN(2)
	store(t, stateDir, refused, other)
	queue(t, r.Images, imageEvent(refused), imageEvent(other))
	sent := 0
	for pass := 1; pass <= imageMaxAttempts; pass++ {
		res := r.FlushImages(t.Context(), openSpool(t), cfg, true)
		sent += res.Sent
		if len(res.Failures) != 1 {
			t.Fatalf("pass %d: FlushImages = %+v", pass, res)
		}
	}
	if sent != 1 {
		t.Fatalf("the other image went %d times in %d passes", sent, imageMaxAttempts)
	}
	if left := queued(t, r.Images); len(left) != 1 || left[0].Attempts != imageMaxAttempts {
		t.Fatalf("image queue holds %+v", left)
	}
	if res := r.FlushImages(t.Context(), openSpool(t), cfg, true); res.Sent != 1 {
		t.Fatalf("last pass: FlushImages = %+v", res)
	}
	bodies := rec.requests()
	if last := bodies[len(bodies)-1]; strings.Contains(last, data) || !strings.Contains(last, `"stringValue":"`+semconv.TermaImageOmittedUnsent+`"`) {
		t.Fatalf("the last request = %.300s", last)
	}
}

// A host out of reach says nothing about the record: it counts no attempt and keeps its place.
func TestAnUnreachableHostCountsNoAttempt(t *testing.T) {
	t.Parallel()
	r, _, stateDir := imageFixture(t, &recorder{}, true)
	img := pixelN(1)
	store(t, stateDir, img)
	queue(t, r.Images, imageEvent(img))
	res := r.FlushImages(t.Context(), openSpool(t), &config.Config{OTLPURL: "http://127.0.0.1:1"}, false)
	if len(res.Failures) != 1 || res.Failed != 1 {
		t.Fatalf("FlushImages = %+v", res)
	}
	if left := queued(t, r.Images); len(left) != 1 || left[0].Attempts != 0 {
		t.Fatalf("image queue holds %+v", left)
	}
}

// A record goes back to the queue as it was spooled, with its attempt counted, not as the
// policy let it leave.
func TestARetriedImageIsRequeuedAsSpooled(t *testing.T) {
	t.Parallel()
	rec := &recorder{refuse: func(string) bool { return true }}
	r, cfg, stateDir := imageFixture(t, rec, true)
	policy := r.Policy
	r.Policy = func(ctx context.Context, cfg *config.Config, team string) (config.Policy, error) {
		p, err := policy(ctx, cfg, team)
		p.IncludeToolContent = false // withholds user.email from what leaves
		return p, err
	}
	img := pixelN(1)
	store(t, stateDir, img)
	e := imageEvent(img)
	e.Attrs[semconv.UserEmailKey] = "dev@example.com"
	queue(t, r.Images, e)
	r.FlushImages(t.Context(), openSpool(t), cfg, false)
	if left := queued(t, r.Images); len(left) != 1 || left[0].Attempts != 1 || left[0].Attrs[semconv.UserEmailKey] != "dev@example.com" {
		t.Fatalf("image queue holds %+v", left)
	}
}

// Of a project's images with as many attempts, the oldest goes first, wherever it is queued.
func TestTheOldestImageGoesFirst(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	r, cfg, stateDir := imageFixture(t, rec, true)
	older, newer := pixelN(1), pixelN(2)
	store(t, stateDir, older, newer)
	first, second := imageEvent(newer), imageEvent(older)
	first.Time, second.Time = time.Now(), time.Now().Add(-time.Minute)
	queue(t, r.Images, first, second)
	r.FlushImages(t.Context(), openSpool(t), cfg, false)
	if bodies := rec.requests(); len(bodies) != 2 || !strings.Contains(bodies[0], older.ID()) {
		t.Fatalf("%d requests, the first not the older image", len(bodies))
	}
}

// The project that went first in a pass goes last in the next.
func TestTheFirstProjectTakesTurns(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	r, cfg, stateDir := imageFixture(t, rec, true)
	if err := keystore.Set(r.ConfigDir, "p2", testKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	var order []string
	for pass := range 2 {
		mine, theirs := pixelN(10*pass+1), pixelN(10*pass+2)
		store(t, stateDir, mine, theirs)
		other := imageEvent(theirs)
		other.Attrs[hookrun.AttrProjectID] = "p2"
		queue(t, r.Images, imageEvent(mine), other)
		r.FlushImages(t.Context(), openSpool(t), cfg, false)
		first := rec.requests()[2*pass]
		order = append(order, map[bool]string{true: "p1", false: "p2"}[strings.Contains(first, mine.ID())])
	}
	if strings.Join(order, ",") != "p1,p2" {
		t.Fatalf("first project by pass = %v", order)
	}
}

// Without the events' spool to read, a pass sweeps no bytes: a record there may name them.
func TestAPassThatCannotReadTheEventsSweepsNothing(t *testing.T) {
	t.Parallel()
	r, cfg, stateDir := imageFixture(t, &recorder{}, true)
	img := pixelN(1)
	store(t, stateDir, img)
	queue(t, r.Images, imageEvent(img))
	if res := r.FlushImages(t.Context(), nil, cfg, false); res.Sent != 1 {
		t.Fatalf("FlushImages = %+v", res)
	}
	if _, ok := promptimage.Load(stateDir, img.ID()); !ok {
		t.Fatal("a pass that could not read the events' spool swept an image")
	}
}

// An image pass is due once a queued image's project is not waiting out its window.
func TestImagesDue(t *testing.T) {
	t.Parallel()
	s := openSpool(t)
	now := time.Now()
	if ImagesDue(s, now) {
		t.Fatal("an empty queue is due")
	}
	queue(t, s, imageEvent(pixelN(1)))
	if !ImagesDue(s, now) {
		t.Fatal("an image with no window is not due")
	}
	s.DestinationFailed("p1", now)
	if ImagesDue(s, now) {
		t.Fatal("an image waiting out its window is due")
	}
	if !ImagesDue(s, now.Add(time.Hour)) {
		t.Fatal("an image whose window closed is not due")
	}
}

// A teardown later in the round counts each image left untried once, those a failure
// deferred included.
func TestTeardownAfterAFailureCountsEachImageOnce(t *testing.T) {
	t.Parallel()
	rec := &recorder{refuse: func(body string) bool { return strings.Contains(body, "s-fail") }}
	r, cfg, stateDir := imageFixture(t, rec, true)
	if err := keystore.Set(r.ConfigDir, "p2", testKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	a, b, d := pixelN(1), pixelN(2), pixelN(3)
	store(t, stateDir, a, b, d)
	failing, theirs := imageEvent(a), imageEvent(d)
	failing.SessionID = "s-fail"
	theirs.Attrs[hookrun.AttrProjectID] = "p2"
	queue(t, r.Images, failing, imageEvent(b), theirs)
	r.SetUp = func() bool { return len(rec.requests()) == 0 }
	res := r.FlushImages(t.Context(), openSpool(t), cfg, false)
	if res.Failed != 1 || res.Deferred != 2 || res.Sent != 0 {
		t.Fatalf("FlushImages = %+v; want one failed and two deferred", res)
	}
}

// A deploy's 503s say nothing about the record: it counts no attempt, waits as long as
// Retry-After asks, and once the host is back leaves with its bytes.
func TestAnUnavailableHostCostsTheImageNothing(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	answered := 0
	var bodies []string
	r, cfg, stateDir := imageFixture(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		mu.Lock()
		defer mu.Unlock()
		bodies = append(bodies, string(b))
		if answered++; answered <= imageMaxAttempts {
			w.Header().Set("Retry-After", "600")
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}), true)
	img := pixelN(1)
	store(t, stateDir, img)
	queue(t, r.Images, imageEvent(img))
	for pass := 1; pass <= imageMaxAttempts; pass++ {
		res := r.FlushImages(t.Context(), openSpool(t), cfg, true)
		if len(res.Failures) != 1 || res.Failures[0].RetryAt.Before(time.Now().Add(9*time.Minute)) {
			t.Fatalf("pass %d: FlushImages = %+v; want a window as long as Retry-After", pass, res)
		}
	}
	if left := queued(t, r.Images); len(left) != 1 || left[0].Attempts != 0 {
		t.Fatalf("image queue holds %+v", left)
	}
	if res := r.FlushImages(t.Context(), openSpool(t), cfg, true); res.Sent != 1 {
		t.Fatalf("FlushImages = %+v", res)
	}
	if last := bodies[len(bodies)-1]; !strings.Contains(last, base64.StdEncoding.EncodeToString(img.Data)) {
		t.Fatalf("the image left without its bytes: %.300s", last)
	}
}

// A refused key says nothing about the record either, and holds up no other team's images.
func TestARefusedKeyCostsTheImageNothing(t *testing.T) {
	t.Parallel()
	rec := &recorder{status: http.StatusUnauthorized, refuse: func(body string) bool { return strings.Contains(body, "s-mine") }}
	r, cfg, stateDir := imageFixture(t, rec, true)
	if err := keystore.Set(r.ConfigDir, "p2", testKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	mine, theirs := pixelN(1), pixelN(2)
	store(t, stateDir, mine, theirs)
	refused, other := imageEvent(mine), imageEvent(theirs)
	refused.SessionID = "s-mine"
	other.Attrs[hookrun.AttrProjectID] = "p2"
	queue(t, r.Images, refused, other)
	res := r.FlushImages(t.Context(), openSpool(t), cfg, false)
	if res.Sent != 1 || len(res.Failures) != 1 || res.Failures[0].ProjectID != "p1" {
		t.Fatalf("FlushImages = %+v", res)
	}
	if left := queued(t, r.Images); len(left) != 1 || left[0].Attempts != 0 || left[0].SessionID != "s-mine" {
		t.Fatalf("image queue holds %+v", left)
	}
}

// Bytes the host refused (400) are not sent again, though the record without them then
// failed: it goes on saying they were refused.
func TestRefusedBytesAreNotSentAgain(t *testing.T) {
	t.Parallel()
	data := base64.StdEncoding.EncodeToString(pixelN(1).Data)
	var mu sync.Mutex
	carried := 0
	r, cfg, stateDir := imageFixture(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(string(b), data) {
			carried++
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}), true)
	img := pixelN(1)
	store(t, stateDir, img)
	queue(t, r.Images, imageEvent(img))
	for range 2 {
		r.FlushImages(t.Context(), openSpool(t), cfg, true)
	}
	left := queued(t, r.Images)
	if carried != 1 || len(left) != 1 || left[0].Attrs[semconv.TermaImageOmittedKey] != semconv.TermaImageOmittedRefused {
		t.Fatalf("the bytes went %d times; image queue holds %+v", carried, left)
	}
}

// A record the host refuses even without its bytes, attempt after attempt, is dropped once
// imageMaxAttempts are spent, and counted as rejected.
func TestARecordRefusedWithoutBytesIsDropped(t *testing.T) {
	t.Parallel()
	rec := &recorder{status: http.StatusUnprocessableEntity, refuse: func(string) bool { return true }}
	r, cfg, stateDir := imageFixture(t, rec, true)
	img := pixelN(1)
	store(t, stateDir, img)
	queue(t, r.Images, imageEvent(img))
	for pass := 1; pass <= imageMaxAttempts; pass++ {
		if res := r.FlushImages(t.Context(), openSpool(t), cfg, true); res.Rejected != 0 {
			t.Fatalf("pass %d dropped it: %+v", pass, res)
		}
	}
	res := r.FlushImages(t.Context(), openSpool(t), cfg, true)
	if res.Rejected != 1 || res.Failed != 0 {
		t.Fatalf("FlushImages = %+v", res)
	}
	if left := queued(t, r.Images); len(left) != 0 {
		t.Fatalf("image queue holds %+v", left)
	}
	if _, ok := promptimage.Load(stateDir, img.ID()); ok {
		t.Fatal("a dropped record's image stayed in the store")
	}
}

// Retry-After reads seconds or a date.
func TestRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	for h, want := range map[string]time.Time{
		"120":                           now.Add(2 * time.Minute),
		"Thu, 12 Nov 2026 12:10:00 GMT": now.Add(10 * time.Minute),
		"Thu, 12 Nov 2026 11:00:00 GMT": {},
		"":                              {},
		"soon":                          {},
	} {
		if got := retryAfter(h, now); !got.Equal(want) {
			t.Errorf("retryAfter(%q) = %v, want %v", h, got, want)
		}
	}
}

// No pass is due while one runs.
func TestNoImagesAreDueDuringAPass(t *testing.T) {
	t.Parallel()
	s := openSpool(t)
	queue(t, s, imageEvent(pixelN(1)))
	s.Drain(t.Context(), spool.DrainOptions{}, func(context.Context, []spool.Pending) {
		if ImagesDue(s, time.Now()) {
			t.Error("images are due while a pass runs")
		}
	})
	if !ImagesDue(s, time.Now()) {
		t.Fatal("images are not due once the pass is over")
	}
}
