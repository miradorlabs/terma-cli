package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/routing"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
)

// The round-trip asks the log store for a since/until window, not a `window` parameter.
func TestCommitRecordedAsksForAWindow(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"

	var mu sync.Mutex
	var queries []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Mirador-Project"); got != "repo-project" {
			t.Errorf("round-trip project = %q, want repository project", got)
		}
		if r.URL.Path != "/v1/logs" {
			t.Errorf("path = %s", r.URL.Path)
		}
		mu.Lock()
		queries = append(queries, r.URL.Query())
		first := len(queries) == 1
		mu.Unlock()
		if first {
			fmt.Fprint(w, `{"logs":[]}`)
			return
		}
		fmt.Fprintf(w, `{"logs":[{"event_name":"terma.commit","attributes":{"sha":%q}}]}`, sha)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("TERMA_API_URL", srv.URL)
	t.Setenv("TERMA_AUTH_URL", srv.URL)
	t.Setenv("TERMA_API_KEY", "")
	t.Setenv("TERMA_ENV", "")
	t.Setenv("TERMA_PROFILE", "")
	if _, err := auth.SaveCredential(config.DefaultProfile, &auth.Credential{
		AccessToken: "ter_cli_test", OrganizationID: "org-test", AuthURL: srv.URL,
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	from, to := time.Now().Add(-time.Hour).Truncate(time.Second), time.Now().Add(time.Hour).Truncate(time.Second)
	cfg.ProjectID = "another-project"
	recorded := testApp.commitRecorded(cfg)
	for i, want := range []bool{false, true} {
		if found, err := recorded(ctx, "repo-project", sha, from, to); err != nil || found != want {
			t.Fatalf("read %d = %v, %v; want %v", i, found, err, want)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(queries) != 2 {
		t.Fatalf("made %d queries, want a miss and then a hit", len(queries))
	}
	for i, q := range queries {
		if q.Has("window") {
			t.Errorf("query %d sends the undocumented window parameter: %v", i, q)
		}
		if f := q.Get("filter"); !strings.Contains(f, `attribute.sha="`+sha+`"`) || !strings.Contains(f, `attribute.event.name="terma.commit"`) {
			t.Errorf("query %d filter = %q", i, f)
		}
		since, errS := time.Parse(time.RFC3339, q.Get("since"))
		until, errU := time.Parse(time.RFC3339, q.Get("until"))
		if errS != nil || errU != nil {
			t.Fatalf("query %d window not RFC3339: %q..%q", i, q.Get("since"), q.Get("until"))
		}
		if !since.Equal(from) || !until.Equal(to) {
			t.Errorf("query %d window %v..%v, want %v..%v", i, since, until, from, to)
		}
	}
}

func TestDoctorBackendReadErrorIsInconclusive(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("TERMA_API_KEY", "ter_srv_test")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "read API unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("TERMA_API_URL", srv.URL)
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := keystore.Set("repo-project", "ter_srv_test", keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	check := doctor.BackendCheck(context.Background(), testApp.doctorProbes(cfg), "repo-project", "scratch", doctor.Check{}, doctor.Progress{})
	if check.Status != doctor.Warn || !check.Inconclusive || !strings.Contains(check.Detail, "could not confirm") {
		t.Fatalf("API read failure is not proof of delivery failure: %+v", check)
	}
}

// A project in another environment is read back from its own data API with its own key.
func TestCommitRecordedReadsTheProjectsOwnEnvironment(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	profileAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the active profile's API was asked for another environment's project: %s", r.URL)
		fmt.Fprint(w, `{"logs":[]}`)
	}))
	t.Cleanup(profileAPI.Close)
	var auths []string
	var mu sync.Mutex
	projectAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		fmt.Fprintf(w, `{"logs":[{"event_name":"terma.commit","attributes":{"sha":%q}}]}`, sha)
	}))
	t.Cleanup(projectAPI.Close)

	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	for _, v := range []string{"TERMA_API_URL", "TERMA_AUTH_URL", "TERMA_API_KEY", "TERMA_ENV", "TERMA_PROFILE"} {
		t.Setenv(v, "")
	}
	if err := config.UpdateProfile(config.DefaultProfile, func(p *config.Profile) {
		p.APIURL, p.AuthURL = profileAPI.URL, profileAPI.URL
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SaveCredential(config.DefaultProfile, &auth.Credential{
		AccessToken: "ter_cli_profile", OrganizationID: "org-test", AuthURL: profileAPI.URL,
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := keystore.Set("dev-project", "ter_srv_dev", keystore.Hosts{OTLP: "http://127.0.0.1:1", API: projectAPI.URL}); err != nil {
		t.Fatal(err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	found, err := testApp.commitRecorded(cfg)(ctx, "dev-project", sha, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil || !found {
		t.Fatalf("commitRecorded = %v, %v", found, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(auths) != 1 || auths[0] != "Bearer ter_srv_dev" {
		t.Fatalf("the project's API was asked with %v, want its own key", auths)
	}
}

// Without recorded hosts, only a routing record naming another built-in environment's
// ingest host moves the project, so a profile's custom data API stays.
func TestProjectAPIFromTheRoutingRecord(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("TERMA_API_URL", "")
	prod, _ := config.EndpointsFor(config.EnvProd)
	dev, _ := config.EndpointsFor(config.EnvDev)
	cfg := &config.Config{OTLPURL: prod.OTLPURL, APIURL: "https://api.custom.example"}
	for id, endpoint := range map[string]string{"dev-project": dev.OTLPURL + "/", "prod-project": prod.OTLPURL} {
		if err := routing.SaveRecord(routing.Record{ProjectID: id, Endpoint: endpoint, Harnesses: []string{"claude"}}); err != nil {
			t.Fatal(err)
		}
	}
	if got := testApp.delivery().API(cfg, "dev-project"); got != dev.APIURL {
		t.Fatalf("projectAPI(dev) = %q, want %q", got, dev.APIURL)
	}
	if got := testApp.delivery().API(cfg, "prod-project"); got != cfg.APIURL {
		t.Fatalf("projectAPI(prod) = %q, want the profile's own %q", got, cfg.APIURL)
	}
	if got := testApp.delivery().API(cfg, "unknown-project"); got != cfg.APIURL {
		t.Fatalf("projectAPI(unknown) = %q", got)
	}
	t.Setenv("TERMA_API_URL", "https://api.override.example")
	if got := testApp.delivery().API(cfg, "dev-project"); got != cfg.APIURL {
		t.Fatalf("an explicit TERMA_API_URL must win, got %q", got)
	}
}
