package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type recordingExchanger struct {
	code      string
	verifier  string
	port      int
	delegated bool
	err       error
	// codes are every code exchanged, in order, and verifiers the verifier each was sent
	// with; failing are codes whose exchange fails with their error.
	codes     []string
	verifiers []string
	failing   map[string]error
}

// ExchangeCode signs in to organization org-<code>, named Org <code>.
func (r *recordingExchanger) ExchangeCode(_ context.Context, code, verifier string, port int) (*LoginResult, error) {
	r.code, r.verifier, r.port = code, verifier, port
	r.codes, r.verifiers = append(r.codes, code), append(r.verifiers, verifier)
	if err := r.failing[code]; err != nil {
		return nil, err
	}
	if r.err != nil {
		return nil, r.err
	}
	cred := &Credential{AccessToken: "mir_cli_test", ExpiresAt: time.Now().Add(time.Hour), OrganizationID: "org-" + code}
	return &LoginResult{Credential: cred, OrganizationName: "Org " + code}, nil
}

// ExchangeDelegatedCode records a delegated exchange as port 0, the port it sends.
func (r *recordingExchanger) ExchangeDelegatedCode(ctx context.Context, code, verifier string) (*Credential, error) {
	r.delegated = true
	res, err := r.ExchangeCode(ctx, code, verifier, 0)
	if err != nil {
		return nil, err
	}
	return res.Credential, nil
}

