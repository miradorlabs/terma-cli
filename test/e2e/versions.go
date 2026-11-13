package e2e

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/e2e/refdiff"
)

// Versions under test. A harness changes weekly, and a collection surface that
// works on this week's build is a claim about that build only. The suite runs
// the same scenarios against the last few releases, fetched as the official
// installers fetch them: Claude Code from its download service, checksum from
// its per-version manifest; Codex from its GitHub release tarballs. Homebrew
// ships only the current build, so the brew-installed binary is the
// "installed" entry and the rest come from the distributions directly.
//
// TERMA_E2E_CLAUDE_VERSIONS / TERMA_E2E_CODEX_VERSIONS (and each harness's own):
// "installed", "lastN", "checked", or a comma list of versions; default
// "installed,last3". TERMA_E2E_ONLY limits a run to the harnesses it names
// (skipUnlessSelected). "checked" is the newest build the history at TERMA_E2E_CHECKED
// has a result of on this platform and every release since, as the nightly runs them
// (sinceChecked); it applies to harnesses that ship releases, not prereleases alone.
// Downloads are cached under test/e2e/versions/ (ignored by git).

// Binary is one harness build to test.
type Binary struct {
	Harness string
	Version string
	Path    string
	// Installed marks the binary found on the PATH rather than a download.
	Installed bool
}

// Label names the build in a subtest.
func (b Binary) Label() string {
	if b.Installed {
		return "installed-" + b.Version
	}
	return "v" + b.Version
}

func versionsDir() string {
	if d := os.Getenv("TERMA_E2E_VERSIONS_DIR"); d != "" {
		return d
	}
	return "versions"
}

// ClaudeBinaries resolves the Claude Code builds to test.
func ClaudeBinaries(t *testing.T) []Binary {
	t.Helper()
	return resolve(t, "claude", os.Getenv("TERMA_E2E_CLAUDE_VERSIONS"), claudeVersionList, ensureClaude)
}

// CodexBinaries resolves the Codex builds to test.
func CodexBinaries(t *testing.T) []Binary {
	t.Helper()
	return resolve(t, "codex", os.Getenv("TERMA_E2E_CODEX_VERSIONS"), codexVersionList, ensureCodex)
}

// selected reports whether this run takes harness's scenarios: every harness, unless
// TERMA_E2E_ONLY, a comma list of harness ids, leaves it out. The frequent check runs each
// harness's scenarios on a job of its own (reference.yml). A desktop app's checks run its
// agent's builds (TestRelayClaudeDesktop runs Claude Code), so they run with its agent's.
func selected(harness string) bool {
	only := os.Getenv("TERMA_E2E_ONLY")
	return only == "" || listed(only, harness) || listed(only, agentOf(harness))
}

// skipUnlessSelected skips a scenario of harness this run does not take (selected).
func skipUnlessSelected(t *testing.T, harness string) {
	t.Helper()
	if !selected(harness) {
		t.Skipf("%s is not among TERMA_E2E_ONLY (%s)", harness, os.Getenv("TERMA_E2E_ONLY"))
	}
}

// unrestricted clears TERMA_E2E_ONLY for t: an offline test of a harness's scenario runs
// whatever harness its run is limited to.
func unrestricted(t *testing.T) {
	t.Setenv("TERMA_E2E_ONLY", "")
}

// installEnv is the environment a vendor's package is installed with: the test's own without
// any credential (an API key, a token, a secret, an auth file), since its install scripts run as
// soon as the release is out. Only a scenario that needs a credential hands it to the build.
func installEnv() []string {
	return slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		name = strings.ToUpper(name)
		return strings.Contains(name, "PASSWORD") || slices.ContainsFunc([]string{"_KEY", "_TOKEN", "_SECRET", "_AUTH"},
			func(suffix string) bool { return strings.HasSuffix(name, suffix) })
	})
}

// listed reports whether the comma list names harness.
func listed(list, harness string) bool {
	for h := range strings.SplitSeq(list, ",") {
		if strings.TrimSpace(h) == harness {
			return true
		}
	}
	return false
}

