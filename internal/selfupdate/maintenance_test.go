package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
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
			c := &Client{ReleaseKeys: testKeys(), BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0"}
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
			cache.CheckedAt = time.Now().Add(-CheckInterval + time.Minute)
			SaveCache(dir, cache)
			_ = noticeOf(c, context.Background(), dir, c.Version)
			if calls != 2 {
				t.Fatal("successful check did not hold for CheckInterval")
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
			sums := fmt.Appendf(nil, "%x  %s\n", sum, AssetName(runtime.GOOS, runtime.GOARCH))
			downloads, lookups := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/" + Repo + "/releases/latest":
					lookups++
					base := "http://" + r.Host
					_ = json.NewEncoder(w).Encode(Release{TagName: tag, PublishedAt: published, Assets: []Asset{{Name: AssetName(runtime.GOOS, runtime.GOARCH), URL: base + "/archive"}, {Name: "checksums.txt", URL: base + "/sums"}, {Name: SignatureName, URL: base + "/sig"}}})
				case "/sums":
					_, _ = w.Write(sums)
				case "/sig":
					_, _ = w.Write(testSign(tag, sums))
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
			started, err := os.Stat(exe)
			if err != nil {
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
			c := &Client{ReleaseKeys: testKeys(), BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0", Binary: started}
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
	cache, _ := c.cachedCheck(ctx, dir, current, CheckInterval)
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

// fakeReleases serves tag as the latest release, published long enough ago to have soaked,
// with a downloadable archive of binary; downloads counts the archive's fetches, and the
// first failLookups lookups fail.
func fakeReleases(t *testing.T, tag string, binary []byte, downloads *int, failLookups int) *httptest.Server {
	t.Helper()
	archive := archiveWith(t, "terma", binary)
	sum := sha256.Sum256(archive)
	sums := fmt.Appendf(nil, "%x  %s\n", sum, AssetName(runtime.GOOS, runtime.GOARCH))
	lookups := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		switch r.URL.Path {
		case "/repos/" + Repo + "/releases/latest":
			if lookups++; lookups <= failLookups {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(Release{TagName: tag, PublishedAt: time.Now().Add(-2 * SoakTime),
				Assets: []Asset{{Name: AssetName(runtime.GOOS, runtime.GOARCH), URL: base + "/archive"}, {Name: "checksums.txt", URL: base + "/sums"}, {Name: SignatureName, URL: base + "/sig"}}})
		case "/sums":
			_, _ = w.Write(sums)
		case "/sig":
			_, _ = w.Write(testSign(tag, sums))
		case "/archive":
			*downloads++
			_, _ = w.Write(archive)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// oldBinary is an installed terma, and the executable as a process of it started from it.
func oldBinary(t *testing.T) (string, os.FileInfo) {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "terma")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	started, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	return exe, started
}

// A release the last check found but that has since been pulled is not installed: the
// install looks the latest release up again, and the check then names that one.
func TestAPulledReleaseIsNeverInstalled(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("in-place updates are Unix-only")
	}
	downloads := 0
	srv := fakeReleases(t, "v1.0.0", []byte("new binary"), &downloads, 0)
	stateDir := t.TempDir()
	exe, started := oldBinary(t)
	SaveCache(stateDir, Cache{CheckedAt: time.Now(), Current: "1.0.0", Latest: "2.0.0", Published: time.Now().Add(-2 * SoakTime)})
	c := &Client{ReleaseKeys: testKeys(), BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0", Binary: started}
	o := c.Auto(context.Background(), t.TempDir(), stateDir, exe, nil)
	if o.Installed != "" || o.Err != nil || o.Notice != "" || downloads != 0 {
		t.Fatalf("Auto = %+v after %d downloads, want nothing installed or announced", o, downloads)
	}
	if got := LoadCache(stateDir).Latest; got != "1.0.0" {
		t.Fatalf("the check still names %s as the latest release", got)
	}
}

// A release whose install failed waits a day before it is tried again, but a newer release,
// such as the patch that fixes it, is tried at once.
func TestAFailedInstallHoldsBackOnlyThatRelease(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("in-place updates are Unix-only")
	}
	for _, tc := range []struct {
		attempted string
		installs  bool
	}{{"2.0.0", true}, {"2.0.1", false}} {
		t.Run(tc.attempted, func(t *testing.T) {
			downloads := 0
			srv := fakeReleases(t, "v2.0.1", []byte("new binary"), &downloads, 0)
			stateDir := t.TempDir()
			exe, started := oldBinary(t)
			SaveCache(stateDir, Cache{CheckedAt: time.Now(), Current: "2.0.0", Latest: "2.0.1",
				Published: time.Now().Add(-2 * SoakTime), AttemptAt: time.Now(), Attempted: tc.attempted})
			c := &Client{ReleaseKeys: testKeys(), BaseURL: srv.URL, HTTP: srv.Client(), Version: "2.0.0", Binary: started}
			o := c.Auto(context.Background(), t.TempDir(), stateDir, exe, nil)
			if installed := o.Installed == "2.0.1"; installed != tc.installs || installed != (downloads == 1) {
				t.Fatalf("Auto = %+v after %d downloads, with %s attempted today", o, downloads, tc.attempted)
			}
			// An install is recorded as the new version's check, which its first pass reuses.
			if cache := LoadCache(stateDir); tc.installs && cache.Current != "2.0.1" {
				t.Fatalf("after installing 2.0.1 the check records %q as current", cache.Current)
			}
		})
	}
}

// Failed lookups in a row space out: 15 minutes, doubling up to an hour.
func TestFailedLookupsBackOff(t *testing.T) {
	t.Parallel()
	for failures, want := range []time.Duration{RetryInterval, RetryInterval, 30 * time.Minute, time.Hour, time.Hour, time.Hour} {
		if got := retryAfter(failures); got != want {
			t.Errorf("retryAfter(%d) = %v, want %v", failures, got, want)
		}
	}
}

// The frequent check asks the latest-release page for its tag, and looks the release up in
// the rate-limited API only when that tag has moved, the page names no release, or the last
// check left no publish time to soak a release by. A page that refuses is a failed look,
// which backs off rather than falling through to the API.
func TestACheckLooksTheReleaseUpOnlyWhenTheTagMoved(t *testing.T) {
	t.Parallel()
	published := time.Now().Add(-2 * SoakTime)
	for _, tc := range []struct {
		name   string
		page   int
		tag    string
		cache  Cache
		lookup bool
	}{
		{"unchanged", http.StatusFound, "v1.0.1", Cache{Latest: "1.0.1", Published: published}, false},
		{"moved", http.StatusFound, "v1.0.2", Cache{Latest: "1.0.1", Published: published}, true},
		{"not a release", http.StatusFound, "nightly", Cache{Latest: "1.0.1", Published: published}, true},
		{"page says nothing", http.StatusOK, "", Cache{Latest: "1.0.1", Published: published}, true},
		{"page throttled", http.StatusTooManyRequests, "", Cache{Latest: "1.0.1", Published: published}, false},
		{"publish time unknown", http.StatusFound, "v1.0.1", Cache{Latest: "1.0.1"}, true},
		{"never checked", http.StatusFound, "v1.0.1", Cache{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lookups := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/" + Repo + "/releases/latest":
					if tc.tag != "" {
						// GitHub answers with an absolute URL.
						w.Header().Set("Location", "https://github.com/"+Repo+"/releases/tag/"+tc.tag)
					}
					w.WriteHeader(tc.page)
				case "/repos/" + Repo + "/releases/latest":
					lookups++
					_ = json.NewEncoder(w).Encode(Release{TagName: "v1.0.2", PublishedAt: published})
				}
			}))
			t.Cleanup(srv.Close)
			stateDir := t.TempDir()
			tc.cache.CheckedAt, tc.cache.Current = time.Now().Add(-CheckInterval-time.Minute), "1.0.0"
			SaveCache(stateDir, tc.cache)
			c := &Client{BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0"}
			cache, _ := c.cachedCheck(context.Background(), stateDir, "1.0.0", CheckInterval)
			if got := lookups == 1; got != tc.lookup {
				t.Fatalf("%d API lookups, want a lookup: %v", lookups, tc.lookup)
			}
			if failed := tc.page >= http.StatusBadRequest; cache.Failed != failed || cache.Failures != map[bool]int{true: 1}[failed] {
				t.Fatalf("the check records %+v, want failed: %v", cache, failed)
			}
			if time.Since(cache.CheckedAt) > time.Minute || !LoadCache(stateDir).CheckedAt.Equal(cache.CheckedAt) {
				t.Fatalf("the check was not recorded: %+v", cache)
			}
		})
	}
}

// The relay looks every CheckInterval; an interactive command, after it has finished, only
// every CommandCheckInterval, so a command seldom waits on the network.
func TestCommandsLookLessOftenThanTheRelay(t *testing.T) {
	t.Parallel()
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_ = json.NewEncoder(w).Encode(Release{TagName: "v1.0.0", PublishedAt: time.Now().Add(-2 * SoakTime)})
	}))
	t.Cleanup(srv.Close)
	stateDir := t.TempDir()
	SaveCache(stateDir, Cache{CheckedAt: time.Now().Add(-CheckInterval - time.Minute), Current: "1.0.0", Latest: "1.0.0"})
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0"}
	c.Maintain(context.Background(), t.TempDir(), stateDir, "terma", io.Discard)
	if requests != 0 {
		t.Fatalf("a command looked %d times within CommandCheckInterval", requests)
	}
	c.Auto(context.Background(), t.TempDir(), stateDir, "terma", nil)
	if requests == 0 {
		t.Fatal("the relay did not look once CheckInterval had passed")
	}
}