// browser drives the login flow the way the approval page has so far: a GET with the code
// and state in the query.
func browser(t *testing.T, authURL string, code string, overrideState string) {
	t.Helper()
	port, state := portFrom(t, authURL)
	if overrideState != "" {
		state = overrideState
	}

	callback := fmt.Sprintf("http://127.0.0.1:%d/callback?code=%s&state=%s", port, url.QueryEscape(code), url.QueryEscape(state))
	resp, err := http.Get(callback)
	if err != nil {
		t.Fatalf("callback request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
}

// postBrowser drives the login flow with a form POST, the code in the body and never in a
// URL, as the approval page is to send it. It returns the listener's status.
func postBrowser(t *testing.T, authURL string, code string, overrideState string) int {
	t.Helper()
	port, state := portFrom(t, authURL)
	if overrideState != "" {
		state = overrideState
	}
	form := url.Values{"code": {code}, "state": {state}}
	resp, err := http.PostForm(fmt.Sprintf("http://127.0.0.1:%d/callback", port), form)
	if err != nil {
		t.Fatalf("callback request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// postCodes posts state and codes as the approval page posts every organization ticked:
// one repeated code field each, in order. It returns the listener's status.
func postCodes(t *testing.T, authURL string, codes ...string) int {
	t.Helper()
	port, state := portFrom(t, authURL)
	resp, err := http.PostForm(fmt.Sprintf("http://127.0.0.1:%d/callback", port), url.Values{"code": codes, "state": {state}})
	if err != nil {
		t.Fatalf("callback request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// loginAll is LoginAll's outcome, from a background run.
type loginAll struct {
	signedIn []LoginResult
	failed   []error
	err      error
}

// startLoginAll runs LoginAll with opts in the background and returns the URL it asks the
// browser to open.
func startLoginAll(t *testing.T, exchanger TokenExchanger, opts LoginOptions) (authURL string, done <-chan loginAll) {
	t.Helper()
	capture := &urlCapture{ch: make(chan string, 1)}
	results := make(chan loginAll, 1)
	opts.AppURL, opts.NoBrowser, opts.Out = "https://app.example.test", true, capture
	go func() {
		signedIn, failed, err := LoginAll(context.Background(), exchanger, opts)
		results <- loginAll{signedIn, failed, err}
	}()
	return <-capture.ch, results
}

type urlCapture struct {
	ch   chan string
	seen bool
	buf  strings.Builder
}

func (c *urlCapture) Write(p []byte) (int, error) {
	c.buf.Write(p)
	if !c.seen {
		if u := extractURL(c.buf.String()); u != "" {
			c.seen = true
			c.ch <- u
		}
	}
	return len(p), nil
}

func extractURL(s string) string {
	i := strings.Index(s, "http://")
	if i < 0 {
		i = strings.Index(s, "https://")
	}
	if i < 0 {
		return ""
	}
	rest := s[i:]
	end := strings.IndexAny(rest, " \n\t")
	if end < 0 {
		return ""
	}
	return rest[:end]
}

func TestLoginAll_ExchangesCodeWithTheVerifierAndPort(t *testing.T) {
	t.Parallel()
	exchanger := &recordingExchanger{}
	authURL, done := startLoginAll(t, exchanger, LoginOptions{Label: "test-host"})
	browser(t, authURL, "mir_cod_abc123", "")

	res := <-done
	if res.err != nil {
		t.Fatalf("LoginAll: %v", res.err)
	}
	if len(res.signedIn) != 1 || res.signedIn[0].Credential.AccessToken != "mir_cli_test" {
		t.Fatalf("expected the exchanged credential, got %+v", res.signedIn)
	}
	if exchanger.code != "mir_cod_abc123" {
		t.Errorf("code = %q, want the one delivered to the callback", exchanger.code)
	}

	parsed, _ := url.Parse(authURL)
	challenge := parsed.Query().Get("challenge")
	sum := sha256.Sum256([]byte(exchanger.verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); challenge != want {
		t.Errorf("challenge %q is not the S256 hash of the verifier sent to the token endpoint", challenge)
	}
	if strings.Contains(authURL, exchanger.verifier) {
		t.Error("the PKCE verifier leaked into the browser URL")
	}

	if mode := parsed.Query().Get("response_mode"); mode != "form_post" {
		t.Errorf("response_mode = %q, want form_post: the page posts the code only to a CLI that says it takes a POST", mode)
	}

	wantPort, _ := strconv.Atoi(parsed.Query().Get("port"))
	if exchanger.port != wantPort {
		t.Errorf("exchanged port = %d, advertised %d", exchanger.port, wantPort)
	}
}

func TestLoginAll_PropagatesExchangeFailure(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("invalid or expired credentials")
	exchanger := &recordingExchanger{err: wantErr}
	authURL, done := startLoginAll(t, exchanger, LoginOptions{})
	browser(t, authURL, "mir_cod_expired", "")

	if err := (<-done).err; !errors.Is(err, wantErr) {
		t.Fatalf("expected the exchange error to surface, got %v", err)
	}
}

func TestCredentialExpired(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cred Credential
		want bool
	}{
		{"zero expiry is treated as expired", Credential{}, true},
		{"past expiry", Credential{ExpiresAt: time.Now().Add(-time.Minute)}, true},
		{"expiring within the skew", Credential{ExpiresAt: time.Now().Add(30 * time.Second)}, true},
		{"comfortably live", Credential{ExpiresAt: time.Now().Add(time.Hour)}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cred.Expired(); got != tc.want {
				t.Errorf("Expired() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewPKCE_ProducesAValidS256Pair(t *testing.T) {
	t.Parallel()
	pkce, err := NewPKCE()
	if err != nil {
		t.Fatalf("NewPKCE: %v", err)
	}
	if len(pkce.Verifier) < 43 || len(pkce.Verifier) > 128 {
		t.Errorf("verifier length %d is outside the RFC 7636 range", len(pkce.Verifier))
	}
	if len(pkce.Challenge) != 43 {
		t.Errorf("challenge length = %d, want 43", len(pkce.Challenge))
	}

	other, err := NewPKCE()
	if err != nil {
		t.Fatalf("NewPKCE: %v", err)
	}
	if other.Verifier == pkce.Verifier {
		t.Error("two logins produced the same verifier")
	}
}

// callbackWithHost issues a raw callback with a chosen Host header, as DNS rebinding does.
func callbackWithHost(t *testing.T, port int, host, code, state string) *http.Response {
	t.Helper()
	target := fmt.Sprintf("http://127.0.0.1:%d/callback?code=%s&state=%s",
		port, url.QueryEscape(code), url.QueryEscape(state))
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("callback request: %v", err)
	}
	return resp
}

func portFrom(t *testing.T, authURL string) (port int, state string) {
	t.Helper()
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse authorize url: %v", err)
	}
	p, err := strconv.Atoi(parsed.Query().Get("port"))
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return p, parsed.Query().Get("state")
}

// TestLoginAll_RejectsCallbackFromAForeignHost refuses a DNS-rebinding page's callback.
func TestLoginAll_RejectsCallbackFromAForeignHost(t *testing.T) {
	t.Parallel()
	exchanger := &recordingExchanger{}
	authURL, done := startLoginAll(t, exchanger, LoginOptions{})
	port, state := portFrom(t, authURL)

	resp := callbackWithHost(t, port, "evil.example.com:"+strconv.Itoa(port), "mir_cod_rebind", state)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("foreign Host should be rejected, got %d", resp.StatusCode)
	}
	if exchanger.code != "" {
		t.Error("a code delivered under a foreign Host must never be exchanged")
	}

	browser(t, authURL, "mir_cod_real", "")
	if err := (<-done).err; err != nil {
		t.Fatalf("LoginAll: %v", err)
	}
	if exchanger.code != "mir_cod_real" {
		t.Errorf("code = %q, want the legitimate one", exchanger.code)
	}
}

// TestLoginAll_WrongStateDoesNotCancelThePendingLogin proves a mismatched callback cannot deny the login.
func TestLoginAll_WrongStateDoesNotCancelThePendingLogin(t *testing.T) {
	t.Parallel()
	exchanger := &recordingExchanger{}
	authURL, done := startLoginAll(t, exchanger, LoginOptions{})

	for i := range 3 {
		browser(t, authURL, fmt.Sprintf("mir_cod_attacker%d", i), "not-the-state-we-issued")
	}
	browser(t, authURL, "mir_cod_real", "")

	if err := (<-done).err; err != nil {
		t.Fatalf("LoginAll should survive hostile callbacks: %v", err)
	}
	if exchanger.code != "mir_cod_real" {
		t.Errorf("code = %q, want the legitimate one — an attacker must not be able to divert or cancel the login", exchanger.code)
	}
}

// TestLoginAll_HostileCallbacksAloneNeverAuthenticate proves mismatched callbacks alone fail closed
// on the timeout, having exchanged nothing.
func TestLoginAll_HostileCallbacksAloneNeverAuthenticate(t *testing.T) {
	t.Parallel()
	exchanger := &recordingExchanger{}
	authURL, done := startLoginAll(t, exchanger, LoginOptions{Timeout: 2 * time.Second})
	browser(t, authURL, "mir_cod_attacker", "not-the-state-we-issued")

	err := (<-done).err
	if err == nil {
		t.Fatal("expected the login to fail when only hostile callbacks arrive")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected a timeout, got %v", err)
	}
	if exchanger.code != "" {
		t.Error("a code with the wrong state must never reach the token endpoint")
	}
}

// TestLoginAll_AcceptsAPostedCallback takes the code from a form POST, as the approval page sends it.
func TestLoginAll_AcceptsAPostedCallback(t *testing.T) {
	t.Parallel()
	exchanger := &recordingExchanger{}
	authURL, done := startLoginAll(t, exchanger, LoginOptions{})

	if status := postBrowser(t, authURL, "mir_cod_posted", ""); status != http.StatusOK {
		t.Errorf("posted callback status = %d, want 200", status)
	}
	if err := (<-done).err; err != nil {
		t.Fatalf("LoginAll: %v", err)
	}
	if exchanger.code != "mir_cod_posted" {
		t.Errorf("code = %q, want the posted one", exchanger.code)
	}
	if want, _ := portFrom(t, authURL); exchanger.port != want {
		t.Errorf("exchanged port = %d, advertised %d", exchanger.port, want)
	}
}

// TestLoginAll_PostReadsOnlyItsBody proves a POST is answered from its form body: a code and state
// in its query are not read.
func TestLoginAll_PostReadsOnlyItsBody(t *testing.T) {
	t.Parallel()
	exchanger := &recordingExchanger{}
	authURL, done := startLoginAll(t, exchanger, LoginOptions{})
	port, state := portFrom(t, authURL)

	base := fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	for _, tc := range []struct {
		name  string
		query string
		body  url.Values
	}{
		{"code and state in the query", "?code=mir_cod_query&state=" + url.QueryEscape(state), url.Values{}},
		{"state in the body, code in the query", "?code=mir_cod_query", url.Values{"state": {state}}},
	} {
		resp, err := http.PostForm(base+tc.query, tc.body)
		if err != nil {
			t.Fatalf("%s: callback request: %v", tc.name, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: a POST must be read from its body only, got %d", tc.name, resp.StatusCode)
		}
	}

	// The body wins outright: a query beside it, even one naming another code, is not read.
	resp, err := http.PostForm(base+"?code=mir_cod_query&state=not-the-state-we-issued",
		url.Values{"code": {"mir_cod_real"}, "state": {state}})
	if err != nil {
		t.Fatalf("callback request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("a POST whose body is right should be accepted whatever its query says, got %d", resp.StatusCode)
	}
	if err := (<-done).err; err != nil {
		t.Fatalf("LoginAll: %v", err)
	}
	if exchanger.code != "mir_cod_real" {
		t.Errorf("code = %q, want the one in the posted body", exchanger.code)
	}
}

// TestLoginAll_RejectsAPostedCallbackFromAForeignHost holds a POST to the same Host check as a GET.
func TestLoginAll_RejectsAPostedCallbackFromAForeignHost(t *testing.T) {
	t.Parallel()
	exchanger := &recordingExchanger{}
	authURL, done := startLoginAll(t, exchanger, LoginOptions{})
	port, state := portFrom(t, authURL)

	form := url.Values{"code": {"mir_cod_rebind"}, "state": {state}}
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/callback", port), strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "evil.example.com:" + strconv.Itoa(port)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("callback request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("foreign Host should be rejected, got %d", resp.StatusCode)
	}

	postBrowser(t, authURL, "mir_cod_real", "")
	if err := (<-done).err; err != nil {
		t.Fatalf("LoginAll: %v", err)
	}
	if exchanger.code != "mir_cod_real" {
		t.Errorf("code = %q, want the legitimate one", exchanger.code)
	}
}

// TestLoginAll_RefusedCallbacksDoNotCancelThePendingLogin: a posted callback with the wrong state,
// an oversized body or another method is refused, and the login still completes.
func TestLoginAll_RefusedCallbacksDoNotCancelThePendingLogin(t *testing.T) {
	t.Parallel()
	exchanger := &recordingExchanger{}
	authURL, done := startLoginAll(t, exchanger, LoginOptions{})
	port, state := portFrom(t, authURL)
	callback := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	if status := postBrowser(t, authURL, "mir_cod_attacker", "not-the-state-we-issued"); status != http.StatusBadRequest {
		t.Errorf("wrong state: status = %d, want 400", status)
	}

	big := url.Values{"code": {strings.Repeat("a", maxCallbackBody)}, "state": {state}}
	resp, err := http.PostForm(callback, big)
	if err != nil {
		t.Fatalf("oversized callback: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("oversized body: status = %d, want 400", resp.StatusCode)
	}

	for _, method := range []string{http.MethodPut, http.MethodHead, http.MethodDelete} {
		req, err := http.NewRequest(method, callback+"?code=mir_cod_other&state="+url.QueryEscape(state), nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s callback: %v", method, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, resp.StatusCode)
		}
		if allow := resp.Header.Get("Allow"); allow != "GET, POST" {
			t.Errorf("%s: Allow = %q, want \"GET, POST\"", method, allow)
		}
	}

	postBrowser(t, authURL, "mir_cod_real", "")
	if err := (<-done).err; err != nil {
		t.Fatalf("LoginAll should survive refused callbacks: %v", err)
	}
	if exchanger.code != "mir_cod_real" {
		t.Errorf("code = %q, want the legitimate one", exchanger.code)
	}
}

// One approval posts a code per organization ticked: every one is exchanged, in the page's
// order, with the one verifier and the listener's port, and each sign-in keeps the name
// its own exchange gave.
func TestLoginAll_ExchangesEveryCode(t *testing.T) {
	t.Parallel()
	exchanger := &recordingExchanger{}
	authURL, done := startLoginAll(t, exchanger, LoginOptions{})
	if status := postCodes(t, authURL, "personal", "acme", "beta"); status != http.StatusOK {
		t.Fatalf("callback status = %d, want 200", status)
	}
	res := <-done
	if res.err != nil || len(res.failed) != 0 {
		t.Fatalf("LoginAll: %v, failed %v", res.err, res.failed)
	}
	var got []string
	for _, s := range res.signedIn {
		got = append(got, s.Credential.OrganizationID+"="+s.OrganizationName)
	}
	if want := []string{"org-personal=Org personal", "org-acme=Org acme", "org-beta=Org beta"}; !slices.Equal(got, want) {
		t.Fatalf("signed in to %v, want %v", got, want)
	}
	if !slices.Equal(exchanger.codes, []string{"personal", "acme", "beta"}) {
		t.Fatalf("exchanged %v, want each code once, in order", exchanger.codes)
	}
	parsed, _ := url.Parse(authURL)
	sum := sha256.Sum256([]byte(exchanger.verifiers[0]))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); parsed.Query().Get("challenge") != want {
		t.Error("the challenge is not the S256 hash of the verifier")
	}
	for _, v := range exchanger.verifiers {
		if v != exchanger.verifiers[0] {
			t.Fatalf("the codes were exchanged with different verifiers: %v", exchanger.verifiers)
		}
	}
	if want, _ := portFrom(t, authURL); exchanger.port != want {
		t.Errorf("exchanged port = %d, advertised %d", exchanger.port, want)
	}
}

// A failed exchange leaves only its own organization without a sign-in: the ones after it
// are still exchanged and kept, and the failure is returned beside them.
func TestLoginAll_OneFailedExchangeKeepsTheOthers(t *testing.T) {
	t.Parallel()
	expired := errors.New("invalid or expired authorization code")
	exchanger := &recordingExchanger{failing: map[string]error{"acme": expired}}
	authURL, done := startLoginAll(t, exchanger, LoginOptions{})
	postCodes(t, authURL, "personal", "acme", "beta")
	res := <-done
	if res.err != nil {
		t.Fatalf("LoginAll: %v", res.err)
	}
	if len(res.signedIn) != 2 || res.signedIn[0].Credential.OrganizationID != "org-personal" || res.signedIn[1].Credential.OrganizationID != "org-beta" {
		t.Fatalf("signed in = %+v, want personal and beta", res.signedIn)
	}
	if len(res.failed) != 1 || !errors.Is(res.failed[0], expired) {
		t.Fatalf("failed = %v, want acme's error", res.failed)
	}
	if !slices.Equal(exchanger.codes, []string{"personal", "acme", "beta"}) {
		t.Fatalf("exchanged %v: a failure stopped the rest", exchanger.codes)
	}
}

// Only when every exchange fails does the sign-in fail, with the first error.
func TestLoginAll_FailsWithTheFirstErrorWhenEveryExchangeFails(t *testing.T) {
	t.Parallel()
	first, second := errors.New("first refusal"), errors.New("second refusal")
	exchanger := &recordingExchanger{failing: map[string]error{"acme": first, "beta": second}}
	authURL, done := startLoginAll(t, exchanger, LoginOptions{})
	postCodes(t, authURL, "acme", "beta")
	res := <-done
	if !errors.Is(res.err, first) || len(res.signedIn) != 0 || len(res.failed) != 2 {
		t.Fatalf("LoginAll = %+v, want the first error", res)
	}
}

// A callback carrying more codes than one approval may tick, or none, is refused as
// malformed; the login still takes the right one after it.
func TestLoginAll_RefusesMoreThanMaxCodesOrNone(t *testing.T) {
	t.Parallel()
	exchanger := &recordingExchanger{}
	authURL, done := startLoginAll(t, exchanger, LoginOptions{})
	many := make([]string, MaxCodes+1)
	for i := range many {
		many[i] = fmt.Sprintf("c%d", i)
	}
	if status := postCodes(t, authURL, many...); status != http.StatusBadRequest {
		t.Errorf("%d codes: status = %d, want 400", len(many), status)
	}
	if status := postCodes(t, authURL); status != http.StatusBadRequest {
		t.Errorf("no code: status = %d, want 400", status)
	}
	if status := postCodes(t, authURL, "acme", ""); status != http.StatusBadRequest {
		t.Errorf("an empty code: status = %d, want 400", status)
	}
	if len(exchanger.codes) != 0 {
		t.Fatalf("a refused callback's codes were exchanged: %v", exchanger.codes)
	}
	if status := postCodes(t, authURL, many[:MaxCodes]...); status != http.StatusOK {
		t.Fatalf("%d codes: status = %d, want 200", MaxCodes, status)
	}
	if res := <-done; res.err != nil || len(res.signedIn) != MaxCodes {
		t.Fatalf("LoginAll = %d signed in, %v", len(res.signedIn), res.err)
	}
}

// A GET still carries one code, the way the page sent it before it posted: the first, and
// only that one, is exchanged.
func TestLoginAll_GetCarriesOneCode(t *testing.T) {
	t.Parallel()
	exchanger := &recordingExchanger{}
	authURL, done := startLoginAll(t, exchanger, LoginOptions{})
	port, state := portFrom(t, authURL)
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/callback?code=first&code=second&state=%s", port, url.QueryEscape(state)))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if res := <-done; res.err != nil || len(res.signedIn) != 1 || !slices.Equal(exchanger.codes, []string{"first"}) {
		t.Fatalf("LoginAll = %+v, exchanged %v", res, exchanger.codes)
	}
}

// Every link asks for one code per organization (codes=multiple). orgs pre-checks the
// organizations named, at most MaxCodes of them, and none pre-checks every one.
func TestBuildAuthorizeURL_AsksForEveryOrganizationsCode(t *testing.T) {
	t.Parallel()
	many := make([]string, MaxCodes+5)
	for i := range many {
		many[i] = fmt.Sprintf("org-%02d", i)
	}
	for _, tc := range []struct {
		name     string
		org      string
		orgs     []string
		wantOrg  string
		wantOrgs string
	}{
		{name: "every organization"},
		{name: "--org", org: "org-x", orgs: []string{"org-x"}, wantOrg: "org-x", wantOrgs: "org-x"},
		{name: "several", orgs: []string{"org-a", "org-b"}, wantOrgs: "org-a,org-b"},
		{name: "over the cap", orgs: many, wantOrgs: strings.Join(many[:MaxCodes], ",")},
	} {
		parsed, err := url.Parse(buildAuthorizeURL("https://app.example.test", "ch", "st", 4242, "host", tc.org, tc.orgs))
		if err != nil {
			t.Fatal(err)
		}
		q := parsed.Query()
		if q.Get("codes") != "multiple" || q.Get("response_mode") != "form_post" {
			t.Errorf("%s: codes = %q, response_mode = %q", tc.name, q.Get("codes"), q.Get("response_mode"))
		}
		if q.Get("org") != tc.wantOrg || q.Get("orgs") != tc.wantOrgs || q.Has("orgs") != (tc.wantOrgs != "") {
			t.Errorf("%s: org = %q, orgs = %q; want %q, %q", tc.name, q.Get("org"), q.Get("orgs"), tc.wantOrg, tc.wantOrgs)
		}
	}
	authURL, done := startLoginAll(t, &recordingExchanger{}, LoginOptions{Organization: "org-x", Organizations: []string{"org-x", "org-y"}})
	if q, _ := url.Parse(authURL); q.Query().Get("orgs") != "org-x,org-y" || q.Query().Get("codes") != "multiple" {
		t.Errorf("LoginAll's link = %s", authURL)
	}
	postCodes(t, authURL, "x")
	<-done
}
