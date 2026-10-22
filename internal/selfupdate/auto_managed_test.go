package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// upgrades stands in for the package managers the relay runs (Client.upgrade): each call
// answers with the next of answers, an error or a version it then installs as Homebrew would,
// in a keg of its own with the brewed link moved to it.
type upgrades struct {
	b     brewed
	t     *testing.T
	asked []UpgradeRequest
	// answers are what each call does in turn: a version installs it, an error fails, a func
	// runs and its answer stands.
	answers []any
	// removeOld has an install remove the keg the relay started from, as a cask upgrade does.
	removeOld bool
}

func (u *upgrades) upgrade(ctx context.Context, m Manager, r UpgradeRequest) (string, error) {
	u.asked = append(u.asked, r)
	if m.Name != "Homebrew" || m.Terma != u.b.link {
		u.t.Fatalf("upgraded through %+v", m)
	}
	if len(u.answers) == 0 {
		u.t.Fatalf("upgraded %d times, more than answered", len(u.asked))
	}
	answer := u.answers[0]
	u.answers = u.answers[1:]
	if f, ok := answer.(func(context.Context) (string, error)); ok {
		return f(ctx)
	}
	if err, ok := answer.(error); ok {
		return "", err
	}
	u.install(answer.(string))
	return answer.(string), nil
}

// install puts version in a keg of its own and moves the link to it.
func (u *upgrades) install(version string) {
	keg := filepath.Join(u.b.prefix, "Caskroom", "terma", version, "terma")
	termaScript(u.t, keg, version)
	if u.removeOld {
		if err := os.RemoveAll(filepath.Dir(u.b.exe)); err != nil {
			u.t.Fatal(err)
		}
	}
	if err := os.Remove(u.b.link); err != nil {
		u.t.Fatal(err)
	}
	if err := os.Symlink(keg, u.b.link); err != nil {
		u.t.Fatal(err)
	}
}

// relayAt is the relay's client at version, started from path, as launchd starts it from
// the brewed link and Linux names its keg, looking at github.
func relayAt(t *testing.T, version, path, github string, u *upgrades) *Client {
	t.Helper()
	started, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{ReleaseKeys: testKeys(), BaseURL: github, Version: version, Binary: started, RunsManagers: u != nil}
	if u != nil {
		c.upgrade = u.upgrade
	}
	return c
}

// The relay upgrades a Homebrew install through Homebrew under the rules of an install in
// place, says nothing, and records what it did for the next command to say once; it then goes
// on as the release it installed. An interactive command leaves the install to the relay.
func TestTheRelayUpgradesAPackageManagersInstallThroughIt(t *testing.T) {
	b := newBrewed(t)
	b.brew(t, "exit 0")
	var github publishing
	srv := github.serve(t, "v1.0.1", true)
	configDir, stateDir := t.TempDir(), t.TempDir()

	command := relayAt(t, "1.0.0", b.link, srv.URL, nil)
	if o := command.Auto(context.Background(), configDir, stateDir, b.link, nil); o.Installed != "" || !strings.Contains(o.Notice, "1.0.1") {
		t.Fatalf("a command's pass = %+v, want only the notice", o)
	}

	u := &upgrades{b: b, t: t, answers: []any{"1.0.1"}}
	relay := relayAt(t, "1.0.0", b.link, srv.URL, u)
	if o := relay.Auto(context.Background(), configDir, stateDir, b.link, nil); o.Installed != "1.0.1" || o.Err != nil {
		t.Fatalf("the relay's pass = %+v, want 1.0.1 installed", o)
	}
	if len(u.asked) != 1 || u.asked[0] != (UpgradeRequest{From: "1.0.0", To: "1.0.1", Log: statePath(stateDir, upgradeLogFile), RefreshTaps: true}) {
		t.Fatalf("asked %+v", u.asked)
	}
	if cache := LoadCache(stateDir); cache.Current != "1.0.1" || cache.Latest != "1.0.1" {
		t.Fatalf("the check records %+v", cache)
	}
	if o := relay.Auto(context.Background(), configDir, stateDir, b.link, nil); o != (Outcome{}) {
		t.Fatalf("the pass after = %+v, want nothing to do as the release it installed", o)
	}
	if u, ok := LoadUpdated(stateDir); !ok || u.From != "1.0.0" || u.To != "1.0.1" || u.Through != "Homebrew" || u.Told {
		t.Fatalf("recorded %+v, %v", u, ok)
	}
	if line := TellUpdated(stateDir); line != "terma updated itself in the background: 1.0.0 → 1.0.1 (Homebrew)." {
		t.Fatalf("the next command says %q", line)
	}
	if line := TellUpdated(stateDir); line != "" {
		t.Fatalf("the command after says %q again", line)
	}
}

