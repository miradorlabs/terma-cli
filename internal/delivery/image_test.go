package delivery

import (
	"context"
	"encoding/base64"
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
}

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.bodies = append(rec.bodies, string(b))
	if rec.refuse != nil && rec.refuse(string(b)) {
		w.WriteHeader(http.StatusServiceUnavailable)
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
// bytes from the store, which forgets them once sent; one whose bytes are gone says so, and
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
