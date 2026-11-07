package e2e

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The desktop apps' update feeds are read for their releases, oldest first, one a version.
func TestDesktopFeedsOffline(t *testing.T) {
	squirrel := `{"currentRelease":"4.16336.0","releases":[{"version":"4.16336.0","updateTo":{"name":"Claude 4.16336.0","version":"4.16336.0",
		"pub_date":"2026-11-03T07:13:41.864Z","url":"https://downloads.claude.ai/releases/darwin/universal/4.16336.0/Claude-61c5.zip","notes":""}}]}`
	rs, err := ParseDesktopFeed("claude-desktop", []byte(squirrel))
	if err != nil || len(rs) != 1 || rs[0].Version != "4.16336.0" || !strings.HasSuffix(rs[0].URL, "Claude-61c5.zip") || rs[0].Released.IsZero() {
		t.Fatalf("Squirrel feed: %+v %v", rs, err)
	}
	appcast := `<?xml version='1.0' encoding='utf-8'?>
<rss xmlns:sparkle="http://www.andymatuschak.org/xml-namespaces/sparkle" version="2.0"><channel><title>Codex</title>
<item><title>26.1029.70936</title><pubDate>Wed, 04 Nov 2026 20:21:01 +0000</pubDate><sparkle:version>24905</sparkle:version>
<sparkle:shortVersionString>26.1029.70936</sparkle:shortVersionString>
<enclosure url="https://persistent.oaistatic.com/codex-app-prod/ChatGPT-darwin-arm64-26.1029.70936.zip" length="1" type="application/octet-stream" />
<sparkle:deltas><enclosure url="https://persistent.oaistatic.com/codex-app-prod/ChatGPT24905-24614-arm64.delta" sparkle:deltaFrom="24614" /></sparkle:deltas></item>
<item><title>26.803.81509</title><pubDate>Tue, 11 Aug 2026 00:24:15 +0000</pubDate><sparkle:version>22943</sparkle:version>
<sparkle:shortVersionString>26.803.81509</sparkle:shortVersionString>
<enclosure url="https://persistent.oaistatic.com/codex-app-prod/ChatGPT-darwin-arm64-26.803.81509.zip" length="1" type="application/octet-stream" /></item>
</channel></rss>`
	rs, err = ParseDesktopFeed("codex-desktop", []byte(appcast))
	if err != nil || len(rs) != 2 {
		t.Fatalf("appcast: %+v %v", rs, err)
	}
	if rs[0].Version != "26.803.81509" || rs[1].Version != "26.1029.70936" || rs[1].Build != "24905" ||
		!strings.HasSuffix(rs[1].URL, "26.1029.70936.zip") || rs[1].Released.Format("2006-01-02") != "2026-11-04" {
		t.Errorf("appcast releases, oldest first, the full archive's: %+v", rs)
	}
}

