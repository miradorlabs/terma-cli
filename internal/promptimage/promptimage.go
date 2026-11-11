// Package promptimage is the images a user puts in an agent's prompt: what each one is, as a
// terma.prompt.image record says it, and its bytes, which wait under the state directory
// until delivery sends them one record at a time. The spool holds what the record says and
// never the bytes, so a few screenshots cannot crowd out a session's other events. Agents
// find the images in their own records; this package knows no agent.
package promptimage

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"image"
	_ "image/gif"  // DecodeConfig reads GIF headers
	_ "image/jpeg" // and JPEG's
	_ "image/png"  // and PNG's
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// DirName is the folder under the state directory that holds the stored images, one file
// per image, named by its id.
const DirName = "images"

// MaxBytes is the largest image a record carries: base64 makes it about 2.8 MB, under the
// 3.5 MB the platform takes in one record. A larger one is sent as what it is, without its bytes.
const MaxBytes = 2 << 20

// maxStoreBytes bounds the folder: past it the oldest images make room for a new one, and
// a record whose bytes went that way leaves without them.
const maxStoreBytes = 256 << 20

// Image is one image a user put in a prompt.
type Image struct {
	Data []byte
	// MIMEType is what the agent recorded; the bytes decide when it recorded none or no image type.
	MIMEType string
	// Label is what the prompt's text shows in the image's place, "" for nothing.
	Label string
	// Index is the image's position among its prompt's images, from 1.
	Index int
}

// FromBase64 is an image from base64 bytes and the media type an agent recorded beside them.
func FromBase64(mimeType, data string) (Image, bool) {
	b, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		if b, err = base64.RawStdEncoding.DecodeString(data); err != nil {
			return Image{}, false
		}
	}
	if len(b) == 0 {
		return Image{}, false
	}
	img := Image{Data: b, MIMEType: strings.ToLower(strings.TrimSpace(mimeType))}
	if sniffed := sniff(b); sniffed != "" && (img.MIMEType == "" || !strings.HasPrefix(img.MIMEType, "image/")) {
		img.MIMEType = sniffed
	}
	if !strings.HasPrefix(img.MIMEType, "image/") {
		return Image{}, false
	}
	return img, true
}

// FromDataURL is an image from a `data:<type>;base64,<bytes>` URL; any other URL is none.
func FromDataURL(url string) (Image, bool) {
	rest, ok := strings.CutPrefix(url, "data:")
	if !ok {
		return Image{}, false
	}
	meta, data, ok := strings.Cut(rest, ",")
	if !ok {
		return Image{}, false
	}
	mimeType, params, _ := strings.Cut(meta, ";")
	if !strings.Contains(";"+params+";", ";base64;") {
		return Image{}, false
	}
	return FromBase64(mimeType, data)
}