func resolve(t *testing.T, harness, spec string, list func() ([]string, error), ensure func(string) (string, error)) []Binary {
	t.Helper()
	// Before fetching a build: a run that does not take harness's scenarios fetches none. A
	// scenario of another harness that drives this one (T3 runs Codex and Claude Code) is the
	// other's, and its selection is already made.
	if h := harnessOf(t); h == "" || h == harness {
		skipUnlessSelected(t, harness)
	}
	if spec == "" {
		spec = "installed,last3"
	}
	var out []Binary
	seen := map[string]bool{}
	add := func(b Binary) {
		if b.Version == "" || seen[b.Version] {
			return
		}
		seen[b.Version] = true
		out = append(out, b)
	}
	for part := range strings.SplitSeq(spec, ",") {
		part = strings.TrimSpace(part)
		switch {
		case part == "":
		case part == "installed":
			if path, err := exec_LookPath(harness); err == nil {
				add(Binary{Harness: harness, Version: numberOf(Version(path)), Path: path, Installed: true})
			} else {
				Note(harness+"/versions", "no installed "+harness+" on PATH")
			}
		case strings.HasPrefix(part, "last"), part == "checked":
			versions, err := list()
			if err != nil {
				t.Fatalf("list %s versions: %v", harness, err)
			}
			n := fallbackLast
			if part != "checked" {
				if n, err = strconv.Atoi(strings.TrimPrefix(part, "last")); err != nil || n <= 0 {
					t.Fatalf("bad version spec %q", part)
				}
			} else if since, why := checkedBuilds(harness, versions); why == "" {
				versions, n = since, len(since)
			} else {
				Note(harness+"/versions", why+": testing the last "+strconv.Itoa(fallbackLast)+" releases")
			}
			if len(versions) > n {
				versions = versions[len(versions)-n:]
			}
			for _, v := range versions {
				if seen[v] {
					continue // the installed build already covers this version
				}
				path, err := ensure(v)
				if err != nil {
					t.Fatalf("fetch %s %s: %v", harness, v, err)
				}
				add(Binary{Harness: harness, Version: v, Path: path})
			}
		default:
			path, err := ensure(part)
			if err != nil {
				t.Fatalf("fetch %s %s: %v", harness, part, err)
			}
			add(Binary{Harness: harness, Version: part, Path: path})
		}
	}
	// Oldest first, so a regression reads as "broke between X and Y".
	sort.Slice(out, func(i, j int) bool { return versionLess(out[i].Version, out[j].Version) })
	return out
}

// numberOf pulls the x.y.z out of a --version line.
func numberOf(line string) string {
	for f := range strings.FieldsSeq(line) {
		// "omp/18.3.0" names itself before the version.
		if _, after, ok := strings.Cut(f, "/"); ok {
			f = after
		}
		f = strings.TrimPrefix(f, "v")
		if isRelease(f) {
			return f
		}
	}
	return ""
}

// isRelease accepts x.y.z with no prerelease suffix.
func isRelease(v string) bool {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if _, err := strconv.Atoi(p); err != nil {
			return false
		}
	}
	return true
}

// versionLess orders versions as the reference ranks them (refdiff.VersionCompare, the one
// ordering of versions in this module): a prerelease before its release, every tie broken.
func versionLess(a, b string) bool { return refdiff.VersionLess(a, b) }

var httpClient = &http.Client{Timeout: 5 * time.Minute}

// fetchAttempts and fetchBackoff are how a download a server or the network failed for a
// while is tried again: release hosts answer 5xx and drop connections under load (many CI jobs
// downloading at once), and one such answer would otherwise end a whole run.
var (
	fetchAttempts = 5
	fetchBackoff  = 3 * time.Second
)

// errNotFound is a download the server says is not there (HTTP 404): an asset a release does
// not ship, not a failure to retry or to stop for.
var errNotFound = errors.New("HTTP 404")

func fetch(url string) ([]byte, error) {
	var err error
	for attempt := range fetchAttempts {
		if attempt > 0 {
			time.Sleep(fetchBackoff << (attempt - 1))
		}
		var body []byte
		var retry bool
		body, retry, err = fetchOnce(url)
		if err == nil || !retry {
			return body, err
		}
	}
	return nil, err
}

// fetchOnce downloads url once; retry says whether a failure was the server's or the
// network's for the moment (5xx, 429, a dropped connection), not the request's (404).
func fetchOnce(url string) (body []byte, retry bool, err error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, fmt.Errorf("%s: %w", url, errNotFound)
	}
	if resp.StatusCode != 200 {
		return nil, resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, 512<<20))
	return body, err != nil, err
}

// --- Claude Code -------------------------------------------------------------

const claudeDownloadBase = "https://downloads.claude.ai/claude-code-releases"

