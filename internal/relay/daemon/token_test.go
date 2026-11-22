package daemon

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

const authURL = "https://auth.example"

// A teardown and setup under the same sign-in leave the token as it was, so an agent
// still running with it is accepted by the relay that setup starts.
func TestARetiredTokenIsRevivedForTheSameSignIn(t *testing.T) {
	stateDir, _, _ := setUpRelay(t)
	if err := RetireToken(stateDir, "org-a", authURL); err != nil {
		t.Fatal(err)
	}
	if claim.Enabled(stateDir) || setUp(stateDir) {
		t.Fatal("a retired token still enables the relay")
	}
	token, ok := ReviveToken(stateDir, "org-a", authURL)
	if !ok || token != "tok" {
		t.Fatalf("ReviveToken = %q, %v", token, ok)
	}
	if now, err := Token(stateDir); err != nil || now != "tok" {
		t.Fatalf("the relay's token after revival = %q, %v", now, err)
	}

	// The agent launched before teardown still presents "tok".
	addr := freeAddr(t)
	c := runConfig(stateDir, 0, nil)
	c.Addr = addr
	startRun(t, c).await(t, "the relay")
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/metrics", bytes.NewReader(nil))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an export with the token from before teardown = %s", resp.Status)
	}
}

// A token kept for one sign-in is never restored for another organization or
// environment, and is discarded rather than kept for later.
func TestARetiredTokenIsNotRevivedForAnotherSignIn(t *testing.T) {
	t.Parallel()
	for _, other := range []struct{ org, authURL string }{{"org-b", authURL}, {"org-a", "https://auth.other"}, {"", authURL}} {
		stateDir, _, _ := setUpRelay(t)
		if err := RetireToken(stateDir, "org-a", authURL); err != nil {
			t.Fatal(err)
		}
		if token, ok := ReviveToken(stateDir, other.org, other.authURL); ok || token != "" {
			t.Fatalf("revived for %+v: %q", other, token)
		}
		if _, ok := ReviveToken(stateDir, "org-a", authURL); ok {
			t.Fatalf("a token offered to %+v was kept", other)
		}
		if setUp(stateDir) {
			t.Fatal("a refused revival wrote a token")
		}
	}
}

// Without a sign-in to keep it for, or once discarded, the token is only removed.
func TestARetiredTokenNeedsASignIn(t *testing.T) {
	t.Parallel()
	stateDir, _, token := setUpRelay(t)
	if err := RetireToken(stateDir, "", authURL); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(token); !os.IsNotExist(err) {
		t.Fatalf("the token survived: %v", err)
	}
	if _, ok := ReviveToken(stateDir, "", authURL); ok {
		t.Fatal("revived a token kept for no one")
	}

	stateDir, _, _ = setUpRelay(t)
	if err := RetireToken(stateDir, "org-a", authURL); err != nil {
		t.Fatal(err)
	}
	if err := DiscardRetiredToken(stateDir); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReviveToken(stateDir, "org-a", authURL); ok {
		t.Fatal("revived a discarded token")
	}
	if err := RetireToken(stateDir, "org-a", authURL); err != nil {
		t.Fatalf("retiring with no token: %v", err)
	}
}

// A token an earlier terma retired names its organization, not an account: it is revived
// for that organization's server key, the same sign-in.
func TestAnEarlierTermasRetiredTokenIsRevivedForItsServerKey(t *testing.T) {
	stateDir, _, _ := setUpRelay(t)
	if err := RetireToken(stateDir, "org-a", authURL); err != nil {
		t.Fatal(err)
	}
	earlier := []byte(`{"token":"tok","organization_id":"org-1","auth_url":"` + authURL + `","retired_at":"2026-11-01T00:00:00Z"}` + "\n")
	if err := os.WriteFile(filepath.Join(stateDir, RetiredTokenFile), earlier, 0o600); err != nil {
		t.Fatal(err)
	}
	if token, ok := ReviveToken(stateDir, "organization:org-1", authURL); !ok || token != "tok" {
		t.Fatalf("ReviveToken = %q, %v; want the earlier terma's token", token, ok)
	}
}
