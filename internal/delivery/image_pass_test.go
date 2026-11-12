package delivery

import (
	"context"
	"encoding/base64"
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
	"github.com/miradorlabs/terma-cli/internal/promptimage"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

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

// An image spooled while a pass runs leaves in that same pass, not at the next hook.
func TestAnImageSpooledDuringAPassLeavesInIt(t *testing.T) {
	t.Parallel()
	first, later := pixelN(1), pixelN(2)
	var r Router
	var once sync.Once
	var mu sync.Mutex
	var bodies []string
	r, cfg, stateDir := imageFixture(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		// A hook spools another image while the first is on its way.
		once.Do(func() { queue(t, r.Images, imageEvent(later)) })
	}), true)
	store(t, stateDir, first, later)
	queue(t, r.Images, imageEvent(first))
	res := r.FlushImages(t.Context(), openSpool(t), cfg, false)
	if res.Sent != 2 || len(bodies) != 2 || !strings.Contains(bodies[1], later.ID()) {
		t.Fatalf("FlushImages = %+v, %d requests", res, len(bodies))
	}
	if left := queued(t, r.Images); len(left) != 0 {
		t.Fatalf("image queue holds %+v", left)
	}
}

// Images held for a key start no pass of their own for a while, so no events' flush starts
// one for them each time it runs; a pass still looks, and delivers once the key is there.
func TestHeldImagesWaitAWhile(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	r, cfg, _ := imageFixture(t, rec, true)
	img := imageEvent(pixelN(1))
	img.Attrs[hookrun.AttrProjectID] = "keyless"
	queue(t, r.Images, img)
	if res := r.FlushImages(t.Context(), openSpool(t), cfg, false); res.Held != 1 || len(res.Waiting) != 0 {
		t.Fatalf("FlushImages = %+v", res)
	}
	now := time.Now()
	if _, held := r.Images.Holds(now)["keyless"]; !held {
		t.Fatal("held images were not held")
	}
	if !r.Images.RetryAt("keyless").IsZero() {
		t.Fatal("held images opened a retry window")
	}
	if ImagesDue(r.Images, now) {
		t.Fatal("images held for a key are due")
	}
	if !ImagesDue(r.Images, now.Add(imageHeldWait+time.Minute)) {
		t.Fatal("held images are not due once their wait is over")
	}
	// The key arrives: the next pass, not forced, delivers.
	if err := keystore.Set(r.ConfigDir, "keyless", testKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	if res := r.FlushImages(t.Context(), openSpool(t), cfg, false); res.Sent != 1 {
		t.Fatalf("FlushImages once the key is there = %+v", res)
	}
	if _, held := r.Images.Holds(time.Now())["keyless"]; held {
		t.Fatal("a delivered project stayed held")
	}
}

// Every outcome has a rule, and every rule a count or a stall's name.
func TestEveryOutcomeHasARule(t *testing.T) {
	t.Parallel()
	for o := range imageOutcomes {
		if rule := imageRules[o]; rule.counter == nil && rule.window != stallWindow {
			t.Errorf("outcome %d has no rule, or counts nowhere", o)
		}
	}
}

// A project is named once in a pass, however many rounds see it: a failure in one round and
// its window in the next make one failure.
func TestAProjectIsNamedOncePerPass(t *testing.T) {
	t.Parallel()
	var r Router
	var once sync.Once
	r, cfg, stateDir := imageFixture(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.ReadAll(req.Body)
		once.Do(func() { queue(t, r.Images, imageEvent(pixelN(2))) })
		w.WriteHeader(http.StatusInternalServerError)
	}), true)
	store(t, stateDir, pixelN(1), pixelN(2))
	queue(t, r.Images, imageEvent(pixelN(1)))
	res := r.FlushImages(t.Context(), openSpool(t), cfg, false)
	if len(res.Failures) != 1 || len(res.Waiting) != 0 || res.Deferred != 1 {
		t.Fatalf("FlushImages = %+v", res)
	}
}