// A release Homebrew's tap or npm's registry does not have yet, or that found Homebrew busy,
// is asked for again soon, backing off, not held back a day as a failed install is, and
// without asking GitHub's API again: the pass's look of the latest tag is enough. Only the
// first tries have Homebrew update its taps. A failure is held a day, and kept for doctor.
func TestAReleaseItsPackageManagerDoesNotOfferYetIsTriedAgainSoon(t *testing.T) {
	b := newBrewed(t)
	b.brew(t, "exit 0")
	var github publishing
	srv := github.serve(t, "v1.0.1", true)
	configDir, stateDir := t.TempDir(), t.TempDir()
	notYet := fmt.Errorf("Homebrew left terma at 1.0.0: %w", ErrNotOffered)
	locked := fmt.Errorf("Homebrew could not upgrade terma to 1.0.1: %w", ErrBusy)
	failed := errors.New("Homebrew could not upgrade terma to 1.0.1 (exit status 1)")
	u := &upgrades{b: b, t: t, answers: []any{notYet, locked, notYet, failed}}
	relay := relayAt(t, "1.0.0", b.link, srv.URL, u)
	pass := func() Outcome { return relay.Auto(context.Background(), configDir, stateDir, b.link, nil) }
	ago := func(d time.Duration) {
		cache := LoadCache(stateDir)
		cache.AttemptAt = time.Now().Add(-d)
		SaveCache(stateDir, cache)
	}

	if o := pass(); !errors.Is(o.Err, ErrNotOffered) || LoadCache(stateDir).Retries != 1 {
		t.Fatalf("first pass = %+v, cache %+v", o, LoadCache(stateDir))
	}
	if pass(); len(u.asked) != 1 {
		t.Fatal("asked again at once")
	}
	for i, wait := range []time.Duration{RetryInterval, 2 * RetryInterval} {
		// Not yet: the wait doubles with each.
		ago(wait - time.Minute)
		if pass(); len(u.asked) != i+1 {
			t.Fatalf("retry %d came before %s", i+1, wait)
		}
		ago(wait + time.Minute)
		if o := pass(); len(u.asked) != i+2 || !retrySoon(o.Err) || LoadCache(stateDir).Retries != i+2 {
			t.Fatalf("retry %d after %s: %+v, asked %d times, cache %+v", i+1, wait, o, len(u.asked), LoadCache(stateDir))
		}
	}
	ago(4*RetryInterval + time.Minute)
	if o := pass(); o.Err != failed || len(u.asked) != 4 {
		t.Fatalf("fourth try = %+v, asked %d times", o, len(u.asked))
	}
	for i, r := range u.asked {
		if r.RefreshTaps != (i < tapRefreshes) {
			t.Errorf("try %d updated Homebrew's taps: %v", i+1, r.RefreshTaps)
		}
	}
	if cache := LoadCache(stateDir); cache.Retries != 0 || cache.Failure != failed.Error() {
		t.Fatalf("after a failure the cache is %+v", cache)
	}
	ago(2 * time.Hour)
	if pass(); len(u.asked) != 4 {
		t.Fatal("a failed install was tried again within the day")
	}
	if lookups, _ := github.asked(); lookups != 1 {
		t.Fatalf("%d API lookups for one release tried four times, want the first look's only", lookups)
	}
	if _, ok := LoadUpdated(stateDir); ok {
		t.Fatal("an update that never happened was recorded")
	}
}

