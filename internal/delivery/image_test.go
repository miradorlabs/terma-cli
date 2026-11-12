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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	// status is what a refusal answers; 503 when unset.
	status int
	// cut, when it holds for a body, closes the connection with no answer: an upload that
	// did not get through.
	cut func(body string) bool
	// delay holds every answer back.
	delay time.Duration
}

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	rec.mu.Lock()
	rec.bodies = append(rec.bodies, string(b))
	rec.mu.Unlock()
	if rec.cut != nil && rec.cut(string(b)) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
		return
	}
	time.Sleep(rec.delay)
	if rec.refuse != nil && rec.refuse(string(b)) {
		w.WriteHeader(cmp.Or(rec.status, http.StatusServiceUnavailable))
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
		Policy:  func(context.Context, *config.Config, string) (config.Policy, error) { return pol, nil },
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
	res := r.FlushImages(t.Context(), nil, cfg, false)
	if res.Sent != 3 || res.Err != nil || !res.Images {
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
	res := r.FlushImages(t.Context(), nil, cfg, false)
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
	res := r.FlushImages(t.Context(), nil, cfg, false)
	if res.Sent != 0 || res.Failed != 1 || res.Err == nil || len(res.Failures) != 1 || len(res.Stalls) != 0 {
		t.Fatalf("FlushImages = %+v", res)
	}
	left := queued(t, r.Images)
	if len(left) != 1 || left[0].Stalls != 0 {
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
	if res := r.FlushImages(t.Context(), nil, cfg, false); res.Sent != 1 || res.Held != 1 {
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
			res := r.FlushImages(t.Context(), nil, cfg, false)
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
	first, second := pixelN(1), pixelN(2)
	rec := &recorder{cut: func(body string) bool { return strings.Contains(body, first.ID()) }}
	r, cfg, stateDir := imageFixture(t, rec, true)
	store(t, stateDir, first, second)
	queue(t, r.Images, imageEvent(first), imageEvent(second))

	res := r.FlushImages(t.Context(), nil, cfg, false)
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
	if len(left) != 2 || left[0].Attrs[semconv.TermaImageIDKey] != second.ID() || left[1].Stalls != 1 {
		t.Fatalf("image queue holds %+v; want the second image, then the stalled one", left)
	}

	// The window over (force stands in for it), the second goes before the one that stalled.
	res = r.FlushImages(t.Context(), nil, cfg, true)
	bodies := rec.requests()[1:]
	if res.Sent != 1 || len(bodies) != 2 || !strings.Contains(bodies[0], second.ID()) || !strings.Contains(bodies[1], first.ID()) {
		t.Fatalf("FlushImages = %+v; second pass sent %d requests", res, len(bodies))
	}
	if strings.Contains(bodies[1], "stalls") {
		t.Fatalf("a record's stalls left the machine: %.300s", bodies[1])
	}
	if _, ok := promptimage.Load(stateDir, first.ID()); !ok {
		t.Fatal("a waiting image's bytes were forgotten")
	}
}

// An image whose upload stalls imageMaxStalls times is sent without its bytes, saying so.
func TestAnImageThatKeepsStallingLeavesWithoutItsBytes(t *testing.T) {
	t.Parallel()
	data := base64.StdEncoding.EncodeToString(pixel)
	rec := &recorder{cut: func(body string) bool { return strings.Contains(body, data) }}
	r, cfg, stateDir := imageFixture(t, rec, true)
	img := promptimage.Image{Data: pixel, MIMEType: "image/png", Index: 1}
	store(t, stateDir, img)
	queue(t, r.Images, imageEvent(img))
	for pass := 1; pass <= imageMaxStalls; pass++ {
		if res := r.FlushImages(t.Context(), nil, cfg, true); len(res.Stalls) != 1 {
			t.Fatalf("pass %d: FlushImages = %+v", pass, res)
		}
	}
	res := r.FlushImages(t.Context(), nil, cfg, true)
	if res.Sent != 1 || res.Err != nil {
		t.Fatalf("FlushImages = %+v", res)
	}
	bodies := rec.requests()
	if last := bodies[len(bodies)-1]; len(bodies) != imageMaxStalls+1 || strings.Contains(last, data) ||
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
	res := r.FlushImages(ctx, nil, cfg, false)
	if len(res.Stalls) != 0 || len(res.Failures) != 0 || res.Deferred != 1 {
		t.Fatalf("FlushImages = %+v", res)
	}
	if !r.Images.RetryAt("p1").IsZero() {
		t.Fatal("a cancelled pass opened a window")
	}
	if left := queued(t, r.Images); len(left) != 1 || left[0].Stalls != 0 {
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
	res := r.FlushImages(t.Context(), nil, cfg, false)
	if res.Sent != 0 || res.Deferred != 1 || res.Failed != 0 || len(res.Stalls) != 0 || len(rec.requests()) != 0 {
		t.Fatalf("short pass: FlushImages = %+v, %d requests", res, len(rec.requests()))
	}
	if !r.Images.RetryAt("p1").IsZero() {
		t.Fatal("an image never tried opened a window")
	}

	r.imagePass = 3 * time.Second
	if res := r.FlushImages(t.Context(), nil, cfg, false); res.Sent != 1 {
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
	done := make(chan Result)
	go func() { done <- r.FlushImages(t.Context(), nil, cfg, false) }()
	<-started
	second := r.FlushImages(t.Context(), nil, cfg, false)
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
	r.FlushImages(t.Context(), nil, cfg, false)
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
	for _, c := range []struct {
		name    string
		pass    context.Context
		err     error
		reached bool
		want    imageOutcome
	}{
		{"delivered", live, nil, true, imageDelivered},
		{"no time left to start it", live, errNoTimeLeft, false, imageDeferred},
		{"pass cancelled mid-upload", cancelled, &url.Error{Op: "Post", Err: context.Canceled}, true, imageDeferred},
		{"pass cancelled before the host", cancelled, &url.Error{Op: "Post", Err: context.Canceled}, false, imageDeferred},
		{"pass ran out mid-upload", expired, &url.Error{Op: "Post", Err: context.DeadlineExceeded}, true, imageStalled},
		{"pass ran out before the host", expired, &url.Error{Op: "Post", Err: context.DeadlineExceeded}, false, imageDeferred},
		{"own deadline mid-upload", live, &url.Error{Op: "Post", Err: context.DeadlineExceeded}, true, imageStalled},
		{"reset mid-upload", live, reset, true, imageStalled},
		{"cut off", live, &url.Error{Op: "Post", Err: io.EOF}, true, imageStalled},
		{"cut off mid-answer", live, &url.Error{Op: "Post", Err: io.ErrUnexpectedEOF}, true, imageStalled},
		{"host refused", live, &IngestError{Status: http.StatusServiceUnavailable}, true, imageFailed},
		{"dial", live, &url.Error{Op: "Post", Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}, false, imageFailed},
		{"dns", live, &url.Error{Op: "Post", Err: &net.DNSError{Err: "no such host", Name: "x"}}, false, imageFailed},
		{"tls certificate", live, &url.Error{Op: "Post", Err: &tls.CertificateVerificationError{}}, false, imageFailed},
		{"tls handshake timeout", live, &url.Error{Op: "Post", Err: timeoutError{}}, false, imageFailed},
		{"own deadline before the host", live, &url.Error{Op: "Post", Err: context.DeadlineExceeded}, false, imageFailed},
		{"something else", live, errors.New("odd"), true, imageFailed},
	} {
		if got := judgeImage(c.pass, c.err, c.reached); got != c.want {
			t.Errorf("%s: judgeImage = %v, want %v", c.name, got, c.want)
		}
	}
}
