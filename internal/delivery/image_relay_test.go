package delivery

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
)

// relayFor is the relay engine as `terma relay run` wires the image sender into it
// (cli's relayImages), with a test's token.
func relayFor(t *testing.T, s *ImageSender) (*relay.Relay, *httptest.Server) {
	t.Helper()
	r := relay.New(relay.Options{Dir: t.TempDir(), Token: "test-token",
		Accepted: s.Nudge, Halt: s.TryHalt, Resume: s.Resume})
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	return r, srv
}

// export is an agent's empty log export, which the relay accepts.
func export(t *testing.T, srv *httptest.Server) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/logs", bytes.NewReader([]byte(`{"resourceLogs":[]}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export = %d", resp.StatusCode)
	}
}

// An update's quiesce, asked while an image uploads, halts the sender at the image's end;
// an export then keeps the relay, the halt is resumed, and the next image goes.
func TestAQuiesceAnExportUndoesSendsTheNextImage(t *testing.T) {
	t.Parallel()
	s, rec := twoImagesHaltedMidUploadBy(t, func(r *relay.Relay) bool { return r.Quiesce(0) })
	r := s.relay
	waitFor(t, "the halt at the image's end", func() bool { return s.stopped() })
	export(t, s.srv) // an agent's turn ends: the relay is kept
	if r.Quiesce(time.Minute) {
		t.Fatal("quiesced with a fresh export")
	}
	waitFor(t, "the second image", func() bool { return len(rec.requests()) == 2 })
}

// An idle exit's try while an image uploads halts nothing: the next image goes.
func TestAnIdleExitTriedMidUploadHaltsNothing(t *testing.T) {
	t.Parallel()
	_, rec := twoImagesHaltedMidUploadBy(t, func(r *relay.Relay) bool { return r.TryIdleExit(time.Nanosecond) })
	waitFor(t, "the second image", func() bool { return len(rec.requests()) == 2 })
}

// senderInRelay is a sender wired into a relay engine, as relayImages wires it.
type senderInRelay struct {
	*ImageSender
	relay *relay.Relay
	srv   *httptest.Server
}

// twoImagesHaltedMidUploadBy runs a sender wired into a relay, with two images for p1 and
// the first one's upload held; while it is held, ask asks the relay to stop, which must not
// succeed mid-upload; then the upload is let go.
func twoImagesHaltedMidUploadBy(t *testing.T, ask func(*relay.Relay) bool) (senderInRelay, *recorder) {
	t.Helper()
	held := newHeldUpload()
	r, cfg, stateDir := imageFixture(t, held, true)
	t.Cleanup(held.free)
	first, second := pixelN(1), pixelN(2)
	store(t, stateDir, first, second)
	queue(t, r.Images, imageEvent(first), imageEvent(second))
	s := NewImageSender(r, openSpool(t), func() (*config.Config, error) { return cfg, nil })
	engine, srv := relayFor(t, s)
	runSender(t, s)
	<-held.arrived
	if ask(engine) {
		t.Fatal("the relay may stop mid-upload")
	}
	held.free()
	return senderInRelay{ImageSender: s, relay: engine, srv: srv}, held.rec
}

// heldUpload is an ingest host that holds its first request until free, recording all.
type heldUpload struct {
	rec            *recorder
	arrived        chan struct{}
	release        chan struct{}
	arrive, freeOn sync.Once
}

func newHeldUpload() *heldUpload {
	return &heldUpload{rec: &recorder{}, arrived: make(chan struct{}), release: make(chan struct{})}
}

func (h *heldUpload) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	h.arrive.Do(func() {
		close(h.arrived)
		<-h.release
	})
	h.rec.ServeHTTP(w, req)
}

// free lets the held request go.
func (h *heldUpload) free() { h.freeOn.Do(func() { close(h.release) }) }
