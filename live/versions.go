package live

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Versions under test. A harness changes weekly, and a collection surface that
// works on this week's build is a claim about that build only. The suite runs
// the same scenarios against the last few releases, fetched as the official
// installers fetch them: Claude Code from its download service, checksum from
// its per-version manifest; Codex from its GitHub release tarballs. Homebrew
// ships only the current build, so the brew-installed binary is the
// "installed" entry and the rest come from the distributions directly.
//
// TERMA_LIVE_CLAUDE_VERSIONS / TERMA_LIVE_CODEX_VERSIONS: "installed", "lastN",
// or a comma list of versions; default "installed,last3". Downloads are cached
// under live/versions/ (ignored by git).

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
	if d := os.Getenv("TERMA_LIVE_VERSIONS_DIR"); d != "" {
		return d
	}
	return "versions"
}

// ClaudeBinaries resolves the Claude Code builds to test.
func ClaudeBinaries(t *testing.T) []Binary {
	t.Helper()
	return resolve(t, "claude", os.Getenv("TERMA_LIVE_CLAUDE_VERSIONS"), claudeVersionList, ensureClaude)
}

// CodexBinaries resolves the Codex builds to test.
func CodexBinaries(t *testing.T) []Binary {
	t.Helper()
	return resolve(t, "codex", os.Getenv("TERMA_LIVE_CODEX_VERSIONS"), codexVersionList, ensureCodex)
}

func resolve(t *testing.T, harness, spec string, list func() ([]string, error), ensure func(string) (string, error)) []Binary {
	t.Helper()
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
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		switch {
		case part == "":
		case part == "installed":
			if path, err := exec_LookPath(harness); err == nil {
				add(Binary{Harness: harness, Version: numberOf(Version(path)), Path: path, Installed: true})
			} else {
				Note(harness+"/versions", "no installed "+harness+" on PATH")
			}
		case strings.HasPrefix(part, "last"):
			n, err := strconv.Atoi(strings.TrimPrefix(part, "last"))
			if err != nil || n <= 0 {
				t.Fatalf("bad version spec %q", part)
			}
			versions, err := list()
			if err != nil {
				t.Fatalf("list %s versions: %v", harness, err)
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
	for _, f := range strings.Fields(line) {
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

func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3 && i < len(pa) && i < len(pb); i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x < y
		}
	}
	return a < b
}

var httpClient = &http.Client{Timeout: 5 * time.Minute}

func fetch(url string) ([]byte, error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 512<<20))
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, bin, 0o755); err != nil {
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
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(path, bin, 0o755); err != nil {
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
