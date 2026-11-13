package api

import (
	"context"
	"encoding/json"
	"io"
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
			"session_id": "s-1", "organization": map[string]string{"id": "org-1", "name": "Personal"},
			"user": map[string]string{"id": "user-1", "email": "dev@example.com"},
		})
	}))
	t.Cleanup(srv.Close)
	return NewAnonymous(srv.URL, "test"), &bodies
}

// A delegated code is redeemed at the same endpoint as a browser one, with redirect_port 0
// sent, not left out: the auth host tells the two apart by its own record of the code, and
// takes 0 only for a delegated one. The body is terma-desktop's shared/fixtures/auth.json
// tokenRequests.delegated, copied beside the grant's lines.
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
	want := pinned.Body
	if len(want) == 0 || len(*bodies) != 1 || !reflect.DeepEqual((*bodies)[0], want) {
		t.Fatalf("token bodies = %v, want %v", *bodies, want)
	}
	if cred.OrganizationID != "org-1" || cred.UserID != "user-1" || cred.SessionID != "s-1" || cred.AuthURL == "" {
		t.Fatalf("credential = %+v", cred)
	}
	if got := client.LastLogin(); got == nil || got.OrganizationName != "Personal" {
		t.Fatalf("LastLogin = %+v, want the organization's name", got)
	}
}

// The browser's exchange is unchanged: its listener's port, as tokenRequests.code pins it.
func TestExchangeCodeSendsTheListenersPort(t *testing.T) {
	t.Parallel()
	client, bodies := tokenBodies(t)
	res, err := client.ExchangeCode(context.Background(), "the-code", "v1", 5555)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"grant_type": "authorization_code", "code": "the-code", "code_verifier": "v1", "redirect_port": float64(5555)}
	if len(*bodies) != 1 || !reflect.DeepEqual((*bodies)[0], want) {
		t.Fatalf("token bodies = %v, want %v", *bodies, want)
	}
	// Each exchange names its own organization, so a sign-in to several never reads
	// another's name from LastLogin.
	if res.OrganizationName != "Personal" || res.Credential.OrganizationID != "org-1" || res.UserID != "user-1" {
		t.Fatalf("result = %+v", res)
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
