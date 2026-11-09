package promptimage

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/semconv"
)

func encoded(t *testing.T, enc func(*bytes.Buffer, image.Image) error, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := enc(&b, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func pngOf(t *testing.T, w, h int) []byte {
	return encoded(t, func(b *bytes.Buffer, m image.Image) error { return png.Encode(b, m) }, w, h)
}

// webpHeader is a WebP's first 30 bytes: RIFF, the WEBP form, and its first chunk.
func webpHeader(chunk string, body []byte) []byte {
	b := append([]byte("RIFF\x00\x00\x00\x00WEBP"+chunk+"\x00\x00\x00\x00"), body...)
	for len(b) < 30 {
		b = append(b, 0)
	}
	return b
}

// An image is what its bytes are: a recorded type that is no image's gives way to the one
// the bytes declare, and each format's header names its size.
func TestImageReadsWhatTheBytesSay(t *testing.T) {
	t.Parallel()
	// Each chunk's data starts at byte 20: a frame tag, then the start code and the size.
	vp8 := make([]byte, 10)
	copy(vp8[3:], []byte{0x9d, 0x01, 0x2a})
	binary.LittleEndian.PutUint16(vp8[6:], 640)
	binary.LittleEndian.PutUint16(vp8[8:], 480)
	vp8l := make([]byte, 5)
	vp8l[0] = 0x2f
	binary.LittleEndian.PutUint32(vp8l[1:], uint32(799)|uint32(599)<<14)
	vp8x := make([]byte, 10)
	vp8x[4], vp8x[5] = 0xff, 0x03 // 1024 - 1, after the flags
	vp8x[7], vp8x[8] = 0xff, 0x02 // 768 - 1
	for _, tc := range []struct {
		name, recorded, mime string
		data                 []byte
		w, h                 int
	}{
		{"png", "image/png", "image/png", pngOf(t, 12, 7), 12, 7},
		{"jpeg as octets", "application/octet-stream", "image/jpeg", encoded(t, func(b *bytes.Buffer, m image.Image) error { return jpeg.Encode(b, m, nil) }, 5, 9), 5, 9},
		{"gif unrecorded", "", "image/gif", encoded(t, func(b *bytes.Buffer, m image.Image) error { return gif.Encode(b, m, nil) }, 3, 4), 3, 4},
		{"webp lossy", "image/webp", "image/webp", webpHeader("VP8 ", vp8), 640, 480},
		{"webp lossless", "image/webp", "image/webp", webpHeader("VP8L", vp8l), 800, 600},
		{"webp extended", "image/webp", "image/webp", webpHeader("VP8X", vp8x), 1024, 768},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img, ok := FromBase64(tc.recorded, base64.StdEncoding.EncodeToString(tc.data))
			if !ok || img.MIMEType != tc.mime {
				t.Fatalf("FromBase64 = %q, %v; want %q", img.MIMEType, ok, tc.mime)
			}
			if w, h, ok := img.Dimensions(); !ok || w != tc.w || h != tc.h {
				t.Fatalf("Dimensions = %d×%d, %v; want %d×%d", w, h, ok, tc.w, tc.h)
			}
		})
	}
}

// Only a base64 data URL of an image is one; text that says it is an image is not.
func TestFromDataURL(t *testing.T) {
	t.Parallel()
	data := base64.StdEncoding.EncodeToString(pngOf(t, 2, 2))
	if img, ok := FromDataURL("data:image/png;base64," + data); !ok || img.MIMEType != "image/png" || len(img.Data) == 0 {
		t.Fatalf("data URL = %+v, %v", img, ok)
	}
	for _, url := range []string{
		"https://example.com/a.png",
		"data:image/png," + data,
		"data:image/png;base64,!!!",
		"data:text/plain;base64," + base64.StdEncoding.EncodeToString([]byte("hello")),
	} {
		if _, ok := FromDataURL(url); ok {
			t.Errorf("%.40q read as an image", url)
		}
	}
}

// A record says what the image is; an image too large to carry says so instead of being kept.
func TestAttrsAndStore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	img := Image{Data: pngOf(t, 4, 3), MIMEType: "image/png", Label: "[Image #1]", Index: 1}
	a := img.Attrs()
	if a[semconv.TermaImageIDKey] != img.ID() || a[semconv.TermaImageSizeKey] != len(img.Data) ||
		a[semconv.TermaImageWidthKey] != 4 || a[semconv.TermaImageHeightKey] != 3 ||
		a[semconv.TermaImageLabelKey] != "[Image #1]" || a[semconv.TermaImageOmittedKey] != nil {
		t.Fatalf("attrs = %v", a)
	}
	if err := Store(dir, img); err != nil {
		t.Fatal(err)
	}
	got, ok := Load(dir, img.ID())
	if !ok || got != base64.StdEncoding.EncodeToString(img.Data) {
		t.Fatalf("Load = %v", ok)
	}
	Remove(dir, img.ID())
	if _, ok := Load(dir, img.ID()); ok {
		t.Fatal("a removed image loads")
	}

	big := Image{Data: append(pngOf(t, 1, 1), make([]byte, MaxBytes)...), MIMEType: "image/png", Index: 2}
	if big.Attrs()[semconv.TermaImageOmittedKey] != semconv.TermaImageOmittedTooLarge {
		t.Fatalf("big attrs = %v", big.Attrs()[semconv.TermaImageOmittedKey])
	}
	if err := Store(dir, big); err != nil {
		t.Fatal(err)
	}
	if _, ok := Load(dir, big.ID()); ok {
		t.Fatal("an image too large to send was kept")
	}
}

// Nothing but an image id names a file, and the store forgets images past their age.
func TestStoreKeepsToItsFolder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"../secret", "", "ABCDEF", "0123"} {
		if _, ok := Load(dir, id); ok || ValidID(id) {
			t.Errorf("%q loads", id)
		}
		Remove(dir, id)
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatal("Remove reached outside the store")
	}
	old, fresh := Image{Data: pngOf(t, 1, 2)}, Image{Data: pngOf(t, 2, 1)}
	for _, img := range []Image{old, fresh} {
		if err := Store(dir, img); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(Dir(dir), old.ID()), past, past); err != nil {
		t.Fatal(err)
	}
	Prune(dir, time.Now().Add(-time.Minute))
	if _, ok := Load(dir, old.ID()); ok {
		t.Error("an old image outlived the prune")
	}
	if _, ok := Load(dir, fresh.ID()); !ok {
		t.Error("a fresh image was pruned")
	}
}
