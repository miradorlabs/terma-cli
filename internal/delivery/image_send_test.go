package delivery

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/promptimage"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

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
