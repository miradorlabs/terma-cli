package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
)

// campaignStub is an active campaign that wants terma 0.43.0 or later.
const campaignStub = `{"campaign":{"id":"c1","title":"Terma needs a newer CLI","message":"m","steps":["terma update","terma setup"],"minimum_client_version":"0.43.0","require_user_session":false,"updated_at":"2026-11-20T10:00:00Z"}}`

// asRelease runs the test as terma at version, a release, rather than the dev build.
func asRelease(t *testing.T, version string) {
	t.Helper()
	prev := testApp.version
	testApp.version = version
	t.Cleanup(func() { testApp.version = prev })
}

const required = "terma 0.43.0 or later is required, this is 0.41.2 — run `terma update`, then `terma setup`"

// doctor leads with an active campaign's notice, its title and its numbered steps, and the
// notice is never a failed check.
func TestDoctorShowsTheRemediationNotice(t *testing.T) {
	userSandbox(t)
	asRelease(t, "0.41.2")
	t.Setenv("TERMA_REMEDIATION_STUB", campaignStub)
	env := testApp.doctorEnv(t.Context())
	env.Remediation = testApp.doctorRemediation(t.Context(), env.Config)
	var b strings.Builder
	printContext(&b, doctor.Context(env))
	out := b.String()
	for _, want := range []string{"Required:    " + required, "Terma needs a newer CLI", "1. terma update", "2. terma setup"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor's context lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "Required:") > strings.Index(out, "Config:") {
		t.Errorf("the notice does not lead:\n%s", out)
	}
	for _, c := range doctor.Run(t.Context(), env, doctor.Progress{}).Checks {
		if strings.Contains(c.Detail+c.Fix, "0.43.0") {
			t.Errorf("the notice became a check: %+v", c)
		}
	}
}

// A dev build is never behind, and a release at or above the minimum is not warned.
func TestDoctorIsQuietWhenCurrent(t *testing.T) {
	userSandbox(t)
	t.Setenv("TERMA_REMEDIATION_STUB", campaignStub)
	for _, v := range []string{"dev", "0.43.0", "0.41.2-dirty"} {
		asRelease(t, v)
		if n := testApp.doctorRemediation(t.Context(), testApp.doctorEnv(t.Context()).Config); n.Warn() {
			t.Errorf("%s was warned: %+v", v, n)
		}
	}
}

