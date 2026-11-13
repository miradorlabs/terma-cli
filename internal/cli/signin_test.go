package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// fakeBrowser is the developer at the approval page. It reads each link a sign-in prints
// and approves it as the page sends it: a code for each organization ticked, in order, in
// one form POST to the CLI's listener. It ticks what the page pre-checks (the link's orgs,
// else every organization, personal first) unless tick says otherwise; an organization in
// fail gets a code the auth host never redeems, as a failed exchange.
type fakeBrowser struct {
	t    *testing.T
	f    *fakeAuth
	tick func(link url.Values) []string
	fail map[string]bool

	seq     atomic.Int32
	wg      sync.WaitGroup
	mu      sync.Mutex
	text    strings.Builder
	scanned int
	links   []url.Values
}

func newFakeBrowser(t *testing.T, f *fakeAuth) *fakeBrowser {
	b := &fakeBrowser{t: t, f: f}
	t.Cleanup(b.wg.Wait)
	return b
}

// ticking is a browser that ticks orgs, in that order, whatever the link pre-checks.
func ticking(t *testing.T, f *fakeAuth, orgs ...string) *fakeBrowser {
	b := newFakeBrowser(t, f)
	b.tick = func(url.Values) []string { return orgs }
	return b
}

func (b *fakeBrowser) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.text.Write(p)
	for {
		rest := b.text.String()[b.scanned:]
		i := strings.Index(rest, "/cli/auth?")
		if i < 0 {
			break
		}
		n := strings.IndexAny(rest[i:], " \n\t")
		if n < 0 {
			break
		}
		b.scanned += i + n
		u, err := url.Parse(rest[i : i+n])
		if err != nil {
			b.t.Errorf("approval link: %v", err)
			break
		}
		link := u.Query()
		b.links = append(b.links, link)
		b.wg.Go(func() { b.approve(link) })
	}
	return len(p), nil
}

