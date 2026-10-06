package delivery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/spool"
)

// The sender refuses redirects: Go keeps the key's Authorization header across a
// same-host https→http downgrade.
func TestOTLPSenderRefusesRedirects(t *testing.T) {
	t.Parallel()
	var followed bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed = true
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/logs", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	s := &otlpSender{
		Endpoint:  redirector.URL,
		APIKey:    "mir_srv_deadbeef",
		ProjectID: "p1",
		Version:   "test",
	}
	_, err := s.Send(context.Background(), []spool.Event{{Name: "terma.commit", Time: time.Now()}})
	if err == nil {
		t.Fatal("expected the redirect to be refused")
	}
	if !strings.Contains(err.Error(), "refusing to follow redirect") {
		t.Fatalf("expected a redirect refusal, got %v", err)
	}
	if followed {
		t.Fatal("the credential-bearing request reached the redirect target")
	}
}

// The ordinary path still works.
func TestOTLPSenderDeliversWithoutRedirect(t *testing.T) {
	t.Parallel()
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := &otlpSender{Endpoint: srv.URL, APIKey: "mir_srv_deadbeef", ProjectID: "p1", Version: "test"}
	if _, err := s.Send(context.Background(), []spool.Event{{Name: "terma.commit", Time: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer mir_srv_deadbeef" {
		t.Fatalf("unexpected Authorization header %q", gotAuth)
	}
}
