package opencode

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

const (
	relayAddr  = "127.0.0.1:43180"
	relayToken = "relay-token-0123456789"
)

// The lines OpenCode wrote when it would not load a plugin file, as captured from each
// build (the path scrubbed): 2.4.1's, 1.19.6's and 1.16.0's.
const (
	refused241  = `timestamp=2026-11-25T14:16:44.745Z level=WARN run=5a5cb7ef message="failed to load plugin" target=/home/user/.config/opencode/plugins/terma.js ref=err_c32582ac cause="Cause([Fail(PluginModule.LoadError: Plugin must export a default definition with an id and an effect or setup function. (cause: SchemaError(Expected string\n  at [\"default\"][\"id\"])))])" http.span=71 role=server`
	refused1196 = `timestamp=2026-11-25T14:16:35.667Z level=ERROR run=55e4b42e message="failed to load plugin" path=file:///home/user/.config/opencode/plugins/terma.js error="Plugin file:///home/user/.config/opencode/plugins/terma.js must default export an object with server()"`
	refused1160 = `ERROR 2026-11-25T14:16:42 +5ms service=plugin path=file:///home/user/.config/opencode/plugins/terma.js error=Plugin file:///home/user/.config/opencode/plugins/terma.js must default export an object with server() failed to load plugin`
)

// before is a moment earlier than every captured refusal line.
var before = time.Date(2026, 11, 25, 14, 0, 0, 0, time.UTC)

// relayRig is an OpenCode on a PATH of its own, a terma to run, and OpenCode's config and
// data directories, all in temporary folders.
type relayRig struct {
	agent  Agent
	bin    string
	plugin string
	logs   string
	terma  string
}

func newRelayRig(t *testing.T, version string) *relayRig {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake opencode is a shell script")
	}
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	t.Setenv(harness.EnvOTLPEndpoint, "")
	r := &relayRig{
		agent:  Agent{ConfigDir: filepath.Join(dir, "config", "terma")},
		bin:    filepath.Join(dir, "bin"),
		plugin: filepath.Join(dir, "config", "opencode", "plugins", "terma.js"),
		logs:   filepath.Join(dir, "data", "opencode", "log"),
		terma:  filepath.Join(dir, "terma", "bin", "terma"),
	}
	writeExec(t, r.terma, "#!/bin/sh\nexit 0\n")
	r.opencode(t, version)
	t.Setenv("PATH", r.bin)
	return r
}