// claudeVersionList is every release, oldest first, from the npm registry,
// which carries the same version numbers as the native distribution.
func claudeVersionList() ([]string, error) { return npmReleases("@anthropic-ai/claude-code") }

func claudePlatform() string {
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x64"
	}
	return runtime.GOOS + "-" + arch
}

// ensureClaude downloads and verifies one Claude Code build the way install.sh
// does: the manifest's checksum for this platform, then the binary.
func ensureClaude(version string) (string, error) {
	dir := filepath.Join(versionsDir(), "claude", version)
	path := filepath.Join(dir, "claude")
	if _, err := os.Stat(path); err == nil {
		return abs(path), nil
	}
	manifest, err := fetch(fmt.Sprintf("%s/%s/manifest.json", claudeDownloadBase, version))
	if err != nil {
		return "", err
	}
	var m struct {
		Platforms map[string]struct {
			Checksum string `json:"checksum"`
		} `json:"platforms"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		return "", fmt.Errorf("manifest %s: %w", version, err)
	}
	platform := claudePlatform()
	want := m.Platforms[platform].Checksum
	if len(want) != 64 {
		return "", fmt.Errorf("claude %s: no %s build in manifest", version, platform)
	}
	bin, err := fetch(fmt.Sprintf("%s/%s/%s/claude", claudeDownloadBase, version, platform))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(bin)
	if hex.EncodeToString(sum[:]) != want {
		return "", fmt.Errorf("claude %s: checksum mismatch", version)
	}
	if err := writeBuild(path, bin); err != nil {
		return "", err
	}
	return abs(path), nil
}

// --- Codex -------------------------------------------------------------------

// codexVersionList is every stable release, oldest first. The list comes from
// the npm registry, which carries the same version numbers as the GitHub
// releases the binaries are fetched from and does not rate-limit anonymous
// reads the way the GitHub API does.
func codexVersionList() ([]string, error) {
	return npmReleases("@openai/codex")
}

// npmReleases lists a package's x.y.z versions, oldest first.
func npmReleases(pkg string) ([]string, error) {
	data, err := fetch("https://registry.npmjs.org/" + pkg)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Versions map[string]json.RawMessage `json:"versions"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	var out []string
	for v := range doc.Versions {
		if isRelease(v) {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return versionLess(out[i], out[j]) })
	return out, nil
}

func codexTriple() string {
	arch := map[string]string{"arm64": "aarch64", "amd64": "x86_64"}[runtime.GOARCH]
	switch runtime.GOOS {
	case "darwin":
		return arch + "-apple-darwin"
	default:
		return arch + "-unknown-linux-musl"
	}
}

// ensureCodex installs the CLI and its companion host. A main binary alone can
// answer prompts but silently lacks the tool execution needed by edit scenarios.
func ensureCodex(version string) (string, error) {
	path, err := ensureCodexExecutable(version, "codex")
	if err != nil {
		return "", err
	}
	if !versionLess(version, "0.153.3") {
		if _, err := ensureCodexExecutable(version, "codex-code-mode-host"); err != nil {
			return "", err
		}
	}
	// On Linux, the bubblewrap its sandbox runs tool commands in where the system has none,
	// beside it, as npm installs it (linux-sandbox/src/bundled_bwrap.rs). A release that ships
	// none (404) uses the system's; any other failure stops the install, or its sandbox's
	// failures would be recorded as the harness's behaviour.
	if runtime.GOOS == "linux" {
		if _, err := ensureCodexExecutable(version, "bwrap"); err != nil && !errors.Is(err, errNotFound) {
			return "", fmt.Errorf("codex %s bwrap: %w", version, err)
		}
	}
	return path, nil
}

func ensureCodexExecutable(version, name string) (string, error) {
	dir := filepath.Join(versionsDir(), "codex", version)
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err == nil {
		return abs(path), nil
	}
	triple := codexTriple()
	url := fmt.Sprintf("https://github.com/openai/codex/releases/download/rust-v%s/%s-%s.tar.gz", version, name, triple)
	data, err := fetch(url)
	if err != nil {
		return "", err
	}
	gz, err := gzip.NewReader(strings.NewReader(string(data)))
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return "", fmt.Errorf("codex %s: no binary in %s", version, url)
		}
		if err != nil {
			return "", err
		}
		if h.Typeflag != tar.TypeReg || (filepath.Base(h.Name) != name && filepath.Base(h.Name) != name+"-"+triple) {
			continue
		}
		bin, err := io.ReadAll(io.LimitReader(tr, 512<<20))
		if err != nil {
			return "", err
		}
		if err := writeBuild(path, bin); err != nil {
			return "", err
		}
		return abs(path), nil
	}
}

