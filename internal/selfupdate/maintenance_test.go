package selfupdate

import (
	"bytes"
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
	"testing"
	"time"
)

func TestUnversionedBuildsAreNotReleases(t *testing.T) {
	for _, v := range []string{"dev", "132b086", "8d6dd91-dirty", "v1.2.3-17-g132b086", "v1.2.3-dirty", "garbage", "1.2", ""} {
		if IsRelease(v) || Newer(v, "v9.0.0") {
			t.Errorf("%q treated as a release", v)
		}
	}
	for _, pair := range [][2]string{{"1.2.3-rc.2", "1.2.3-rc.10"}, {"1.2.3-alpha.9", "1.2.3-beta"}, {"1.2.3-beta", "1.2.3"}} {
		if !Newer(pair[0], pair[1]) || Newer(pair[1], pair[0]) {
			t.Errorf("incorrect ordering: %v", pair)
		}
	}
	if Newer("1.2.3", "garbage") || Newer("1.2.3", "1.2.3+build.2") {
		t.Fatal("invalid candidate or metadata changed ordering")
	}
}

func TestFailedCheckRetriesAfter15Minutes(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if calls == 1 {
					w.WriteHeader(status)
					return
				}
				fmt.Fprint(w, `{"tag_name":"v2.0.0","assets":[]}`)
			}))
			defer srv.Close()
			c := &Client{BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0"}
			dir := t.TempDir()
			// A failed refresh must retry even when an earlier successful result exists.
			SaveCache(dir, Cache{CheckedAt: time.Now().Add(-25 * time.Hour), Latest: "1.0.0"})
			_ = c.Notice(context.Background(), dir, c.Version)
			cache := LoadCache(dir)
			if calls != 1 || !cache.Failed {
				t.Fatalf("failure not recorded: %+v, calls %d", cache, calls)
			}
			cache.CheckedAt = time.Now().Add(-14 * time.Minute)
			SaveCache(dir, cache)
			_ = c.Notice(context.Background(), dir, c.Version)
			if calls != 1 {
				t.Fatal("retried before 15 minutes")
			}
			cache.CheckedAt = time.Now().Add(-16 * time.Minute)
			SaveCache(dir, cache)
			if msg := c.Notice(context.Background(), dir, c.Version); calls != 2 || !strings.Contains(msg, "2.0.0") {
				t.Fatalf("did not recover after 15 minutes: calls %d, %q", calls, msg)
			}
			cache = LoadCache(dir)
			if cache.Failed {
				t.Fatal("successful retry retained failure state")
			}
			cache.CheckedAt = time.Now().Add(-16 * time.Minute)
			SaveCache(dir, cache)
			_ = c.Notice(context.Background(), dir, c.Version)
			if calls != 2 {
				t.Fatal("successful check did not retain daily interval")
			}
		})
	}
}

func TestMaintainRequiresOptInAndVerifiesUpdates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("in-place updates are Unix-only")
	}
	for _, mode := range []string{"notify", "auto", "tampered", "locked", "managed"} {
		t.Run(mode, func(t *testing.T) {
			binary := []byte("new binary")
			archive := archiveWith(t, "terma", binary)
			sum := sha256.Sum256(archive)
			downloads, lookups := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/" + Repo + "/releases/latest":
					lookups++
					base := "http://" + r.Host
					_ = json.NewEncoder(w).Encode(Release{TagName: "v2.0.0", Assets: []Asset{{Name: AssetName(runtime.GOOS, runtime.GOARCH), URL: base + "/archive"}, {Name: "checksums.txt", URL: base + "/sums"}}})
				case "/sums":
					fmt.Fprintf(w, "%x  %s\n", sum, AssetName(runtime.GOOS, runtime.GOARCH))
				case "/archive":
					downloads++
					if mode == "tampered" {
						fmt.Fprint(w, "tampered")
					} else {
						_, _ = w.Write(archive)
					}
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			dir := t.TempDir()
			exe := filepath.Join(t.TempDir(), "terma")
			if mode == "managed" {
				exe = filepath.Join(t.TempDir(), "Cellar", "terma", "1.0.0", "bin", "terma")
				if err := os.MkdirAll(filepath.Dir(exe), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(exe, []byte("old binary"), 0755); err != nil {
				t.Fatal(err)
			}
			if mode != "notify" {
				if err := SavePreferences(dir, Preferences{Auto: true}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "locked" {
				unlock, err := Lock(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
			}
			c := &Client{BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0"}
			var out bytes.Buffer
			for range 2 {
				c.Maintain(context.Background(), dir, exe, &out)
			}
			got, err := os.ReadFile(exe)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "auto" {
				if !bytes.Equal(got, binary) || downloads != 1 {
					t.Fatalf("update missing/repeated: %q, downloads %d, %s", got, downloads, &out)
				}
			} else if string(got) != "old binary" {
				t.Fatal("binary replaced without successful authorized update")
			}
			if mode == "tampered" && (downloads != 1 || !strings.Contains(out.String(), "checksum mismatch")) {
				t.Fatalf("failed update not reported/throttled: %d, %s", downloads, &out)
			}
			if mode == "locked" && lookups != 0 {
				t.Fatal("contended updater still made requests")
			}
			if mode == "managed" && (downloads != 0 || !strings.Contains(out.String(), "brew upgrade terma")) {
				t.Fatalf("managed installation: %s", &out)
			}
			if mode == "notify" && (downloads != 0 || !strings.Contains(out.String(), "terma update")) {
				t.Fatalf("notification: %s", &out)
			}
		})
	}
}

func TestManualCacheTimestampIsReusable(t *testing.T) {
	dir := t.TempDir()
	SaveCache(dir, Cache{CheckedAt: time.Now(), Current: "1.0.0", Latest: "2.0.0"})
	c := &Client{BaseURL: "http://invalid.invalid", Version: "1.0.0"}
	if msg := c.Notice(context.Background(), dir, c.Version); !strings.Contains(msg, "2.0.0") {
		t.Fatal(msg)
	}
}