// opencode puts an OpenCode whose --version prints version on the rig's PATH.
func (r *relayRig) opencode(t *testing.T, version string) {
	t.Helper()
	writeExec(t, filepath.Join(r.bin, "opencode"), "#!/bin/sh\necho "+version+"\n")
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// setup writes the plugin as terma setup does, running hook, and dates it before the
// captured refusals.
func (r *relayRig) setup(t *testing.T, hook ...string) agents.RelayResult {
	t.Helper()
	if len(hook) == 0 {
		hook = []string{r.terma, "hook"}
	}
	result, err := r.agent.ConfigureRelay(context.Background(), agents.RelayConfig{Endpoint: "http://" + relayAddr, Token: relayToken, HookCommand: hook})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	r.date(t, before)
	return result
}

func (r *relayRig) date(t *testing.T, at time.Time) {
	t.Helper()
	if err := os.Chtimes(r.plugin, at, at); err != nil {
		t.Fatal(err)
	}
}

// log writes OpenCode's log file name with lines, modified at.
func (r *relayRig) log(t *testing.T, name string, at time.Time, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(r.logs, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(r.logs, name)
	body := "INFO 2026-11-25T14:16:30 +1ms service=default opencode\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func (r *relayRig) config(t *testing.T) *opencodeConfig {
	t.Helper()
	data, err := os.ReadFile(r.plugin)
	if err != nil {
		t.Fatal(err)
	}
	cfg, ok := readPluginConfig(data)
	if !ok {
		t.Fatal("the plugin holds no configuration")
	}
	return cfg
}

// rewrite replaces the plugin's configuration line with cfg's, keeping this build's body.
func (r *relayRig) rewrite(t *testing.T, cfg *opencodeConfig) {
	t.Helper()
	src, err := renderPlugin(*cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.plugin, src, 0o600); err != nil {
		t.Fatal(err)
	}
	r.date(t, before)
}

// problem is doctor's verdict, and whether the agent reports itself pointed at the relay.
func (r *relayRig) problem(t *testing.T) (detail, fix string, problem, pointed bool) {
	t.Helper()
	detail, fix, problem = r.agent.RelayProblem(relayAddr)
	pointed, known := r.agent.RelayPointed(relayAddr)
	if !known {
		t.Fatal("RelayPointed is unknown with OpenCode on the PATH")
	}
	return detail, fix, problem, pointed
}

// Setup writes the plugin pointed at the relay with its token inline and the terma it was
// given by absolute path, says OpenCode must be restarted, and doctor finds nothing wrong.
func TestConfigureRelayWritesTheAbsoluteHookCommand(t *testing.T) {
	r := newRelayRig(t, "1.19.6")
	result := r.setup(t)
	if !reflect.DeepEqual(result.Paths, []string{r.plugin}) {
		t.Errorf("paths %q, want the plugin", result.Paths)
	}
	if len(result.Notes) != 1 || !strings.Contains(result.Notes[0], "Restart OpenCode") || !strings.Contains(result.Notes[0], "opencode serve") {
		t.Errorf("notes %q", result.Notes)
	}
	cfg := r.config(t)
	if !reflect.DeepEqual(cfg.HookCommand, []string{r.terma, "hook"}) {
		t.Errorf("hook command %q, want %q", cfg.HookCommand, r.terma)
	}
	if cfg.Endpoint != "http://"+relayAddr || cfg.Headers[opencodeAuthorizationHeader] != "Bearer "+relayToken || cfg.HeadersHelper != "" {
		t.Errorf("destination %q, headers helper %q", cfg.Endpoint, cfg.HeadersHelper)
	}
	if detail, _, problem, pointed := r.problem(t); problem || !pointed {
		t.Errorf("a fresh setup: problem %v (%s), pointed %v", problem, detail, pointed)
	}
}

// An OpenCode too old for the plugin is refused before anything is written, is not
// installed for setup --yes, and doctor says why; each major has its own minimum.
func TestConfigureRelayRefusesAnOpenCodeTooOld(t *testing.T) {
	for _, c := range []struct {
		version string
		refused bool
	}{
		{"1.15.0", true}, {"1.15.9", true}, {"1.16.0", false}, {"1.19.6", false}, {"2.0.0", false}, {"2.4.1", false},
	} {
		t.Run(c.version, func(t *testing.T) {
			r := newRelayRig(t, c.version)
			if installed := r.agent.Installed(context.Background()); installed == c.refused {
				t.Errorf("installed %v", installed)
			}
			_, err := r.agent.ConfigureRelay(context.Background(), agents.RelayConfig{Endpoint: "http://" + relayAddr, Token: relayToken, HookCommand: []string{r.terma, "hook"}})
			if refused := err != nil; refused != c.refused {
				t.Fatalf("setup error %v, want refused %v", err, c.refused)
			}
			if !c.refused {
				return
			}
			if !strings.Contains(err.Error(), "OpenCode 1.16.0 or later is required (found "+c.version+")") {
				t.Errorf("setup said %q", err)
			}
			if exists(r.plugin) {
				t.Error("setup wrote the plugin of an OpenCode it refused")
			}
			if detail, fix, problem := r.agent.RelayProblem(relayAddr); !problem || !strings.Contains(detail, "1.16.0 or later") || fix != "upgrade OpenCode, then run terma setup" {
				t.Errorf("doctor: %v %q %q", problem, detail, fix)
			}
		})
	}
}

// Each of doctor's checks fires on its own fault alone, and none makes the plugin read as
// pointed but the advisory one.
func TestRelayProblemEachCheckAlone(t *testing.T) {
	for _, c := range []struct {
		name    string
		fault   func(t *testing.T, r *relayRig)
		detail  string
		fix     string
		pointed bool
	}{
		{"too old", func(t *testing.T, r *relayRig) { r.opencode(t, "1.15.0") },
			"OpenCode 1.16.0 or later is required (found 1.15.0)", "upgrade OpenCode, then run terma setup", false},
		{"plugin missing", func(t *testing.T, r *relayRig) { _ = os.Remove(r.plugin) },
			"is missing, not this terma's or not pointed at the local relay", "terma setup", false},
		{"plugin from another build", func(t *testing.T, r *relayRig) {
			data, _ := os.ReadFile(r.plugin)
			_ = os.WriteFile(r.plugin, append([]byte("// an earlier build\n"), data...), 0o600)
		}, "is missing, not this terma's or not pointed at the local relay", "terma setup", false},
		{"plugin pointed elsewhere", func(t *testing.T, r *relayRig) {
			cfg := r.config(t)
			cfg.Endpoint = "http://127.0.0.1:9"
			r.rewrite(t, cfg)
		}, "is missing, not this terma's or not pointed at the local relay", "terma setup", false},
		{"terma gone", func(t *testing.T, r *relayRig) { _ = os.Remove(r.terma) },
			"runs " + "TERMA" + ", which is not there or cannot be run", "terma setup", false},
		{"terma not executable", func(t *testing.T, r *relayRig) { _ = os.Chmod(r.terma, 0o644) },
			"runs " + "TERMA" + ", which is not there or cannot be run", "terma setup", false},
		{"bare terma off the PATH", func(t *testing.T, r *relayRig) {
			cfg := r.config(t)
			cfg.HookCommand = []string{"terma", "hook"}
			r.rewrite(t, cfg)
		}, "runs terma, which is not there or cannot be run", "terma setup", false},
		{"refused by OpenCode 2.4.1", func(t *testing.T, r *relayRig) { r.log(t, "2026-11-25T141640.log", time.Now(), refused241) },
			"OpenCode refused to load terma's plugin", "terma setup, then restart OpenCode", false},
		{"refused by OpenCode 1.19.6", func(t *testing.T, r *relayRig) { r.log(t, "2026-11-25T141630.log", time.Now(), refused1196) },
			"must default export an object with server()", "terma setup, then restart OpenCode", false},
		{"refused by OpenCode 1.16.0", func(t *testing.T, r *relayRig) { r.log(t, "2026-11-25T141637.log", time.Now(), refused1160) },
			"must default export an object with server()", "terma setup, then restart OpenCode", false},
		{"a shell export of OpenCode's own", func(t *testing.T, r *relayRig) { t.Setenv(harness.EnvOTLPEndpoint, "https://collector.example.com") },
			"OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example.com", "unset it", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRelayRig(t, "1.19.6")
			r.setup(t)
			c.fault(t, r)
			detail, fix, problem, pointed := r.problem(t)
			want := strings.ReplaceAll(c.detail, "TERMA", output.TildePath(r.terma))
			if !problem || !strings.Contains(detail, want) || fix != c.fix {
				t.Errorf("problem %v, detail %q, fix %q; want %q, %q", problem, detail, fix, want, c.fix)
			}
			if pointed != c.pointed {
				t.Errorf("pointed %v, want %v", pointed, c.pointed)
			}
		})
	}
}

// With every fault at once, doctor names them in order as each is put right.
func TestRelayProblemOrder(t *testing.T) {
	r := newRelayRig(t, "1.19.6")
	r.setup(t)
	cfg := r.config(t)
	_ = os.Remove(r.terma)
	r.log(t, "2026-11-25T141630.log", time.Now(), refused1196)
	t.Setenv(harness.EnvOTLPEndpoint, "https://collector.example.com")
	data, _ := os.ReadFile(r.plugin)
	_ = os.WriteFile(r.plugin, append([]byte("// an earlier build\n"), data...), 0o600)
	r.opencode(t, "1.15.0")

	for _, step := range []struct {
		want  string
		fixIt func()
	}{
		{"1.16.0 or later is required", func() { r.opencode(t, "1.19.6") }},
		{"is missing, not this terma's", func() { r.rewrite(t, cfg) }},
		{"which is not there or cannot be run", func() { writeExec(t, r.terma, "#!/bin/sh\nexit 0\n") }},
		{"OpenCode refused to load terma's plugin", func() { _ = os.RemoveAll(r.logs) }},
		{"OTEL_EXPORTER_OTLP_ENDPOINT", func() { t.Setenv(harness.EnvOTLPEndpoint, "") }},
	} {
		if detail, _, problem := r.agent.RelayProblem(relayAddr); !problem || !strings.Contains(detail, step.want) {
			t.Fatalf("problem %v %q, want %q", problem, detail, step.want)
		}
		step.fixIt()
	}
	if detail, _, problem := r.agent.RelayProblem(relayAddr); problem {
		t.Fatalf("all put right, yet %q", detail)
	}
}

// The log check reads OpenCode's newest log alone, and in it only a refusal of terma's
// plugin no older than the plugin file: setup since has rewritten what OpenCode refused.
func TestRelayProblemReadsOnlyARefusalSinceThePlugin(t *testing.T) {
	r := newRelayRig(t, "1.19.6")
	r.setup(t)
	other := strings.ReplaceAll(refused1196, "terma.js", "someone-elses.js")
	r.log(t, "older.log", time.Now().Add(-time.Hour), refused1196)
	r.log(t, "newest.log", time.Now(), other)
	if detail, _, problem := r.agent.RelayProblem(relayAddr); problem {
		t.Errorf("another plugin's refusal, or one in an older log, was reported: %q", detail)
	}

	r.log(t, "newest.log", time.Now(), refused1196)
	r.date(t, time.Date(2026, 11, 25, 14, 20, 0, 0, time.UTC))
	if detail, _, problem := r.agent.RelayProblem(relayAddr); problem {
		t.Errorf("a refusal older than the plugin file was reported: %q", detail)
	}

	// 1.16.0 writes the time to the second: a line in the second the plugin was written counts.
	r.log(t, "newest.log", time.Now(), refused1160)
	r.date(t, time.Date(2026, 11, 25, 14, 16, 42, 300e6, time.UTC))
	if _, _, problem := r.agent.RelayProblem(relayAddr); !problem {
		t.Error("a refusal in the second the plugin was written was not reported")
	}

	// A line with no time terma can read counts.
	r.log(t, "newest.log", time.Now(), `message="failed to load plugin" path=terma.js error="no time here"`)
	r.date(t, time.Now())
	if detail, _, problem := r.agent.RelayProblem(relayAddr); !problem || !strings.HasSuffix(detail, ": no time here") {
		t.Errorf("a refusal with no time: %v %q", problem, detail)
	}
}

// Doctor quotes OpenCode's own reason, from whichever field each build writes it in, cut
// short.
func TestRefusalReason(t *testing.T) {
	t.Parallel()
	for line, want := range map[string]string{
		refused241:  `Cause([Fail(PluginModule.LoadError: Plugin must export a default definition with an id and an effect or setup function. (cause: SchemaError(Expected string\n  at [\"default\"][\"id\"])))])`,
		refused1196: "Plugin file:///home/user/.config/opencode/plugins/terma.js must default export an object with server()",
		refused1160: "Plugin file:///home/user/.config/opencode/plugins/terma.js must default export an object with server()",
	} {
		if got := refusalReason(line); got != want {
			t.Errorf("reason %q, want %q", got, want)
		}
	}
	long := `message="failed to load plugin" error="` + strings.Repeat("x", 300) + `"`
	if got := refusalReason(long); len([]rune(got)) != maxReason+1 || !strings.HasSuffix(got, "…") {
		t.Errorf("a long reason was cut to %d characters", len([]rune(got)))
	}
}

// The plugin's bearer is the relay's token or not; with no plugin, an inert one or a
// headers helper there is nothing to compare.
func TestRelayTokenCurrent(t *testing.T) {
	r := newRelayRig(t, "1.19.6")
	if _, known := r.agent.RelayTokenCurrent(relayToken); known {
		t.Error("no plugin, yet a token is known")
	}
	r.setup(t)
	if matches, known := r.agent.RelayTokenCurrent(relayToken); !matches || !known {
		t.Errorf("current token: matches %v known %v", matches, known)
	}
	if matches, known := r.agent.RelayTokenCurrent("another-token"); matches || !known {
		t.Errorf("stale token: matches %v known %v", matches, known)
	}
	cfg := r.config(t)
	cfg.Headers, cfg.HeadersHelper = nil, filepath.Join(t.TempDir(), "helper")
	r.rewrite(t, cfg)
	if _, known := r.agent.RelayTokenCurrent(relayToken); known {
		t.Error("a headers helper's token is known")
	}
	if err := os.WriteFile(r.plugin, []byte(opencodePluginSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, known := r.agent.RelayTokenCurrent(relayToken); known {
		t.Error("an inert plugin's token is known")
	}
}

// Refresh brings the plugin's body up to this build's, keeps a terma that still runs at its
// absolute path, and puts this terma in place of a bare name or a path that is gone.
func TestRefreshPluginRepairsTheHookCommand(t *testing.T) {
	self, err := procinfo.AbsExecutable()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		hook func(r *relayRig) []string
		want func(r *relayRig) string
	}{
		{"a live absolute path", func(r *relayRig) []string { return []string{r.terma, "hook"} }, func(r *relayRig) string { return r.terma }},
		{"a bare terma", func(*relayRig) []string { return []string{"terma", "hook"} }, func(*relayRig) string { return self }},
		{"a path that is gone", func(r *relayRig) []string { return []string{filepath.Join(filepath.Dir(r.terma), "gone"), "hook"} }, func(*relayRig) string { return self }},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRelayRig(t, "1.19.6")
			r.setup(t, c.hook(r)...)
			data, _ := os.ReadFile(r.plugin)
			if err := os.WriteFile(r.plugin, append([]byte("// an earlier build\n"), data...), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, changed, err := (exporter{}).RefreshPlugin(); err != nil || !changed {
				t.Fatalf("refresh: changed %v, err %v", changed, err)
			}
			cfg := r.config(t)
			if want := []string{c.want(r), "hook"}; !reflect.DeepEqual(cfg.HookCommand, want) {
				t.Errorf("hook command %q, want %q", cfg.HookCommand, want)
			}
			if _, current := pluginCurrent(r.plugin); !current {
				t.Error("the refreshed plugin is not this build's")
			}
			if cfg.Endpoint != "http://"+relayAddr || cfg.Headers[opencodeAuthorizationHeader] != "Bearer "+relayToken {
				t.Errorf("the destination was not kept: %q", cfg.Endpoint)
			}
		})
	}
}
