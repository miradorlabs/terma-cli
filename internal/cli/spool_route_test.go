package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/routing"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

const (
	routeProdProject = "aaaaaaaa-0000-4000-8000-00000000000a"
	routeDevProject  = "bbbbbbbb-0000-4000-8000-00000000000b"
)

// ingestHost stands in for one environment's ingest host, accepting only its own key.
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

// routingSandbox gives a test its own config dir and a profile host no one listens on, so
// ignoring the routing record fails fast.
func routingSandbox(t *testing.T) {
	t.Helper()
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("TERMA_OTLP_URL", "")
	if err := config.UpdateProfile(config.DefaultProfile, func(p *config.Profile) { p.OTLPURL = "http://127.0.0.1:1" }); err != nil {
		t.Fatal(err)
	}
}

// routeProject stores a project's key and the routing record naming its host.
func routeProject(t *testing.T, projectID, key, endpoint string) {
	t.Helper()
	if err := keystore.Set(projectID, key, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	if endpoint == "" {
		return
	}
	if err := routing.SaveRecord(routing.Record{ProjectID: projectID, Endpoint: endpoint, Signals: []string{"logs"}, Harnesses: []string{"claude"}}); err != nil {
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

// Each project's events go to the host its routing record names, and no key is shown to
// another host.
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

// A refused project is named with its host and its events stay queued; the other's leave.
func TestSpoolFlushOneRefusedProjectDoesNotHoldUpAnother(t *testing.T) {
	routingSandbox(t)
	prod := newIngestHost(t, "ter_srv_prod")
	dev := newIngestHost(t, "ter_srv_dev")
	routeProject(t, routeProdProject, "ter_srv_prod", prod.URL)
	routeProject(t, routeDevProject, "ter_srv_revoked", dev.URL)
	s := spoolForTest(t)
	// The refused project first.
	for _, id := range []string{routeDevProject, routeDevProject, routeProdProject} {
		appendEvent(t, s, id, time.Now())
	}

	out, err := runTerma(t, "spool", "flush")
	if err == nil {
		t.Fatalf("a refused project must fail the flush:\n%s", out)
	}
	// The report is printed; the error is what `terma` prints after "Error:".
	said := out + err.Error()
	for _, want := range []string{"Flushed 1 event", "keeping 2 queued after a failed delivery", "retrying team " + routeDevProject + " after", dev.URL, "403"} {
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

// A refused project backs off alone: the next flush delivers the others without asking
// it again, and --force (doctor) asks at once.
func TestSpoolFlushARefusedProjectWaitsAlone(t *testing.T) {
	routingSandbox(t)
	prod := newIngestHost(t, "ter_srv_prod")
	dev := newIngestHost(t, "ter_srv_dev")
	routeProject(t, routeProdProject, "ter_srv_prod", prod.URL)
	routeProject(t, routeDevProject, "ter_srv_revoked", dev.URL)
	s := spoolForTest(t)
	appendEvent(t, s, routeDevProject, time.Now())
	appendEvent(t, s, routeProdProject, time.Now())
	if out, err := runTerma(t, "spool", "flush"); err == nil {
		t.Fatalf("a refused project must fail the flush:\n%s", out)
	}
	if next := s.NextAttempt(); !next.IsZero() {
		t.Fatalf("one project's refusal opened the spool-wide window until %s", next)
	}

	// The next commit, flushed the way a hook flushes it.
	appendEvent(t, s, routeProdProject, time.Now())
	out, err := runTerma(t, "spool", "flush")
	if code, ok := exitCodeOf(err); !ok || code != ExitIncomplete {
		t.Fatalf("a pass that only waited on a project must exit %d (left work), got %v:\n%s", ExitIncomplete, err, out)
	}
	for _, want := range []string{"Flushed 1 event", "keeping 1 queued after a failed delivery", "retrying team " + routeDevProject + " after"} {
		if !strings.Contains(out, want) {
			t.Fatalf("flush does not say %q:\n%s", want, out)
		}
	}
	if n := len(prod.keysSeen()); n != 2 {
		t.Fatalf("the healthy project's host was asked %d times over two flushes, want 2", n)
	}
	if n := len(dev.keysSeen()); n != 1 {
		t.Fatalf("the refusing host was asked %d times, want 1: its window was still open", n)
	}
	if queued := queuedProjects(t); len(queued) != 1 || queued[0] != routeDevProject {
		t.Fatalf("queue = %v, want the refused project's event only", queued)
	}
	// Only the refused project waits out a retry window (doctor reports it as failing delivery).
	if windows := openSpool().RetryWindows(time.Now()); len(windows) != 1 || windows[routeDevProject].IsZero() {
		t.Fatalf("retry windows = %v, want only %s's", windows, routeDevProject)
	}

	if _, err := runTerma(t, "spool", "flush", "--force"); err == nil {
		t.Fatal("the key is still refused")
	}
	if n := len(dev.keysSeen()); n != 2 {
		t.Fatalf("--force must ask the refusing host again, asked %d times", n)
	}
}

// A project with no routing record reaches the hosts stored with its key.
func TestSpoolFlushUsesTheHostsStoredWithTheKey(t *testing.T) {
	routingSandbox(t)
	dev := newIngestHost(t, "ter_srv_dev")
	stale := newIngestHost(t, "ter_srv_dev")
	// No routing record at all: the key's own hosts are all there is.
	if err := keystore.Set(routeDevProject, "ter_srv_dev", keystore.Hosts{OTLP: dev.URL, API: "http://127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	// And they outrank a routing record, which only restates them.
	routeProject(t, routeProdProject, "ter_srv_dev", stale.URL)
	if err := keystore.Set(routeProdProject, "ter_srv_dev", keystore.Hosts{OTLP: dev.URL}); err != nil {
		t.Fatal(err)
	}
	s := spoolForTest(t)
	appendEvent(t, s, routeDevProject, time.Now())
	appendEvent(t, s, routeProdProject, time.Now())

	if out, err := runTerma(t, "spool", "flush"); err != nil {
		t.Fatalf("flush: %v\n%s", err, out)
	}
	if n := len(dev.keysSeen()); n != 2 {
		t.Fatalf("the key's own host received %d sends, want 2", n)
	}
	if n := len(stale.keysSeen()); n != 0 {
		t.Fatalf("the routing record's host was used over the key's own: %d sends", n)
	}
}

// --otlp-url and TERMA_OTLP_URL still decide over the routing record.
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

// Another project's refusal is a doctor warning naming that project and its host.
func TestDoctorBackendWarnsForAnotherProjectsRefusal(t *testing.T) {
	routingSandbox(t)
	prod := newIngestHost(t, "ter_srv_prod")
	dev := newIngestHost(t, "ter_srv_dev")
	routeProject(t, routeProdProject, "ter_srv_prod", prod.URL)
	routeProject(t, routeDevProject, "ter_srv_revoked", dev.URL)
	s := spoolForTest(t)
	appendEvent(t, s, routeDevProject, time.Now())
	appendEvent(t, s, routeProdProject, time.Now())
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}

	check := doctor.BackendCheck(context.Background(), testApp.doctorProbes(cfg), routeProdProject)
	if check.Status == doctor.Fail {
		t.Fatalf("another project's refusal must not fail this repository's check: %+v", check)
	}
	for _, want := range []string{"flushed 1 event to " + prod.URL, "another team's events were not delivered", routeDevProject, dev.URL} {
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
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}

	check := doctor.BackendCheck(context.Background(), testApp.doctorProbes(cfg), routeDevProject)
	if check.Status != doctor.Fail || !strings.Contains(check.Detail, "this team's events were not delivered: refused by "+dev.URL) {
		t.Fatalf("this project's refusal must fail, naming the host: %+v", check)
	}
	if !strings.Contains(check.Fix, "team "+routeDevProject+" was refused by "+dev.URL) {
		t.Fatalf("fix = %q", check.Fix)
	}
}