func (b *fakeBrowser) approve(link url.Values) {
	orgs := b.preChecked(link)
	if b.tick != nil {
		orgs = b.tick(link)
	}
	form := url.Values{"state": {link.Get("state")}}
	for _, org := range orgs {
		code := fmt.Sprintf("bc-%s-%d", org[:8], b.seq.Add(1))
		if !b.fail[org] {
			b.f.browserCode(code, org)
		}
		form.Add("code", code)
	}
	resp, err := http.PostForm("http://127.0.0.1:"+link.Get("port")+"/callback", form)
	if err != nil {
		b.t.Errorf("approve: %v", err)
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// preChecked is what the page pre-checks on link: its orgs, else every organization the
// developer belongs to, personal first.
func (b *fakeBrowser) preChecked(link url.Values) []string {
	if orgs := link.Get("orgs"); orgs != "" {
		return strings.Split(orgs, ",")
	}
	listed := cmpOrgs(b.f.orgs)
	var ids []string
	for _, o := range listed {
		if o.Type == "personal" {
			ids = append([]string{o.ID}, ids...)
		} else {
			ids = append(ids, o.ID)
		}
	}
	return ids
}

// cmpOrgs is what /v1/organizations lists for orgs: fakeOrgs when nil.
func cmpOrgs(orgs []organization) []organization {
	if orgs == nil {
		return fakeOrgs
	}
	return orgs
}

// approvalLinks are the links the browser was shown.
func (b *fakeBrowser) approvalLinks() []url.Values {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.links)
}

var clientCo = organization{ID: "33333333-3333-4333-8333-333333333333", Name: "ClientCo", Role: "member"}

// personalBetaOf is Beta Labs as the developer's personal organization.
func personalBetaOf() organization {
	return organization{ID: orgB().ID, Name: "Beta Labs", Role: "admin", Type: "personal"}
}

// threeOrgs makes the developer a member of Acme, Beta Labs (personal) and ClientCo.
func threeOrgs(f *fakeAuth) {
	f.orgs = []organization{orgA(), personalBetaOf(), clientCo}
}

// browserSignInHere runs a sign-in whose approval link b approves; it returns what the
// sign-in printed.
func browserSignInHere(t *testing.T, b *fakeBrowser, opts signInOptions) (*signInResult, string, error) {
	t.Helper()
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetOut(&out)
	cmd.SetErr(io.MultiWriter(&out, b))
	cmd.SetIn(strings.NewReader(""))
	opts.noBrowser = true
	res, err := testApp.signIn(cmd, cfg, opts)
	return res, out.String(), err
}

// activeOrg is the profile's active sign-in and its recorded organization.
func activeOrg(t *testing.T) (credential, profile string) {
	t.Helper()
	if cred, err := auth.LoadCredential(testApp.dir, config.DefaultProfile); err == nil {
		credential = cred.OrganizationID
	}
	file, err := config.LoadFile(testApp.dir)
	if err != nil {
		t.Fatal(err)
	}
	if p := file.Profiles[config.DefaultProfile]; p != nil {
		profile = p.OrganizationID
	}
	return credential, profile
}

// signedInOrgs are the organizations the profile holds a sign-in for, sorted.
func signedInOrgs(t *testing.T) []string {
	t.Helper()
	creds, err := auth.Credentials(testApp.dir, config.DefaultProfile)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range creds {
		ids = append(ids, c.OrganizationID)
	}
	slices.Sort(ids)
	return ids
}

// One approval signs in to every organization ticked, each exchanged with the one
// verifier, and activates the personal one on a new machine. It says which signed in and
// names the organization still without a sign-in.
func TestBrowserSignInSignsInToEveryOrganizationTicked(t *testing.T) {
	f := newFakeAuth(t)
	threeOrgs(f)
	authSandbox(t, f)
	b := newFakeBrowser(t, f)
	b.tick = func(url.Values) []string { return []string{orgB().ID, orgA().ID} }

	res, out, err := browserSignInHere(t, b, signInOptions{})
	if err != nil {
		t.Fatalf("sign-in: %v\n%s", err, out)
	}
	links := b.approvalLinks()
	if len(links) != 1 || links[0].Get("codes") != "multiple" || links[0].Has("orgs") || links[0].Has("org") {
		t.Fatalf("approval links = %v: want one, every organization pre-checked", links)
	}
	if got := signedInOrgs(t); !slices.Equal(got, []string{orgA().ID, orgB().ID}) {
		t.Fatalf("signed in to %v, want Acme and Beta Labs", got)
	}
	// A machine not migrated keeps every organization's sign-in in credentials.json, as
	// it always could; the migration copies them to v2.
	if _, err := os.Stat(config.CredentialsV2Path(testApp.dir)); err == nil || config.Migrated(testApp.dir) {
		t.Fatal("a sign-in on a machine not migrated wrote credentials.v2.json")
	}
	if cred, profile := activeOrg(t); cred != orgB().ID || profile != orgB().ID || res.cred.OrganizationID != orgB().ID || res.orgName != "Beta Labs" {
		t.Fatalf("active = %s, profile %s, result %s %q; want personal Beta Labs", cred, profile, res.cred.OrganizationID, res.orgName)
	}
	if v := f.verifiersSent(); len(v) != 2 || v[0] != v[1] || v[0] == "" {
		t.Fatalf("verifiers sent = %v, want one verifier for both codes", v)
	}
	for _, want := range []string{
		"Signed in to Beta Labs and Acme\n",
		"Not signed in to ClientCo. Run `terma teams --authorize` to add it.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sign-in lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Not signed in to Acme") || strings.Contains(out, "Not signed in to Beta") {
		t.Errorf("an organization that signed in was named as not:\n%s", out)
	}
}

// Exactly one organization is active after a sign-in, by the rule, whatever order the
// page sent the codes in: the one asked for; else the profile's, still signed in; else
// personal; else the first signed in, as listed.
func TestBrowserSignInActivatesOneOrganizationByTheRule(t *testing.T) {
	shared := organization{ID: orgB().ID, Name: "Beta Labs", Role: "member", Type: "shared"}
	for _, tc := range []struct {
		name   string
		orgs   []organization
		opts   signInOptions
		before func(*testing.T, *fakeAuth)
		tick   []string
		want   string
	}{
		{name: "the organization asked for", orgs: []organization{orgA(), personalBetaOf(), clientCo},
			opts: signInOptions{org: orgRef{ID: orgA().ID}}, tick: []string{orgB().ID, orgA().ID}, want: orgA().ID},
		{name: "the profile's, still signed in", orgs: []organization{orgA(), personalBetaOf(), clientCo},
			opts:   signInOptions{orgs: []string{orgA().ID, orgB().ID}},
			before: func(t *testing.T, f *fakeAuth) { storeActiveSignIn(t, f, clientCo) },
			tick:   []string{orgB().ID, orgA().ID}, want: clientCo.ID},
		{name: "personal", orgs: []organization{orgA(), personalBetaOf(), clientCo},
			tick: []string{orgA().ID, clientCo.ID, orgB().ID}, want: orgB().ID},
		{name: "the first signed in, as listed", orgs: []organization{clientCo, orgA(), shared},
			tick: []string{orgB().ID, orgA().ID}, want: orgA().ID},
	} {
		for _, reversed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reversed=%v", tc.name, reversed), func(t *testing.T) {
				f := newFakeAuth(t)
				f.orgs = tc.orgs
				authSandbox(t, f)
				if tc.before != nil {
					tc.before(t, f)
				}
				tick := slices.Clone(tc.tick)
				if reversed {
					slices.Reverse(tick)
				}
				res, out, err := browserSignInHere(t, ticking(t, f, tick...), tc.opts)
				if err != nil {
					t.Fatalf("sign-in: %v\n%s", err, out)
				}
				if cred, profile := activeOrg(t); cred != tc.want || profile != tc.want || res.cred.OrganizationID != tc.want {
					t.Fatalf("active = %s, profile %s, result %s; want %s\n%s", cred, profile, res.cred.OrganizationID, tc.want, out)
				}
			})
		}
	}
}

