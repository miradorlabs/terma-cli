package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// upgrades is a stand-in for the package managers the relay runs (Client.Upgrade): each
// call answers with the next of answers, an error or a version it then installs at the
// brewed link.
type upgrades struct {
	b       brewed
	t       *testing.T
	asked   []string
	answers []any
}

func (u *upgrades) upgrade(_ context.Context, m Manager, version string) (string, error) {
	u.asked = append(u.asked, m.Name+" "+version)
	if len(u.answers) == 0 {
		u.t.Fatalf("upgraded %d times, more than answered", len(u.asked))
	}
	answer := u.answers[0]
	u.answers = u.answers[1:]
	if err, ok := answer.(error); ok {
		return "", err
	}
	termaScript(u.t, u.b.exe+"-"+answer.(string), answer.(string))
	if err := os.Remove(u.b.link); err != nil {
		u.t.Fatal(err)
	}
	if err := os.Symlink(u.b.exe+"-"+answer.(string), u.b.link); err != nil {
		u.t.Fatal(err)
	}
	return answer.(string), nil
}

// relayOf is the relay's client for the brewed terma, running from its link, as launchd
// starts it, and looking at github.
func relayOf(t *testing.T, b brewed, github string, u *upgrades) *Client {
	t.Helper()
	started, err := os.Stat(b.link)
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{ReleaseKeys: testKeys(), BaseURL: github, Version: "1.0.0", Binary: started}
	if u != nil {
		c.Upgrade = u.upgrade
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

	command := relayOf(t, b, srv.URL, nil)
	if o := command.Auto(context.Background(), configDir, stateDir, b.link, nil); o.Installed != "" || !strings.Contains(o.Notice, "1.0.1") {
		t.Fatalf("a command's pass = %+v, want only the notice", o)
	}

	u := &upgrades{b: b, t: t, answers: []any{"1.0.1"}}
	relay := relayOf(t, b, srv.URL, u)
	if o := relay.Auto(context.Background(), configDir, stateDir, b.link, nil); o.Installed != "1.0.1" || o.Err != nil {
		t.Fatalf("the relay's pass = %+v, want 1.0.1 installed", o)
	}
	if fmt.Sprint(u.asked) != "[Homebrew 1.0.1]" {
		t.Fatalf("asked %v", u.asked)
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

// A release Homebrew's tap or npm's registry does not have yet is asked for again soon,
// backing off, not held back a day as a failed install is; a failure is held a day, and kept
// for doctor.
func TestAReleaseItsPackageManagerDoesNotOfferYetIsTriedAgainSoon(t *testing.T) {
	b := newBrewed(t)
	b.brew(t, "exit 0")
	var github publishing
	srv := github.serve(t, "v1.0.1", true)
	configDir, stateDir := t.TempDir(), t.TempDir()
	notYet := fmt.Errorf("Homebrew left terma at 1.0.0: %w", ErrNotOffered)
	failed := errors.New("Homebrew could not upgrade terma to 1.0.1 (exit status 1)")
	u := &upgrades{b: b, t: t, answers: []any{notYet, notYet, failed}}
	relay := relayOf(t, b, srv.URL, u)
	pass := func() Outcome { return relay.Auto(context.Background(), configDir, stateDir, b.link, nil) }
	ago := func(d time.Duration) {
		cache := LoadCache(stateDir)
		cache.AttemptAt = time.Now().Add(-d)
		SaveCache(stateDir, cache)
	}

	if o := pass(); !errors.Is(o.Err, ErrNotOffered) || LoadCache(stateDir).Unoffered != 1 {
		t.Fatalf("first pass = %+v, cache %+v", o, LoadCache(stateDir))
	}
	if pass(); len(u.asked) != 1 {
		t.Fatal("asked again at once")
	}
	ago(RetryInterval + time.Minute)
	if o := pass(); len(u.asked) != 2 || LoadCache(stateDir).Unoffered != 2 {
		t.Fatalf("after %s: %+v, asked %d times, cache %+v", RetryInterval, o, len(u.asked), LoadCache(stateDir))
	}
	// The second time it waits twice as long.
	ago(RetryInterval + time.Minute)
	if pass(); len(u.asked) != 2 {
		t.Fatal("did not back off")
	}
	ago(2*RetryInterval + time.Minute)
	if o := pass(); o.Err != failed || len(u.asked) != 3 {
		t.Fatalf("third pass = %+v, asked %d times", o, len(u.asked))
	}
	if cache := LoadCache(stateDir); cache.Unoffered != 0 || cache.Failure != failed.Error() {
		t.Fatalf("after a failure the cache is %+v", cache)
	}
	ago(2 * time.Hour)
	if pass(); len(u.asked) != 3 {
		t.Fatal("a failed install was tried again within the day")
	}
	if _, ok := LoadUpdated(stateDir); ok {
		t.Fatal("an update that never happened was recorded")
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