// A tap a release or more behind installs the release it names, between the one running and
// the latest: it is installed and said, and the latest is asked for again soon.
func TestALaggingTapInstallsTheReleaseBetween(t *testing.T) {
	b := newBrewed(t)
	b.brew(t, "exit 0")
	var github publishing
	srv := github.serve(t, "v1.0.2", true)
	configDir, stateDir := t.TempDir(), t.TempDir()
	u := &upgrades{b: b, t: t, answers: []any{"1.0.1"}}
	relay := relayAt(t, "1.0.0", b.link, srv.URL, u)
	if o := relay.Auto(context.Background(), configDir, stateDir, b.link, nil); o.Installed != "1.0.1" || o.Err != nil {
		t.Fatalf("the relay's pass = %+v, want 1.0.1 installed", o)
	}
	cache := LoadCache(stateDir)
	if cache.Current != "1.0.1" || cache.Latest != "1.0.2" || cache.Attempted != "1.0.2" || cache.Retries != 1 {
		t.Fatalf("the check records %+v", cache)
	}
	if u, ok := LoadUpdated(stateDir); !ok || u.From != "1.0.0" || u.To != "1.0.1" {
		t.Fatalf("recorded %+v, %v", u, ok)
	}
}

// On Linux the relay's executable is its keg, the link resolved, which a cask upgrade
// removes and a formula's leaves behind: either way the relay that upgraded goes on to
// restart from the link, and takes the keg it ran from for another install's.
func TestTheRelayRunningFromItsKegUpgradesThroughTheLink(t *testing.T) {
	for _, removeOld := range []bool{true, false} {
		t.Run(fmt.Sprintf("old keg removed %v", removeOld), func(t *testing.T) {
			b := newBrewed(t)
			b.brew(t, "exit 0")
			var github publishing
			srv := github.serve(t, "v1.0.1", true)
			configDir, stateDir := t.TempDir(), t.TempDir()
			if i := InstallOf(b.exe, "1.0.0"); i.Terma() != b.link {
				t.Fatalf("the keg's install restarts from %s, not the link", i.Terma())
			}
			u := &upgrades{b: b, t: t, answers: []any{"1.0.1"}, removeOld: removeOld}
			relay := relayAt(t, "1.0.0", b.exe, srv.URL, u)
			if o := relay.Auto(context.Background(), configDir, stateDir, b.exe, nil); o.Installed != "1.0.1" {
				t.Fatalf("the relay's pass = %+v, want 1.0.1 installed", o)
			}
			if o := relay.Auto(context.Background(), configDir, stateDir, b.exe, nil); !o.Replaced {
				t.Fatalf("the pass after = %+v, want the keg it ran from taken for replaced", o)
			}
			if got, err := InstalledVersion(t.Context(), b.link); got != "1.0.1" || err != nil {
				t.Fatalf("the link runs %q, %v", got, err)
			}
		})
	}
}

