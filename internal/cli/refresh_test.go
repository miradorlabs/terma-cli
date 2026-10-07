package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
)

// sandboxMachine keeps a refresh away from the developer's real home-directory files.
func sandboxMachine(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig")) // no setting of the developer's own
}

// Outside a repository a refresh still updates the machine's files.
func TestRefreshOutsideARepositoryRefreshesTheMachine(t *testing.T) {
	useConfigDir(t, t.TempDir())
	sandboxMachine(t)
	t.Chdir(t.TempDir())
	statusLine := plantStaleStatusLine(t)

	out, err := runTerma(t, "update", "--refresh")
	if err != nil {
		t.Fatalf("refresh: %v\n%s", err, out)
	}
	if !strings.Contains(out, statusLine) {
		t.Fatalf("output:\n%s", out)
	}
	requireRefreshed(t, statusLine)
}

func TestRefreshIsExclusiveWithTheOtherModes(t *testing.T) {
	useConfigDir(t, t.TempDir())
	for _, other := range []string{"--check", "--force", "--auto=on"} {
		if _, err := runTerma(t, "update", "--refresh", other); err == nil {
			t.Fatalf("accepted --refresh with %s", other)
		}
	}
}

// recordSteps replaces the programs an update runs, recording each and failing fail.
func recordSteps(t *testing.T, fail string) *[][]string {
	t.Helper()
	var steps [][]string
	original := testApp.runUpdateStep
	testApp.runUpdateStep = func(_ context.Context, _ io.Writer, argv ...string) error {
		steps = append(steps, argv)
		if filepath.Base(argv[0]) == fail {
			return errors.New("exit status 1")
		}
		return nil
	}
	t.Cleanup(func() { testApp.runUpdateStep = original })
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

// A Homebrew installation is upgraded by its brew, then the upgraded binary finishes.
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
	if _, err := testApp.runUpdate(context.Background(), latestRelease(t, "v2.0.0"), t.TempDir(), exe, &out, false, false); err != nil {
		t.Fatalf("update: %v\n%s", err, &out)
	}
	want := [][]string{{brew, "upgrade", "--cask", "terma"}, {filepath.Join(prefix, "bin", "terma"), "update", "--refresh"}}
	if !slices.EqualFunc(*steps, want, slices.Equal) {
		t.Fatalf("ran %q, want %q", *steps, want)
	}

	// A failed upgrade names the command, and nothing refreshes.
	steps = recordSteps(t, "brew")
	_, err = testApp.runUpdate(context.Background(), latestRelease(t, "v2.0.0"), t.TempDir(), exe, &out, false, false)
	if err == nil || !strings.Contains(err.Error(), "brew upgrade terma") || len(*steps) != 1 {
		t.Fatalf("failed upgrade: %v, ran %q", err, *steps)
	}

	// Without its brew, the developer is told what to run and nothing runs.
	if err := os.Remove(brew); err != nil {
		t.Fatal(err)
	}
	steps = recordSteps(t, "")
	_, err = testApp.runUpdate(context.Background(), latestRelease(t, "v2.0.0"), t.TempDir(), exe, &out, false, false)
	if err == nil || !strings.Contains(err.Error(), "brew upgrade terma") || len(*steps) != 0 {
		t.Fatalf("no brew: %v, ran %q", err, *steps)
	}
}

// A project's own npm dependency is never upgraded from here; the developer is sent there.
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
	_, err = testApp.runUpdate(context.Background(), latestRelease(t, "v2.0.0"), t.TempDir(), exe, &out, false, false)
	if err == nil || !strings.Contains(err.Error(), project) || !strings.Contains(err.Error(), "`npm install @miradorlabs/terma@latest`") || strings.Contains(err.Error(), " -g ") || len(*steps) != 0 {
		t.Fatalf("project dependency: %v, ran %q", err, *steps)
	}
}