// A lookup that fails just before an install is retried as a failed check is, in 15
// minutes, not held back a day as a failed install would be.
func TestAFailedLookupBeforeAnInstallIsRetriedSoon(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("in-place updates are Unix-only")
	}
	downloads := 0
	srv := fakeReleases(t, "v2.0.0", []byte("new binary"), &downloads, 1)
	stateDir := t.TempDir()
	exe, started := oldBinary(t)
	SaveCache(stateDir, Cache{CheckedAt: time.Now(), Current: "1.0.0", Latest: "2.0.0", Published: time.Now().Add(-2 * SoakTime)})
	c := &Client{ReleaseKeys: testKeys(), BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0", Binary: started}
	if o := c.Auto(context.Background(), t.TempDir(), stateDir, exe, nil); o.Err == nil || o.Installed != "" {
		t.Fatalf("Auto = %+v, want the failed lookup reported", o)
	}
	cache := LoadCache(stateDir)
	if cache.Attempted != "" || !cache.Failed || cache.Failures != 1 {
		t.Fatalf("after a failed lookup the check records %+v, want a failed check and no attempt", cache)
	}
	// Within the backoff, no lookup at all: one would succeed here, and install.
	if o := c.Auto(context.Background(), t.TempDir(), stateDir, exe, nil); o.Installed != "" || o.Err != nil || downloads != 0 {
		t.Fatalf("Auto = %+v after %d downloads within the backoff, want no lookup", o, downloads)
	}
	cache.CheckedAt = time.Now().Add(-RetryInterval - time.Minute)
	SaveCache(stateDir, cache)
	if o := c.Auto(context.Background(), t.TempDir(), stateDir, exe, nil); o.Installed != "2.0.0" || downloads != 1 {
		t.Fatalf("Auto = %+v after %d downloads, want 2.0.0 installed once the retry is due", o, downloads)
	}
}