// A relay stopped while its package manager ran leaves the release free to try at once,
// and when the manager, left to finish, installed it, the next relay, on that release, says
// so once, as for any other update: only when the release's keg is from the time the manager
// could have been running, and only when a manager was left running at all.
func TestAnUpgradeTheRelayWasStoppedDuringIsSaidByTheNext(t *testing.T) {
	said := "terma updated itself in the background: 1.0.0 → 1.0.1 (Homebrew)."
	for _, tc := range []struct {
		name string
		stop error
		// installed is when the release's keg was put in place, after the attempt began.
		installed time.Duration
		want      string
	}{
		{"left running, installed meanwhile", fmt.Errorf("%w: %w", context.Canceled, errLeftRunning), 2 * time.Minute, said},
		// Installed long after: by hand, not by the manager the relay left.
		{"left running, installed long after", fmt.Errorf("%w: %w", context.Canceled, errLeftRunning), 2 * time.Hour, ""},
		// Stopped before any manager started (during npm view, or refused to start).
		{"nothing left running", context.Canceled, 2 * time.Minute, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBrewed(t)
			b.brew(t, "exit 0")
			var github publishing
			srv := github.serve(t, "v1.0.1", true)
			configDir, stateDir := t.TempDir(), t.TempDir()
			ctx, stop := context.WithCancel(context.Background())
			u := &upgrades{b: b, t: t}
			u.answers = []any{func(context.Context) (string, error) {
				stop()
				return "", tc.stop
			}}
			if o := relayAt(t, "1.0.0", b.link, srv.URL, u).Auto(ctx, configDir, stateDir, b.link, nil); o.Installed != "" {
				t.Fatalf("the stopped relay's pass = %+v", o)
			}
			cache := LoadCache(stateDir)
			if cache.Interrupted.IsZero() != (tc.stop == context.Canceled) || !cache.Stopped || cache.held(cache.Latest, "1.0.0", time.Now()) {
				t.Fatalf("after the stop the attempt is %+v", cache.Attempt)
			}
			u.install("1.0.1") // the manager finishes, or someone installs it
			keg := filepath.Join(b.prefix, "Caskroom", "terma", "1.0.1")
			at := cache.AttemptAt.Add(tc.installed)
			if err := os.Chtimes(keg, at, at); err != nil {
				t.Fatal(err)
			}
			next := relayAt(t, "1.0.1", b.link, srv.URL, &upgrades{b: b, t: t})
			if o := next.Auto(context.Background(), configDir, stateDir, b.link, nil); o != (Outcome{}) {
				t.Fatalf("the next relay's pass = %+v", o)
			}
			if line := TellUpdated(stateDir); line != tc.want {
				t.Fatalf("the next command says %q, want %q", line, tc.want)
			}
		})
	}
}