// The old binary hands the refresh to the new one; a failed refresh says how to retry.
func TestUpdateRefreshesWithTheReplacedBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no self-update on windows")
	}
	archive := tarGzWith(t, "terma", []byte("#!/bin/sh\necho new\n"))
	sum := sha256.Sum256(archive)
	asset := selfupdate.AssetName(runtime.GOOS, runtime.GOARCH)
	sums := []byte(hex.EncodeToString(sum[:]) + "  " + asset + "\n")
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+selfupdate.Repo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		host := "http://" + r.Host
		_, _ = w.Write([]byte(`{"tag_name":"v2.0.0","assets":[{"name":"checksums.txt","browser_download_url":"` + host + `/sums"},{"name":"` + selfupdate.SignatureName + `","browser_download_url":"` + host + `/sig"},{"name":"` + asset + `","browser_download_url":"` + host + `/archive"}]}`))
	})
	mux.HandleFunc("/sums", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(sums) })
	mux.HandleFunc("/sig", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(selfupdate.Sign(priv, sums)) })
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
		client := &selfupdate.Client{BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0", ReleaseKeys: []ed25519.PublicKey{pub}}
		if _, err := testApp.runUpdate(context.Background(), client, t.TempDir(), exe, &out, false, false); err != nil {
			t.Fatalf("update: %v\n%s", err, &out)
		}
		if want := [][]string{{exe, "update", "--refresh"}}; !slices.EqualFunc(*steps, want, slices.Equal) {
			t.Fatalf("ran %q, want %q", *steps, want)
		}
		if retry := strings.Contains(out.String(), "`terma update` to retry"); retry != (fail != "") {
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

// The first interactive command under a newer release refreshes the machine once.
func TestRefreshAfterUpgradeRunsOncePerRelease(t *testing.T) {
	gitRepo(t)
	sandboxMachine(t)
	statusLine := plantStaleStatusLine(t)

	original := testApp.version
	testApp.version = "9.9.9"
	t.Cleanup(func() { testApp.version = original })
	var out bytes.Buffer
	testApp.refreshAfterUpgrade(context.Background(), &out)
	if !strings.Contains(out.String(), "refreshed 1 file(s)") {
		t.Fatalf("output:\n%s", &out)
	}
	requireRefreshed(t, statusLine)

	out.Reset()
	testApp.refreshAfterUpgrade(context.Background(), &out)
	if out.Len() != 0 {
		t.Fatalf("second run under the same release was not silent:\n%s", &out)
	}
}

// plantStaleStatusLine writes an earlier build's status-line wrap and returns its file.
func plantStaleStatusLine(t *testing.T) string {
	t.Helper()
	c := claudeHarness(t)
	if _, err := c.InstallStatusLine(); err != nil {
		t.Fatal(err)
	}
	path, err := c.ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"statusLine":{"type":"command","command":"exec terma hook statusline"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func requireRefreshed(t *testing.T, path string) {
	t.Helper()
	if data, _ := os.ReadFile(path); strings.Contains(string(data), `"exec terma hook statusline"`) {
		t.Fatalf("the stale status line was not refreshed:\n%s", data)
	}
}

// The refresh a newer release runs after an upgrade takes over the relay an earlier release
// runs at once, rather than at the next agent hook.
func TestARefreshTakesOverARelayOfAnEarlierRelease(t *testing.T) {
	useConfigDir(t, t.TempDir())
	sandboxMachine(t)
	t.Chdir(t.TempDir())
	dir, err := daemon.Dir(testApp.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claim.TokenPath(testApp.stateDir), []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	done := make(chan daemon.Result, 1)
	go func() {
		res, err := daemon.Run(ctx, daemon.Config{StateDir: testApp.stateDir, Addr: "127.0.0.1:0", Idle: time.Hour, Version: "1.2.0",
			Engine: relay.Options{Dir: filepath.Join(dir, relay.OutboxDir)}})
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	awaitRelayRecord(t, dir, 15*time.Second, done)

	original := testApp.version
	testApp.version = "1.3.0"
	t.Cleanup(func() { testApp.version = original })
	if out, err := runTerma(t, "update", "--refresh"); err != nil {
		t.Fatalf("refresh: %v\n%s", err, out)
	}
	select {
	case res := <-done:
		if !res.Replaced {
			t.Fatalf("Run = %+v, want Replaced", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the relay of the earlier release kept running after the refresh")
	}
}
