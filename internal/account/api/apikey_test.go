package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
)

const testProjectID = "770e8400-e29b-41d4-a716-446655440000"

func TestCreateServerKey_PostsToAuthHostAndReturnsPlaintextOnce(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody createServerKeyRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(createServerKeyResponse{
			Key: "mir_srv_plaintext",
			ServerKey: ServerKey{
				ID:        "key-1",
				ProjectID: testProjectID,
				Name:      "claude-code@laptop",
				KeyPrefix: "mir_srv_0123…",
			},
		})
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL, liveCredential(), testProjectID)

	key, meta, err := client.CreateServerKey(context.Background(), testProjectID, "claude-code@laptop", "desc")
	if err != nil {
		t.Fatalf("CreateServerKey: %v", err)
	}

	// Minting is a credential operation and belongs on the auth host, not the data plane.
	if gotMethod != http.MethodPost || gotPath != "/v1/api-keys/server" {
		t.Errorf("called %s %s, want POST /v1/api-keys/server", gotMethod, gotPath)
	}
	if gotBody.ProjectID != testProjectID || gotBody.Name != "claude-code@laptop" {
		t.Errorf("request body = %+v", gotBody)
	}
	if key != "mir_srv_plaintext" {
		t.Errorf("key = %q", key)
	}
	if meta.KeyPrefix != "mir_srv_0123…" {
		t.Errorf("key prefix = %q", meta.KeyPrefix)
	}
	// The plaintext must not also be reachable through the metadata struct, or it will
	// eventually be rendered by something that only meant to print the prefix.
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "mir_srv_plaintext") {
		t.Errorf("the plaintext key is reachable through ServerKey: %s", encoded)
	}
}

// A server key cannot mint another. Catching it locally turns an opaque 403 into a
// message that names the fix.
func TestCreateServerKey_RefusesUnderAServerKey(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()

	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	client, err := New(&config.Config{
		ProfileName: config.DefaultProfile,
		APIURL:      srv.URL,
		AuthURL:     srv.URL,
		APIKey:      "mir_srv_ci",
		ProjectID:   testProjectID,
	}, Options{Version: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, _, err = client.CreateServerKey(context.Background(), testProjectID, "ci", "")
	if err == nil {
		t.Fatal("CreateServerKey succeeded under a server key")
	}
	if called {
		t.Error("a request was sent; the refusal must be local")
	}
	if !strings.Contains(err.Error(), "TERMA_API_KEY") {
		t.Errorf("error = %q, want it to name the environment variable to unset", err)
	}
}

func TestCreateServerKey_RequiresProjectAndName(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()

	client := newTestClient(t, srv.URL, liveCredential(), testProjectID)

	if _, _, err := client.CreateServerKey(context.Background(), "", "ci", ""); err == nil {
		t.Error("CreateServerKey accepted an empty project")
	}
	if _, _, err := client.CreateServerKey(context.Background(), testProjectID, "", ""); err == nil {
		t.Error("CreateServerKey accepted an empty name")
	}
	if called {
		t.Error("a request was sent for input that could be rejected locally")
	}
}

// A 201 carrying no key would otherwise be reported as success, and the caller would
// write an empty credential into a harness config.
func TestCreateServerKey_RejectsEmptyKeyInResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"server_key":{"id":"key-1"}}`))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL, liveCredential(), testProjectID)

	if _, _, err := client.CreateServerKey(context.Background(), testProjectID, "ci", ""); err == nil {
		t.Fatal("CreateServerKey accepted a response with no key")
	}
}

func TestCreateServerKey_SurfacesGatewayError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"INVALID_ARGUMENT","message":"no such project in this organization"}}`))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL, liveCredential(), testProjectID)

	_, _, err := client.CreateServerKey(context.Background(), testProjectID, "ci", "")
	if err == nil {
		t.Fatal("CreateServerKey ignored a 400")
	}
	if !strings.Contains(err.Error(), "no such project") {
		t.Errorf("error = %q, want the gateway's own message", err)
	}
}
