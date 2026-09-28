package cmd

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

	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/shim"
)

// Doctor's round-trip reads the scratch commit's terma.commit record back, so it has to
// ask the way the log store documents: a since/until window, not a `window` parameter it
// does not. A miss is polled again rather than reported.
func TestWaitForCommitEventAsksForAWindow(t *testing.T) {
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
	flags = globalFlags{}
	if _, err := auth.SaveCredential(config.DefaultProfile, &auth.Credential{
		AccessToken: "ter_cli_test", OrganizationID: "org-test", AuthURL: srv.URL,
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var notes []string
	progress := doctorProgress{note: func(text string) { notes = append(notes, text) }}
	started := time.Now()
	cfg.ProjectID = "another-project"
	found, err := waitForCommitEvent(ctx, cfg, "repo-project", sha, progress)
	if err != nil || !found {
		t.Fatalf("waitForCommitEvent = %v, %v; want the second poll to find the record", found, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(queries) != 2 {
		t.Fatalf("made %d queries, want a miss and then a hit", len(queries))
	}
	if len(notes) != len(queries) {
		t.Errorf("noted progress %d times over %d polls", len(notes), len(queries))
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
		if since.After(started) || until.Before(started) || until.Sub(since) != 2*commitLogWindow {
			t.Errorf("query %d window %v..%v does not bracket the scratch commit by %v", i, since, until, commitLogWindow)
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
	flags = globalFlags{}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := keystore.Set("repo-project", "ter_srv_test", keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	d := doctorRun{ctx: context.Background(), cfg: cfg, projectID: "repo-project", scratchSHA: "scratch"}
	check := d.backendReceives()
	if check.Status != doctor.Warn || !check.Inconclusive || !strings.Contains(check.Detail, "could not confirm") {
		t.Fatalf("API read failure is not proof of delivery failure: %+v", check)
	}
}

func TestDoctorSkipsBackendProbeWhenHookBinaryDiffers(t *testing.T) {
	d := doctorRun{binaryCheck: doctor.Check{Status: doctor.Warn, Fix: "replace the stale binary"}}
	check := d.backendReceives()
	if check.Status != doctor.Warn || !check.Inconclusive || check.Fix != d.binaryCheck.Fix {
		t.Fatalf("must identify the binary mismatch before flushing or polling: %+v", check)
	}
}

// A repository whose project lives in another environment than the active profile's
// is read back from that environment's data API, with the project's own key. The
// signed-in credential is bound to the profile's auth host, and asking the profile's
// API for the project's scratch commit found nothing on every run.
func TestWaitForCommitEventReadsTheProjectsOwnEnvironment(t *testing.T) {
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
	flags = globalFlags{}
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
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	found, err := waitForCommitEvent(ctx, cfg, "dev-project", sha, doctorProgress{})
	if err != nil || !found {
		t.Fatalf("waitForCommitEvent = %v, %v", found, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(auths) != 1 || auths[0] != "Bearer ter_srv_dev" {
		t.Fatalf("the project's API was asked with %v, want its own key", auths)
	}
}

// Where a key's hosts were never recorded, a routing record naming another built-in
// environment's ingest host places the project; one naming the profile's own ingest
// host changes nothing, so a profile with a custom data API keeps it.
func TestProjectAPIFromTheRoutingRecord(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("TERMA_API_URL", "")
	flags = globalFlags{}
	prod, _ := config.EndpointsFor(config.EnvProd)
	dev, _ := config.EndpointsFor(config.EnvDev)
	cfg := &config.Config{OTLPURL: prod.OTLPURL, APIURL: "https://api.custom.example"}
	for id, endpoint := range map[string]string{"dev-project": dev.OTLPURL + "/", "prod-project": prod.OTLPURL} {
		if err := shim.SaveRecord(shim.Record{ProjectID: id, Endpoint: endpoint, Harnesses: []string{"claude"}}); err != nil {
			t.Fatal(err)
		}
	}
	if got := projectAPI(cfg, "dev-project"); got != dev.APIURL {
		t.Fatalf("projectAPI(dev) = %q, want %q", got, dev.APIURL)
	}
	if got := projectAPI(cfg, "prod-project"); got != cfg.APIURL {
		t.Fatalf("projectAPI(prod) = %q, want the profile's own %q", got, cfg.APIURL)
	}
	if got := projectAPI(cfg, "unknown-project"); got != cfg.APIURL {
		t.Fatalf("projectAPI(unknown) = %q", got)
	}
	t.Setenv("TERMA_API_URL", "https://api.override.example")
	if got := projectAPI(cfg, "dev-project"); got != cfg.APIURL {
		t.Fatalf("an explicit TERMA_API_URL must win, got %q", got)
	}
}