// sniff names the image type its first bytes declare, "" for none it knows.
func sniff(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(b, []byte("\xff\xd8\xff")):
		return "image/jpeg"
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return "image/gif"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

// ID is the SHA-256 of the image's bytes, in lowercase hex.
func (i Image) ID() string {
	sum := sha256.Sum256(i.Data)
	return hex.EncodeToString(sum[:])
}

// Dimensions are the width and height the image's header names; false when it names none
// this package reads (PNG, JPEG, GIF and WebP).
func (i Image) Dimensions() (width, height int, ok bool) {
	if sniff(i.Data) == "image/webp" {
		return webpDimensions(i.Data)
	}
	c, _, err := image.DecodeConfig(bytes.NewReader(i.Data))
	if err != nil || c.Width <= 0 || c.Height <= 0 {
		return 0, 0, false
	}
	return c.Width, c.Height, true
}

// webpDimensions reads a WebP's canvas from its first chunk: lossy (VP8), lossless (VP8L)
// or extended (VP8X).
func webpDimensions(b []byte) (int, int, bool) {
	if len(b) < 30 {
		return 0, 0, false
	}
	switch string(b[12:16]) {
	case "VP8 ":
		if b[23] != 0x9d || b[24] != 0x01 || b[25] != 0x2a {
			return 0, 0, false
		}
		w, h := int(binary.LittleEndian.Uint16(b[26:28])&0x3fff), int(binary.LittleEndian.Uint16(b[28:30])&0x3fff)
		return w, h, w > 0 && h > 0
	case "VP8L":
		if b[20] != 0x2f {
			return 0, 0, false
		}
		bits := binary.LittleEndian.Uint32(b[21:25])
		return int(bits&0x3fff) + 1, int(bits>>14&0x3fff) + 1, true
	case "VP8X":
		w := int(b[24]) | int(b[25])<<8 | int(b[26])<<16
		h := int(b[27]) | int(b[28])<<8 | int(b[29])<<16
		return w + 1, h + 1, true
	}
	return 0, 0, false
}

// Attrs are what a terma.prompt.image record says of the image, its bytes aside: delivery
// adds those from the store when the record leaves.
func (i Image) Attrs() map[string]any {
	attrs := map[string]any{
		semconv.TermaImageIDKey:       i.ID(),
		semconv.TermaImageIndexKey:    i.Index,
		semconv.TermaImageMimeTypeKey: i.MIMEType,
		semconv.TermaImageSizeKey:     len(i.Data),
	}
	if i.Label != "" {
		attrs[semconv.TermaImageLabelKey] = i.Label
	}
	if w, h, ok := i.Dimensions(); ok {
		attrs[semconv.TermaImageWidthKey] = w
		attrs[semconv.TermaImageHeightKey] = h
	}
	if len(i.Data) > MaxBytes {
		attrs[semconv.TermaImageOmittedKey] = semconv.TermaImageOmittedTooLarge
	}
	return attrs
}

// Dir is the image folder under the state directory stateDir.
func Dir(stateDir string) string { return filepath.Join(stateDir, DirName) }

// ValidID reports whether id is an image id, so it is safe as a file name.
func ValidID(id string) bool {
	if len(id) != sha256.Size*2 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Keep is what a terma.prompt.image record says of img, its bytes aside, and keeps those
// bytes under stateDir for delivery when collect says the team takes them: an image the
// policy withholds never leaves, so its bytes are not copied. An error is a store that could
// not keep them; the record still says what the image was.
func Keep(stateDir string, img Image, collect bool) (map[string]any, error) {
	attrs := img.Attrs()
	if !collect {
		return attrs, nil
	}
	return attrs, Store(stateDir, img)
}

// Store keeps the image's bytes under stateDir until delivery has sent every record that
// names them, once per id, the oldest images going first when the folder is full. An image
// over MaxBytes is never sent, so it is not kept.
func Store(stateDir string, img Image) error {
	if len(img.Data) == 0 || len(img.Data) > MaxBytes {
		return nil
	}
	dir := Dir(stateDir)
	path := filepath.Join(dir, img.ID())
	if _, err := os.Stat(path); err == nil {
		// Touched, so an image pasted again outlives the prune of the first paste, and a
		// pass already sending it keeps it for the new record.
		now := time.Now()
		return os.Chtimes(path, now, now)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	makeRoom(dir, int64(len(img.Data)), maxStoreBytes)
	return config.WriteFileAtomicNoSync(path, img.Data, 0o600)
}

// makeRoom removes the oldest images until n more bytes fit under limit.
func makeRoom(dir string, n, limit int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type stored struct {
		name string
		size int64
		mod  time.Time
	}
	var files []stored
	var used int64
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			files = append(files, stored{e.Name(), info.Size(), info.ModTime()})
			used += info.Size()
		}
	}
	if used+n <= limit {
		return
	}
	slices.SortFunc(files, func(a, b stored) int { return a.mod.Compare(b.mod) })
	for _, f := range files {
		if used+n <= limit {
			return
		}
		if os.Remove(filepath.Join(dir, f.name)) == nil {
			used -= f.size
		}
	}
}

// Load is the stored image id's bytes in base64, false when the store no longer has it.
func Load(stateDir, id string) (string, bool) {
	if !ValidID(id) {
		return "", false
	}
	b, err := os.ReadFile(filepath.Join(Dir(stateDir), id))
	if err != nil || len(b) == 0 {
		return "", false
	}
	return base64.StdEncoding.EncodeToString(b), true
}

// Sweep forgets the stored images ids that no record still waiting names (named false),
// unless stored or touched since since: a hook keeping one again while delivery ran has a
// record on its way that needs it.
func Sweep(stateDir string, ids []string, named func(id string) bool, since time.Time) {
	for _, id := range ids {
		if !ValidID(id) || named(id) {
			continue
		}
		path := filepath.Join(Dir(stateDir), id)
		if info, err := os.Stat(path); err == nil && info.ModTime().Before(since) {
			_ = os.Remove(path)
		}
	}
}

// Prune removes the images stored before before, the age past which the spool drops the
// records that name them; and temporary files a crashed write left behind.
func Prune(stateDir string, before time.Time) {
	dir := Dir(stateDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || !info.ModTime().Before(before) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}
