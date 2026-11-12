package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// reference/new-builds.sh against a registry and a published reference of the test's own: which
// builds the frequent check captures, from what each harness has released and what the reference
// has published of it.
func TestNewBuilds(t *testing.T) {
	for _, tool := range []string{"bash", "curl", "jq", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	type published struct {
		Version     string     `json:"version"`
		CapturedAt  time.Time  `json:"captured_at"`
		AttemptedAt *time.Time `json:"attempted_at,omitempty"`
		Partial     bool       `json:"partial,omitempty"`
	}
	now := time.Now().UTC()
	hoursAgo := func(h int) *time.Time { at := now.Add(-time.Duration(h) * time.Hour); return &at }
	whole := func(versions ...string) []published {
		var out []published
		for _, v := range versions {
			out = append(out, published{Version: v, CapturedAt: now.Add(-48 * time.Hour)})
		}
		return out
	}
	partial := func(v string, attempted *time.Time) published {
		return published{Version: v, CapturedAt: now.Add(-48 * time.Hour), AttemptedAt: attempted, Partial: true}
	}
	releases := []string{"1.0.0", "1.0.1", "1.0.2", "1.0.3", "1.0.4", "1.0.5", "1.1.0-beta.1"}
	for _, c := range []struct {
		name  string
		index []published // nil: the reference has no index (404)
		down  bool        // the reference answers 500
		want  string
	}{
		{name: "never published: the newest release alone", want: "claude=1.0.5"},
		{name: "the newest five not published, a late backport among them",
			index: whole("1.0.0", "1.0.1", "1.0.3", "1.0.5"), want: "claude=1.0.2,1.0.4"},
		{name: "every release published", index: whole("1.0.1", "1.0.2", "1.0.3", "1.0.4", "1.0.5")},
		{name: "a partial newest, attempted long ago, is tried again",
			index: append(whole("1.0.1", "1.0.2", "1.0.3", "1.0.4"), partial("1.0.5", hoursAgo(7))), want: "claude=1.0.5"},
		{name: "a partial newest, attempted lately, waits though captured long ago",
			index: append(whole("1.0.1", "1.0.2", "1.0.3", "1.0.4"), partial("1.0.5", hoursAgo(1)))},
		{name: "a partial newest from before attempted_at waits from its capture",
			index: append(whole("1.0.1", "1.0.2", "1.0.3", "1.0.4"), partial("1.0.5", nil)), want: "claude=1.0.5"},
		{name: "a partial older build stays as it is",
			index: append(whole("1.0.1", "1.0.2", "1.0.3", "1.0.5"), partial("1.0.4", hoursAgo(7)))},
		{name: "a reference that cannot be read is no build to capture", down: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/registry/@anthropic-ai/claude-code":
					versions := map[string]any{}
					for _, v := range releases {
						versions[v] = map[string]any{}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"versions": versions, "dist-tags": map[string]string{"latest": "1.0.5"}})
				case "/reference/claude/index.json":
					switch {
					case c.down:
						http.Error(w, "down", http.StatusInternalServerError)
					case c.index == nil:
						http.NotFound(w, r)
					default:
						_ = json.NewEncoder(w).Encode(map[string]any{"versions": c.index})
					}
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			cmd := exec.Command("bash", "reference/new-builds.sh")
			cmd.Env = append(cmd.Environ(), "TERMA_REFERENCE_HARNESSES=claude", "TERMA_NPM_REGISTRY="+srv.URL+"/registry",
				"TERMA_REFERENCE_URL="+srv.URL+"/reference")
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("new-builds.sh: %v\n%s", err, out)
			}
			if got := strings.TrimSpace(string(out)); got != c.want {
				t.Errorf("new-builds.sh = %q, want %q", got, c.want)
			}
		})
	}
}
