package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/selfupdate"
	"github.com/miradorlabs/terma-cli/internal/shim"
)

// sandboxMachine keeps a refresh away from the developer's real shims, status line and
// OpenCode plugin.
func sandboxMachine(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
}

// A refresh brings the repository's committed hooks up to this build from what its
// binding records, never brings back a file someone removed, and leaves the binding as
// it was.
func TestRefreshUpdatesTheRepositoryFromItsBinding(t *testing.T) {
	repo := installRepo(t)
	sandboxMachine(t)
	if out, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude", "--yes", "--no-doctor"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	binding, _ := os.ReadFile(filepath.Join(repo, ".terma", "settings.json"))

	// What an earlier build wrote: no SubagentStop hook yet.
	settings := filepath.Join(repo, ".claude", "settings.json")
	var doc map[string]map[string]json.RawMessage
	data, _ := os.ReadFile(settings)
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc["hooks"], "SubagentStop")
	stale, _ := json.MarshalIndent(doc, "", "  ")
	if err := os.WriteFile(settings, stale, 0o644); err != nil {
		t.Fatal(err)
	}
	// And a hook file somebody deleted.
	postCommit := filepath.Join(repo, ".terma", "hooks", "post-commit")
	if err := os.Remove(postCommit); err != nil {
		t.Fatal(err)
	}

	out, err := runTerma(t, "update", "--refresh")
	if err != nil {
		t.Fatalf("refresh: %v\n%s", err, out)
	}
	if !strings.Contains(out, "updated .claude/settings.json") || !strings.Contains(out, "git add .claude/settings.json") {
		t.Fatalf("refresh does not report the committed file:\n%s", out)
	}
	if data, _ := os.ReadFile(settings); !strings.Contains(string(data), `"SubagentStop"`) {
		t.Fatalf("the Claude hooks were not refreshed:\n%s", data)
	}
	if _, err := os.Stat(postCommit); !os.IsNotExist(err) {
		t.Fatalf("a removed hook file was brought back (stat err = %v)", err)
	}
	if after, _ := os.ReadFile(filepath.Join(repo, ".terma", "settings.json")); !bytes.Equal(after, binding) {
		t.Fatalf("the binding changed:\n%s", after)
	}

	if out, err := runTerma(t, "update", "--refresh"); err != nil || !strings.Contains(out, "Nothing to refresh") {
		t.Fatalf("second refresh: %v\n%s", err, out)
	}
}

