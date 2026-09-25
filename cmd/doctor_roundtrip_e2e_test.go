//go:build unix

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

// Exercise real installed hooks, background delivery and API read-back together.
func TestDoctorScratchCommitRoundTrip(t *testing.T) {
	bin := termaBinary(t)
	repo := installRepo(t)
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TERMA_HOOKS", "1")
	t.Setenv("TERMA_API_KEY", testServerKey)
	t.Setenv("TERMA_NO_UPDATE_CHECK", "1")
	var mu sync.Mutex
	commits := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/v1/identity" {
			// What the API gateway says about a server key: the one project it belongs to.
			fmt.Fprintf(w, `{"project_id":%q,"organization_id":"org","auth_type":"server_key"}`, testProjectID)
			return
		}
		if r.Method == http.MethodPost {
			var body struct {
				ResourceLogs []struct {
					ScopeLogs []struct {
						LogRecords []struct {
							EventName  string `json:"eventName"`
							Attributes []struct {
								Key   string `json:"key"`
								Value struct {
									String string `json:"stringValue"`
								} `json:"value"`
							} `json:"attributes"`
						} `json:"logRecords"`
					} `json:"scopeLogs"`
				} `json:"resourceLogs"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			for _, resource := range body.ResourceLogs {
				for _, scope := range resource.ScopeLogs {
					for _, record := range scope.LogRecords {
						if record.EventName != "terma.commit" {
							continue
						}
						for _, attr := range record.Attributes {
							if attr.Key == "sha" {
								commits[attr.Value.String] = true
							}
						}
					}
				}
			}
			fmt.Fprint(w, `{}`)
			return
		}
		for sha := range commits {
			if strings.Contains(r.URL.Query().Get("filter"), sha) {
				fmt.Fprintf(w, `{"logs":[{"event_name":"terma.commit","attributes":{"sha":%q}}]}`, sha)
				return
			}
		}
		fmt.Fprint(w, `{"logs":[]}`)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("TERMA_OTLP_URL", srv.URL)
	t.Setenv("TERMA_API_URL", srv.URL)
	t.Setenv("TERMA_AUTH_URL", srv.URL)
	if err := keystore.Set(testProjectID, testServerKey); err != nil {
		t.Fatal(err)
	}
	if err := termaproject.Save(repo, &termaproject.File{Project: termaproject.Project{ID: testProjectID}}); err != nil {
		t.Fatal(err)
	}
	if out, err := runTerma(t, "install", "--harness", "none", "--yes", "--no-doctor"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, args := range [][]string{{"add", ".terma"}, {"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "commit", "-qm", "initial"}} {
		if out, err := gitx.Git(ctx, repo, args...); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	bound, err := termaproject.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	sha, check := scratchCommit(ctx, repo, bound)
	if check.Status != doctor.Pass {
		t.Fatalf("scratch: %+v", check)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	d := doctorRun{ctx: ctx, cfg: cfg, projectID: testProjectID, scratchSHA: sha}
	if check := d.backendReceives(); check.Status != doctor.Pass || !strings.Contains(check.Detail, "round-trip confirmed") {
		t.Fatalf("backend: %+v", check)
	}
}

// A first install leaves its hook files uncommitted until the developer adds them.
// The install's immediate doctor check must run those files in its scratch worktree.
func TestDoctorScratchCommitWithUncommittedHooks(t *testing.T) {
	s := newInstallSandbox(t)
	s.env = append(s.env, "HOME="+s.mkdir("home"))
	repo := s.mkdir("repo")
	s.git(repo, "init", "-q")
	s.git(repo, "commit", "--allow-empty", "-qm", "initial")
	s.install(repo)
	if tracked := s.git(repo, "ls-files", hookmgr.ShimDir); tracked != "" {
		t.Fatalf("hook files were unexpectedly committed: %s", tracked)
	}

	t.Setenv("HOME", filepath.Join(s.base, "home"))
	t.Setenv("PATH", filepath.Dir(s.bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TERMA_ENV", "dev")
	t.Setenv("TERMA_CONFIG_DIR", filepath.Join(s.base, "config"))
	t.Setenv("TERMA_HOOKS", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	bound, err := termaproject.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, check := scratchCommit(ctx, repo, bound); check.Status != doctor.Pass {
		t.Fatalf("uncommitted hooks: %+v", check)
	}
}
