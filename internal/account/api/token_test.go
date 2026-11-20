package api

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
)

// tokenBodies serves the token endpoint and returns the JSON bodies it was sent.
func tokenBodies(t *testing.T) (*Client, *[]map[string]any) {
	t.Helper()
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != tokenPath || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("token body is not JSON: %v", err)
		}
		bodies = append(bodies, body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "mir_cli_x", "refresh_token": "mir_clr_x", "token_type": "Bearer", "expires_in": 3600,
			"session_id": "s-1", "family_id": "f-1",
			"user": map[string]string{"id": "user-1", "email": "dev@example.com"},
		})
	}))
	t.Cleanup(srv.Close)
	return NewSignIn(srv.URL, "test", testInstallation), &bodies
}

const testInstallation = "c2d7a4b1-0e2f-4a6b-9c1d-2e3f4a5b6c7d"

// A delegated code is redeemed at the same endpoint as a browser one, with redirect_port 0
// sent, not left out: the auth host tells the two apart by its own record of the code, and
// takes 0 only for a delegated one. The body is terma-desktop's shared/fixtures/auth.json
// tokenRequests.delegated, copied beside the grant's lines, plus the installation's id,
// which every exchange of a user session carries.
func TestExchangeDelegatedCodeSendsRedirectPortZero(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../auth/testdata/delegated_grant.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		TokenRequests struct {
			Delegated struct {
				Input struct{ Code, Verifier string }
				Body  map[string]any
			}
		} `json:"tokenRequests"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	pinned := fixture.TokenRequests.Delegated
	client, bodies := tokenBodies(t)
	cred, err := client.ExchangeDelegatedCode(context.Background(), pinned.Input.Code, pinned.Input.Verifier)
	if err != nil {
		t.Fatal(err)
	}
	want := maps.Clone(pinned.Body)
	want["installation_id"] = testInstallation
	if len(pinned.Body) == 0 || len(*bodies) != 1 || !reflect.DeepEqual((*bodies)[0], want) {
		t.Fatalf("token bodies = %v, want %v", *bodies, want)
	}
	if cred.UserID != "user-1" || cred.SessionID != "s-1" || cred.FamilyID != "f-1" || cred.AuthURL == "" {
		t.Fatalf("credential = %+v", cred)
	}
}

// Signing in needs the installation's id: a code exchanged without one is never sent.
func TestExchangeNeedsTheInstallation(t *testing.T) {
	t.Parallel()
	if _, err := NewAnonymous("http://127.0.0.1:1", "test").ExchangeCode(context.Background(), "c", "v", 5555); err == nil {
		t.Fatal("an exchange without an installation id was sent")
	}
}

// The browser's exchange is unchanged: its listener's port, as tokenRequests.code pins it.
func TestExchangeCodeSendsTheListenersPort(t *testing.T) {
	t.Parallel()
	client, bodies := tokenBodies(t)
	if _, err := client.ExchangeCode(context.Background(), "the-code", "v1", 5555); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"grant_type": "authorization_code", "code": "the-code", "code_verifier": "v1", "redirect_port": float64(5555),
		"installation_id": testInstallation}
	if len(*bodies) != 1 || !reflect.DeepEqual((*bodies)[0], want) {
		t.Fatalf("token bodies = %v, want %v", *bodies, want)
	}
}

// A refresh names no port at all (tokenRequests.refresh).
func TestRefreshBodyHasNoRedirectPort(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(tokenRequest{GrantType: "refresh_token", RefreshToken: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw), `{"grant_type":"refresh_token","refresh_token":"r1"}`; got != want {
		t.Fatalf("refresh body = %s, want %s", got, want)
	}
}