// Outside a repository a refresh still updates the machine's files, and says where the
// committed ones get theirs.
func TestRefreshOutsideARepositoryRefreshesTheMachine(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	sandboxMachine(t)
	t.Chdir(t.TempDir())
	binDir, err := shim.InstallShims([]string{shim.AgentClaude})
	if err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(binDir, shim.AgentClaude)
	current, _ := os.ReadFile(claude)
	if err := os.WriteFile(claude, append(bytes.Clone(current), "# an earlier build\n"...), 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := runTerma(t, "update", "--refresh")
	if err != nil {
		t.Fatalf("refresh: %v\n%s", err, out)
	}
	if !strings.Contains(out, "updated "+claude) || !strings.Contains(out, "inside each repository") {
		t.Fatalf("output:\n%s", out)
	}
	if got, _ := os.ReadFile(claude); !bytes.Equal(got, current) {
		t.Fatalf("shim not refreshed:\n%s", got)
	}
}

func TestRefreshIsExclusiveWithTheOtherModes(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	for _, other := range []string{"--check", "--force", "--auto=on"} {
		if _, err := runTerma(t, "update", "--refresh", other); err == nil {
			t.Fatalf("accepted --refresh with %s", other)
		}
	}
}

// recordSteps replaces the programs an update runs, recording each command line and
// failing the ones fail names.
func recordSteps(t *testing.T, fail string) *[][]string {
	t.Helper()
	var steps [][]string
	original := runUpdateStep
	runUpdateStep = func(_ context.Context, _ io.Writer, argv ...string) error {
		steps = append(steps, argv)
		if filepath.Base(argv[0]) == fail {
			return errors.New("exit status 1")
		}
		return nil
	}
	t.Cleanup(func() { runUpdateStep = original })
	return &steps
}

func latestRelease(t *testing.T, tag string) *selfupdate.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"` + tag + `","assets":[]}`))
	}))
	t.Cleanup(srv.Close)
	return &selfupdate.Client{BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0"}
}

// A Homebrew installation is upgraded by its own brew, then the upgraded binary — found
// through the link Homebrew moves — finishes the update.
func TestUpdateUpgradesThroughThePackageManager(t *testing.T) {
	prefix, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(prefix, "Caskroom", "terma", "1.0.0", "terma")
	brew := filepath.Join(prefix, "bin", "brew")
	for _, p := range []string{exe, brew} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	steps := recordSteps(t, "")
	var out bytes.Buffer
	if err := runUpdate(context.Background(), latestRelease(t, "v2.0.0"), t.TempDir(), exe, &out, false, false); err != nil {
		t.Fatalf("update: %v\n%s", err, &out)
	}
	want := [][]string{{brew, "upgrade", "--cask", "terma"}, {filepath.Join(prefix, "bin", "terma"), "update", "--refresh"}}
	if !slices.EqualFunc(*steps, want, slices.Equal) {
		t.Fatalf("ran %q, want %q", *steps, want)
	}

	// A failed upgrade names the command, and nothing refreshes.
	steps = recordSteps(t, "brew")
	err = runUpdate(context.Background(), latestRelease(t, "v2.0.0"), t.TempDir(), exe, &out, false, false)
	if err == nil || !strings.Contains(err.Error(), "brew upgrade terma") || len(*steps) != 1 {
		t.Fatalf("failed upgrade: %v, ran %q", err, *steps)
	}

	// Without its brew, the developer is told what to run and nothing runs.
	if err := os.Remove(brew); err != nil {
		t.Fatal(err)
	}
	steps = recordSteps(t, "")
	err = runUpdate(context.Background(), latestRelease(t, "v2.0.0"), t.TempDir(), exe, &out, false, false)
	if err == nil || !strings.Contains(err.Error(), "brew upgrade terma") || len(*steps) != 0 {
		t.Fatalf("no brew: %v, ran %q", err, *steps)
	}
}

// After replacing itself, the old binary hands the refresh to the new one; a refresh
// that fails leaves the update in place and says how to retry.
func TestUpdateRefreshesWithTheReplacedBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no self-update on windows")
	}
	archive := tarGzWith(t, "terma", []byte("#!/bin/sh\necho new\n"))
	sum := sha256.Sum256(archive)
	asset := selfupdate.AssetName(runtime.GOOS, runtime.GOARCH)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+selfupdate.Repo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		host := "http://" + r.Host
		_, _ = w.Write([]byte(`{"tag_name":"v2.0.0","assets":[{"name":"checksums.txt","browser_download_url":"` + host + `/sums"},{"name":"` + asset + `","browser_download_url":"` + host + `/archive"}]}`))
	})
	mux.HandleFunc("/sums", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(hex.EncodeToString(sum[:]) + "  " + asset + "\n"))
	})
	mux.HandleFunc("/archive", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for _, fail := range []string{"", "terma"} {
		exe := filepath.Join(t.TempDir(), "terma")
		if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
			t.Fatal(err)
		}
		steps := recordSteps(t, fail)
		var out bytes.Buffer
		client := &selfupdate.Client{BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0"}
		if err := runUpdate(context.Background(), client, t.TempDir(), exe, &out, false, false); err != nil {
			t.Fatalf("update: %v\n%s", err, &out)
		}
		if want := [][]string{{exe, "update", "--refresh"}}; !slices.EqualFunc(*steps, want, slices.Equal) {
			t.Fatalf("ran %q, want %q", *steps, want)
		}
		if retry := strings.Contains(out.String(), "`terma update --refresh` to retry"); retry != (fail != "") {
			t.Fatalf("refresh failing=%v, output:\n%s", fail != "", &out)
		}
	}
}

func tarGzWith(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The first interactive command under a newer release refreshes the machine's files
// once, and points at the repository's committed ones instead of rewriting them.
func TestRefreshAfterUpgradeRunsOncePerRelease(t *testing.T) {
	repo := installRepo(t)
	sandboxMachine(t)
	if out, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude", "--yes", "--no-doctor"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	settings := filepath.Join(repo, ".claude", "settings.json")
	data, _ := os.ReadFile(settings)
	stale := bytes.Replace(data, []byte(`"SubagentStop"`), []byte(`"SubagentStopOld"`), 1)
	if err := os.WriteFile(settings, stale, 0o644); err != nil {
		t.Fatal(err)
	}
	binDir, err := shim.InstallShims([]string{shim.AgentCodex})
	if err != nil {
		t.Fatal(err)
	}
	codex := filepath.Join(binDir, shim.AgentCodex)
	current, _ := os.ReadFile(codex)
	if err := os.WriteFile(codex, append(bytes.Clone(current), "# an earlier build\n"...), 0o755); err != nil {
		t.Fatal(err)
	}

	original := Version
	Version = "9.9.9"
	t.Cleanup(func() { Version = original })
	dir := os.Getenv("TERMA_CONFIG_DIR")
	var out bytes.Buffer
	refreshAfterUpgrade(context.Background(), dir, &out)
	if !strings.Contains(out.String(), "refreshed 1 file(s)") || !strings.Contains(out.String(), "`terma update --refresh` here") {
		t.Fatalf("output:\n%s", &out)
	}
	if got, _ := os.ReadFile(codex); !bytes.Equal(got, current) {
		t.Fatal("shim not refreshed")
	}
	if got, _ := os.ReadFile(settings); !bytes.Equal(got, stale) {
		t.Fatal("an automatic refresh rewrote a committed file")
	}

	out.Reset()
	refreshAfterUpgrade(context.Background(), dir, &out)
	if out.Len() != 0 {
		t.Fatalf("second run under the same release was not silent:\n%s", &out)
	}
}
