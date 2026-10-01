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
	"strconv"
	"strings"
	"testing"
	"time"
)

type recordingExchanger struct {
	code     string
	verifier string
	port     int
	err      error
}

func (r *recordingExchanger) ExchangeCode(_ context.Context, code, verifier string, port int) (*Credential, error) {
	r.code, r.verifier, r.port = code, verifier, port
	if r.err != nil {
		return nil, r.err
	}
	return &Credential{AccessToken: "mir_cli_test", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// browser drives the login flow the way the approval page does: read the URL the
// CLI printed, then GET its loopback callback with a code and the echoed state.
func browser(t *testing.T, authURL string, code string, overrideState string) {
	t.Helper()
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse authorize url: %v", err)
	}
	q := parsed.Query()

	state := q.Get("state")
	if overrideState != "" {
		state = overrideState
	}
	port := q.Get("port")

	callback := "http://127.0.0.1:" + port + "/callback?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(state)
	resp, err := http.Get(callback)
	if err != nil {
		t.Fatalf("callback request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
}

// captureAuthURL scrapes the URL out of what Login writes to its output writer.
// The CLI always prints it (browser or not), so this mirrors what a user copies.
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

func TestLogin_ExchangesCodeWithTheVerifierAndPort(t *testing.T) {
	exchanger := &recordingExchanger{}
	capture := &urlCapture{ch: make(chan string, 1)}

	done := make(chan error, 1)
	var cred *Credential
	go func() {
		var err error
		cred, err = Login(context.Background(), exchanger, LoginOptions{
			AppURL:    "https://app.example.test",
			Label:     "test-host",
			NoBrowser: true,
			Out:       capture,
		})
		done <- err
	}()

	authURL := <-capture.ch
	browser(t, authURL, "mir_cod_abc123", "")

	if err := <-done; err != nil {
		t.Fatalf("Login: %v", err)
	}
	if cred == nil || cred.AccessToken != "mir_cli_test" {
		t.Fatalf("expected the exchanged credential, got %+v", cred)
	}
	if exchanger.code != "mir_cod_abc123" {
		t.Errorf("code = %q, want the one delivered to the callback", exchanger.code)
	}

	// The verifier must never appear in the browser URL — only its hash does.
	parsed, _ := url.Parse(authURL)
	challenge := parsed.Query().Get("challenge")
	sum := sha256.Sum256([]byte(exchanger.verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); challenge != want {
		t.Errorf("challenge %q is not the S256 hash of the verifier sent to the token endpoint", challenge)
	}
	if strings.Contains(authURL, exchanger.verifier) {
		t.Error("the PKCE verifier leaked into the browser URL")
	}

	// The port redeemed must be the port advertised, or the server's binding check
	// would reject every real login.
	wantPort, _ := strconv.Atoi(parsed.Query().Get("port"))
	if exchanger.port != wantPort {
		t.Errorf("exchanged port = %d, advertised %d", exchanger.port, wantPort)
	}
}

func TestLogin_PropagatesExchangeFailure(t *testing.T) {
	wantErr := errors.New("invalid or expired credentials")
	exchanger := &recordingExchanger{err: wantErr}
	capture := &urlCapture{ch: make(chan string, 1)}

	done := make(chan error, 1)
	go func() {
		_, err := Login(context.Background(), exchanger, LoginOptions{
			AppURL:    "https://app.example.test",
			NoBrowser: true,
			Out:       capture,
		})
		done <- err
	}()

	browser(t, <-capture.ch, "mir_cod_expired", "")

	if err := <-done; !errors.Is(err, wantErr) {
		t.Fatalf("expected the exchange error to surface, got %v", err)
	}
}

func TestCredentialExpired(t *testing.T) {
	tests := []struct {
		name string
		cred Credential
		want bool
	}{
		{"zero expiry is treated as expired", Credential{}, true},
		{"past expiry", Credential{ExpiresAt: time.Now().Add(-time.Minute)}, true},
		// Inside the skew: a token this close to expiry would die mid-request.
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
	pkce, err := NewPKCE()
	if err != nil {
		t.Fatalf("NewPKCE: %v", err)
	}
	if len(pkce.Verifier) < 43 || len(pkce.Verifier) > 128 {
		t.Errorf("verifier length %d is outside the RFC 7636 range", len(pkce.Verifier))
	}
	// The server rejects any challenge that is not exactly 43 base64url characters.
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

// callbackWithHost issues a raw callback with a chosen Host header, which is how a DNS
// rebinding attack reaches a loopback listener.
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

// TestLogin_RejectsCallbackFromAForeignHost covers DNS rebinding: a page served from a
// domain that resolves to 127.0.0.1 reaches this listener carrying its own Host, and the
// same-origin policy would then let it read the response.
func TestLogin_RejectsCallbackFromAForeignHost(t *testing.T) {
	exchanger := &recordingExchanger{}
	capture := &urlCapture{ch: make(chan string, 1)}

	done := make(chan error, 1)
	go func() {
		_, err := Login(context.Background(), exchanger, LoginOptions{
			AppURL: "https://app.example.test", NoBrowser: true, Out: capture,
		})
		done <- err
	}()

	authURL := <-capture.ch
	port, state := portFrom(t, authURL)

	resp := callbackWithHost(t, port, "evil.example.com:"+strconv.Itoa(port), "mir_cod_rebind", state)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("foreign Host should be rejected, got %d", resp.StatusCode)
	}
	if exchanger.code != "" {
		t.Error("a code delivered under a foreign Host must never be exchanged")
	}

	// The real callback must still work — the guard is not simply blocking everything.
	browser(t, authURL, "mir_cod_real", "")
	if err := <-done; err != nil {
		t.Fatalf("Login: %v", err)
	}
	if exchanger.code != "mir_cod_real" {
		t.Errorf("code = %q, want the legitimate one", exchanger.code)
	}
}

// TestLogin_WrongStateDoesNotCancelThePendingLogin closes a denial of service: any local
// process, and any web page that guesses the port, can hit the callback. If a mismatched
// state aborted the login, an attacker could stop a user from ever logging in.
func TestLogin_WrongStateDoesNotCancelThePendingLogin(t *testing.T) {
	exchanger := &recordingExchanger{}
	capture := &urlCapture{ch: make(chan string, 1)}

	done := make(chan error, 1)
	go func() {
		_, err := Login(context.Background(), exchanger, LoginOptions{
			AppURL: "https://app.example.test", NoBrowser: true, Out: capture,
		})
		done <- err
	}()

	authURL := <-capture.ch

	// Three hostile attempts, then the genuine one.
	for i := range 3 {
		browser(t, authURL, fmt.Sprintf("mir_cod_attacker%d", i), "not-the-state-we-issued")
	}
	browser(t, authURL, "mir_cod_real", "")

	if err := <-done; err != nil {
		t.Fatalf("Login should survive hostile callbacks: %v", err)
	}
	if exchanger.code != "mir_cod_real" {
		t.Errorf("code = %q, want the legitimate one — an attacker must not be able to divert or cancel the login", exchanger.code)
	}
}

// TestLogin_HostileCallbacksAloneNeverAuthenticate is the other half of the DoS fix: a
// mismatched state must not merely be non-fatal, it must never authenticate. With no
// legitimate callback the login fails closed, on the timeout, having exchanged nothing.
func TestLogin_HostileCallbacksAloneNeverAuthenticate(t *testing.T) {
	exchanger := &recordingExchanger{}
	capture := &urlCapture{ch: make(chan string, 1)}

	done := make(chan error, 1)
	go func() {
		_, err := Login(context.Background(), exchanger, LoginOptions{
			AppURL:    "https://app.example.test",
			NoBrowser: true,
			Out:       capture,
			Timeout:   2 * time.Second,
		})
		done <- err
	}()

	authURL := <-capture.ch
	browser(t, authURL, "mir_cod_attacker", "not-the-state-we-issued")

	err := <-done
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