// A configuration that does not load still gets the notice: it is read from the flags'
// and the environment's auth host, before doctor's early return.
func TestDoctorShowsTheNoticeWithoutAConfiguration(t *testing.T) {
	userSandbox(t)
	asRelease(t, "0.41.2")
	t.Setenv("TERMA_REMEDIATION_STUB", campaignStub)
	if err := os.WriteFile(filepath.Join(testApp.dir, "config.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "doctor")
	if err == nil {
		t.Fatalf("doctor passed with a broken configuration:\n%s", out)
	}
	for _, want := range []string{"Required:    " + required, "1. terma update", "FAIL  configuration"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor lacks %q:\n%s", want, out)
		}
	}
}

// A campaign that wants a person's sign-in warns a machine without one, which a dev build
// is too.
func TestDoctorAsksForAUserSession(t *testing.T) {
	userSandbox(t)
	t.Setenv("TERMA_REMEDIATION_STUB", strings.Replace(campaignStub, `"require_user_session":false`, `"require_user_session":true`, 1))
	n := testApp.doctorRemediation(t.Context(), testApp.doctorEnv(t.Context()).Config)
	if !n.NeedsUserSession || n.Behind || !strings.HasSuffix(n.Summary(), "run `terma setup`") {
		t.Fatalf("signed out: %+v %q", n, n.Summary())
	}
	// Under TERMA_API_KEY setup signs in with the key: the advice unsets it first.
	t.Setenv("TERMA_API_KEY", "set-for-the-test")
	n = testApp.doctorRemediation(t.Context(), testApp.doctorEnv(t.Context()).Config)
	if !n.NeedsUserSession || !strings.HasSuffix(n.Summary(), "run `terma setup` with TERMA_API_KEY unset") {
		t.Fatalf("under TERMA_API_KEY: %+v %q", n, n.Summary())
	}
}

// Only a person's sign-in for this environment that still sends is a user session.
func TestHasUserSession(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if hasUserSession(cfg) || hasUserSession(nil) {
		t.Fatal("signed out, or no configuration, is a user session")
	}
	signInTo(t, gateway, orgA())
	if cfg, err = testApp.loadConfig(); err != nil || !hasUserSession(cfg) {
		t.Fatalf("a person's sign-in is not a user session: %v", err)
	}
	serverKey := *cfg
	serverKey.ServerKeySignIn = true
	viaEnv := *cfg
	viaEnv.APIKey = "set"
	other := *cfg
	other.AuthURL = "https://auth.elsewhere.example"
	for name, c := range map[string]*config.Config{"server-key profile": &serverKey, "TERMA_API_KEY": &viaEnv, "another environment": &other} {
		if hasUserSession(c) {
			t.Errorf("%s is a user session", name)
		}
	}
}

// Setup cautions near its start and carries on.
func TestSetupShowsTheRemediationNotice(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	asRelease(t, "0.41.2")
	t.Setenv("TERMA_REMEDIATION_STUB", campaignStub)
	signInTo(t, gateway, orgA())
	out, err := runTerma(t, "setup", "--harness", "codex")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	notice := strings.Index(out, "! Required      "+required)
	if notice < 0 || !strings.Contains(out, "    2. terma setup") {
		t.Fatalf("setup lacks the notice:\n%s", out)
	}
	if agents := strings.Index(out, "Agents"); agents < notice {
		t.Errorf("the notice comes after setup's steps:\n%s", out)
	}
	if !strings.Contains(out, "✓ Signed in") || !strings.Contains(out, "✓ Machine") {
		t.Errorf("setup did not carry on:\n%s", out)
	}
}

// An older platform without the route, a failing one or a malformed answer prints nothing.
func TestRemediationFailuresAreSilent(t *testing.T) {
	userSandbox(t)
	asRelease(t, "0.41.2")
	for name, serve := range map[string]http.HandlerFunc{
		"404":       func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) },
		"503":       func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) },
		"malformed": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"campaign":{`)) },
	} {
		srv := httptest.NewServer(serve)
		t.Setenv("TERMA_REMEDIATION_STUB", "")
		t.Setenv("TERMA_AUTH_URL", srv.URL)
		env := testApp.doctorEnv(t.Context())
		env.Remediation = testApp.doctorRemediation(t.Context(), env.Config)
		srv.Close()
		var b strings.Builder
		printContext(&b, doctor.Context(env))
		if env.Remediation.Warn() || strings.Contains(b.String(), "Required") {
			t.Errorf("%s: printed a notice:\n%s", name, b.String())
		}
	}
	t.Setenv("TERMA_REMEDIATION_STUB", "not json")
	if out, _ := runTerma(t, "doctor"); strings.Contains(out, "Required") || strings.Contains(out, "remediation") {
		t.Errorf("a malformed answer printed:\n%s", out)
	}
}

// `terma status` reads local state alone: it never asks for the campaign.
func TestStatusNeverAsksForTheCampaign(t *testing.T) {
	userSandbox(t)
	var asked atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "remediation") {
			asked.Store(true)
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("TERMA_REMEDIATION_STUB", "")
	t.Setenv("TERMA_AUTH_URL", srv.URL)
	_, _ = runTerma(t, "status")
	if asked.Load() {
		t.Fatal("status asked for the remediation campaign")
	}
	_, _ = runTerma(t, "doctor")
	if !asked.Load() {
		t.Fatal("doctor did not ask for the remediation campaign")
	}
}

// Setup under a server key, for a campaign that wants a person's sign-in, says to unset
// the key, and carries on.
func TestSetupUnderAServerKeyAsksForAUserSession(t *testing.T) {
	userSandbox(t)
	t.Setenv("TERMA_REMEDIATION_STUB", strings.Replace(campaignStub, `"require_user_session":false`, `"require_user_session":true`, 1))
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.APIKey = "set-for-the-test"
	ui := newSetupUI(io.Discard, false)
	var b strings.Builder
	ui.out = &b
	testApp.cautionRemediation(t.Context(), ui, cfg)
	if !strings.Contains(b.String(), "run `terma setup` with TERMA_API_KEY unset") {
		t.Fatalf("setup under a server key:\n%s", b.String())
	}
}
