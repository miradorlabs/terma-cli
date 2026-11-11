package delivery

import (
	"cmp"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

// imageEvent is a spooled terma.prompt.image record of img, as an agent's hook writes it.
func imageEvent(img promptimage.Image) spool.Event {
	attrs := img.Attrs()
	attrs[hookrun.AttrProjectID] = "p1"
	attrs[semconv.GenAIMainAgentNameKey] = "fake"
	return spool.Event{Repository: app, Name: semconv.TermaPromptImageEvent, SessionID: "s1", Attrs: attrs}
}

type recorder struct {
	mu     sync.Mutex
	bodies []string
	refuse func(body string) bool
	// status is what a refusal answers; 503 when unset.
	status int
}

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.bodies = append(rec.bodies, string(b))
	if rec.refuse != nil && rec.refuse(string(b)) {
		w.WriteHeader(cmp.Or(rec.status, http.StatusServiceUnavailable))
	}
}

func imageFixture(t *testing.T, rec *recorder, prompts bool) (Router, *spool.Spool, *config.Config, string) {
	t.Helper()
	cfgDir, stateDir := t.TempDir(), t.TempDir()
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	if err := keystore.Set(cfgDir, "p1", testKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	s, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pol := config.Policy{Mode: config.ModeRepo, Repositories: listed, TeamID: "p1", FetchedAt: time.Now(), IncludePrompts: prompts, IncludeToolContent: true}
	r := Router{ConfigDir: cfgDir, StateDir: stateDir, OTLPPinned: true,
		Policy:  func(context.Context, *config.Config, string) (config.Policy, error) { return pol, nil },
		Consent: func(string, hookrun.Consent) bool { return true }}
	return r, s, &config.Config{OTLPURL: srv.URL}, stateDir
}

// Each image leaves in a request of its own, after the project's other events, with its
// bytes from the store, which forgets them once no waiting record names them; one whose bytes are gone says so, and
// one too large to carry leaves as it was spooled.
func TestImagesLeaveOneToARequest(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	r, s, cfg, stateDir := imageFixture(t, rec, true)
	stored := promptimage.Image{Data: pixel, MIMEType: "image/png", Label: "[Image #1]", Index: 1}
	gone := promptimage.Image{Data: append(append([]byte{}, pixel...), 0), MIMEType: "image/png", Index: 2}
	big := promptimage.Image{Data: append(append([]byte{}, pixel...), make([]byte, promptimage.MaxBytes)...), MIMEType: "image/png", Index: 3}
	if err := promptimage.Store(stateDir, stored); err != nil {
		t.Fatal(err)
	}
	quota := spool.Event{Repository: app, Name: semconv.TermaSessionQuotaEvent, SessionID: "s1", Attrs: map[string]any{hookrun.AttrProjectID: "p1"}}
	if err := s.Append(imageEvent(stored), quota, imageEvent(gone), imageEvent(big)); err != nil {
		t.Fatal(err)
	}
	res := r.Flush(t.Context(), s, cfg, true, 0)
	if res.Sent != 4 || res.Err != nil {
		t.Fatalf("Flush = %+v", res)
	}
	if len(rec.bodies) != 4 {
		t.Fatalf("%d requests; want the batch and one per image", len(rec.bodies))
	}
	if !strings.Contains(rec.bodies[0], semconv.TermaSessionQuotaEvent) || strings.Contains(rec.bodies[0], semconv.TermaPromptImageEvent) {
		t.Fatalf("first request = %.200s", rec.bodies[0])
	}
	data := base64.StdEncoding.EncodeToString(pixel)
	for i, want := range []struct{ id, has, lacks string }{
		{stored.ID(), data, semconv.TermaImageOmittedKey},
		{gone.ID(), semconv.TermaImageOmittedUnavailable, semconv.TermaImageDataKey},
		{big.ID(), semconv.TermaImageOmittedTooLarge, semconv.TermaImageDataKey},
	} {
		body := rec.bodies[i+1]
		if strings.Count(body, `"eventName":"`+semconv.TermaPromptImageEvent+`"`) != 1 || !strings.Contains(body, want.id) || !strings.Contains(body, want.has) || strings.Contains(body, want.lacks) {
			t.Errorf("image %d request = %.300s", i+1, body)
		}
	}
	if _, ok := promptimage.Load(stateDir, stored.ID()); ok {
		t.Error("a sent image stayed in the store")
	}
}

// An image is what was said: a policy that withholds prompts withholds the whole record.
func TestImagesStayWhenPromptsDo(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	r, s, cfg, stateDir := imageFixture(t, rec, false)
	img := promptimage.Image{Data: pixel, MIMEType: "image/png", Index: 1}
	if err := promptimage.Store(stateDir, img); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(imageEvent(img)); err != nil {
		t.Fatal(err)
	}
	res := r.Flush(t.Context(), s, cfg, true, 0)
	if res.Sent != 0 || res.Withheld != 1 || len(rec.bodies) != 0 {
		t.Fatalf("Flush = %+v, %d requests", res, len(rec.bodies))
	}
	if _, ok := promptimage.Load(stateDir, img.ID()); ok {
		t.Error("a withheld image's bytes stayed in the store")
	}
}

// A refused image goes back to the spool as it came, its bytes left in the store for the
// next pass, and what was sent before it stays sent.
func TestARefusedImageKeepsItsBytesOutOfTheSpool(t *testing.T) {
	t.Parallel()
	rec := &recorder{refuse: func(body string) bool { return strings.Contains(body, semconv.TermaPromptImageEvent) }}
	r, s, cfg, stateDir := imageFixture(t, rec, true)
	img := promptimage.Image{Data: pixel, MIMEType: "image/png", Index: 1}
	if err := promptimage.Store(stateDir, img); err != nil {
		t.Fatal(err)
	}
	quota := spool.Event{Repository: app, Name: semconv.TermaSessionQuotaEvent, SessionID: "s1", Attrs: map[string]any{hookrun.AttrProjectID: "p1"}}
	if err := s.Append(quota, imageEvent(img)); err != nil {
		t.Fatal(err)
	}
	res := r.Flush(t.Context(), s, cfg, true, 0)
	if res.Sent != 1 || res.Err == nil {
		t.Fatalf("Flush = %+v", res)
	}
	left, err := s.Peek(10)
	if err != nil || len(left) != 1 || left[0].Name != semconv.TermaPromptImageEvent {
		t.Fatalf("spool holds %+v, %v", left, err)
	}
	if _, ok := left[0].Attrs[semconv.TermaImageDataKey]; ok {
		t.Fatal("the image's bytes went into the spool")
	}
	if _, ok := promptimage.Load(stateDir, img.ID()); !ok {
		t.Fatal("a refused image left the store")
	}
}

// The same image in two prompts is one stored file: sending the first record, in an earlier
// batch, leaves the bytes the second still needs.
func TestAnImageSentOnceStaysForItsNextRecord(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	r, s, cfg, stateDir := imageFixture(t, rec, true)
	img := promptimage.Image{Data: pixel, MIMEType: "image/png", Index: 1}
	if err := promptimage.Store(stateDir, img); err != nil {
		t.Fatal(err)
	}
	first, second := imageEvent(img), imageEvent(img)
	first.Attrs[semconv.TermaMessageIDKey], second.Attrs[semconv.TermaMessageIDKey] = "m1", "m2"
	events := []spool.Event{first}
	for i := range spool.DefaultBatch {
		events = append(events, spool.Event{Repository: app, Name: semconv.TermaSessionQuotaEvent, SessionID: fmt.Sprint("s", i), Attrs: map[string]any{hookrun.AttrProjectID: "p1"}})
	}
	if err := s.Append(append(events, second)...); err != nil {
		t.Fatal(err)
	}
	if res := r.Flush(t.Context(), s, cfg, true, 0); res.Err != nil {
		t.Fatalf("Flush = %+v", res)
	}
	data := base64.StdEncoding.EncodeToString(pixel)
	carried := 0
	for _, body := range rec.bodies {
		if strings.Contains(body, semconv.TermaPromptImageEvent) {
			if !strings.Contains(body, data) {
				t.Errorf("an image left without its bytes: %.300s", body)
			}
			carried++
		}
	}
	if carried != 2 {
		t.Fatalf("%d image requests, want 2", carried)
	}
	if _, ok := promptimage.Load(stateDir, img.ID()); ok {
		t.Error("an image every record has carried stayed in the store")
	}
}

// An image a record still waiting names keeps its bytes, though another record carried it.
func TestAWaitingRecordKeepsItsImage(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	r, s, cfg, stateDir := imageFixture(t, rec, true)
	img := promptimage.Image{Data: pixel, MIMEType: "image/png", Index: 1}
	if err := promptimage.Store(stateDir, img); err != nil {
		t.Fatal(err)
	}
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
	if err := s.Append(imageEvent(img), held); err != nil {
		t.Fatal(err)
	}
	if res := r.Flush(t.Context(), s, cfg, true, 0); res.Sent != 1 || res.Held != 1 {
		t.Fatalf("Flush = %+v", res)
	}
	if _, ok := promptimage.Load(stateDir, img.ID()); !ok {
		t.Fatal("the bytes a held record needs were forgotten")
	}
}

// An image the platform refuses outright goes again without its bytes, saying so, and holds
// up nothing after it.
func TestAnImageRefusedOutrightLeavesWithoutItsBytes(t *testing.T) {
	t.Parallel()
	data := base64.StdEncoding.EncodeToString(pixel)
	rec := &recorder{status: http.StatusBadRequest, refuse: func(body string) bool { return strings.Contains(body, data) }}
	r, s, cfg, stateDir := imageFixture(t, rec, true)
	img := promptimage.Image{Data: pixel, MIMEType: "image/png", Index: 1}
	if err := promptimage.Store(stateDir, img); err != nil {
		t.Fatal(err)
	}
	quota := spool.Event{Repository: app, Name: semconv.TermaSessionQuotaEvent, SessionID: "s1", Attrs: map[string]any{hookrun.AttrProjectID: "p1"}}
	if err := s.Append(imageEvent(img), quota); err != nil {
		t.Fatal(err)
	}
	res := r.Flush(t.Context(), s, cfg, true, 0)
	if res.Sent != 2 || res.Err != nil {
		t.Fatalf("Flush = %+v", res)
	}
	last := rec.bodies[len(rec.bodies)-1]
	if strings.Contains(last, data) || !strings.Contains(last, semconv.TermaImageOmittedRefused) {
		t.Fatalf("resent image = %.300s", last)
	}
	if left, _ := s.Peek(10); len(left) != 0 {
		t.Fatalf("spool holds %+v", left)
	}
}