// storeActiveSignIn stores a working sign-in to org and makes it the profile's.
func storeActiveSignIn(t *testing.T, f *fakeAuth, org organization) {
	t.Helper()
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f, org)); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) {
		p.SelectOrganization(org.ID, org.Name)
	}); err != nil {
		t.Fatal(err)
	}
}

// The rule picks the same organization whatever order its codes came in.
func TestActiveAfterSignInIgnoresTheExchangeOrder(t *testing.T) {
	stored := map[string]bool{"cur": true, "gone": false}
	has := func(signedIn []string) func(string) bool {
		return func(id string) bool { return stored[id] || slices.Contains(signedIn, id) }
	}
	for _, tc := range []struct {
		name, asked, current, personal string
		listed, signedIn               []string
		want                           string
	}{
		{name: "asked", asked: "a", current: "cur", personal: "p", signedIn: []string{"p", "a", "b"}, want: "a"},
		{name: "asked but not approved", asked: "x", current: "cur", personal: "p", signedIn: []string{"p", "a"}, want: "cur"},
		{name: "current, signed in before", current: "cur", personal: "p", signedIn: []string{"p", "a"}, want: "cur"},
		{name: "current, signed in now", current: "a", personal: "p", signedIn: []string{"p", "a"}, want: "a"},
		{name: "personal", current: "gone", personal: "p", signedIn: []string{"a", "p", "b"}, want: "p"},
		{name: "first as listed", personal: "q", listed: []string{"c", "b", "a"}, signedIn: []string{"a", "b"}, want: "b"},
	} {
		perms := [][]string{tc.signedIn, slices.Clone(tc.signedIn)}
		slices.Reverse(perms[1])
		if len(tc.signedIn) == 3 {
			s := tc.signedIn
			perms = append(perms, []string{s[1], s[0], s[2]}, []string{s[2], s[0], s[1]})
		}
		for _, order := range perms {
			if got := activeAfterSignIn(tc.asked, tc.current, tc.personal, tc.listed, order, has(order)); got != tc.want {
				t.Errorf("%s, codes %v: active %q, want %q", tc.name, order, got, tc.want)
			}
		}
	}
	// Unlisted, the first signed in is the first in the page's order: the page's primary.
	if got := activeAfterSignIn("", "", "", nil, []string{"b", "a"}, has(nil)); got != "b" {
		t.Errorf("unlisted: active %q, want b", got)
	}
}

// A failed exchange leaves only its organization without a sign-in: the others are
// stored, and it is named as not signed in.
func TestBrowserSignInKeepsTheOthersWhenAnExchangeFails(t *testing.T) {
	f := newFakeAuth(t)
	threeOrgs(f)
	authSandbox(t, f)
	b := ticking(t, f, orgB().ID, orgA().ID, clientCo.ID)
	b.fail = map[string]bool{orgA().ID: true}
	_, out, err := browserSignInHere(t, b, signInOptions{})
	if err != nil {
		t.Fatalf("sign-in: %v\n%s", err, out)
	}
	if got := signedInOrgs(t); !slices.Equal(got, []string{orgB().ID, clientCo.ID}) {
		t.Fatalf("signed in to %v, want Beta Labs and ClientCo", got)
	}
	if !strings.Contains(out, "Signed in to Beta Labs and ClientCo\n") || !strings.Contains(out, "Not signed in to Acme. Run `terma teams --authorize` to add it.\n") {
		t.Fatalf("sign-in output:\n%s", out)
	}
	if cred, _ := activeOrg(t); cred != orgB().ID {
		t.Fatalf("active = %s, want personal", cred)
	}
}

