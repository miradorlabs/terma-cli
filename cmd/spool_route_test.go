package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/shim"
)

const (
	routeProdProject = "aaaaaaaa-0000-4000-8000-00000000000a"
	routeDevProject  = "bbbbbbbb-0000-4000-8000-00000000000b"
)

// ingestHost stands in for one environment's OTLP ingest host. It accepts only
// its own key and answers any other the way the real host does.
type ingestHost struct {
	*httptest.Server
	mu   sync.Mutex
	auth []string
}

func newIngestHost(t *testing.T, key string) *ingestHost {
	t.Helper()
	h := &ingestHost{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		h.mu.Lock()
		h.auth = append(h.auth, got)
		h.mu.Unlock()
		if got != "Bearer "+key {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":7,"message":"invalid OTLP API key"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *ingestHost) keysSeen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.auth...)
}

// routingSandbox gives a test its own config dir with no explicit ingest override,
// and a profile default that no one listens on: a regression that ignores the
// routing record fails here, fast, instead of sending test keys to a real host.
func routingSandbox(t *testing.T) {
	t.Helper()
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("TERMA_OTLP_URL", "")
	flags = globalFlags{}
	if err := config.UpdateProfile(config.DefaultProfile, func(p *config.Profile) { p.OTLPURL = "http://127.0.0.1:1" }); err != nil {
		t.Fatal(err)
	}
}

// routeProject stores a key for a project and the routing record `terma install`
// writes, naming the host the project's agents export to.
func routeProject(t *testing.T, projectID, key, endpoint string) {
	t.Helper()
	if err := keystore.Set(projectID, key); err != nil {
		t.Fatal(err)
	}
	if endpoint == "" {
		return
	}
	if err := shim.SaveRecord(shim.Record{ProjectID: projectID, Endpoint: endpoint, Harnesses: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
}

func queuedProjects(t *testing.T) []string {
	t.Helper()
	events, err := spoolForTest(t).Peek(100)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range events {
		id, _ := e.Attrs[hookrun.AttrProjectID].(string)
		ids = append(ids, id)
	}
	return ids
}

// A developer with one repository bound to a dev-deployment project and another to a
// production one had every flush refused with "invalid OTLP API key": the flusher sent
// both projects to the active profile's host, so the dev project's key went to
// production. Each project's events go to the host its routing record names — the
// one its agents already export to — and no key is ever shown to the other host.
func TestSpoolFlushSendsEachProjectToItsOwnHost(t *testing.T) {
	routingSandbox(t)
	prod := newIngestHost(t, "ter_srv_prod")
	dev := newIngestHost(t, "ter_srv_dev")
	routeProject(t, routeProdProject, "ter_srv_prod", prod.URL)
	routeProject(t, routeDevProject, "ter_srv_dev", dev.URL)
	s := spoolForTest(t)
	for _, id := range []string{routeDevProject, routeProdProject, routeDevProject, routeProdProject} {
		appendEvent(t, s, id, time.Now())
	}

	out, err := runTerma(t, "spool", "flush")
	if err != nil {
		t.Fatalf("flush: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Flushed 4 events") {
		t.Fatalf("unexpected report:\n%s", out)
	}
	for host, want := range map[*ingestHost]string{prod: "Bearer ter_srv_prod", dev: "Bearer ter_srv_dev"} {
		seen := host.keysSeen()
		if len(seen) == 0 {
			t.Fatalf("host %s received nothing", host.URL)
		}
		for _, got := range seen {
			if got != want {
				t.Fatalf("host %s was sent %q; only its own project's key may reach it", host.URL, got)
			}
		}
	}
	if n, _, _ := s.Pending(); n != 0 {
		t.Fatalf("pending after delivery = %d", n)
	}
}

// One project refused must not hold up another. The refused project is named with
// the host that refused it, its events stay queued, and the other project's leave.
func TestSpoolFlushOneRefusedProjectDoesNotHoldUpAnother(t *testing.T) {
	routingSandbox(t)
	prod := newIngestHost(t, "ter_srv_prod")
	dev := newIngestHost(t, "ter_srv_dev")
	routeProject(t, routeProdProject, "ter_srv_prod", prod.URL)
	routeProject(t, routeDevProject, "ter_srv_revoked", dev.URL)
	s := spoolForTest(t)
	// The refused project first, where it blocked everything behind it.
	for _, id := range []string{routeDevProject, routeDevProject, routeProdProject} {
		appendEvent(t, s, id, time.Now())
	}

	out, err := runTerma(t, "spool", "flush")
	if err == nil {
		t.Fatalf("a refused project must fail the flush:\n%s", out)
	}
	// The report is printed; the error is what `terma` prints after "Error:".
	said := out + err.Error()
	for _, want := range []string{"Flushed 1 event", "keeping 2 queued after a failed delivery", routeDevProject, dev.URL, "403"} {
		if !strings.Contains(said, want) {
			t.Fatalf("flush does not say %q:\n%s", want, said)
		}
	}
	queued := queuedProjects(t)
	if len(queued) != 2 || queued[0] != routeDevProject || queued[1] != routeDevProject {
		t.Fatalf("queue after flush = %v, want only the refused project's two events", queued)
	}
	if n := len(dev.keysSeen()); n != 1 {
		t.Fatalf("the refusing host was asked %d times in one pass, want 1", n)
	}
}

// --otlp-url and TERMA_OTLP_URL are an explicit choice, a local collector or a test
// server, and still decide over the routing record.
func TestSpoolFlushOverrideWinsOverTheRoutingRecord(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	override := newIngestHost(t, "ter_srv_prod")
	t.Setenv("TERMA_OTLP_URL", override.URL)
	routeProject(t, routeProdProject, "ter_srv_prod", "http://127.0.0.1:1")
	appendEvent(t, spoolForTest(t), routeProdProject, time.Now())

	if out, err := runTerma(t, "spool", "flush"); err != nil {
		t.Fatalf("flush: %v\n%s", err, out)
	}
	if len(override.keysSeen()) != 1 {
		t.Fatal("the override host received nothing")
	}
}

// Doctor flushes every project's events, not only its repository's. Another project's
// refusal used to fail this repository's check as "check network / OTLP endpoint"
// followed by the active profile's host — not the host that refused, and not this
// repository's problem. It is a warning here, naming the project and its host.
func TestDoctorBackendWarnsForAnotherProjectsRefusal(t *testing.T) {
	routingSandbox(t)
	prod := newIngestHost(t, "ter_srv_prod")
	dev := newIngestHost(t, "ter_srv_dev")
	routeProject(t, routeProdProject, "ter_srv_prod", prod.URL)
	routeProject(t, routeDevProject, "ter_srv_revoked", dev.URL)
	s := spoolForTest(t)
	appendEvent(t, s, routeDevProject, time.Now())
	appendEvent(t, s, routeProdProject, time.Now())
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}

	d := doctorRun{ctx: context.Background(), cfg: cfg, projectID: routeProdProject}
	check := d.backendReceives()
	if check.Status == doctor.Fail {
		t.Fatalf("another project's refusal must not fail this repository's check: %+v", check)
	}
	for _, want := range []string{"flushed 1 event to " + prod.URL, "another project's events were not delivered", routeDevProject, dev.URL} {
		if !strings.Contains(check.Detail, want) {
			t.Fatalf("detail does not say %q: %s", want, check.Detail)
		}
	}
	if !strings.Contains(check.Fix, "was refused by "+dev.URL) {
		t.Fatalf("a refused key is not a network problem; fix = %q", check.Fix)
	}
}

func TestDoctorBackendFailsOnThisProjectsRefusal(t *testing.T) {
	routingSandbox(t)
	dev := newIngestHost(t, "ter_srv_dev")
	routeProject(t, routeDevProject, "ter_srv_revoked", dev.URL)
	appendEvent(t, spoolForTest(t), routeDevProject, time.Now())
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}

	d := doctorRun{ctx: context.Background(), cfg: cfg, projectID: routeDevProject}
	check := d.backendReceives()
	if check.Status != doctor.Fail || !strings.Contains(check.Detail, "this project's events were not delivered: refused by "+dev.URL) {
		t.Fatalf("this project's refusal must fail, naming the host: %+v", check)
	}
	if !strings.Contains(check.Fix, "project "+routeDevProject+" was refused by "+dev.URL) {
		t.Fatalf("fix = %q", check.Fix)
	}
}
