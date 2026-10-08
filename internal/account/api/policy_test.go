package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
)

const testCapture = `"capture":{"exclude_prompts":true,"exclude_tool_content":false}`

func TestCollectionPolicyWireContract(t *testing.T) {
	t.Setenv("TERMA_POLICY_STUB", "")
	for _, mode := range []string{"global", "per_repository"} {
		t.Run(mode, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/policy" || r.URL.Query().Get("project_id") != "team" || r.Header.Get("Authorization") != "Bearer mir_cli_live" || r.Header.Get(projectHeader) != "" {
					t.Errorf("wrong policy authentication/URL")
				}
				body := "{}"
				if mode == "per_repository" {
					body = `{"repositories":["github.com/miradorlabs/mirador-platform","github.com/acme/sales"]}`
				}
				fmt.Fprintf(w, `{"policy":{"version":"1.0","terma":{%s,"%s":%s}},"revision":4,"updated_at":"2026-09-30T12:27:05.490205Z"}`, testCapture, mode, body)
			}))
			defer srv.Close()
			c := newSplitTestClient(t, "http://127.0.0.1:1", srv.URL, liveCredential(), "team")
			p, err := c.CollectionPolicy(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if p.Global() != (mode == "global") || p.IncludePrompts || !p.IncludeToolContent || p.CollectsNothing || p.Revision != 4 || p.FetchedAt.IsZero() {
				t.Fatalf("wrong translated policy: %+v", p)
			}
			if !p.Admits(config.Repository{Origin: "github.com/acme/sales"}) || p.Global() != p.Admits(config.Repository{Origin: "github.com/acme/web"}) {
				t.Fatalf("wrong repository list: %+v", p.Repositories)
			}
		})
	}
}

// A team's server key reads that team's policy itself, as the bearer, naming no team: the
// key is bound to its own.
func TestCollectionPolicyWithAServerKey(t *testing.T) {
	t.Setenv("TERMA_POLICY_STUB", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/policy" || r.URL.Query().Has("project_id") || r.Header.Get("Authorization") != "Bearer ter_srv_team" {
			t.Errorf("wrong policy authentication/URL: %s %s", r.URL, r.Header.Get("Authorization"))
		}
		fmt.Fprintf(w, `{"policy":{"version":"1.0","terma":{%s,"global":{}}},"revision":2,"updated_at":"2026-09-30T12:27:05Z"}`, testCapture)
	}))
	defer srv.Close()
	c, err := New(&config.Config{Dir: t.TempDir(), AuthURL: srv.URL, APIURL: "http://127.0.0.1:1", APIKey: "ter_srv_team"}, Options{Version: "test", ProjectID: "team"})
	if err != nil {
		t.Fatal(err)
	}
	if p, err := c.CollectionPolicy(t.Context()); err != nil || !p.Global() || p.Revision != 2 {
		t.Fatalf("policy = %+v, %v", p, err)
	}
}

