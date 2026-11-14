package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// reference/new-builds.sh against a registry, an update feed and a published reference of the
// test's own: which builds the frequent check captures, from what each harness has released and
// what the reference has published of it.
func TestNewBuilds(t *testing.T) {
	for _, tool := range []string{"bash", "curl", "jq", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	now := time.Now().UTC()
	stamp := func(hours int) string { return now.Add(-time.Duration(hours) * time.Hour).Format(time.RFC3339Nano) }
	// published is an index entry as refgen writes it: whole, captured two days ago.
	published := func(version string) map[string]any {
		return map[string]any{"version": version, "captured_at": stamp(48)}
	}
	whole := func(versions ...string) []map[string]any {
		var out []map[string]any
		for _, v := range versions {
			out = append(out, published(v))
		}
		return out
	}
	// partial is a partial entry whose last attempt was at attempted ("" for an entry from
	// before attempted_at).
	partial := func(version, attempted string) map[string]any {
		e := published(version)
		e["partial"] = true
		if attempted != "" {
			e["attempted_at"] = attempted
		}
		return e
	}
	withAttempts := func(e map[string]any, n int) map[string]any { e["attempts"] = n; return e }
	releases := []string{"1.0.0", "1.0.1", "1.0.2", "1.0.3", "1.0.4", "1.0.5", "1.1.0-beta.1"}
	afterFour := func(e map[string]any) []map[string]any { return append(whole("1.0.1", "1.0.2", "1.0.3", "1.0.4"), e) }
	for _, c := range []struct {
		name    string
		harness string           // claude unless set
		index   []map[string]any // nil: the reference has no index (404)
		down    bool             // the reference answers 500
		env     []string         // the script's settings
		want    string
	}{
		{name: "never published: the newest release alone", want: "claude=1.0.5"},
		{name: "the newest five not published, a late backport among them",
			index: whole("1.0.0", "1.0.1", "1.0.3", "1.0.5"), want: "claude=1.0.2,1.0.4"},
		{name: "every release published", index: whole("1.0.1", "1.0.2", "1.0.3", "1.0.4", "1.0.5")},
		{name: "a partial newest, attempted long ago, is tried again",
			index: afterFour(partial("1.0.5", stamp(7))), want: "claude=1.0.5"},
		{name: "a partial newest, attempted lately, waits though captured long ago",
			index: afterFour(partial("1.0.5", stamp(1)))},
		{name: "a partial newest from before attempted_at waits from its capture",
			index: afterFour(partial("1.0.5", "")), want: "claude=1.0.5"},
		{name: "a partial build among the newest five is tried again too",
			index: append(whole("1.0.1", "1.0.2", "1.0.3", "1.0.5"), partial("1.0.4", stamp(7))), want: "claude=1.0.4"},
		{name: "a partial build older than the newest five stays as it is",
			index: append(whole("1.0.1", "1.0.2", "1.0.3", "1.0.4", "1.0.5"), partial("1.0.0", stamp(7)))},
		{name: "an attempt stamped with an offset is read as the time it is",
			index: afterFour(partial("1.0.5", now.Add(-time.Hour).Format("2006-01-02T15:04:05.000+00:00")))},
		{name: "a partial build that has used its attempts stays as it is",
			index: afterFour(withAttempts(partial("1.0.5", stamp(7)), 4))},
		{name: "a partial build with attempts to spare is tried again",
			index: afterFour(withAttempts(partial("1.0.5", stamp(7)), 3)), want: "claude=1.0.5"},
		{name: "a page from before attempts were counted made one",
			index: afterFour(partial("1.0.5", stamp(7))), env: []string{"TERMA_REFERENCE_MAX_ATTEMPTS=1"}},
		{name: "an attempt time that does not parse is an old one",
			index: afterFour(partial("1.0.5", "yesterday")), want: "claude=1.0.5"},
		{name: "a reference that cannot be read is no build to capture", down: true},
		{name: "a desktop app never published: its feed's newest", harness: "claude-desktop", want: "claude-desktop=1.2.0"},
		{name: "a desktop app's newest published", harness: "claude-desktop", index: whole("1.2.0")},
		{name: "a desktop app's partial newest, attempted long ago", harness: "claude-desktop",
			index: []map[string]any{partial("1.2.0", stamp(7))}, want: "claude-desktop=1.2.0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			harness := c.harness
			if harness == "" {
				harness = "claude"
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/registry/@anthropic-ai/claude-code":
					versions := map[string]any{}
					for _, v := range releases {
						versions[v] = map[string]any{}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"versions": versions, "dist-tags": map[string]string{"latest": "1.0.5"}})
				case "/feeds/claude-desktop":
					_ = json.NewEncoder(w).Encode(map[string]any{"releases": []any{
						map[string]any{"updateTo": map[string]string{"version": "1.1.0"}},
						map[string]any{"updateTo": map[string]string{"version": "1.2.0"}}}})
				case "/reference/" + harness + "/index.json":
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
			cmd.Env = append(cmd.Environ(), "TERMA_REFERENCE_HARNESSES="+harness, "TERMA_NPM_REGISTRY="+srv.URL+"/registry",
				"TERMA_REFERENCE_FEEDS_URL="+srv.URL+"/feeds", "TERMA_REFERENCE_URL="+srv.URL+"/reference")
			cmd.Env = append(cmd.Env, c.env...)
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

// reference/capture-args.sh: every build a run set out to capture is attempted, and one whose job
// did not say it finished (its capture.json), or whose desktop job failed before it recorded a
// profile of it, is unfinished. Files are found by what they say: download-artifact unpacks one
// artifact into <runs> itself and several into folders of their names.
func TestCaptureArgs(t *testing.T) {
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	write := func(t *testing.T, path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	captureArgs := func(t *testing.T, runs string, env ...string) []string {
		t.Helper()
		cmd := exec.Command("bash", "reference/capture-args.sh", runs)
		cmd.Env = append(cmd.Environ(), env...)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("capture-args.sh: %v\n%s", err, out)
		}
		return strings.Fields(string(out))
	}

	// Several artifacts, each in a folder of its name.
	runs := t.TempDir()
	write(t, filepath.Join(runs, "capture-claude", "capture.json"), `{"job":"macos","harness":"claude","finished":true}`)
	write(t, filepath.Join(runs, "capture-gemini", "capture.json"), `{"job":"macos","harness":"gemini","finished":true}`)
	write(t, filepath.Join(runs, "capture-codex", "capture.json"), `{"job":"macos","harness":"codex","finished":false}`)
	write(t, filepath.Join(runs, "compat-linux", "capture.json"), `{"job":"linux","finished":false}`)
	// The desktop job failed for one app: the other's profile is there.
	write(t, filepath.Join(runs, "profiles-desktop", "profiles.json"), `[{"harness":"codex-desktop","version":"26.1106.1"},{"harness":"claude-desktop","version":"1.1.0"}]`)
	got := captureArgs(t, runs, "BUILDS=claude=2.1.1\ngemini=0.9.0\ncodex=0.200.0\nclaude-desktop=1.1.0,1.2.0\ncodex-desktop=26.1106.1",
		"CLAUDE_BUILDS=2.1.1", "CODEX_BUILDS=0.200.0", "DESKTOP_CAPTURE=failure")
	want := []string{
		"-attempted", "claude=2.1.1",
		"-attempted", "gemini=0.9.0",
		"-attempted", "codex=0.200.0", "-unfinished", "codex=0.200.0", // its run did not end
		"-attempted", "claude-desktop=1.1.0,1.2.0", "-unfinished", "claude-desktop=1.2.0", // the desktop job failed, recording 1.1.0 alone
		"-attempted", "codex-desktop=26.1106.1", // recorded beside it
		"-unfinished", "claude=2.1.1", "-unfinished", "codex=0.200.0", // Linux's run did not end
	}
	if !slices.Equal(got, want) {
		t.Errorf("several artifacts: capture-args.sh =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// One artifact, unpacked into <runs> itself: Copilot's capture, whole.
	runs = t.TempDir()
	write(t, filepath.Join(runs, "capture.json"), `{"job":"macos","harness":"copilot","finished":true}`)
	write(t, filepath.Join(runs, "compat.json"), `[]`)
	if got := captureArgs(t, runs, "BUILDS=copilot=1.0.112"); !slices.Equal(got, []string{"-attempted", "copilot=1.0.112"}) {
		t.Errorf("one artifact: %q", got)
	}
	// One that says nothing of its end, or nothing at all, did not finish.
	for _, runs := range []string{t.TempDir(), func() string {
		r := t.TempDir()
		write(t, filepath.Join(r, "capture.json"), `{"job":"macos","harness":"copilot","finished":false}`)
		return r
	}()} {
		if got := captureArgs(t, runs, "BUILDS=copilot=1.0.112"); !slices.Equal(got, []string{"-attempted", "copilot=1.0.112", "-unfinished", "copilot=1.0.112"}) {
			t.Errorf("a run that did not end: %q", got)
		}
	}
	// A desktop job that failed, alone: its profiles unpacked into <runs> itself.
	runs = t.TempDir()
	write(t, filepath.Join(runs, "profiles.json"), `[{"harness":"codex-desktop","version":"26.1111.1"}]`)
	if got := captureArgs(t, runs, "BUILDS=codex-desktop=26.1106.1,26.1111.1", "DESKTOP_CAPTURE=failure"); !slices.Equal(got,
		[]string{"-attempted", "codex-desktop=26.1106.1,26.1111.1", "-unfinished", "codex-desktop=26.1106.1"}) {
		t.Errorf("a desktop job alone: %q", got)
	}
	// A desktop job that succeeded finished every app it ran.
	if got := captureArgs(t, t.TempDir(), "BUILDS=claude-desktop=1.2.0", "DESKTOP_CAPTURE=success"); !slices.Equal(got, []string{"-attempted", "claude-desktop=1.2.0"}) {
		t.Errorf("a desktop job that succeeded: %q", got)
	}

	// The markers capture-done.sh writes are the ones capture-args.sh reads: a run that wrote its
	// compat.json finished, one that did not, did not.
	done := func(t *testing.T, report string, args ...string) {
		t.Helper()
		if out, err := exec.Command("bash", append([]string{"reference/capture-done.sh", report}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("capture-done.sh: %v\n%s", err, out)
		}
	}
	runs = t.TempDir()
	write(t, filepath.Join(runs, "captures", "capture-claude", "compat.json"), `[]`)
	done(t, filepath.Join(runs, "captures", "capture-claude"), "macos", "claude")
	done(t, filepath.Join(runs, "captures", "compat-linux"), "linux")
	if got := captureArgs(t, runs, "BUILDS=claude=2.1.1", "CLAUDE_BUILDS=2.1.1"); !slices.Equal(got,
		[]string{"-attempted", "claude=2.1.1", "-unfinished", "claude=2.1.1"}) {
		t.Errorf("macOS finished, Linux not: %q", got)
	}
}

// reference/capture-plan.sh: the detect job's outputs from the builds to capture, one job a
// command-line harness, the desktop apps' scenarios together, and no line that is not a harness
// and its versions.
func TestCapturePlan(t *testing.T) {
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	out := filepath.Join(t.TempDir(), "outputs")
	cmd := exec.Command("bash", "reference/capture-plan.sh", out)
	cmd.Stdin = strings.NewReader("claude=2.1.1,2.1.2\nclaude-desktop=1.2.0\nt3=0.1.9\n../etc=1\ncodex=0.2;rm\ngemini=latest\ndsh=0.2.2,last1\n")
	taken, err := cmd.Output()
	if err != nil {
		t.Fatalf("capture-plan.sh: %v", err)
	}
	if got, want := strings.TrimSpace(string(taken)), "claude=2.1.1,2.1.2\nclaude-desktop=1.2.0\nt3=0.1.9"; got != want {
		t.Errorf("taken:\n%s\nwant\n%s", got, want)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	outputs := string(data)
	for _, want := range []string{
		"claude=2.1.1,2.1.2\n", "claude_desktop=1.2.0\n", "t3=0.1.9\n",
		`matrix={"include":[{"harness":"claude","versions":"2.1.1,2.1.2"},{"harness":"t3","versions":"0.1.9"}]}` + "\n",
		"desktop_tests=^(TestClaudeDesktopProfile)$\n",
		"builds<<EOF\nclaude=2.1.1,2.1.2\nclaude-desktop=1.2.0\nt3=0.1.9\nEOF\n",
	} {
		if !strings.Contains(outputs, want) {
			t.Errorf("outputs lack %q:\n%s", want, outputs)
		}
	}
	if strings.Contains(outputs, "etc") || strings.Contains(outputs, "rm") || strings.Contains(outputs, "latest") || strings.Contains(outputs, "last1") {
		t.Errorf("an input that names no harness reached the outputs:\n%s", outputs)
	}

	cmd = exec.Command("bash", "reference/capture-plan.sh", out+"-none")
	cmd.Stdin = strings.NewReader("")
	if taken, err := cmd.Output(); err != nil || len(taken) > 0 {
		t.Errorf("no builds: %q, %v", taken, err)
	}
	if _, err := os.Stat(out + "-none"); !os.IsNotExist(err) {
		t.Errorf("no builds wrote outputs: %v", err)
	}
}
