package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPut_SendsExactlyOnePrecondition proves every PUT carries exactly one of If-None-Match
// and If-Match.
func TestPut_SendsExactlyOnePrecondition(t *testing.T) {
	tests := []struct {
		name      string
		pre       Precondition
		wantName  string
		wantValue string
		wantErr   string
	}{
		{name: "create", pre: Precondition{CreateOnly: true}, wantName: "If-None-Match", wantValue: "*"},
		{name: "replace", pre: Precondition{ReplaceETag: `"v7"`}, wantName: "If-Match", wantValue: `"v7"`},
		{name: "neither is refused locally", pre: Precondition{}, wantErr: "precondition"},
		{name: "both is refused locally", pre: Precondition{CreateOnly: true, ReplaceETag: `"v7"`}, wantErr: "not both"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got http.Header
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Clone()
				w.Header().Set("ETag", `"v8"`)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"slug":"x"}`))
			}))
			defer server.Close()

			client := newTestClient(t, server.URL, liveCredential(), "proj-1")
			var out map[string]any
			meta, err := client.Put(context.Background(), "/v1/dashboards/x", tc.pre, map[string]any{"title": "x"}, &out)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error mentioning %q, got none", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not mention %q", err, tc.wantErr)
				}
				if got != nil {
					t.Error("a request was sent despite an invalid precondition")
				}
				return
			}

			if err != nil {
				t.Fatalf("Put: %v", err)
			}
			if v := got.Get(tc.wantName); v != tc.wantValue {
				t.Errorf("%s = %q, want %q", tc.wantName, v, tc.wantValue)
			}
			for _, other := range []string{"If-Match", "If-None-Match"} {
				if other != tc.wantName {
					if _, present := got[http.CanonicalHeaderKey(other)]; present {
						t.Errorf("%s was sent alongside %s", other, tc.wantName)
					}
				}
			}
			if meta.ETag != `"v8"` {
				t.Errorf("ETag = %q, want the one the server returned", meta.ETag)
			}
		})
	}
}

// TestPut_CreatedIsDistinguishedFromReplaced tells a 201 from a 200.
func TestPut_CreatedIsDistinguishedFromReplaced(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusCreated} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{}`))
		}))
		client := newTestClient(t, server.URL, liveCredential(), "proj-1")

		var out map[string]any
		meta, err := client.Put(context.Background(), "/v1/dashboards/x", Precondition{CreateOnly: true}, map[string]any{}, &out)
		server.Close()
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		if want := status == http.StatusCreated; meta.Created() != want {
			t.Errorf("status %d: Created() = %v, want %v", status, meta.Created(), want)
		}
	}
}

// TestDelete_RefusesWithoutAnETag refuses an unconditional delete.
func TestDelete_RefusesWithoutAnETag(t *testing.T) {
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer server.Close()

	client := newTestClient(t, server.URL, liveCredential(), "proj-1")
	if err := client.Delete(context.Background(), "/v1/dashboards/x", ""); err == nil {
		t.Fatal("expected an error when no etag is known")
	}
	if called {
		t.Error("an unconditional DELETE reached the server")
	}
}

func TestDelete_SendsIfMatch(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("If-Match")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, liveCredential(), "proj-1")
	if err := client.Delete(context.Background(), "/v1/dashboards/x", `"v3"`); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got != `"v3"` {
		t.Errorf("If-Match = %q, want %q", got, `"v3"`)
	}
}

// A 404 is "not found" and nothing near it is.
func TestErrorClassifiers(t *testing.T) {
	tests := []struct {
		status       int
		wantNotFound bool
	}{
		{status: http.StatusNotFound, wantNotFound: true},
		{status: http.StatusPreconditionFailed},
		{status: http.StatusPreconditionRequired},
		{status: http.StatusConflict},
	}

	for _, tc := range tests {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{"code": "X", "message": "nope"},
			})
		}))
		client := newTestClient(t, server.URL, liveCredential(), "proj-1")

		var out map[string]any
		err := client.Get(context.Background(), "/v1/dashboards/x", nil, &out)
		server.Close()

		if IsNotFound(err) != tc.wantNotFound {
			t.Errorf("status %d: IsNotFound = %v, want %v", tc.status, IsNotFound(err), tc.wantNotFound)
		}
	}
}
