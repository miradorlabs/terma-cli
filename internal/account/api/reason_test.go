package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
)

// A refusal's AIP-193 reason is read from error.reason, and named among its details.
func TestParseErrorReadsTheReason(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":{"code":"PERMISSION_DENIED","reason":"TEAM_NOT_FOUND","message":"no such team"}}`)
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, liveCredential(), "team")
	err := c.AuthGet(context.Background(), "/v1/policy", nil, &struct{}{})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden || apiErr.Reason != ReasonTeamNotFound {
		t.Fatalf("err = %#v, want a 403 with TEAM_NOT_FOUND", err)
	}
	if got := apiErr.Error(); got != "no such team (PERMISSION_DENIED, TEAM_NOT_FOUND)" {
		t.Fatalf("message = %q", got)
	}
}

// A refresh the auth host refuses keeps its refusal: still auth.ErrNotLoggedIn, with the
// same message, and beneath it the 401 and its reason, marked as a refresh's.
func TestARefusedRefreshKeepsItsReason(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body, reason string
	}{
		{"organization not accessible", `{"error":{"code":"UNAUTHENTICATED","reason":"ORG_NOT_ACCESSIBLE","message":"organization not accessible"}}`, ReasonOrgNotAccessible},
		{"no reason", `{"error":{"code":"UNAUTHENTICATED","message":"invalid refresh token"}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reads := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tokenPath {
					w.WriteHeader(http.StatusUnauthorized)
					fmt.Fprint(w, tc.body)
					return
				}
				reads++
				w.Write([]byte(`{}`))
			}))
			defer srv.Close()
			expired := liveCredential()
			expired.ExpiresAt = time.Now().Add(-time.Hour)
			c := newTestClient(t, srv.URL, expired, "team")
			err := c.AuthGet(context.Background(), "/v1/policy", nil, &struct{}{})
			if !errors.Is(err, auth.ErrNotLoggedIn) || err.Error() != "not logged in: session expired, run `terma setup`" {
				t.Fatalf("err = %v, want the signed-out refusal it always was", err)
			}
			var refresh *RefreshError
			var apiErr *Error
			if !errors.As(err, &refresh) || !errors.As(refresh.Err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized || apiErr.Reason != tc.reason {
				t.Fatalf("err = %#v: the refresh's 401 and reason %q are gone", err, tc.reason)
			}
			if reads != 0 {
				t.Fatalf("the request went %d times after its refresh failed", reads)
			}
		})
	}
}

// A request refused outright, not its refresh, is no RefreshError, even with the reason a
// refresh would carry.
func TestARefusedRequestIsNoRefresh(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":{"code":"PERMISSION_DENIED","reason":"ORG_NOT_ACCESSIBLE","message":"no"}}`)
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, liveCredential(), "team")
	err := c.AuthGet(context.Background(), "/v1/policy", nil, &struct{}{})
	var refresh *RefreshError
	if err == nil || errors.As(err, &refresh) {
		t.Fatalf("err = %#v, want a plain refusal", err)
	}
}

// A 2.x policy is read only by a reader that matches wildcards, and is marked so; the
// single-team reader refuses it, keeping its last validated one, as before.
func TestPolicyMajorTwoIsReadOnlyWithWildcards(t *testing.T) {
	t.Setenv("TERMA_POLICY_STUB", "")
	for _, version := range []string{"1.1", "2.0"} {
		t.Run(version, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"policy":{"version":%q,"terma":{%s,"per_repository":{"repositories":["github.com/acme/*"]}}},"revision":3,"updated_at":"2026-11-13T12:00:00Z"}`, version, testCapture)
			}))
			defer srv.Close()
			c := newSplitTestClient(t, "http://127.0.0.1:1", srv.URL, liveCredential(), "team")
			_, single := c.CollectionPolicy(context.Background())
			if (single == nil) != (version == "1.1") {
				t.Fatalf("CollectionPolicy(%s) = %v", version, single)
			}
			p, err := c.TeamPolicy(context.Background())
			if err != nil || p.Wildcards != (version == "2.0") || p.Revision != 3 || len(p.Repositories) != 1 {
				t.Fatalf("TeamPolicy(%s) = %+v, %v", version, p, err)
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"policy":{"version":"3.0","terma":{%s,"global":{}}},"revision":3,"updated_at":"2026-11-13T12:00:00Z"}`, testCapture)
	}))
	defer srv.Close()
	c := newSplitTestClient(t, "http://127.0.0.1:1", srv.URL, liveCredential(), "team")
	if _, err := c.TeamPolicy(context.Background()); err == nil {
		t.Fatal("TeamPolicy read a 3.x policy")
	}
}
