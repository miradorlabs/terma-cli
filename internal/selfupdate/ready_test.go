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
	// built says whether its build has uploaded its files yet; absent names one still to come.
	built  bool
	absent string
	// open names a file whose download answers already while the API lists it as being sent.
	open string
	// refuse, set, is the status every download answers with, as a throttled address gets.
	refuse int
	// duringArchive, set, runs as the archive is asked for, before any of it is sent.
	duringArchive func(r *http.Request)
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
			case !p.built || name == p.absent:
				w.WriteHeader(http.StatusNotFound)
			default:
				w.Header().Set("Location", "https://release-assets.githubusercontent.com/"+name)
				w.WriteHeader(http.StatusFound)
			}
		case r.URL.Path == "/repos/"+Repo+"/releases/latest":
			p.lookups++
			rel := Release{TagName: p.tag, PublishedAt: time.Now()}
			for name, path := range map[string]string{archiveName: "/archive", checksumsName: "/sums", SignatureName: "/sig"} {
				if !p.built || name == p.absent {
					continue
				}
				state := "uploaded"
				if name == p.open {
					state = "open"
				}
				rel.Assets = append(rel.Assets, Asset{Name: name, URL: base + path, State: state})
			}
			_ = json.NewEncoder(w).Encode(rel)
		case r.URL.Path == "/sums":
			_, _ = w.Write(sums)
		case r.URL.Path == "/sig":
			_, _ = w.Write(testSign(p.tag, sums))
		case r.URL.Path == "/archive":
			if p.duringArchive != nil {
				p.duringArchive(r)
			}
			_, _ = w.Write(archive)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// change alters what the fake publishes, between an updater's passes.
