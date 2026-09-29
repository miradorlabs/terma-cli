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

	termaproject "github.com/miradorlabs/terma-cli/internal/project"
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
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("ZDOTDIR", "")
}

// leaveOldShims leaves what per-repository routing installed on a machine: a PATH shim,
// and the routing record beside it. It returns the shim directory.
func leaveOldShims(t *testing.T) string {
	t.Helper()
	binDir, err := shim.ShimBinDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, shim.AgentCodex), []byte("#!/bin/sh\n# terma per-repo routing shim for codex.\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := shim.SaveRecord(shim.Record{ProjectID: testProjectID, Harnesses: []string{shim.AgentCodex}, CLI: true}); err != nil {
		t.Fatal(err)
	}
	return binDir
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
	// What an earlier build wrote: its own version in the binding…
	bound, err := termaproject.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	bound.Install.Version = "v0.0.1"
	if err := termaproject.Save(repo, bound); err != nil {
		t.Fatal(err)
	}

	// …and no SubagentStop hook yet.
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
	// The binding now names the terma that last wrote the committed files, and says
	// nothing else new: the rest of it is the onboarder's.
	after, err := termaproject.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if after.Install.Version != Version || !strings.Contains(out, "git add .claude/settings.json "+termaproject.FileName) {
		t.Fatalf("refresh should stamp terma_version %q and list the binding: %q\n%s", Version, after.Install.Version, out)
	}
	after.Install.Version = bound.Install.Version
	if after.Project != bound.Project || !after.Install.InstalledAt.Equal(bound.Install.InstalledAt) || after.Install.HookManager != bound.Install.HookManager {
		t.Fatalf("the binding changed beyond terma_version:\n%+v\nwas\n%+v", after, bound)
	}

	if out, err := runTerma(t, "update", "--refresh"); err != nil || !strings.Contains(out, "Nothing to refresh") {
		t.Fatalf("second refresh: %v\n%s", err, out)
	}
}

// Outside a repository a refresh still updates the machine — here, taking away the
// per-repository routing an earlier terma installed — and says where the committed
// files get theirs.
func TestRefreshOutsideARepositoryRefreshesTheMachine(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	sandboxMachine(t)
	t.Chdir(t.TempDir())
	binDir := leaveOldShims(t)

	out, err := runTerma(t, "update", "--refresh")
	if err != nil {
		t.Fatalf("refresh: %v\n%s", err, out)
	}
	if !strings.Contains(out, "per-repository routing (removed)") || !strings.Contains(out, "inside each repository") {
		t.Fatalf("output:\n%s", out)
	}
	if _, err := os.Stat(binDir); !os.IsNotExist(err) {
		t.Fatalf("the shims survived a refresh: %v", err)
	}
}

// A refresh restarts terma's relay, so the build it just installed is the one serving;
// with no relay service there is nothing to restart.
func TestRefreshRestartsTheRelay(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	sandboxMachine(t)
	t.Chdir(t.TempDir())
	r := fakeRelay(t)
	if out, err := runTerma(t, "update", "--refresh"); err != nil || r.restarts != 0 {
		t.Fatalf("no relay installed: restarts=%d err=%v\n%s", r.restarts, err, out)
	}
	r.installed = "/usr/local/bin/terma"
	if out, err := runTerma(t, "update", "--refresh"); err != nil || r.restarts != 1 {
		t.Fatalf("relay installed: restarts=%d err=%v\n%s", r.restarts, err, out)
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

// A project's own npm dependency is never upgraded from here, and the developer is told
// to update it in that project — not to make an unrelated global install.
func TestUpdateSendsAProjectDependencyToItsProject(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, "app")
	exe := filepath.Join(project, "node_modules", "@miradorlabs", "terma", "vendor", "terma")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	steps := recordSteps(t, "")
	var out bytes.Buffer
	err = runUpdate(context.Background(), latestRelease(t, "v2.0.0"), t.TempDir(), exe, &out, false, false)
	if err == nil || !strings.Contains(err.Error(), project) || !strings.Contains(err.Error(), "`npm install @miradorlabs/terma@latest`") || strings.Contains(err.Error(), " -g ") || len(*steps) != 0 {
		t.Fatalf("project dependency: %v, ran %q", err, *steps)
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
	binDir := leaveOldShims(t)

	original := Version
	Version = "9.9.9"
	t.Cleanup(func() { Version = original })
	dir := os.Getenv("TERMA_CONFIG_DIR")
	var out bytes.Buffer
	refreshAfterUpgrade(context.Background(), dir, &out)
	if !strings.Contains(out.String(), "refreshed 1 file(s)") || !strings.Contains(out.String(), "`terma update --refresh` here") {
		t.Fatalf("output:\n%s", &out)
	}
	if _, err := os.Stat(binDir); !os.IsNotExist(err) {
		t.Fatalf("the old shims survived: %v", err)
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

// The first install under a newer release refreshes what earlier versions wrote on the
// machine before it verifies anything and records it — so the refresh that follows an
// interactive command has nothing left to do. The old per-repository routing is one of
// those things, and install says it took it away.
func TestInstallRefreshesTheMachineOnANewRelease(t *testing.T) {
	installRepo(t)
	sandboxMachine(t)
	install := func() string {
		t.Helper()
		out, err := runTerma(t, "install", "--harness", "none", "--project", testProjectID, "--adapters", "claude", "--yes", "--no-doctor", "--verbose")
		if err != nil {
			t.Fatalf("install: %v\n%s", err, out)
		}
		return out
	}
	install()
	binDir := leaveOldShims(t)

	original := Version
	Version = "9.9.9"
	t.Cleanup(func() { Version = original })
	if out := install(); !strings.Contains(out, "removed the old per-repository routing") {
		t.Fatalf("install should take the old routing away as a step:\n%s", out)
	}
	if _, err := os.Stat(binDir); !os.IsNotExist(err) {
		t.Fatalf("the old shims survived: %v", err)
	}
	var after bytes.Buffer
	refreshAfterUpgrade(context.Background(), os.Getenv("TERMA_CONFIG_DIR"), &after)
	if after.Len() != 0 {
		t.Fatalf("the refresh after the command ran again:\n%s", &after)
	}
	if out := install(); strings.Contains(out, "Refreshed") {
		t.Fatalf("a second install under the same release refreshed again:\n%s", out)
	}
}