// A terma whose binary another install has since replaced is no longer what is installed:
// it installs nothing over it, even a release later than its own, which may be earlier than
// the one now in place. Nor does a process that does not know what it started from, however
// it is called: Maintain after an interactive command as much as the relay's Auto.
func TestAStaleProcessNeverReplacesTheInstalledBinary(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("in-place updates are Unix-only")
	}
	for _, tc := range []struct {
		name  string
		knows bool
	}{{"knows its binary", true}, {"does not know its binary", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			configDir, stateDir := t.TempDir(), t.TempDir()
			exe, started := oldBinary(t)
			newer, older := 0, 0
			latest := fakeReleases(t, "v1.0.3", []byte("1.0.3"), &newer, 0)
			withdrawn := fakeReleases(t, "v1.0.2", []byte("1.0.2"), &older, 0)
			// The relay installs 1.0.3 while a command of 1.0.1 is still running.
			relay := &Client{ReleaseKeys: testKeys(), BaseURL: latest.URL, HTTP: latest.Client(), Version: "1.0.1", Binary: started}
			if o := relay.Auto(context.Background(), configDir, stateDir, exe, nil); o.Installed != "1.0.3" || newer != 1 {
				t.Fatalf("the relay's install = %+v after %d downloads", o, newer)
			}
			// 1.0.3 is then pulled, and that command's updater runs: it must not put 1.0.2 in place.
			stale := &Client{ReleaseKeys: testKeys(), BaseURL: withdrawn.URL, HTTP: withdrawn.Client(), Version: "1.0.1"}
			if tc.knows {
				stale.Binary = started
			}
			var out bytes.Buffer
			stale.Maintain(context.Background(), configDir, stateDir, exe, &out)
			if got, _ := os.ReadFile(exe); string(got) != "1.0.3" || older != 0 {
				t.Fatalf("the installed binary is now %q after %d downloads (%s), want 1.0.3 left in place", got, older, &out)
			}
			if said := strings.Contains(out.String(), "updated while this command ran"); said != tc.knows {
				t.Fatalf("Maintain said %q; a process that knows its binary says it was updated meanwhile", &out)
			}
			if o := stale.Auto(context.Background(), configDir, stateDir, exe, nil); o.Installed != "" || o.Replaced != tc.knows || older != 0 {
				t.Fatalf("Auto = %+v after %d downloads, want nothing installed", o, older)
			}
		})
	}
}

