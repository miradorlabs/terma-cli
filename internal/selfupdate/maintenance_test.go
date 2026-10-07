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
	t.Parallel()
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
	t.Parallel()
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
			_ = noticeOf(c, context.Background(), dir, c.Version)
			cache := LoadCache(dir)
			if calls != 1 || !cache.Failed {
				t.Fatalf("failure not recorded: %+v, calls %d", cache, calls)
			}
			cache.CheckedAt = time.Now().Add(-14 * time.Minute)
			SaveCache(dir, cache)
			_ = noticeOf(c, context.Background(), dir, c.Version)
			if calls != 1 {
				t.Fatal("retried before 15 minutes")
			}
			cache.CheckedAt = time.Now().Add(-16 * time.Minute)
			SaveCache(dir, cache)
			if msg := noticeOf(c, context.Background(), dir, c.Version); calls != 2 || !strings.Contains(msg, "2.0.0") {
				t.Fatalf("did not recover after 15 minutes: calls %d, %q", calls, msg)
			}
			cache = LoadCache(dir)
			if cache.Failed {
				t.Fatal("successful retry retained failure state")
			}
			cache.CheckedAt = time.Now().Add(-16 * time.Minute)
			SaveCache(dir, cache)
			_ = noticeOf(c, context.Background(), dir, c.Version)
			if calls != 2 {
				t.Fatal("successful check did not retain daily interval")
			}
		})
	}
}

// Automatic updates are on unless turned off: "auto" saves no preference at all.
func TestMaintainUpdatesByDefaultAndVerifiesUpdates(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("in-place updates are Unix-only")
	}
	// "soaking" is a major release published just now, which waits; "patch" is a patch
	// release published just now, which does not.
	for _, mode := range []string{"notify", "auto", "tampered", "locked", "managed", "soaking", "patch"} {
		t.Run(mode, func(t *testing.T) {
			tag, published := "v2.0.0", time.Now().Add(-SoakTime-time.Hour)
			switch mode {
			case "soaking":
				published = time.Now().Add(-time.Hour)
			case "patch":
				tag, published = "v1.0.1", time.Now()
			}
			binary := []byte("new binary")
			archive := archiveWith(t, "terma", binary)
			sum := sha256.Sum256(archive)
			downloads, lookups := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/" + Repo + "/releases/latest":
					lookups++
					base := "http://" + r.Host
					_ = json.NewEncoder(w).Encode(Release{TagName: tag, PublishedAt: published, Assets: []Asset{{Name: AssetName(runtime.GOOS, runtime.GOARCH), URL: base + "/archive"}, {Name: "checksums.txt", URL: base + "/sums"}}})
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
			configDir, stateDir := t.TempDir(), t.TempDir()
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
			if mode == "notify" {
				if err := SavePreferences(configDir, Preferences{Auto: false}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "locked" {
				unlock, err := Lock(stateDir)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
			}
			c := &Client{BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0"}
			var out bytes.Buffer
			for range 2 {
				c.Maintain(context.Background(), configDir, stateDir, exe, &out)
			}
			got, err := os.ReadFile(exe)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "auto" || mode == "patch" {
				if !bytes.Equal(got, binary) || downloads != 1 {
					t.Fatalf("update missing/repeated: %q, downloads %d, %s", got, downloads, &out)
				}
			} else if string(got) != "old binary" {
				t.Fatal("binary replaced without successful authorized update")
			}
			if mode == "tampered" && (downloads != 1 || !strings.Contains(out.String(), "checksum mismatch")) {
				t.Fatalf("failed update not reported/throttled: %d, %s", downloads, &out)
			}
			// The opt-in is a setting; the check's record is state.
			if checked := !LoadCache(stateDir).CheckedAt.IsZero(); checked == (mode == "locked") || !LoadCache(configDir).CheckedAt.IsZero() {
				t.Fatalf("check recorded in the state directory: %v, in the config directory: %v", checked, !LoadCache(configDir).CheckedAt.IsZero())
			}
			if mode == "locked" && lookups != 0 {
				t.Fatal("contended updater still made requests")
			}
			// The notice names `terma update` for a managed installation too; nothing is downloaded.
			if mode == "managed" && (downloads != 0 || !strings.Contains(out.String(), "Run `terma update`")) {
				t.Fatalf("managed installation: %s", &out)
			}
			if (mode == "notify" || mode == "soaking") && (downloads != 0 || !strings.Contains(out.String(), "terma update")) {
				t.Fatalf("notification: %s", &out)
			}
		})
	}
}

func TestManualCacheTimestampIsReusable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	SaveCache(dir, Cache{CheckedAt: time.Now(), Current: "1.0.0", Latest: "2.0.0"})
	c := &Client{BaseURL: "http://invalid.invalid", Version: "1.0.0"}
	if msg := noticeOf(c, context.Background(), dir, c.Version); !strings.Contains(msg, "2.0.0") {
		t.Fatal(msg)
	}
}

// noticeOf is the update notice c's cached check gives for current.
func noticeOf(c *Client, ctx context.Context, dir, current string) string {
	if !IsRelease(current) {
		return ""
	}
	cache, _ := c.cachedCheck(ctx, dir, current)
	return notice(cache, current)
}

func TestPreferencesDefaultOnAndRecordOnlyTheOptOut(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if p, err := LoadPreferences(dir); err != nil || !p.Auto {
		t.Fatalf("no choice recorded: %+v %v, want automatic updates", p, err)
	}
	if err := SavePreferences(dir, Preferences{Auto: false}); err != nil {
		t.Fatal(err)
	}
	if p, _ := LoadPreferences(dir); p.Auto {
		t.Fatal("the opt-out did not stick")
	}
	if err := SavePreferences(dir, Preferences{Auto: true}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "auto_update") {
		t.Fatalf("turning updates back on left a choice behind: %s", data)
	}
	if p, _ := LoadPreferences(dir); !p.Auto {
		t.Fatal("turned back on, but off")
	}
}

func TestOnlyMinorAndMajorReleasesSoak(t *testing.T) {
	t.Parallel()
	now := time.Now()
	fresh, soaked := now.Add(-time.Hour), now.Add(-SoakTime)
	for _, c := range []struct {
		current, candidate string
		published          time.Time
		want               bool
	}{
		{"1.2.3", "1.2.4", fresh, false},
		{"1.2.3", "1.2.4", time.Time{}, false},
		{"1.2.3", "1.3.0", fresh, true},
		{"1.2.3", "2.0.0", fresh, true},
		{"1.2.3", "1.3.0", soaked, false},
		{"1.2.3", "2.0.0", soaked, false},
		// A check recorded before releases' dates were waits for the next one.
		{"1.2.3", "1.3.0", time.Time{}, true},
		{"v1.2.3", "v1.3.0-rc.1", fresh, true},
	} {
		if got := Soaking(c.current, c.candidate, c.published, now); got != c.want {
			t.Errorf("Soaking(%s → %s, published %s ago) = %v, want %v", c.current, c.candidate, now.Sub(c.published), got, c.want)
		}
	}
}