// Every exchange failing fails the sign-in, as one failing did, and stores nothing.
func TestBrowserSignInFailsWhenEveryExchangeFails(t *testing.T) {
	f := newFakeAuth(t)
	threeOrgs(f)
	authSandbox(t, f)
	b := ticking(t, f, orgB().ID, orgA().ID)
	b.fail = map[string]bool{orgA().ID: true, orgB().ID: true}
	_, out, err := browserSignInHere(t, b, signInOptions{})
	if err == nil || !strings.Contains(err.Error(), "invalid or expired credentials") {
		t.Fatalf("sign-in = %v\n%s", err, out)
	}
	if got := signedInOrgs(t); len(got) != 0 {
		t.Fatalf("a failed sign-in stored %v", got)
	}
	if strings.Contains(out, "Signed in to") {
		t.Fatalf("a failed sign-in said it signed in:\n%s", out)
	}
}

// Each organization's sign-in replaces only its own stored one, which is ended; an
// organization signing in for the first time ends nothing.
func TestBrowserSignInRevokesEachReplacedSession(t *testing.T) {
	f := newFakeAuth(t)
	threeOrgs(f)
	authSandbox(t, f)
	old := storedSession(f, orgA())
	old.AccessToken, old.SessionID = fakeToken(orgA().ID, "u-old"), "s-old"
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, old); err != nil {
		t.Fatal(err)
	}
	_, out, err := browserSignInHere(t, ticking(t, f, orgB().ID, orgA().ID), signInOptions{orgs: []string{orgA().ID, orgB().ID}})
	if err != nil {
		t.Fatalf("sign-in: %v\n%s", err, out)
	}
	if got := f.revokedTokens(); !slices.Equal(got, []string{old.AccessToken}) {
		t.Fatalf("revoked %v, want only Acme's replaced session", got)
	}
	if cred, err := auth.LoadCredentialFor(testApp.dir, config.DefaultProfile, orgA().ID); err != nil || cred.SessionID == "s-old" {
		t.Fatalf("Acme's sign-in = %+v, %v; want the new one", cred, err)
	}
}

// A single code, the one organization asked for, signs in exactly as before: stored,
// active, the profile's, and one exchange.
func TestBrowserSignInWithOneCodeSignsInAsBefore(t *testing.T) {
	f := newFakeAuth(t)
	threeOrgs(f)
	authSandbox(t, f)
	b := newFakeBrowser(t, f)
	res, out, err := browserSignInHere(t, b, signInOptions{org: parseOrgRef(orgA().ID)})
	if err != nil {
		t.Fatalf("sign-in: %v\n%s", err, out)
	}
	if links := b.approvalLinks(); len(links) != 1 || links[0].Get("org") != orgA().ID || links[0].Get("orgs") != orgA().ID {
		t.Fatalf("approval links = %v, want org and orgs set to Acme", links)
	}
	if got := f.exchanges.Load(); got != 1 {
		t.Fatalf("exchanges = %d, want one", got)
	}
	if got := signedInOrgs(t); !slices.Equal(got, []string{orgA().ID}) {
		t.Fatalf("signed in to %v", got)
	}
	if cred, profile := activeOrg(t); cred != orgA().ID || profile != orgA().ID || res.orgName != "Acme" || res.reused {
		t.Fatalf("active = %s, profile %s, result %+v", cred, profile, res)
	}
	file, _ := config.LoadFile(testApp.dir)
	if p := file.Profiles[config.DefaultProfile]; p.OrganizationName != "Acme" {
		t.Fatalf("profile = %+v", p)
	}
	if !strings.Contains(out, "Signed in to Acme\n") {
		t.Fatalf("sign-in output:\n%s", out)
	}
}

// Organizations left without a sign-in are named in one line, not one line each: often
// they were left unticked on purpose.
func TestNotSignedInIsOneLine(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		names []string
		want  string
	}{
		{nil, ""},
		{[]string{"ClientCo"}, "Not signed in to ClientCo. Run `terma teams --authorize` to add it."},
		{[]string{"A", "B", "C"}, "Not signed in to 3 organizations: A, B and C. Run `terma teams --authorize` to add them."},
		{[]string{"A", "B", "C", "D", "E", "F", "G"}, "Not signed in to 7 organizations, including A, B, C, D and E. Run `terma teams --authorize` to add them."},
	} {
		if got := notSignedIn(c.names); got != c.want {
			t.Errorf("notSignedIn(%v) = %q, want %q", c.names, got, c.want)
		}
	}
}
