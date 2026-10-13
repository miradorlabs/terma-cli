package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// publishing is a fake GitHub whose latest release changes between an updater's passes, as
// releases do under a relay that keeps running.
type publishing struct {
	mu sync.Mutex
	// tag is the latest release's, published just now: only a patch installs without a soak.
	tag string
	// built says whether its build has uploaded its files yet.
	built bool
	// refuse, set, is the status every download answers with, as a throttled address gets.
	refuse int
	// lookups counts the API's answers, probes the downloads asked after without the API.
	lookups, probes int
	// agents are the User-Agents that asked.
	agents map[string]bool
}

// binaryOf is the terma a release tagged tag installs.
func binaryOf(tag string) []byte { return []byte("terma " + tag) }

// serve starts the fake, with tag as its latest release.
func (p *publishing) serve(t *testing.T, tag string, built bool) *httptest.Server {
	t.Helper()
	p.tag, p.built, p.agents = tag, built, map[string]bool{}
	archiveName := AssetName(runtime.GOOS, runtime.GOARCH)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.agents[r.Header.Get("User-Agent")] = true
		base := "http://" + r.Host
		archive := archiveWith(t, "terma", binaryOf(p.tag))
		sum := sha256.Sum256(archive)
		sums := fmt.Appendf(nil, "%x  %s\n", sum, archiveName)
		switch name, download := strings.CutPrefix(r.URL.Path, "/"+Repo+"/releases/download/"+p.tag+"/"); {
		case r.URL.Path == "/"+Repo+"/releases/latest":
			w.Header().Set("Location", "https://github.com/"+Repo+"/releases/tag/"+p.tag)
			w.WriteHeader(http.StatusFound)
		case download:
			p.probes++
			switch {
			case p.refuse != 0:
				w.WriteHeader(p.refuse)
			case !p.built:
				w.WriteHeader(http.StatusNotFound)
			default:
				w.Header().Set("Location", "https://release-assets.githubusercontent.com/"+name)
				w.WriteHeader(http.StatusFound)
			}
		case r.URL.Path == "/repos/"+Repo+"/releases/latest":
			p.lookups++
			rel := Release{TagName: p.tag, PublishedAt: time.Now()}
			if p.built {
				rel.Assets = []Asset{{Name: archiveName, URL: base + "/archive", State: "uploaded"},
					{Name: checksumsName, URL: base + "/sums", State: "uploaded"}, {Name: SignatureName, URL: base + "/sig", State: "uploaded"}}
			}
			_ = json.NewEncoder(w).Encode(rel)
		case r.URL.Path == "/sums":
			_, _ = w.Write(sums)
		case r.URL.Path == "/sig":
			_, _ = w.Write(testSign(p.tag, sums))
		case r.URL.Path == "/archive":
			_, _ = w.Write(archive)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// publish makes tag the latest release, built or still waiting for its build.
func (p *publishing) publish(tag string, built bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tag, p.built = tag, built
}

// asked is how many API lookups and download probes the fake has answered.
func (p *publishing) asked() (lookups, probes int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lookups, p.probes
}

// askedAs are the User-Agents the fake has answered.
func (p *publishing) askedAs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Sorted(maps.Keys(p.agents))
}

// installedAt is the terma now at exe.
func installedAt(t *testing.T, exe string) string {
	t.Helper()
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A process that installed a release goes on as it: the relay, which keeps running until its
// agents pause, installs the release after it too, rather than taking its own install for
// another's and installing nothing more until it restarts. It looks no more often for that,
// and still says which build is asking.
func TestAProcessInstallsTheReleaseAfterTheOneItInstalled(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("in-place updates are Unix-only")
	}
	var github publishing
	srv := github.serve(t, "v1.0.1", true)
	configDir, stateDir := t.TempDir(), t.TempDir()
	exe, started := oldBinary(t)
	c := &Client{ReleaseKeys: testKeys(), BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0", Binary: started}
	if o := c.Auto(context.Background(), configDir, stateDir, exe, nil); o.Installed != "1.0.1" || installedAt(t, exe) != "terma v1.0.1" {
		t.Fatalf("Auto = %+v, binary %q; want 1.0.1 installed", o, installedAt(t, exe))
	}
	// Nothing newer yet: it is not a process whose binary another install replaced.
	if o := c.Auto(context.Background(), configDir, stateDir, exe, nil); o != (Outcome{}) {
		t.Fatalf("Auto = %+v on the release it installed, want nothing to do", o)
	}
	github.publish("v1.0.2", true)
	if o := c.Auto(context.Background(), configDir, stateDir, exe, nil); o.Installed != "1.0.2" || installedAt(t, exe) != "terma v1.0.2" {
		t.Fatalf("Auto = %+v, binary %q; want 1.0.2 installed over the 1.0.1 it installed", o, installedAt(t, exe))
	}
	if cache := LoadCache(stateDir); cache.Current != "1.0.2" || cache.Latest != "1.0.2" {
		t.Fatalf("the check records %+v, want 1.0.2 current and latest", cache)
	}
	// One lookup per release: the pass between them asked the release page alone.
	if lookups, _ := github.asked(); lookups != 2 {
		t.Fatalf("%d API lookups for two releases", lookups)
	}
	if got := github.askedAs(); !slices.Equal(got, []string{userAgentPrefix + "1.0.0"}) {
		t.Fatalf("asked as %v, want only the build that is running", got)
	}
	// Another install's release is still left alone, whatever this process installed before.
	next := exe + ".next"
	if err := os.WriteFile(next, []byte("by hand"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, exe); err != nil {
		t.Fatal(err)
	}
	github.publish("v1.0.3", true)
	if o := c.Auto(context.Background(), configDir, stateDir, exe, nil); !o.Replaced || installedAt(t, exe) != "by hand" {
		t.Fatalf("Auto = %+v, binary %q; want another install's binary left in place", o, installedAt(t, exe))
	}
}

// A release GitHub already names the latest, but whose build still waits for its approval or
// has not finished, is not tried: trying it would fail and hold it back a day
// (AttemptInterval). Each pass asks after its signature instead, without the API, a command
// says nothing of it, and the pass after its files arrive installs it.
func TestAReleaseStillBeingBuiltIsWaitedFor(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("in-place updates are Unix-only")
	}
	var github publishing
	srv := github.serve(t, "v1.0.1", false)
	configDir, stateDir := t.TempDir(), t.TempDir()
	exe, started := oldBinary(t)
	c := &Client{ReleaseKeys: testKeys(), BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0", Binary: started}
	for pass := 1; pass <= 3; pass++ {
		if o := c.Auto(context.Background(), configDir, stateDir, exe, nil); o != (Outcome{}) {
			t.Fatalf("pass %d: Auto = %+v on a release with no files yet, want nothing done or said", pass, o)
		}
		cache := LoadCache(stateDir)
		if cache.Attempted != "" || !cache.AttemptAt.IsZero() || cache.Failed || cache.Latest != "1.0.1" {
			t.Fatalf("pass %d: the check records %+v, want 1.0.1 found and no attempt or failure", pass, cache)
		}
		// The first pass found the release in the API; the rest ask for its signature alone.
		if lookups, probes := github.asked(); lookups != 1 || probes != pass-1 {
			t.Fatalf("pass %d: %d API lookups and %d download probes", pass, lookups, probes)
		}
	}
	var out bytes.Buffer
	c.Maintain(context.Background(), configDir, stateDir, exe, &out)
	if out.Len() != 0 || installedAt(t, exe) != "old binary" {
		t.Fatalf("a command said %q of a release that cannot be installed yet", &out)
	}
	github.publish("v1.0.1", true)
	if o := c.Auto(context.Background(), configDir, stateDir, exe, nil); o.Installed != "1.0.1" || installedAt(t, exe) != "terma v1.0.1" {
		t.Fatalf("Auto = %+v, binary %q; want 1.0.1 installed once it was built", o, installedAt(t, exe))
	}
}

// Asking after a release's files and being refused, as a throttled address is, is a failed
// look: it backs off as one does, and is not followed by a lookup in the rate-limited API.
func TestARefusedDownloadProbeIsAFailedLook(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("in-place updates are Unix-only")
	}
	github := publishing{refuse: http.StatusTooManyRequests}
	srv := github.serve(t, "v1.0.1", true)
	stateDir := t.TempDir()
	exe, started := oldBinary(t)
	SaveCache(stateDir, Cache{CheckedAt: time.Now(), Current: "1.0.0", Latest: "1.0.1", Published: time.Now()})
	c := &Client{ReleaseKeys: testKeys(), BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0", Binary: started}
	if o := c.Auto(context.Background(), t.TempDir(), stateDir, exe, nil); o.Err == nil || o.Installed != "" {
		t.Fatalf("Auto = %+v, want the refusal reported", o)
	}
	cache := LoadCache(stateDir)
	if !cache.Failed || cache.Failures != 1 || cache.Attempted != "" {
		t.Fatalf("the check records %+v, want a failed look and no attempt", cache)
	}
	if lookups, _ := github.asked(); lookups != 0 {
		t.Fatalf("%d API lookups after the download page refused", lookups)
	}
}

// A release is ready once its archive for this platform, its checksums and their signature
// are each wholly uploaded; one still being sent is not there yet. Without its signature it
// is unsigned as well, which `terma update` refuses whatever the reason.
func TestAReleaseIsReadyOnceItsFilesAreUploaded(t *testing.T) {
	t.Parallel()
	whole := []Asset{{Name: "terma_Linux_arm64.tar.gz", State: "uploaded"}, {Name: checksumsName, State: "uploaded"}, {Name: SignatureName, State: "uploaded"}}
	if _, _, _, err := PickAsset(&Release{TagName: "v1.2.3", Assets: whole}, "linux", "arm64"); err != nil {
		t.Fatalf("a whole release: %v", err)
	}
	if _, _, _, err := PickAsset(&Release{TagName: "v1.2.3"}, "linux", "arm64"); !errors.Is(err, ErrNotReady) {
		t.Fatalf("a release with no files = %v, want ErrNotReady", err)
	}
	for missing := range whole {
		for _, how := range []string{"absent", "open"} {
			assets := slices.Clone(whole)
			if how == "open" {
				assets[missing].State = "open"
			} else {
				assets = slices.Delete(assets, missing, missing+1)
			}
			_, _, _, err := PickAsset(&Release{TagName: "v1.2.3", Assets: assets}, "linux", "arm64")
			if !errors.Is(err, ErrNotReady) {
				t.Errorf("%s %s = %v, want ErrNotReady", whole[missing].Name, how, err)
			}
			if unsigned := whole[missing].Name == SignatureName; errors.Is(err, ErrUnsigned) != unsigned {
				t.Errorf("%s %s = %v, want ErrUnsigned: %v", whole[missing].Name, how, err, unsigned)
			}
		}
	}
}
