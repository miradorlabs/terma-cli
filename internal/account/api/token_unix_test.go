//go:build unix

package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// A rotation the store failed to take leaves it holding the refresh token the server
// spent. Every later refresh must redeem the client's own live token, never take the
// stored one back: the server's reuse detection would revoke the session. Saves fail once,
// then twice in a row, before the store takes the pair again. The store turns read-only
// as the first exchange reaches the server, after its token was marked, and stays so for
// every failing save.
func TestClient_KeepsARotationTheStoreMissed(t *testing.T) {
	t.Parallel()
	for _, failures := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d failed saves", failures), func(t *testing.T) {
			t.Parallel()
			testFailedSaves(t, failures)
		})
	}
}

func testFailedSaves(t *testing.T, failures int) {
	var mu sync.Mutex
	spent := map[string]bool{}
	revoked := false
	n := 0
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req tokenRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		defer mu.Unlock()
		if spent[req.RefreshToken] {
			revoked = true
		}
		if revoked {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		spent[req.RefreshToken] = true
		n++
		if n == 1 {
			// A read-only directory: the store keeps the spent token through every save.
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Error(err)
			}
		}
		// Each pair is already within the expiry skew, so every Refresh redeems again.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": fmt.Sprintf("mir_cli_%d", n), "refresh_token": fmt.Sprintf("mir_clr_%d", n), "expires_in": 30,
			"session_id": "session-1", "organization": map[string]string{"id": "org-test"},
		})
	}))
	defer srv.Close()
	isRevoked := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return revoked
	}
	writable := func() {
		t.Helper()
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	// Tokens in credentials.json, where a failed write leaves the spent pair.
	if err := config.UpdateFile(dir, func(f *config.File) { f.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SaveCredential(dir, config.DefaultProfile, &auth.Credential{AccessToken: "mir_cli_0", RefreshToken: "mir_clr_0",
		SessionID: "session-1", OrganizationID: "org-test", AuthURL: srv.URL, ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	client, err := New(&config.Config{Dir: dir, ProfileName: config.DefaultProfile, APIURL: srv.URL, AuthURL: srv.URL}, Options{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}

	for i := 1; i <= failures; i++ {
		if err := client.Refresh(context.Background()); err == nil {
			t.Fatalf("refresh %d stored its pair in a read-only directory", i)
		}
		if isRevoked() {
			t.Fatalf("refresh %d redeemed a spent refresh token and the session was revoked", i)
		}
		if got, want := client.Credential().RefreshToken, fmt.Sprintf("mir_clr_%d", i); got != want {
			t.Fatalf("after %d failed saves the client holds %q, want the live %q", i, got, want)
		}
	}

	writable()
	if err := client.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh once the store is writable: %v", err)
	}
	if isRevoked() {
		t.Fatal("the refresh redeemed a spent refresh token and the session was revoked")
	}
	stored, err := auth.LoadCredential(dir, config.DefaultProfile)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("mir_clr_%d", failures+1)
	if got := client.Credential().RefreshToken; stored.RefreshToken != want || got != want {
		t.Errorf("stored %q, client %q; want both the live %q", stored.RefreshToken, got, want)
	}
}

// A store that failed to take a rotation, with tokens in the keychain, still holds the
// spent token. The client's live token, which the store does not hold, is marked in this
// client alone, never in a store that cannot even take its pair: so it is still sent, and
// the session moves on, while the store stays read-only.
func TestClient_ALiveTokenTheStoreMissedIsMarkedHereAlone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var n atomic.Int32
	log, srv := newTokenLog(t, func(w http.ResponseWriter) {
		i := n.Add(1)
		if i == 1 {
			// From the first answer on, neither the keychain nor credentials.json takes a
			// pair, so the store keeps the spent mir_clr_0.
			secret.RefuseForTest(t, dir, "credential/")
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Error(err)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": fmt.Sprintf("mir_cli_%d", i), "refresh_token": fmt.Sprintf("mir_clr_%d", i), "expires_in": 30,
			"session_id": "session-1", "organization": map[string]string{"id": "org-test"},
		})
	})
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := auth.SaveCredential(dir, config.DefaultProfile, &auth.Credential{AccessToken: "mir_cli_0", RefreshToken: "mir_clr_0",
		SessionID: "session-1", OrganizationID: "org-test", AuthURL: srv.URL, ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	client, err := New(&config.Config{Dir: dir, ProfileName: config.DefaultProfile, APIURL: srv.URL, AuthURL: srv.URL}, Options{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}

	if err := client.Refresh(context.Background()); err == nil {
		t.Fatal("first Refresh stored its pair in a read-only store")
	}
	if got := storedFor(t, client).RefreshToken; got != "mir_clr_0" {
		t.Fatalf("stored refresh token = %q, want the spent mir_clr_0 kept", got)
	}

	err = client.Refresh(context.Background())
	if err == nil || strings.Contains(err.Error(), "before sending it") {
		t.Fatalf("second Refresh = %v, want the live token sent and its pair not stored", err)
	}
	if want := []string{"mir_clr_0", "mir_clr_1"}; !slices.Equal(log.tokens(), want) {
		t.Errorf("refresh tokens sent = %q, want %q", log.tokens(), want)
	}
	if got := client.Credential().RefreshToken; got != "mir_clr_2" {
		t.Errorf("client refresh token = %q, want the live mir_clr_2", got)
	}
}

// A rotation the store missed leaves the client's live pair in this process alone. When a
// refresh of that pair then goes unanswered, its mark is this client's alone too. Once the
// store takes writes again, a call that sends nothing must not publish the pair without
// its mark: a fresh process would read its token as never sent, and send it after the
// server's grace. The store keeps the spent token, whose own mark it still has, and a fresh
// process past the budget sends nothing.
func TestClient_AMarkedPairTheStoreMissedIsNotPublishedWithoutItsMark(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var n atomic.Int32
	log, srv := newTokenLog(t, func(w http.ResponseWriter) {
		if n.Add(1) > 1 {
			badGateway(w)
			return
		}
		// From the first answer on, credentials.json takes no write.
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "mir_cli_1", "refresh_token": "mir_clr_1", "expires_in": 3600,
			"session_id": "session-1", "organization": map[string]string{"id": "org-test"},
		})
	})
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := config.UpdateFile(dir, func(f *config.File) { f.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SaveCredential(dir, config.DefaultProfile, &auth.Credential{AccessToken: "mir_cli_0", RefreshToken: "mir_clr_0",
		SessionID: "session-1", OrganizationID: "org-test", AuthURL: srv.URL, ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Dir: dir, ProfileName: config.DefaultProfile, APIURL: srv.URL, AuthURL: srv.URL}
	client, err := New(cfg, Options{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	quickRetries(client, 5)

	if err := client.Refresh(context.Background()); err == nil {
		t.Fatal("first Refresh stored its pair in a read-only store")
	}
	if err := client.RefreshRefused(context.Background(), TokenSHA256("mir_cli_1")); !errors.As(err, new(unsettledError)) {
		t.Fatalf("refresh of the live pair = %v, want an unknown outcome", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Nothing is due: the stored pair is not the refused one, and the held one is live.
	if err := client.RefreshRefused(context.Background(), TokenSHA256("mir_cli_other")); err != nil {
		t.Fatalf("Refresh with nothing to redeem = %v", err)
	}

	stored, err := auth.LoadCredential(dir, config.DefaultProfile)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken == "mir_clr_1" && stored.UnsettledSince().IsZero() {
		t.Fatal("the store holds the live pair without the mark of its unanswered refresh")
	}
	sent := len(log.tokens())
	fresh, err := New(cfg, Options{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	fresh.now = func() time.Time { return time.Now().Add(2 * RefreshBudget) }
	if err := fresh.RefreshRefused(context.Background(), TokenSHA256(stored.AccessToken)); !errors.Is(err, errRefreshUnsettled) {
		t.Errorf("fresh process's refresh = %v, want a sign-in to redo", err)
	}
	if got := log.tokens()[sent:]; len(got) != 0 {
		t.Errorf("fresh process sent %q; want nothing", got)
	}
}

// A save of the live pair that fails half way, the keychain taking its tokens and
// credentials.json not, leaves the store giving the live token under the spent token's
// mark, which does not count for it. Its refresh must then be marked in the store before
// it is sent, as any stored token's is: credentials.json still takes no write, so the
// token is not sent at all, and no process can later read it as never sent.
func TestClient_ALiveTokenTheKeychainTookIsMarkedInTheStore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var n atomic.Int32
	var unlock atomic.Value // func()
	log, srv := newTokenLog(t, func(w http.ResponseWriter) {
		if n.Add(1) > 1 {
			badGateway(w)
			return
		}
		// The first rotation reaches neither the keychain nor credentials.json.
		unlock.Store(secret.FailForTest(t, dir))
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "mir_cli_1", "refresh_token": "mir_clr_1", "expires_in": 30,
			"session_id": "session-1", "organization": map[string]string{"id": "org-test"},
		})
	})
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := auth.SaveCredential(dir, config.DefaultProfile, &auth.Credential{AccessToken: "mir_cli_0", RefreshToken: "mir_clr_0",
		SessionID: "session-1", OrganizationID: "org-test", AuthURL: srv.URL, ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	client, err := New(&config.Config{Dir: dir, ProfileName: config.DefaultProfile, APIURL: srv.URL, AuthURL: srv.URL}, Options{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	quickRetries(client, 5)

	if err := client.Refresh(context.Background()); err == nil {
		t.Fatal("first Refresh stored its pair in a store that takes nothing")
	}
	// The keychain takes writes again; credentials.json still does not.
	unlock.Load().(func())()
	if got := storedFor(t, client).RefreshToken; got != "mir_clr_0" {
		t.Fatalf("stored refresh token = %q, want the spent mir_clr_0 kept", got)
	}

	err = client.Refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "before sending it") {
		t.Fatalf("second Refresh = %v, want the live token not sent, its mark not written", err)
	}
	if got := storedFor(t, client).RefreshToken; got != "mir_clr_1" {
		t.Fatalf("stored refresh token = %q, want the live mir_clr_1 the keychain took", got)
	}
	if want := []string{"mir_clr_0"}; !slices.Equal(log.tokens(), want) {
		t.Errorf("refresh tokens sent = %q, want %q", log.tokens(), want)
	}
}

// A refresh token that cannot be marked before it is sent is not sent: were its answer
// lost, nothing would say the server may have spent it. Nor is it remembered as sent: once
// the store takes marks again, well past the budget, it is sent as a fresh refresh.
func TestClient_AnUnmarkableRefreshIsNotSent(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "mir_cli_1", "refresh_token": "mir_clr_1", "expires_in": 3600,
			"session_id": "session-1", "organization": map[string]string{"id": "org-test"},
		})
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := config.UpdateFile(dir, func(f *config.File) { f.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SaveCredential(dir, config.DefaultProfile, &auth.Credential{AccessToken: "mir_cli_0", RefreshToken: "mir_clr_0",
		SessionID: "session-1", OrganizationID: "org-test", AuthURL: srv.URL, ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	client, err := New(&config.Config{Dir: dir, ProfileName: config.DefaultProfile, APIURL: srv.URL, AuthURL: srv.URL}, Options{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := client.Refresh(context.Background()); err == nil || errors.As(err, new(unsettledError)) {
		t.Fatalf("Refresh = %v, want the mark's failure", err)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("%d requests sent without a mark; want none", n)
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	client.now = func() time.Time { return time.Now().Add(2 * RefreshBudget) }
	if err := client.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh once the store takes marks: %v", err)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("%d requests; want the token sent once", n)
	}
}

// A refresh that finds its token's mark in the future (the wall clock went back) expires
// it. When credentials.json cannot take that, the refresh fails all the same, and this
// client keeps its verdict: once the store is writable and the clock has come round to
// the stored mark, which then looks in budget, this process still never sends the token.
func TestClient_AFailedExpireStillNeverSends(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := config.UpdateFile(dir, func(f *config.File) { f.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SaveCredential(dir, config.DefaultProfile, &auth.Credential{AccessToken: "mir_cli_0", RefreshToken: "mir_clr_0",
		SessionID: "session-1", OrganizationID: "org-test", AuthURL: srv.URL, ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	start := time.Now().Round(0)
	future := start.Add(time.Minute)
	if err := auth.Locked(context.Background(), dir, func(tx *auth.Tx) error {
		return tx.MarkUnsettled(config.DefaultProfile, "org-test", "mir_clr_0", future)
	}); err != nil {
		t.Fatal(err)
	}
	client, err := New(&config.Config{Dir: dir, ProfileName: config.DefaultProfile, APIURL: srv.URL, AuthURL: srv.URL}, Options{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	client.pauseUnit = time.Millisecond
	client.now = func() time.Time { return start }
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err = client.Refresh(context.Background())
	if !errors.Is(err, errRefreshUnsettled) || !strings.Contains(err.Error(), "may not be sent again") {
		t.Fatalf("Refresh = %v, want errRefreshUnsettled and the failed write", err)
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stored, err := auth.LoadCredential(dir, config.DefaultProfile)
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.UnsettledSince(); !got.Equal(future) {
		t.Fatalf("stored mark = %v, want the future one the failed write left", got)
	}
	client.now = func() time.Time { return future.Add(time.Second) }
	if err := client.Refresh(context.Background()); !errors.Is(err, errRefreshUnsettled) {
		t.Fatalf("Refresh with the stored mark in budget = %v, want errRefreshUnsettled", err)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("%d requests sent; want the token never sent", n)
	}
}

// TestHoldCredentialLock is the hung process TestClient_RefreshGivesUpOnAHungHolder waits
// behind: it takes the credential lock, says so, and never lets go until it is killed.
func TestHoldCredentialLock(t *testing.T) {
	dir := os.Getenv("TERMA_HOLD_CREDENTIAL_LOCK")
	if dir == "" {
		t.Skip("run only as TestClient_RefreshGivesUpOnAHungHolder's child")
	}
	_ = auth.Locked(context.Background(), dir, func(*auth.Tx) error {
		fmt.Println("held")
		time.Sleep(time.Hour)
		return nil
	})
}

// A process that hangs holding the credential lock stalls no other: a refresh waits for
// it no longer than its deadline, then fails without redeeming anything.
func TestClient_RefreshGivesUpOnAHungHolder(t *testing.T) {
	t.Parallel()
	var redemptions atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redemptions.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := config.UpdateFile(dir, func(f *config.File) { f.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SaveCredential(dir, config.DefaultProfile, &auth.Credential{AccessToken: "mir_cli_0", RefreshToken: "mir_clr_0",
		SessionID: "session-1", OrganizationID: "org-test", AuthURL: srv.URL, ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}

	holder := exec.Command(os.Args[0], "-test.run=^TestHoldCredentialLock$")
	holder.Env = append(os.Environ(), "TERMA_HOLD_CREDENTIAL_LOCK="+dir)
	out, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "held\n" {
		t.Fatalf("holder said %q, %v; want it holding the lock", line, err)
	}

	client, err := New(&config.Config{Dir: dir, ProfileName: config.DefaultProfile, APIURL: srv.URL, AuthURL: srv.URL},
		Options{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	const wait = 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	start := time.Now()
	err = client.Refresh(ctx)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "another terma process held them") {
		t.Fatalf("Refresh behind a hung holder = %v; want the lock's deadline", err)
	}
	if elapsed > wait+2*time.Second {
		t.Fatalf("Refresh waited %v behind a hung holder; want about its %v deadline", elapsed, wait)
	}
	if n := redemptions.Load(); n != 0 {
		t.Fatalf("%d redemptions without the lock; want none", n)
	}
}