func (p *publishing) change(to func(p *publishing)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	to(p)
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

// lookedBefore is the state directory of a machine on version whose last look, a minute
// ago, found nothing newer: its next asks the release page before the API.
func lookedBefore(t *testing.T, version string) string {
	t.Helper()
	stateDir := t.TempDir()
	SaveCache(stateDir, Cache{CheckedAt: time.Now().Add(-time.Minute), Current: version, Latest: version, Published: time.Now().Add(-2 * SoakTime)})
	return stateDir
}

// A process that installed a release goes on as it: the relay, which keeps running until its
// agents pause, installs the release after it too, rather than taking its own install for
// another's and installing nothing more until it restarts. It looks no more often for that,
// and still says which build is asking and trying.
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
	github.change(func(p *publishing) { p.tag = "v1.0.2" })
	if o := c.Auto(context.Background(), configDir, stateDir, exe, nil); o.Installed != "1.0.2" || installedAt(t, exe) != "terma v1.0.2" {
		t.Fatalf("Auto = %+v, binary %q; want 1.0.2 installed over the 1.0.1 it installed", o, installedAt(t, exe))
	}
	if cache := LoadCache(stateDir); cache.Current != "1.0.2" || cache.Latest != "1.0.2" || cache.AttemptedBy != "1.0.0" {
		t.Fatalf("the check records %+v, want 1.0.2 current and latest, tried by the 1.0.0 that runs", cache)
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
	github.change(func(p *publishing) { p.tag = "v1.0.3" })
	if o := c.Auto(context.Background(), configDir, stateDir, exe, nil); !o.Replaced || installedAt(t, exe) != "by hand" {
		t.Fatalf("Auto = %+v, binary %q; want another install's binary left in place", o, installedAt(t, exe))
	}
}

// A release GitHub already names the latest, but whose build still waits for its approval or
// has not finished, is not tried: trying it would fail and hold it back a day
// (AttemptInterval). Each of the relay's passes asks after its signature instead, a command
// asks nothing between its hourly looks and says nothing of it, and the pass after its files
// arrive installs it: the one time the release is looked up in the API.
func TestAReleaseStillBeingBuiltIsWaitedFor(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("in-place updates are Unix-only")
	}
	var github publishing
	srv := github.serve(t, "v1.0.1", false)
	configDir, stateDir := t.TempDir(), lookedBefore(t, "1.0.0")
	exe, started := oldBinary(t)
	c := &Client{ReleaseKeys: testKeys(), BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0", Binary: started}
	for pass := 1; pass <= 3; pass++ {
		if o := c.Auto(context.Background(), configDir, stateDir, exe, nil); o != (Outcome{}) {
			t.Fatalf("pass %d: Auto = %+v on a release with no files yet, want nothing done or said", pass, o)
		}
		cache := LoadCache(stateDir)
		if cache.Attempted != "" || !cache.AttemptAt.IsZero() || cache.Failed || cache.Latest != "1.0.1" || !cache.Unbuilt {
			t.Fatalf("pass %d: the check records %+v, want 1.0.1 found unbuilt and no attempt or failure", pass, cache)
		}
		if lookups, probes := github.asked(); lookups != 0 || probes != pass {
			t.Fatalf("pass %d: %d API lookups and %d download probes, want none and one a pass", pass, lookups, probes)
		}
	}
	// A command says nothing of it, to a developer who turned automatic updates off either,
	// and asks nothing within the hour of the last look.
	notifying := t.TempDir()
	if err := SavePreferences(notifying, Preferences{Auto: false}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	c.Maintain(context.Background(), configDir, stateDir, exe, &out)
	c.Maintain(context.Background(), notifying, stateDir, exe, &out)
	if _, probes := github.asked(); out.Len() != 0 || probes != 3 || installedAt(t, exe) != "old binary" {
		t.Fatalf("commands said %q and made %d download probes of a release that cannot be installed yet", &out, probes-3)
	}
	github.change(func(p *publishing) { p.built = true })
	if o := c.Auto(context.Background(), configDir, stateDir, exe, nil); o.Installed != "1.0.1" || installedAt(t, exe) != "terma v1.0.1" {
		t.Fatalf("Auto = %+v, binary %q; want 1.0.1 installed once it was built", o, installedAt(t, exe))
	}
	if lookups, _ := github.asked(); lookups != 1 {
		t.Fatalf("%d API lookups for a release that waited for its build, want one", lookups)
	}
}

// A release is built once its signature, its checksums and this platform's archive each
// answer as a download. The signature is asked for first, so a release still waiting costs
// one request; without any one of the three it is not built.
func TestAReleaseIsBuiltOnlyWithAllItsFiles(t *testing.T) {
	t.Parallel()
	archiveName := AssetName(runtime.GOOS, runtime.GOARCH)
	for _, tc := range []struct {
		absent string
		probes int
	}{{"", 3}, {SignatureName, 1}, {checksumsName, 2}, {archiveName, 3}} {
		github := publishing{absent: tc.absent}
		srv := github.serve(t, "v1.0.1", true)
		c := &Client{BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0"}
		built, err := c.built(context.Background(), "1.0.1")
		if _, probes := github.asked(); err != nil || built != (tc.absent == "") || probes != tc.probes {
			t.Errorf("without %q: built = %v, %v after %d probes; want %d probes", tc.absent, built, err, probes, tc.probes)
		}
	}
}

// A release whose files answer as downloads while the API still lists one as being sent is
// not tried either: the next look asks again, and installs it once the API has it whole.
func TestAReleaseTheAPIStillListsAsUploadingIsWaitedFor(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("in-place updates are Unix-only")
	}
	github := publishing{open: AssetName(runtime.GOOS, runtime.GOARCH)}
	srv := github.serve(t, "v1.0.1", true)
	configDir, stateDir := t.TempDir(), lookedBefore(t, "1.0.0")
	exe, started := oldBinary(t)
	c := &Client{ReleaseKeys: testKeys(), BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0", Binary: started}
	if o := c.Auto(context.Background(), configDir, stateDir, exe, nil); o != (Outcome{}) {
		t.Fatalf("Auto = %+v on a release whose archive is still being sent, want nothing done or said", o)
	}
	if cache := LoadCache(stateDir); cache.Attempted != "" || !cache.Unbuilt || installedAt(t, exe) != "old binary" {
		t.Fatalf("the check records %+v, want 1.0.1 unbuilt and no attempt", cache)
	}
	github.change(func(p *publishing) { p.open = "" })
	if o := c.Auto(context.Background(), configDir, stateDir, exe, nil); o.Installed != "1.0.1" || installedAt(t, exe) != "terma v1.0.1" {
		t.Fatalf("Auto = %+v, binary %q; want 1.0.1 installed once the API had it whole", o, installedAt(t, exe))
	}
}

// Asking after a release's files and being refused, as a throttled address is, is a failed
// look: it backs off as one does, and is not followed by a lookup in the rate-limited API.
func TestARefusedDownloadProbeIsAFailedLook(t *testing.T) {
	t.Parallel()
	github := publishing{refuse: http.StatusTooManyRequests}
	srv := github.serve(t, "v1.0.1", true)
	stateDir := lookedBefore(t, "1.0.0")
	exe, started := oldBinary(t)
	c := &Client{ReleaseKeys: testKeys(), BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0", Binary: started}
	for pass := 1; pass <= 2; pass++ {
		if o := c.Auto(context.Background(), t.TempDir(), stateDir, exe, nil); o != (Outcome{}) {
			t.Fatalf("pass %d: Auto = %+v, want nothing done", pass, o)
		}
		cache := LoadCache(stateDir)
		if !cache.Failed || cache.Failures != 1 || cache.Attempted != "" || cache.Latest != "1.0.0" {
			t.Fatalf("pass %d: the check records %+v, want one failed look and no attempt", pass, cache)
		}
		// The second pass falls within the backoff, and asks nothing at all.
		if lookups, probes := github.asked(); lookups != 0 || probes != 1 {
			t.Fatalf("pass %d: %d API lookups and %d download probes after the download page refused", pass, lookups, probes)
		}
	}
}

// An install cut short because its process was told to stop, as a relay is that restarts on
// the release before while the next downloads, is not the release's failure: it is not held
// back, and the next process to pass installs it.
func TestAnInstallCutShortByItsProcessStoppingIsNoAttempt(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("in-place updates are Unix-only")
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	// The process is stopped with the archive on its way, and has gone before any arrives.
	github := publishing{duringArchive: func(r *http.Request) {
		stop()
		<-r.Context().Done()
	}}
	srv := github.serve(t, "v1.0.1", true)
	configDir, stateDir := t.TempDir(), t.TempDir()
	exe, started := oldBinary(t)
	c := &Client{ReleaseKeys: testKeys(), BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0", Binary: started}
	if o := c.Auto(ctx, configDir, stateDir, exe, nil); !errors.Is(o.Err, context.Canceled) || o.Installed != "" {
		t.Fatalf("Auto = %+v, want the install cut short", o)
	}
	if cache := LoadCache(stateDir); cache.Attempted != "" || !cache.AttemptAt.IsZero() || cache.AttemptedBy != "" {
		t.Fatalf("an install its process was stopped during left an attempt behind: %+v", cache)
	}
	github.change(func(p *publishing) { p.duringArchive = nil })
	next := &Client{ReleaseKeys: testKeys(), BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0", Binary: started}
	if o := next.Auto(context.Background(), configDir, stateDir, exe, nil); o.Installed != "1.0.1" {
		t.Fatalf("Auto = %+v, want the next process to install 1.0.1 at once", o)
	}
}

// A release whose install failed is held back from the build that tried it and from earlier
// ones, not from a later build: a relay still running an earlier terma, going on as the
// release it installed, may fail for a reason of its own, such as a release key it lacks.
func TestALaterBuildIsNotHeldBackByAnEarlierBuildsFailedInstall(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("in-place updates are Unix-only")
	}
	for _, tc := range []struct {
		name               string
		running, installed string
		triedBy            string
		installs           bool
	}{
		{"tried by an earlier build", "1.0.1", "", "1.0.0", true},
		{"tried by this build", "1.0.1", "", "1.0.1", false},
		{"tried by a build that did not say", "1.0.1", "", "", false},
		{"tried by a later build", "1.0.0", "1.0.1", "1.0.1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var github publishing
			srv := github.serve(t, "v1.0.2", true)
			stateDir := t.TempDir()
			exe, started := oldBinary(t)
			SaveCache(stateDir, Cache{CheckedAt: time.Now(), Current: "1.0.1", Latest: "1.0.2", Published: time.Now(),
				AttemptAt: time.Now(), Attempted: "1.0.2", AttemptedBy: tc.triedBy})
			c := &Client{ReleaseKeys: testKeys(), BaseURL: srv.URL, HTTP: srv.Client(), Version: tc.running, installed: tc.installed, Binary: started}
			o := c.Auto(context.Background(), t.TempDir(), stateDir, exe, nil)
			if installed := o.Installed == "1.0.2"; installed != tc.installs || o.Err != nil {
				t.Fatalf("Auto = %+v, want 1.0.2 installed: %v", o, tc.installs)
			}
		})
	}
}

// A binary rewritten where it stands is no longer the one this process started from, though
// it is the same file: its size and time say so, as they would of a later install's file
// given the inode of one this process installed.
func TestABinaryChangedInPlaceIsAnotherInstalls(t *testing.T) {
	t.Parallel()
	exe, started := oldBinary(t)
	c := &Client{Version: "1.0.0", Binary: started}
	if c.Replaced(exe) {
		t.Fatal("the binary the process started from counts as replaced")
	}
	if err := os.WriteFile(exe, []byte("another install's, written over it"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !c.Replaced(exe) {
		t.Fatal("a binary rewritten in place still counts as the one the process started from")
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
