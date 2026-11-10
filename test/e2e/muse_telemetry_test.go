package e2e

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TelemetryRoute is what one way of steering a build's own telemetry did, in one profile turn.
type TelemetryRoute struct {
	// Control is what the run set: "default" for nothing, else a setting or a variable.
	Control string `json:"control"`
	// Outcome is "vendor" (sent where the build sends by default), "redirected" (sent to the
	// endpoint the control named, and not to the vendor), or "off" (sent nowhere).
	Outcome string `json:"outcome"`
	// Hosts are where the build sent it, the endpoint the control named as <endpoint>; Paths the
	// paths it posted to.
	Hosts []string `json:"hosts,omitempty"`
	Paths []string `json:"paths,omitempty"`
	// Credential says whether the requests carried an Authorization header: the build's own
	// sign-in, which goes wherever its telemetry does.
	Credential bool `json:"credential"`
	// Notice is what the build printed of its telemetry, where it printed anything.
	Notice string `json:"notice,omitempty"`
}

// TestMuseProbeTelemetryRouting: where Muse's own telemetry can be sent, one profile turn a
// control, each behind the recording proxy with a local endpoint beside it: by default (Meta's
// host), with settings.json's telemetry.endpoint, with OTEL_EXPORTER_OTLP_ENDPOINT, and with each
// of its off switches. The Harness Reference says from it whether a user can redirect the
// telemetry, and that the build's credential goes with it. Seconds a control.
func TestMuseProbeTelemetryRouting(t *testing.T) {
	skipUnlessProbe(t, "telemetry-routing")
	forEachMuse(t, func(t *testing.T, b Binary) {
		track(t)
		profileOf(t, b)
		controls := []struct {
			name     string
			settings func(endpoint string) map[string]any
			env      func(endpoint string) []string
		}{
			{name: "default"},
			{name: "settings.json telemetry.endpoint", settings: func(e string) map[string]any { return map[string]any{"endpoint": e} }},
			{name: "OTEL_EXPORTER_OTLP_ENDPOINT", env: func(e string) []string { return []string{"OTEL_EXPORTER_OTLP_ENDPOINT=" + e} }},
			{name: "settings.json telemetry.enabled false", settings: func(string) map[string]any { return map[string]any{"enabled": false} }},
			{name: "TBH_DISABLE_TELEMETRY=1", env: func(string) []string { return []string{"TBH_DISABLE_TELEMETRY=1"} }},
		}
		var routes []TelemetryRoute
		for _, c := range controls {
			sb := New(t, Isolated)
			proxy := StartEgressProxy(t)
			var calls atomic.Int32
			provider := httptest.NewServer(museProvider(&calls, nil))
			t.Cleanup(provider.Close)
			var mu sync.Mutex
			var sunk []*http.Request
			sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				sunk = append(sunk, r)
				mu.Unlock()
			}))
			t.Cleanup(sink.Close)
			sb.UseMuse(provider.URL)
			if c.settings != nil {
				data, _ := json.Marshal(map[string]any{"schema_version": 1, "telemetry": c.settings(sink.URL)})
				sb.writeAbs(filepath.Join(sb.Home, ".config", "muse", "settings.json"), string(data)+"\n")
			}
			env := append(sb.termaEnv(), proxy.Env()...)
			if c.env != nil {
				env = append(env, c.env(sink.URL)...)
			}
			ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
			cmd := exec.CommandContext(ctx, b.Path, sb.museArgs(profilePrompt, false)...)
			cmd.Dir, cmd.Env = sb.Repo, env
			cmd.WaitDelay = 5 * time.Second
			out, err := cmd.CombinedOutput()
			cancel()
			if err != nil {
				t.Fatalf("muse exec (%s): %v\n%s", c.name, err, tail(string(out), 2000))
			}
			time.Sleep(egressSettle)
			route := TelemetryRoute{Control: c.name, Outcome: "off"}
			hosts, paths := map[string]bool{}, map[string]bool{}
			for _, r := range proxy.Requests() {
				if strings.Contains(r.Path, "/telemetry/") {
					route.Outcome = "vendor"
					hosts[r.Host] = true
					paths[r.Path] = true
					route.Credential = route.Credential || len(r.Header["authorization"]) > 0
				}
			}
			mu.Lock()
			for _, r := range sunk {
				route.Outcome = "redirected"
				hosts["<endpoint>"] = true
				paths[r.URL.Path] = true
				route.Credential = route.Credential || r.Header.Get("Authorization") != ""
			}
			mu.Unlock()
			if route.Outcome == "redirected" && len(hosts) > 1 {
				route.Outcome = "vendor and redirected"
			}
			route.Hosts, route.Paths = slices.Sorted(maps.Keys(hosts)), slices.Sorted(maps.Keys(paths))
			for line := range strings.SplitSeq(string(out), "\n") {
				if l := strings.TrimSpace(line); strings.HasPrefix(l, "notice:") && strings.Contains(l, "telemetry") {
					route.Notice = strings.ReplaceAll(l, strings.TrimPrefix(sink.URL, "http://"), "<endpoint>")
					break
				}
			}
			t.Logf("%s: %s %v %v credential=%v %s", c.name, route.Outcome, route.Hosts, route.Paths, route.Credential, route.Notice)
			routes = append(routes, route)
		}
		RecordProbe(t, b, "telemetry-routing", map[string]any{"routes": routes})
	})
}
