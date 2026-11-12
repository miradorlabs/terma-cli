package delivery

import (
	"cmp"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
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
