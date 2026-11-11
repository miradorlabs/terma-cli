package relay

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/shape"
)

// vendor is an agent's vendor: what each copy brought it, as sent.
type vendor struct {
	mu     sync.Mutex
	copies []vendorCopy
	srv    *httptest.Server
}

type vendorCopy struct {
	path   string
	header http.Header
	body   []byte
}

func newVendor(t *testing.T) *vendor {
	v := &vendor{}
	v.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		v.mu.Lock()
		v.copies = append(v.copies, vendorCopy{r.URL.Path, r.Header.Clone(), body})
		v.mu.Unlock()
	}))
	t.Cleanup(v.srv.Close)
	return v
}

func (v *vendor) received() []vendorCopy {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]vendorCopy(nil), v.copies...)
}

// An agent pointed at the relay still sends its vendor its own telemetry: the relay passes an
// exact copy on, the agent's own credential with it and never the relay's token, and routes the
// export as any other.
func TestMirrorCopiesTheExportToTheVendor(t *testing.T) {
	t.Parallel()
	u, v := newUpstream(t), newVendor(t)
	f := newFixture()
	f.mirrors = []shape.Mirror{{Prefix: "/muse-code/telemetry/", Upstream: v.srv.URL}}
	r, srv := f.relay(t, u, allPolicies(u))
	plain, _ := proto.Marshal(mixedLogs())
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	_, _ = zw.Write(plain)
	_ = zw.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/"+token+"/muse-code/telemetry/logs", bytes.NewReader(zipped.Bytes()))
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("User-Agent", "muse-build/1.4.15")
	req.Header.Set("Authorization", "Bearer vendor-credential")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the mirrored export was answered %d", resp.StatusCode)
	}
	waitFor(t, func() bool { return len(v.received()) == 1 })
	got := v.received()[0]
	if got.path != "/muse-code/telemetry/logs" || !bytes.Equal(got.body, zipped.Bytes()) {
		t.Errorf("the vendor's copy: %s, %d bytes, want the export as sent", got.path, len(got.body))
	}
	if got.header.Get("Authorization") != "Bearer vendor-credential" || got.header.Get("Content-Encoding") != "gzip" ||
		got.header.Get("User-Agent") != "muse-build/1.4.15" {
		t.Errorf("the vendor's copy carried %v", got.header)
	}
	waitForCounters(t, r, func(c map[string]int) bool { return c["received.logs"] > 0 && c["mirrored.sent"] == 1 })

	// The relay's own token, in a header, is never the vendor's to see.
	if code := post(t, srv, "/muse-code/telemetry/traces", []byte{}, "application/x-protobuf", token, false); code != http.StatusOK {
		t.Fatalf("a header-token export was answered %d", code)
	}
	waitFor(t, func() bool { return len(v.received()) == 2 })
	if h := v.received()[1].header.Get("Authorization"); h != "" {
		t.Errorf("the relay's token reached the vendor: %q", h)
	}

	// Without the relay's token nothing is copied: the relay is no open proxy to the vendor.
	if code := post(t, srv, "/muse-code/telemetry/logs", plain, "application/x-protobuf", "vendor-credential", false); code != http.StatusUnauthorized {
		t.Errorf("an export without the relay's token was answered %d", code)
	}
	if n := len(v.received()); n != 2 {
		t.Errorf("an unauthorized export was copied to the vendor: %d copies", n)
	}
}
