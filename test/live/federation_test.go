package live

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type federationTransport func(*http.Request) (*http.Response, error)

func (f federationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGitHubIdentityToken(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		ok         bool
	}{
		{"success", `{"value":"test.jwt.signature"}`, 200, true},
		{"denied", `secret response must not leak`, 403, false},
		{"empty", `{"value":""}`, 200, false},
		{"malformed", `secret malformed response`, 200, false},
		{"command injection", `{"value":"token\n::warning::oops"}`, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: federationTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Query().Get("audience") != "https://api.anthropic.com" || r.URL.Query().Get("existing") != "keep" {
					t.Fatal("incorrect OIDC query")
				}
				if r.Header.Get("Authorization") != "Bearer request-secret" {
					t.Fatal("missing request credential")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			token, err := githubIdentityToken(client, "https://example.test/token?existing=keep", "request-secret")
			if (err == nil) != tc.ok {
				t.Fatalf("unexpected outcome: %v", err)
			}
			if tc.ok && token != "test.jwt.signature" {
				t.Fatal("incorrect token")
			}
			if err != nil && (strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), tc.body)) {
				t.Fatal("response leaked")
			}
		})
	}
}

func TestGitHubIdentityTokenRejectsMissingOrInsecureConfiguration(t *testing.T) {
	for _, endpoint := range []string{"", "http://example.test", "https://user:password@example.test"} {
		if _, err := githubIdentityToken(nil, endpoint, "credential"); err == nil {
			t.Fatal("accepted invalid endpoint")
		}
	}
	if _, err := githubIdentityToken(nil, "https://example.test", ""); err == nil {
		t.Fatal("accepted missing credential")
	}
}

func TestClaudeFederationDoesNotReachOtherRoutes(t *testing.T) {
	t.Setenv("ANTHROPIC_FEDERATION_RULE_ID", "configured-rule")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "invalid-if-used")
	t.Setenv("ANTHROPIC_API_KEY", "dummy-api-key")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "subscription-token")
	for _, tc := range []struct {
		name          string
		route         Route
		baseURL, want string
	}{
		{"subscription", RouteSubscription, "", "CLAUDE_CODE_OAUTH_TOKEN=subscription-token"},
		{"synthetic API", RouteAPIKey, "http://localhost:1234", "ANTHROPIC_API_KEY=dummy-api-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := &Sandbox{T: t, Mode: Isolated, Dir: t.TempDir(), Terma: "/tmp/terma", ClaudeBaseURL: tc.baseURL}
			env := sb.claudeEnv(tc.route)
			found := false
			for _, entry := range env {
				if entry == tc.want {
					found = true
				}
				if strings.HasPrefix(entry, "ANTHROPIC_FEDERATION_") || strings.HasPrefix(entry, "ANTHROPIC_IDENTITY_") || strings.HasPrefix(entry, "ACTIONS_ID_TOKEN_") {
					t.Fatal("federation credential leaked to unrelated route")
				}
			}
			if !found {
				t.Fatal("route credential missing")
			}
		})
	}
}