func abs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}

// fallbackLast is how many releases "checked" tests where no history says what was checked.
const fallbackLast = 3

// maxSinceChecked bounds the releases "checked" tests after the newest checked build: a burst
// of releases is tested whole up to it; beyond it, its newest are, and the older ones of the
// burst are never tested, the next night starting from the newest.
const maxSinceChecked = 5

// checkedBuilds is the "checked" spec for harness among its releases, oldest first: the
// builds sinceChecked picks by the history at TERMA_E2E_CHECKED, of this platform alone,
// since another (the Linux machine job, which tests the newest release every night) says
// nothing of what this one checked. why says why there are none, "" where there are.
func checkedBuilds(harness string, releases []string) (builds []string, why string) {
	return checkedBuildsOn(runtime.GOOS+"/"+runtime.GOARCH, harness, releases)
}

func checkedBuildsOn(platform, harness string, releases []string) (builds []string, why string) {
	path := os.Getenv("TERMA_E2E_CHECKED")
	if path == "" {
		return nil, "no history of checked builds (TERMA_E2E_CHECKED)"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "the history of checked builds is unreadable"
	}
	var history []CompatRow
	if err := json.Unmarshal(data, &history); err != nil {
		return nil, "the history of checked builds is unreadable"
	}
	var checked []string
	for _, e := range history {
		// A build whose every check was skipped was not checked.
		if e.Harness == harness && e.Platform == platform && (e.Result == "pass" || e.Result == "fail") {
			checked = append(checked, e.Version)
		}
	}
	builds = sinceChecked(releases, checked, maxSinceChecked)
	if len(builds) == 0 {
		return nil, "the history has checked no " + harness + " release on " + platform
	}
	return builds, ""
}

// sinceChecked picks from releases, oldest first: the newest checked build, which is the
// baseline what is new tonight is judged against and keeps terma tested on the build people
// run, and every release after it, the newest max of them. A checked build no longer
// released is no baseline; the releases after it still are. None where nothing was checked.
func sinceChecked(releases, checked []string, max int) []string {
	newest := ""
	for _, v := range checked {
		if isRelease(v) && (newest == "" || versionLess(newest, v)) {
			newest = v
		}
	}
	if newest == "" {
		return nil
	}
	var after []string
	for _, v := range releases {
		if versionLess(newest, v) {
			after = append(after, v)
		}
	}
	sort.Slice(after, func(i, j int) bool { return versionLess(after[i], after[j]) })
	if len(after) > max {
		after = after[len(after)-max:]
	}
	var out []string
	for _, v := range releases {
		if v == newest {
			out = append(out, v)
			break
		}
	}
	return append(out, after...)
}

// pathNameRE is what a harness id or a version may be where it names a file or a folder: a
// letter or digit, then letters, digits, dots, plus signs, underscores and dashes.
var pathNameRE = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]*$`)

// CheckPathName says why name, a harness id or a version (what kind says), cannot name a file
// or folder of the suite's or the published reference's, nil where it can: it is
// [0-9A-Za-z][0-9A-Za-z.+_-]* and holds no "..". A version comes from a registry or a vendor's
// update feed, and is never trusted to stay inside the folder it names a file in.
func CheckPathName(kind, name string) error {
	if !pathNameRE.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("refusing %s %q to name a file: want a letter or digit, then letters, digits and . + _ -, and no \"..\"", kind, name)
	}
	return nil
}

// ensureNpmBuild installs version of an npm package under the versions directory and returns its
// command bin, as `npm install` puts it in node_modules/.bin.
func ensureNpmBuild(harness, pkg, bin string) func(version string) (string, error) {
	return func(version string) (string, error) {
		if err := CheckPathName(harness+" version", version); err != nil {
			return "", err
		}
		dir := filepath.Join(versionsDir(), harness, version)
		path := filepath.Join(dir, "node_modules", ".bin", bin)
		if _, err := os.Stat(path); err == nil {
			return abs(path), nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "npm", "install", "--silent", "--no-audit", "--no-fund", "--prefix", dir, pkg+"@"+version)
		cmd.Env = installEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("npm install %s@%s: %v\n%s", pkg, version, err, out)
		}
		return abs(path), nil
	}
}