// What a command installed by itself it says at once, so the next command does not say it
// again.
func TestAnUpdateACommandMadeIsToldOnce(t *testing.T) {
	unixLayouts(t)
	var github publishing
	srv := github.serve(t, "v1.0.1", true)
	exe, started := oldBinary(t)
	stateDir := t.TempDir()
	c := &Client{ReleaseKeys: testKeys(), BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0", Binary: started}
	var out strings.Builder
	c.Maintain(context.Background(), t.TempDir(), stateDir, exe, &out)
	if !strings.Contains(out.String(), "Updated terma to 1.0.1") {
		t.Fatalf("Maintain said %q", out.String())
	}
	if u, ok := LoadUpdated(stateDir); !ok || !u.Told || u.How() != "in place" {
		t.Fatalf("recorded %+v, %v", u, ok)
	}
	if line := TellUpdated(stateDir); line != "" {
		t.Fatalf("the next command says %q again", line)
	}
}

// A relay told to stop during its pass's look, which then answers from the last record,
// starts no install: neither the package manager, which would outlive it or be stopped
// halfway with it, nor an attempt on record.
func TestARelayStoppingDuringItsLookStartsNoInstall(t *testing.T) {
	b := newBrewed(t)
	b.brew(t, "exit 0")
	var github publishing
	srv := github.serve(t, "v1.0.1", true)
	configDir, stateDir := t.TempDir(), t.TempDir()
	u := &upgrades{b: b, t: t, answers: []any{fmt.Errorf("Homebrew left terma at 1.0.0: %w", ErrNotOffered)}}
	relay := relayAt(t, "1.0.0", b.link, srv.URL, u)
	relay.Auto(context.Background(), configDir, stateDir, b.link, nil)
	// Due for its retry.
	cache := LoadCache(stateDir)
	cache.AttemptAt = time.Now().Add(-RetryInterval - time.Minute)
	SaveCache(stateDir, cache)
	before := LoadCache(stateDir).Attempt

	ctx, stop := context.WithCancel(context.Background())
	stop()
	if o := relay.Auto(ctx, configDir, stateDir, b.link, nil); o.Installed != "" || o.Err != nil {
		t.Fatalf("the stopping relay's pass = %+v", o)
	}
	if len(u.asked) != 1 {
		t.Fatalf("the package manager ran %d times, the second as the relay stopped", len(u.asked))
	}
	if after := LoadCache(stateDir).Attempt; after != before {
		t.Fatalf("the stopping relay recorded an attempt: %+v, was %+v", after, before)
	}
}

// A relay back on the release before, whose retry meets the package manager the stopped
// attempt left still running, busy, keeps the attempt interrupted: what that manager goes on
// to install is the background update's, said once by the relay on it.
func TestARetryThatFindsTheLeftUpgradeBusyStillClaimsIt(t *testing.T) {
	b := newBrewed(t)
	b.brew(t, "exit 0")
	var github publishing
	srv := github.serve(t, "v1.0.1", true)
	configDir, stateDir := t.TempDir(), t.TempDir()
	ctx, stop := context.WithCancel(context.Background())
	u := &upgrades{b: b, t: t}
	u.answers = []any{
		func(context.Context) (string, error) {
			stop()
			return "", fmt.Errorf("%w: %w", context.Canceled, errLeftRunning)
		},
		fmt.Errorf("Homebrew could not upgrade terma to 1.0.1: %w", ErrBusy),
	}
	relayAt(t, "1.0.0", b.link, srv.URL, u).Auto(ctx, configDir, stateDir, b.link, nil)
	// The service starts the relay again, still on 1.0.0: Homebrew has not moved the link yet.
	again := relayAt(t, "1.0.0", b.link, srv.URL, u)
	if o := again.Auto(context.Background(), configDir, stateDir, b.link, nil); !errors.Is(o.Err, ErrBusy) {
		t.Fatalf("the retry = %+v, want Homebrew busy", o)
	}
	if cache := LoadCache(stateDir); cache.Interrupted.IsZero() || cache.Retries != 1 {
		t.Fatalf("after the busy retry the attempt is %+v", cache.Attempt)
	}
	u.install("1.0.1") // the left manager finishes
	next := relayAt(t, "1.0.1", b.link, srv.URL, &upgrades{b: b, t: t})
	next.Auto(context.Background(), configDir, stateDir, b.link, nil)
	if line := TellUpdated(stateDir); line != "terma updated itself in the background: 1.0.0 → 1.0.1 (Homebrew)." {
		t.Fatalf("the next command says %q", line)
	}
}

// Only an install by `terma update` takes an interrupted attempt off the background's
// account; its look alone (`terma update --check`) leaves the claim.
func TestOnlyAnInstallByHandUnclaims(t *testing.T) {
	stateDir := t.TempDir()
	began := time.Now()
	SaveCache(stateDir, Cache{Current: "1.0.0", Latest: "1.0.1",
		Attempt: Attempt{AttemptAt: began, Attempted: "1.0.1", AttemptedBy: "1.0.0", Stopped: true, Interrupted: began}})
	Checked(stateDir, "1.0.0", &Release{TagName: "v1.0.1", PublishedAt: time.Now()})
	if cache := LoadCache(stateDir); !cache.landed("1.0.1", began.Add(time.Minute)) {
		t.Fatalf("terma update --check took the claim off: %+v", cache.Attempt)
	}
	InstalledByHand(stateDir)
	if cache := LoadCache(stateDir); cache.landed("1.0.1", began.Add(time.Minute)) {
		t.Fatalf("an install by hand is still claimed: %+v", cache.Attempt)
	}
}
