package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// releaseHost serves a latest release (archive, checksums, signature, policy) the way
// GitHub does, by API and by download name.
type releaseHost struct {
	*httptest.Server
	tag       string
	minimum   string
	unsigned  bool
	badPolicy bool
	downloads atomic.Int64
	sign      func([]byte) []byte
	archive   []byte
}

func newReleaseHost(t *testing.T, tag, minimum string) *releaseHost {
	t.Helper()
	h := &releaseHost{tag: tag, minimum: minimum, sign: testSigner(t), archive: platformArchive(t, []byte("new binary"))}
	h.Server = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.Close)
	return h
}

func (h *releaseHost) policy() []byte {
	return []byte(fmt.Sprintf(`{"min_version": %q}`, h.minimum))
}

func (h *releaseHost) sums() []byte {
	a := sha256.Sum256(h.archive)
	policy := h.policy()
	if h.badPolicy {
		policy = []byte(`{"min_version": "0.0.1"}`)
	}
	p := sha256.Sum256(policy)
	return []byte(fmt.Sprintf("%x  %s\n%x  %s\n", a, AssetName(runtime.GOOS, runtime.GOARCH), p, PolicyFile))
}

func (h *releaseHost) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/repos/" + Repo + "/releases/latest":
		base := "http://" + r.Host + "/dl"
		assets := []Asset{{Name: AssetName(runtime.GOOS, runtime.GOARCH), URL: base + "/archive"}, {Name: "checksums.txt", URL: base + "/checksums.txt"}}
		if !h.unsigned {
			assets = append(assets, Asset{Name: "checksums.txt.sig", URL: base + "/checksums.txt.sig"})
		}
		_ = json.NewEncoder(w).Encode(Release{TagName: h.tag, Assets: assets})
	case "/dl/archive":
		h.downloads.Add(1)
		_, _ = w.Write(h.archive)
	case "/dl/checksums.txt":
		_, _ = w.Write(h.sums())
	case "/dl/checksums.txt.sig":
		if h.unsigned {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(h.sign(h.sums()))
	case "/dl/policy.json":
		_, _ = w.Write(h.policy())
	default:
		http.NotFound(w, r)
	}
}

func (h *releaseHost) client(version string) *Client {
	return &Client{BaseURL: h.URL, DownloadURL: h.URL + "/dl", HTTP: h.Client(), Version: version}
}

func oldBinary(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "terma")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func binaryIs(t *testing.T, exe, want string) {
	t.Helper()
	if got, _ := os.ReadFile(exe); string(got) != want {
		t.Fatalf("binary is %q, want %q", got, want)
	}
}

func TestApplyRefusesAnUnsignedRelease(t *testing.T) {
	h := newReleaseHost(t, "v2.0.0", "0.0.0")
	h.unsigned = true
	c := h.client("1.0.0")
	rel, err := c.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	exe := oldBinary(t)
	if _, err := c.Apply(context.Background(), rel, exe, nil); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("an unsigned release: %v", err)
	}
	binaryIs(t, exe, "old binary")

	// Signed, but by a key this build does not trust.
	h.unsigned = false
	_ = testSigner(t) // trust a different key from the one the host signs with
	rel, _ = c.Latest(context.Background())
	if _, err := c.Apply(context.Background(), rel, exe, nil); err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("a release signed by another key: %v", err)
	}
	binaryIs(t, exe, "old binary")
	if n := h.downloads.Load(); n != 0 {
		t.Fatalf("downloaded the archive %d times before the signature was checked", n)
	}
}

func TestFetchPolicyTrustsOnlySignedChecksums(t *testing.T) {
	h := newReleaseHost(t, "v2.0.0", "1.5.0")
	c := h.client("1.0.0")
	p, err := c.FetchPolicy(context.Background())
	if err != nil || p.MinVersion != "1.5.0" {
		t.Fatalf("policy %+v, %v", p, err)
	}
	if !BelowMinimum("1.0.0", p) || BelowMinimum("1.5.0", p) || BelowMinimum("dev", p) {
		t.Fatal("BelowMinimum: 1.0.0 is below 1.5.0; 1.5.0 and a source build are not")
	}
	h.badPolicy = true
	if _, err := c.FetchPolicy(context.Background()); err == nil {
		t.Fatal("a policy that does not match the signed checksums was trusted")
	}
	h.badPolicy, h.unsigned = false, true
	if _, err := c.FetchPolicy(context.Background()); err == nil {
		t.Fatal("a policy from an unsigned release was trusted")
	}
}

func TestBackgroundUpdatesByDefault(t *testing.T) {
	h := newReleaseHost(t, "v2.0.0", "0.0.0")
	dir, exe := t.TempDir(), oldBinary(t)
	if got := h.client("1.0.0").Background(context.Background(), dir, exe, t.Logf); got != "2.0.0" {
		t.Fatalf("installed %q, want 2.0.0 with no saved preference", got)
	}
	binaryIs(t, exe, "new binary")
}

func TestBackgroundRespectsTheOptOutAndPackageManagers(t *testing.T) {
	h := newReleaseHost(t, "v2.0.0", "1.5.0")
	dir, exe := t.TempDir(), oldBinary(t)
	if err := SavePreferences(dir, Preferences{Auto: false}); err != nil {
		t.Fatal(err)
	}
	if got := h.client("1.0.0").Background(context.Background(), dir, exe, t.Logf); got != "" {
		t.Fatalf("an opted-out machine installed %s", got)
	}
	binaryIs(t, exe, "old binary")
	// Below the minimum and opted out: warned, never forced.
	if w := MinimumWarning(dir, "1.0.0"); !strings.Contains(w, "oldest supported version (1.5.0)") || !strings.Contains(w, "`terma update`") {
		t.Fatalf("warning = %q", w)
	}
	if w := MinimumWarning(dir, "1.5.0"); w != "" {
		t.Fatalf("a supported version was warned: %q", w)
	}

	managed := filepath.Join(t.TempDir(), "Cellar", "terma", "1.0.0", "bin", "terma")
	if err := os.MkdirAll(filepath.Dir(managed), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managed, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := h.client("1.0.0").Background(context.Background(), t.TempDir(), managed, t.Logf); got != "" {
		t.Fatalf("a Homebrew-owned binary was replaced with %s", got)
	}
}

func TestBackgroundBelowTheMinimumSkipsTheDailyThrottle(t *testing.T) {
	h := newReleaseHost(t, "v2.0.0", "2.0.0")
	dir, exe := t.TempDir(), oldBinary(t)
	// An attempt an hour ago would hold an ordinary update back for a day.
	SaveCache(dir, Cache{CheckedAt: time.Now(), Latest: "2.0.0", Current: "1.0.0", AttemptAt: time.Now().Add(-time.Hour)})
	if got := h.client("1.0.0").Background(context.Background(), dir, exe, t.Logf); got != "2.0.0" {
		t.Fatalf("below the minimum installed %q, want 2.0.0 at once", got)
	}
}

func TestBackgroundNeverReplacesASourceBuild(t *testing.T) {
	h := newReleaseHost(t, "v2.0.0", "9.0.0")
	exe := oldBinary(t)
	if got := h.client("v0.1.5-9-g28e96aa").Background(context.Background(), t.TempDir(), exe, t.Logf); got != "" {
		t.Fatalf("a source build was replaced with %s", got)
	}
	binaryIs(t, exe, "old binary")
}