// The Claude Code build a Claude Desktop release pins is read from its archive's JavaScript.
func TestClaudeDesktopPinOffline(t *testing.T) {
	js := `function Q_(){return JSON.parse('{"version":"2.1.317","manifest":{"version":"2.1.317","commit":"3d3e","buildDate":"2026-11-02T22:41:52Z",` +
		`"platforms":{"darwin-arm64":{"binary":"claude.zst","checksum":"f2e2","size":107811821,"bundle":{"checksum":"9118","size":107815075}}}},` +
		`"baseUrl":"https://downloads.claude.ai/claude-code-releases","sdkWrapperVersion":"0.3.317"}')}function BEt(){return JSON.parse('{"version":"b308",` +
		`"manifest":{"platforms":{}},"baseUrl":"https://downloads.claude.ai/claude-ssh-releases"}')}`
	dir := t.TempDir()
	bundle := filepath.Join(dir, "Claude.app")
	if err := os.MkdirAll(filepath.Join(bundle, "Contents", "Resources"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAsar(t, filepath.Join(bundle, "Contents", "Resources", "app.asar"), map[string]string{".vite/build/index.chunk-x.js": js, "package.json": "{}"})
	pin, err := claudeDesktopPinOf(DesktopApp{Bundle: bundle})
	if err != nil {
		t.Fatal(err)
	}
	p := pin.Manifest.Platforms["darwin-arm64"]
	if pin.Version != "2.1.317" || pin.SDKWrapperVersion != "0.3.317" || pin.BaseURL != "https://downloads.claude.ai/claude-code-releases" ||
		p.Checksum != "f2e2" || p.Bundle == nil || p.Bundle.Checksum != "9118" {
		t.Errorf("pin: %+v", pin)
	}
}

// writeAsar writes an Electron archive of files.
func writeAsar(t *testing.T, path string, files map[string]string) {
	t.Helper()
	type node struct {
		Files  map[string]*node `json:"files,omitempty"`
		Offset string           `json:"offset,omitempty"`
		Size   int64            `json:"size,omitempty"`
	}
	root := &node{Files: map[string]*node{}}
	var body []byte
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for k := range files {
			if !yield(k) {
				return
			}
		}
	}) {
		parts := strings.Split(name, "/")
		n := root
		for _, dir := range parts[:len(parts)-1] {
			if n.Files[dir] == nil {
				n.Files[dir] = &node{Files: map[string]*node{}}
			}
			n = n.Files[dir]
		}
		n.Files[parts[len(parts)-1]] = &node{Offset: itoa(len(body)), Size: int64(len(files[name]))}
		body = append(body, files[name]...)
	}
	header, _ := json.Marshal(root)
	str := make([]byte, 8)
	binary.LittleEndian.PutUint32(str[0:4], uint32(len(header)+4))
	binary.LittleEndian.PutUint32(str[4:8], uint32(len(header)))
	pickle := append(str, header...)
	out := make([]byte, 8)
	binary.LittleEndian.PutUint32(out[0:4], 4)
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(pickle)))
	out = append(append(out, pickle...), body...)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int) string { data, _ := json.Marshal(n); return string(data) }

// How a process was started is read back from the kernel: this test's own command line.
func TestProcArgsOffline(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("KERN_PROCARGS2 is macOS's")
	}
	t.Setenv("TERMA_PROCARGS_PROBE", "x")
	argv, env, err := procArgs(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(argv, os.Args) {
		t.Errorf("argv %q, want %q", argv, os.Args)
	}
	if len(env) == 0 || !slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "PATH=") }) {
		t.Errorf("the environment the process started with: %d variables", len(env))
	}
}