// A release that another install, one taking no lock, put in place while the download ran
// is left there: the executable is checked again just before it would be replaced.
func TestAnInstallRefusesABinaryReplacedDuringTheDownload(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("in-place updates are Unix-only")
	}
	exe, started := oldBinary(t)
	archive := archiveWith(t, "terma", []byte("1.0.2"))
	sum := sha256.Sum256(archive)
	sums := fmt.Appendf(nil, "%x  %s\n", sum, AssetName(runtime.GOOS, runtime.GOARCH))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		switch r.URL.Path {
		case "/repos/" + Repo + "/releases/latest":
			_ = json.NewEncoder(w).Encode(Release{TagName: "v1.0.2", PublishedAt: time.Now().Add(-2 * SoakTime),
				Assets: []Asset{{Name: AssetName(runtime.GOOS, runtime.GOARCH), URL: base + "/archive"}, {Name: "checksums.txt", URL: base + "/sums"}, {Name: SignatureName, URL: base + "/sig"}}})
		case "/sums":
			_, _ = w.Write(sums)
		case "/sig":
			_, _ = w.Write(testSign("v1.0.2", sums))
		case "/archive":
			// install.sh puts 1.0.3 in place while the archive is on its way.
			next := exe + ".next"
			if err := os.WriteFile(next, []byte("1.0.3"), 0o755); err == nil {
				_ = os.Rename(next, exe)
			}
			_, _ = w.Write(archive)
		}
	}))
	t.Cleanup(srv.Close)
	c := &Client{ReleaseKeys: testKeys(), BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.1", Binary: started}
	stateDir := t.TempDir()
	o := c.Auto(context.Background(), t.TempDir(), stateDir, exe, nil)
	if got, _ := os.ReadFile(exe); !o.Replaced || o.Installed != "" || o.Err != nil || string(got) != "1.0.3" {
		t.Fatalf("Auto = %+v, binary now %q; want Replaced and 1.0.3 left in place", o, got)
	}
	// The release was not tried on the binary now in place, whose process may try it at once.
	if cache := LoadCache(stateDir); cache.Attempted != "" || !cache.AttemptAt.IsZero() {
		t.Fatalf("an install that lost to another installer left an attempt behind: %+v", cache)
	}
}