// Opt-in smoke test: TERMA_POLICY_SMOKE_LOGIN=1 for the saved dev login, or a dev token
// in TERMA_POLICY_SMOKE_TOKEN_FILE.
func TestCollectionPolicyDev(t *testing.T) {
	keyFile := os.Getenv("TERMA_POLICY_SMOKE_TOKEN_FILE")
	useLogin := os.Getenv("TERMA_POLICY_SMOKE_LOGIN") == "1"
	if keyFile == "" && !useLogin {
		t.Skip("requires an explicit dev developer token file")
	}
	t.Setenv("TERMA_POLICY_STUB", "")
	endpoints, err := config.EndpointsFor(config.EnvDev)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthURL: endpoints.AuthURL, APIURL: endpoints.APIURL, ProjectID: os.Getenv("TERMA_POLICY_SMOKE_PROJECT")}
	if endpoint := os.Getenv("TERMA_POLICY_SMOKE_AUTH_URL"); endpoint != "" {
		cfg.AuthURL = endpoint
	}
	opts := Options{Version: "policy-smoke-test"}
	if useLogin {
		cfg.ProfileName = config.DefaultProfile
	} else {
		key, err := os.ReadFile(keyFile)
		if err != nil {
			t.Fatal("could not read policy smoke-test token")
		}
		opts.Credential = &auth.Credential{AccessToken: strings.TrimSpace(string(key)), AuthURL: cfg.AuthURL, ExpiresAt: time.Now().Add(time.Hour)}
	}
	c, err := New(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if c.projectID == "" {
		var response struct {
			Projects []struct {
				ID string `json:"id"`
			} `json:"projects"`
		}
		if err := c.AuthGet(t.Context(), "/v1/projects", nil, &response); err != nil {
			t.Fatal(err)
		}
		if len(response.Projects) == 0 {
			t.Fatal("no dev team available for policy smoke test")
		}
		c.projectID = response.Projects[0].ID
	}
	p, err := c.CollectionPolicy(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != config.ModeRepo && p.Mode != config.ModeGlobal {
		t.Fatal("unsupported deployed policy")
	}
	t.Logf("dev policy parsed: mode=%s revision=%d include_prompts=%t include_tool_content=%t", p.Mode, p.Revision, p.IncludePrompts, p.IncludeToolContent)
}

// A minor version only adds fields, so 1.1 is read like 1.0 and carries git_hooks.
func TestCollectionPolicyMinorVersionAndGitHooks(t *testing.T) {
	t.Setenv("TERMA_POLICY_STUB", "")
	for _, tt := range []struct {
		name, version, hooks string
		wantHooks            bool
	}{
		{"1.1 with git hooks", "1.1", `,"git_hooks":true`, true},
		{"1.1 without git hooks", "1.1", "", false},
		{"1.0 with git hooks", "1.0", `,"git_hooks":true`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"policy":{"version":%q,"terma":{%s,"global":{}%s}},"revision":1,"updated_at":"2026-09-30T12:27:05Z"}`,
					tt.version, testCapture, tt.hooks)
			}))
			defer srv.Close()
			c := newTestClient(t, srv.URL, liveCredential(), "team")
			p, err := c.CollectionPolicy(context.Background())
			if err != nil {
				t.Fatalf("version %s refused: %v", tt.version, err)
			}
			if p.GitHooks != tt.wantHooks || !p.Global() || p.Revision != 1 {
				t.Fatalf("wrong translated policy: %+v", p)
			}
		})
	}
}

// The offline stub unmarshals into config.Policy, so it carries git_hooks with no code.
func TestCollectionPolicyStubGitHooks(t *testing.T) {
	t.Setenv("TERMA_POLICY_STUB", `{"mode":"repo","repositories":["github.com/acme/app"],"git_hooks":true}`)
	c := newTestClient(t, "http://127.0.0.1:1", liveCredential(), "team")
	p, err := c.CollectionPolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !p.GitHooks {
		t.Fatalf("stub policy did not set GitHooks: %+v", p)
	}
}

func TestCollectionPolicyMissingInvalidAndUnavailable(t *testing.T) {
	t.Setenv("TERMA_POLICY_STUB", "")
	valid := fmt.Sprintf(`{"policy":{"version":"1.0","terma":{%s,"global":{}}},"revision":1,"updated_at":"2026-09-30T12:27:05Z"}`, testCapture)
	for _, tt := range []struct {
		name, body string
		status     int
		wantError  bool
	}{
		{"unset", `{}`, 200, false},
		{"null", `{"policy":null}`, 200, false},
		{"empty document", `{"policy":{}}`, 200, true},
		{"unknown version", strings.Replace(valid, `"1.0"`, `"2.0"`, 1), 200, true},
		{"version without a minor", strings.Replace(valid, `"1.0"`, `"1"`, 1), 200, true},
		{"empty version", strings.Replace(valid, `"1.0"`, `""`, 1), 200, true},
		{"missing capture switch", strings.Replace(valid, `"exclude_prompts":true,`, "", 1), 200, true},
		{"both modes", strings.Replace(valid, `"global":{}`, `"global":{},"per_repository":{}`, 1), 200, true},
		{"no modes", strings.Replace(valid, `,"global":{}`, "", 1), 200, true},
		{"per repository without a list", strings.Replace(valid, `"global":{}`, `"per_repository":{}`, 1), 200, true},
		{"per repository with a null list", strings.Replace(valid, `"global":{}`, `"per_repository":{"repositories":null}`, 1), 200, true},
		{"per repository with only the old folder list", strings.Replace(valid, `"global":{}`, `"per_repository":{"folders":["repo"]}`, 1), 200, true},
		{"per repository with an empty list", strings.Replace(valid, `"global":{}`, `"per_repository":{"repositories":[]}`, 1), 200, false},
		{"unavailable", `{}`, 503, true},
		{"forbidden", `{}`, 403, true},
		{"missing endpoint", `{}`, 404, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tt.status); fmt.Fprint(w, tt.body) }))
			defer srv.Close()
			c := newTestClient(t, srv.URL, liveCredential(), "team")
			p, err := c.CollectionPolicy(context.Background())
			if (err != nil) != tt.wantError {
				t.Fatalf("policy=%+v error=%v", p, err)
			}
			if !tt.wantError && (p.Global() || p.CollectsNothing || p.Admits(config.Repository{Origin: "github.com/acme/web"})) {
				t.Fatal("an unset policy or an empty list admitted a repository")
			}
			if wantUnset := tt.name == "unset" || tt.name == "null"; !tt.wantError && p.Unset != wantUnset {
				t.Fatalf("Unset = %v, want %v", p.Unset, wantUnset)
			}
		})
	}
}