// An app-server's methods are read from its JSON schema bundle, by the file that names them.
func TestAppServerSurfaceOffline(t *testing.T) {
	dir := t.TempDir()
	req := `{"oneOf":[{"type":"object","properties":{"method":{"enum":["thread/start"]},"params":{}}},
		{"type":"object","properties":{"method":{"type":"string","enum":["turn/start"]}}}]}`
	note := `{"oneOf":[{"properties":{"method":{"const":"turn/completed"}}}]}`
	for name, body := range map[string]string{"ClientRequest.json": req, "ServerNotification.json": note, "v2/ThreadStartParams.json": `{"properties":{"cwd":{}}}`} {
		path := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := AppServerSurfaceOf(dir)
	if !slices.Equal(got["ClientRequest"], []string{"thread/start", "turn/start"}) || !slices.Equal(got["ServerNotification"], []string{"turn/completed"}) || len(got) != 2 {
		t.Errorf("surface: %v", got)
	}
}

// How an app started its agent keeps the app's own variables' values, a secret's redacted and
// a list of folders <path>, and leaves the login session's out: a child started so is read back.
func TestLaunchScrubOffline(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("KERN_PROCARGS2 is macOS's")
	}
	// The test binary as the hooks probe's recorder, which waits on its standard input: a
	// platform binary's (/bin/sleep's) environment the kernel keeps from other processes.
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, hookRecorderArg, t.TempDir(), "Probe", hookMatchAll)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	cmd.Env = []string{"HOME=/Users/someone", "PATH=/usr/bin:/bin", "CLAUDE_CODE_ENTRYPOINT=claude-desktop-3p",
		"ANTHROPIC_API_KEY=synthetic-gateway-key", "TERMA_PROBE_FOLDERS=/a:/b:/c", "TERMA_PROBE_TOKEN_LIKE=sk-abcdefghijklmnopqrstu"}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	l, err := launchOf(cmd.Process.Pid, os.Getpid(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"CLAUDE_CODE_ENTRYPOINT": "claude-desktop-3p", "ANTHROPIC_API_KEY": "<redacted>",
		"TERMA_PROBE_FOLDERS": "<path>", "TERMA_PROBE_TOKEN_LIKE": "<redacted>"}
	for k, v := range want {
		if l.EnvValues[k] != v {
			t.Errorf("%s = %q, want %q", k, l.EnvValues[k], v)
		}
	}
	// Set otherwise than this process has it: the app's doing, its value the machine's.
	if l.EnvValues["HOME"] != "<session>" || l.EnvValues["PATH"] != "<path>" {
		t.Errorf("HOME and PATH: %v", l.EnvValues)
	}
	// What the app passes on as it has it is counted, not listed.
	cmd2 := exec.Command(exe, hookRecorderArg, t.TempDir(), "Probe", hookMatchAll)
	cmd2.Env = append(os.Environ(), "CLAUDE_CODE_ENTRYPOINT=claude-desktop-3p")
	stdin2, _ := cmd2.StdinPipe()
	defer stdin2.Close()
	if err := cmd2.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd2.Process.Kill(); _ = cmd2.Wait() }()
	inherited, err := launchOf(cmd2.Process.Pid, os.Getpid(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(inherited.Env, "CLAUDE_CODE_ENTRYPOINT") || inherited.Inherited == 0 || slices.Contains(inherited.Env, "HOME") {
		t.Errorf("only what the app set is listed: %v, %d inherited", inherited.Env, inherited.Inherited)
	}
	if len(l.Argv) != 5 || l.Argv[1] != hookRecorderArg {
		t.Errorf("argv %q", l.Argv)
	}
}

// A property list of an organization's managed preferences.
func TestPlistXMLOffline(t *testing.T) {
	got := string(plistXML(map[string]any{"inferenceProvider": "gateway", "disableAutoUpdates": true, "url": "http://127.0.0.1:1/?a=1&b=<2>"}))
	for _, want := range []string{"<key>disableAutoUpdates</key>\n\t<true/>", "<key>inferenceProvider</key>\n\t<string>gateway</string>", "a=1&amp;b=&lt;2&gt;"} {
		if !strings.Contains(got, want) {
			t.Errorf("plist lacks %q:\n%s", want, got)
		}
	}
}

// The machine's names an app writes into a value of its own are placeholders.
func TestMachineNamesOffline(t *testing.T) {
	m := machine{home: "/Users/runner", user: "runner", host: "Mac-1730000000000.local"}
	none := map[string]string{}
	got := machineNames("service.name=claude-code-desktop,os.type=darwin,os.version=26.6.2,process.owner=runner,host.arch=arm64", none, m)
	if want := "service.name=claude-code-desktop,os.type=darwin,os.version=<os-release>,process.owner=<user>,host.arch=arm64"; got != want {
		t.Errorf("machineNames = %q, want %q", got, want)
	}
	if got := machineNames("the test runners run", none, m); got != "the test runners run" {
		t.Errorf("a word that holds the name is no name: %q", got)
	}
	if got := machineNames("host.name=Mac-1730000000000.local,peer=Mac-1730000000000", none, m); got != "host.name=<host>,peer=<host>" {
		t.Errorf("the machine's name: %q", got)
	}
	// The run's places go before the user's name inside them: a path is <app>/…, <home>/…, never
	// /Users/<user>/…, whatever order the places are given in.
	app := DesktopApp{Bundle: "/Users/runner/work/terma-cli/terma-cli/test/e2e/versions/codex-desktop/26.1029.70936/ChatGPT.app"}
	places := map[string]string{"/Users/runner": "<home>", app.Bundle: "<app>", filepath.Dir(app.Bundle): "<app-dir>",
		"/Users/runner/work/terma-cli/terma-cli": "<checkout>"}
	for in, want := range map[string]string{
		app.Bundle + "/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex": "<app>/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex",
		"/Users/runner/Library/Logs/Claude-3p/cli-diagnostics.jsonl":                   "<home>/Library/Logs/Claude-3p/cli-diagnostics.jsonl",
		"/Users/runner/work/terma-cli/terma-cli/go.mod":                                "<checkout>/go.mod",
		"/Users/runner/Library/Application Support/Claude-3p/skills-plugin/<uuid>":     "<home>/Library/Application Support/Claude-3p/skills-plugin/<uuid>",
		"/opt/homebrew/bin/node":      "node",
		"/Users/someone-else/.config": "<home>/.config",
	} {
		if got := machineNames(in, places, m); got != want {
			t.Errorf("machineNames(%q)\n got %q\nwant %q", in, got, want)
		}
	}
	// Without the run's places (a value the places miss), MachinePaths still names the home.
	if got := machineNames("/Users/runner/Library/Logs/x.jsonl", none, m); got != "<home>/Library/Logs/x.jsonl" {
		t.Errorf("a home the places missed: %q", got)
	}
}

// A desktop app's places: its bundle and folder, the checkout the suite runs from, the home.
func TestDesktopPlacesOffline(t *testing.T) {
	app := DesktopApp{Bundle: "/x/test/e2e/versions/codex-desktop/1.0/ChatGPT.app"}
	got := desktopPlaces(app, "/Users/runner")
	for lit, name := range map[string]string{app.Bundle: "<app>", filepath.Dir(app.Bundle): "<app-dir>", "/Users/runner": "<home>"} {
		if got[lit] != name {
			t.Errorf("%s = %q, want %q (%v)", lit, got[lit], name, got)
		}
	}
	wd, _ := os.Getwd()
	if checkout := filepath.Dir(filepath.Dir(wd)); got[checkout] != "<checkout>" {
		t.Errorf("checkout %s: %v", checkout, got)
	}
}

// A change to machine-wide state as root runs only on a throwaway runner: a developer's machine
// runs eslogger and lsof under sudo, nothing else.
func TestRootChangesGuardOffline(t *testing.T) {
	for _, c := range []struct {
		actions, runner, allow string
		ok                     bool
	}{
		{"", "", "", false},
		{"true", "self-hosted", "", false},
		{"true", "", "", false},
		{"", "github-hosted", "", false},
		{"true", "github-hosted", "", true},
		{"", "", "1", true},
		{"", "", "yes", false},
	} {
		t.Setenv("GITHUB_ACTIONS", c.actions)
		t.Setenv("RUNNER_ENVIRONMENT", c.runner)
		t.Setenv(envAllowRootChanges, c.allow)
		cmd, err := rootCommand("rm", "-f", "/Library/Managed Preferences/x.plist")
		if ok := err == nil; ok != c.ok {
			t.Errorf("%+v: allowed %v, want %v (%v)", c, ok, c.ok, err)
		}
		if err == nil && !slices.Equal(cmd.Args, []string{"sudo", "-n", "rm", "-f", "/Library/Managed Preferences/x.plist"}) {
			t.Errorf("command = %q", cmd.Args)
		}
		if err != nil && !strings.Contains(err.Error(), "eslogger and lsof") {
			t.Errorf("the refusal says not why: %v", err)
		}
	}
}

// Every sudo the suite runs is eslogger's, or goes through rootCommand.
func TestSudoOnlyThroughGuardOffline(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(line, `"sudo"`) || strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			eslogger := f == "filetrace_eslogger.go" && strings.Contains(line, `"/usr/bin/eslogger"`)
			guard := f == "desktop.go" && strings.Contains(line, `desktopCmd("sudo", append([]string{"-n"}, args...)...)`)
			if !eslogger && !guard {
				t.Errorf("%s:%d runs sudo outside rootCommand: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// A desktop release's version names the folder it is unpacked in: a feed's version that would
// name one outside it is refused.
func TestDesktopFeedRefusesEscapingVersionOffline(t *testing.T) {
	squirrel := `{"releases":[{"updateTo":{"version":"../../../escaped","url":"https://x/a.zip","pub_date":"2026-11-01T00:00:00Z"}}]}`
	if _, err := ParseDesktopFeed("claude-desktop", []byte(squirrel)); err == nil || !strings.Contains(err.Error(), "escaped") {
		t.Errorf("squirrel: %v", err)
	}
	appcast := `<rss xmlns:sparkle="http://www.andymatuschak.org/xml-namespaces/sparkle"><channel><item><sparkle:version>1</sparkle:version>` +
		`<sparkle:shortVersionString>../../../escaped</sparkle:shortVersionString><enclosure url="https://x/a.zip"/></item></channel></rss>`
	if _, err := ParseDesktopFeed("codex-desktop", []byte(appcast)); err == nil || !strings.Contains(err.Error(), "escaped") {
		t.Errorf("appcast: %v", err)
	}
	ok := `{"releases":[{"updateTo":{"version":"4.16336.0","url":"https://x/a.zip","pub_date":"2026-11-01T00:00:00Z"}}]}`
	if rs, err := ParseDesktopFeed("claude-desktop", []byte(ok)); err != nil || len(rs) != 1 {
		t.Errorf("a good feed: %v %v", rs, err)
	}
}
