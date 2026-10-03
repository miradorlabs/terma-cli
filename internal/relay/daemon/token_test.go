package daemon

import (
	"bytes"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

const authURL = "https://auth.example"

// A teardown and setup under the same sign-in leave the token as it was, so an agent
// still running with it is accepted by the relay that setup starts.
func TestARetiredTokenIsRevivedForTheSameSignIn(t *testing.T) {
	dir, _ := setUpRelay(t)
	if err := RetireToken("org-a", authURL); err != nil {
		t.Fatal(err)
	}
	if claim.Enabled() || setUp() {
		t.Fatal("a retired token still enables the relay")
	}
	token, ok := ReviveToken("org-a", authURL)
	if !ok || token != "tok" {
		t.Fatalf("ReviveToken = %q, %v", token, ok)
	}
	if now, err := Token(); err != nil || now != "tok" {
		t.Fatalf("the relay's token after revival = %q, %v", now, err)
	}

	// The agent launched before teardown still presents "tok".
	addr := freeAddr(t)
	c := runConfig(dir, 0, nil)
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
	for _, other := range []struct{ org, authURL string }{{"org-b", authURL}, {"org-a", "https://auth.other"}, {"", authURL}} {
		setUpRelay(t)
		if err := RetireToken("org-a", authURL); err != nil {
			t.Fatal(err)
		}
		if token, ok := ReviveToken(other.org, other.authURL); ok || token != "" {
			t.Fatalf("revived for %+v: %q", other, token)
		}
		if _, ok := ReviveToken("org-a", authURL); ok {
			t.Fatalf("a token offered to %+v was kept", other)
		}
		if setUp() {
			t.Fatal("a refused revival wrote a token")
		}
	}
}

// Without a sign-in to keep it for, or once discarded, the token is only removed.
func TestARetiredTokenNeedsASignIn(t *testing.T) {
	_, token := setUpRelay(t)
	if err := RetireToken("", authURL); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(token); !os.IsNotExist(err) {
		t.Fatalf("the token survived: %v", err)
	}
	if _, ok := ReviveToken("", authURL); ok {
		t.Fatal("revived a token kept for no one")
	}

	setUpRelay(t)
	if err := RetireToken("org-a", authURL); err != nil {
		t.Fatal(err)
	}
	if err := DiscardRetiredToken(); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReviveToken("org-a", authURL); ok {
		t.Fatal("revived a discarded token")
	}
	if err := RetireToken("org-a", authURL); err != nil {
		t.Fatalf("retiring with no token: %v", err)
	}
}
