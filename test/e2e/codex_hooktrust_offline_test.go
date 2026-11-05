package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A hook's trust replaces the hash recorded for its key, keeping the rest of its table and of
// the file, and a hook with no record gets a table of its own.
func TestWithCodexHookTrustOffline(t *testing.T) {
	config := `model = "x"

[hooks.state."/h/hooks.json:pre_tool_use:0:0"]
enabled = true
trusted_hash = "sha256:old"

[projects."/repo"]
trust_level = "trusted"

[hooks.state."/h/hooks.json:stop:0:0"]
trusted_hash = "sha256:kept"
`
	got := withCodexHookTrust(config, map[string]string{
		"/h/hooks.json:pre_tool_use:0:0":  "sha256:new",
		"/h/hooks.json:session_start:0:0": "sha256:added",
	})
	hashes := trustedHashes(got)
	want := map[string]string{
		"/h/hooks.json:pre_tool_use:0:0":  "sha256:new",
		"/h/hooks.json:session_start:0:0": "sha256:added",
		"/h/hooks.json:stop:0:0":          "sha256:kept",
	}
	if len(hashes) != len(want) {
		t.Errorf("trusted hashes = %v, want %v\n%s", hashes, want, got)
	}
	for k, v := range want {
		if hashes[k] != v {
			t.Errorf("%s trusted at %q, want %q\n%s", k, hashes[k], v, got)
		}
	}
	for _, s := range []string{`model = "x"`, "enabled = true", `[projects."/repo"]`, `trust_level = "trusted"`} {
		if !strings.Contains(got, s) {
			t.Errorf("%s lost:\n%s", s, got)
		}
	}
	if n := strings.Count(got, "trusted_hash"); n != 3 {
		t.Errorf("%d trusted_hash lines, want 3:\n%s", n, got)
	}
	if strings.Contains(got, "sha256:old") {
		t.Errorf("the old hash stayed:\n%s", got)
	}
	if again := withCodexHookTrust(got, map[string]string{"/h/hooks.json:pre_tool_use:0:0": "sha256:new"}); again != got {
		t.Errorf("trusting a trusted hook again changed the file:\n%s\nto\n%s", got, again)
	}
}

// fakeCodexAppServer is a Codex whose app-server lists one hook, k1 hashed h1, trusted once
// config.toml records that hash (never, if stubborn), and only where it was started with the
// hooks feature on.
func fakeCodexAppServer(t *testing.T, stubborn bool) string {
	t.Helper()
	trusted := `grep -q 'trusted_hash = "h1"' "$CODEX_HOME/config.toml" 2>/dev/null`
	if stubborn {
		trusted = "false"
	}
	script := `#!/bin/sh
# names trusted_hash, as a build with hook trust does
case " $* " in *" features.hooks=true "*) on=1 ;; *) on= ;; esac
mkdir -p "$CODEX_HOME/skills/.system" "$CODEX_HOME/log" && : > "$CODEX_HOME/state_5.sqlite" && : > "$CODEX_HOME/log/app.log"
while IFS= read -r line; do
	id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
	case "$line" in
	*'"method":"initialize"'*) printf '{"id":%s,"result":{}}\n' "$id" ;;
	*'"method":"hooks/list"'*)
		status=untrusted
		if ` + trusted + `; then status=trusted; fi
		hooks='[]'
		[ -n "$on" ] && hooks='[{"key":"k1","currentHash":"h1","trustStatus":"'$status'","isManaged":false},{"key":"m1","currentHash":"hm","trustStatus":"managed","isManaged":true}]'
		printf '{"id":%s,"result":{"data":[{"cwd":"/r","hooks":%s,"warnings":[],"errors":[]}]}}\n' "$id" "$hooks" ;;
	esac
done
`
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func hookTrustSandbox(t *testing.T, codex string) *Sandbox {
	t.Helper()
	dir := t.TempDir()
	sb := &Sandbox{T: t, Dir: dir, Home: dir, Repo: dir, CodexHome: filepath.Join(dir, "codex-home"), Codex: Binary{Harness: "codex", Path: codex}}
	if err := os.MkdirAll(sb.CodexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	return sb
}

// The sandbox trusts the hooks the build's own app-server lists, at the hashes it gives, and
// checks the build then trusts them; a build without hook trust is left alone.
func TestTrustCodexHooksOffline(t *testing.T) {
	config := []string{"exec", "--json", "-C", "/r", "-c", "features.hooks=true", "--sandbox", "workspace-write"}

	sb := hookTrustSandbox(t, fakeCodexAppServer(t, false))
	// The home's own files stay; what the app-server added to it goes.
	sb.writeAbs(filepath.Join(sb.CodexHome, "log", "codex-login.log"), "login\n")
	trust := trustCodexHooks(t, sb, config)
	for _, p := range []string{"skills", "state_5.sqlite", "log/app.log"} {
		if fileExists(filepath.Join(sb.CodexHome, p)) {
			t.Errorf("the app-server's %s stayed in CODEX_HOME", p)
		}
	}
	if !fileExists(filepath.Join(sb.CodexHome, "log", "codex-login.log")) {
		t.Error("the home's own log/codex-login.log was removed")
	}
	if !trust.Gated || !trust.Trusted() || trust.Probe() != "trusted" || trust.Err != nil {
		t.Errorf("trust = %+v (%s), want every hook trusted", trust, trust)
	}
	if trust.Statuses["trusted"] != 1 || trust.Statuses["managed"] != 1 {
		t.Errorf("statuses = %v, want 1 trusted and 1 managed", trust.Statuses)
	}
	data, _ := os.ReadFile(filepath.Join(sb.CodexHome, "config.toml"))
	if h := trustedHashes(string(data)); len(h) != 1 || h["k1"] != "h1" {
		t.Errorf("config.toml trusts %v, want k1 at h1 alone (a managed hook needs none):\n%s", h, data)
	}

	// The options the run takes reach the app-server: without the feature it lists nothing.
	trust = trustCodexHooks(t, hookTrustSandbox(t, fakeCodexAppServer(t, false)), []string{"exec"})
	if !trust.Gated || trust.Err == nil || trust.Probe() != "untrusted" {
		t.Errorf("with no hooks listed, trust = %+v, want an error", trust)
	}

	trust = trustCodexHooks(t, hookTrustSandbox(t, fakeCodexAppServer(t, true)), config)
	if trust.Trusted() || trust.Probe() != "untrusted" || trust.Err == nil {
		t.Errorf("a build that never trusts: trust = %+v, want untrusted", trust)
	}
	if s := trust.String(); !strings.Contains(s, "1 untrusted") || !strings.Contains(s, "not set") {
		t.Errorf("untrusted trust reads %q", s)
	}

	plain := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(plain, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if trust := trustCodexHooks(t, hookTrustSandbox(t, plain), config); trust.Gated || trust.Probe() != "" || !trust.Trusted() {
		t.Errorf("a build without hook trust: trust = %+v", trust)
	}
}
