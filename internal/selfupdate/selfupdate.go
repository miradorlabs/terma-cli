// Package selfupdate replaces the running binary with the latest GitHub release.
//
// Hooks are thin shims that never change; this is how the logic behind them gets
// new versions without anyone re-running install. The swap is atomic (download to
// a sibling temp file, verify the release checksum, rename over the executable) so
// a hook that fires mid-update runs either the old binary or the new one, never a
// half-written file.
package selfupdate

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Repo is the GitHub repository releases are published from.
const Repo = "miradorlabs/terma-cli"

// BinaryName is the executable inside each release archive.
const BinaryName = "terma"

// CheckInterval is how often the passive "update available" notice re-checks.
const CheckInterval = 24 * time.Hour

// RetryInterval bounds how long a failed release lookup suppresses another check.
const RetryInterval = 15 * time.Minute

// maxDownload bounds a release archive and any file read out of it. The binary is a
// few tens of megabytes; this is the ceiling that keeps a wrong or hostile asset from
// being read into memory whole.
const maxDownload = 256 << 20

// userAgentPrefix precedes the version in the User-Agent GitHub sees.
const userAgentPrefix = "terma-cli/"

// Release is the subset of the GitHub release payload the updater reads.
type Release struct {
	TagName    string  `json:"tag_name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []Asset `json:"assets"`
}

// Asset is one downloadable file of a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Version is Release's version without the leading "v".
func (r Release) Version() string { return strings.TrimPrefix(r.TagName, "v") }

// Client talks to GitHub. Zero value works; fields exist for tests.
type Client struct {
	HTTP    *http.Client
	BaseURL string // GitHub API base; defaults to https://api.github.com
	Version string // the running version, for User-Agent
}

// httpClient is the configured client, or one with a timeout: a release lookup must
// not hang a command on a network that has gone quiet.
func (c *Client) httpClient() *http.Client {
	client := &http.Client{Timeout: 30 * time.Second}
	if c.HTTP != nil {
		configured := *c.HTTP
		client = &configured
	}
	previous := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := c.checkDownloadOrigin(req.URL.String()); err != nil {
			return err
		}
		if previous != nil {
			return previous(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return client
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return "https://api.github.com"
}

// ErrNoRelease means GitHub has no accessible stable release.
var ErrNoRelease = errors.New("no published release is available")

// Latest fetches the newest stable release.
func (c *Client) Latest(ctx context.Context) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+"/repos/"+Repo+"/releases/latest", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgentPrefix+c.Version)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoRelease
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github releases: HTTP %d", resp.StatusCode)
	}
	var rel Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, err
	}
	if !IsRelease(rel.TagName) || rel.Draft || rel.Prerelease {
		return nil, errors.New("github releases: latest is not a valid published release")
	}
	return &rel, nil
}

// AssetName is the archive goreleaser publishes for a platform, matching the
// name_template in .goreleaser.yaml (terma_Darwin_arm64.tar.gz, terma_Linux_x86_64.tar.gz).
func AssetName(goos, goarch string) string {
	arch := goarch
	if goarch == "amd64" {
		arch = "x86_64"
	}
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return BinaryName + "_" + strings.ToUpper(goos[:1]) + goos[1:] + "_" + arch + ext
}

// PickAsset finds the archive and the checksums file for this platform.
func PickAsset(rel *Release, goos, goarch string) (archive, checksums *Asset, err error) {
	want := AssetName(goos, goarch)
	for i := range rel.Assets {
		switch rel.Assets[i].Name {
		case want:
			archive = &rel.Assets[i]
		case "checksums.txt":
			checksums = &rel.Assets[i]
		}
	}
	if archive == nil {
		return nil, nil, fmt.Errorf("release %s has no asset for %s/%s (%s)", rel.TagName, goos, goarch, want)
	}
	if checksums == nil {
		return nil, nil, fmt.Errorf("release %s has no checksums.txt", rel.TagName)
	}
	return archive, checksums, nil
}

// ParseChecksums reads goreleaser's checksums.txt ("<sha256>  <file>" per line).
// Lines that do not carry a hex digest are ignored.
func ParseChecksums(r io.Reader) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		digest := strings.ToLower(fields[0])
		if _, err := hex.DecodeString(digest); err != nil {
			continue
		}
		out[fields[1]] = digest
	}
	return out
}

// Apply downloads the archive, verifies it against checksums, and swaps the
// binary at exePath. Returns the installed version.
func (c *Client) Apply(ctx context.Context, rel *Release, exePath string, out io.Writer) (string, error) {
	archive, sums, err := PickAsset(rel, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		return "", errors.New("self-update is not supported on Windows yet — download the release from https://github.com/" + Repo + "/releases")
	}
	sumsBody, err := c.get(ctx, sums.URL)
	if err != nil {
		return "", err
	}
	want := ParseChecksums(bytes.NewReader(sumsBody))[archive.Name]
	if want == "" {
		return "", fmt.Errorf("checksums.txt has no entry for %s", archive.Name)
	}
	if out != nil {
		fmt.Fprintf(out, "Downloading %s (%d KB)...\n", archive.Name, archive.Size/1024)
	}
	data, err := c.get(ctx, archive.URL)
	if err != nil {
		return "", err
	}
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != want {
		return "", fmt.Errorf("checksum mismatch for %s — refusing to install", archive.Name)
	}
	binary, err := extractBinary(data)
	if err != nil {
		return "", err
	}
	if err := replaceExecutable(exePath, binary); err != nil {
		return "", err
	}
	return rel.Version(), nil
}

func (c *Client) get(ctx context.Context, target string) ([]byte, error) {
	if err := c.checkDownloadOrigin(target); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgentPrefix+c.Version)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d", target, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDownload))
}

// checkDownloadOrigin refuses an asset URL that does not come from GitHub.
//
// Both the archive and checksums.txt are fetched from the browser_download_url in the
// release payload — a value this code does not choose. If that payload is ever
// controlled (a compromised release, a proxy in front of the API, a BaseURL pointed
// somewhere unexpected), an arbitrary URL would be fetched and its bytes considered
// for installation as the running binary. The checksum is no defence there: it comes
// from the same payload, so an attacker who supplies the archive supplies its hash.
// Pinning the origin means the release metadata can only ever redirect the download
// within GitHub, which is the trust anchor this updater already relies on.
//
// An explicit BaseURL (tests, and a GitHub Enterprise host) is honoured as an origin
// too: it was configured by whoever built this client, not named by a release.
func (c *Client) checkDownloadOrigin(target string) error {
	u, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("invalid download URL %q: %w", target, err)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid download URL %q: missing host", target)
	}
	if u.Scheme == "https" && isGitHubHost(u.Hostname()) {
		return nil
	}
	if c.BaseURL != "" {
		if base, err := url.Parse(c.base()); err == nil && base.Host != "" &&
			u.Scheme == base.Scheme && u.Host == base.Host {
			return nil
		}
	}
	return fmt.Errorf(
		"refusing to download a release asset from %q: release downloads must come from GitHub over https", target)
}

func isGitHubHost(host string) bool {
	for _, domain := range []string{"github.com", "githubusercontent.com"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

// extractBinary pulls the terma executable out of a tar.gz release archive.
func extractBinary(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeReg && filepath.Base(hdr.Name) == BinaryName {
			return io.ReadAll(io.LimitReader(tr, maxDownload))
		}
	}
	return nil, errors.New("archive does not contain the terma binary")
}

// replaceExecutable writes the new binary beside the old one and renames it into
// place, so the swap is atomic for any hook that fires during it.
func replaceExecutable(exePath string, binary []byte) error {
	resolved, err := filepath.EvalSymlinks(exePath)
	if err == nil {
		exePath = resolved
	}
	info, err := os.Stat(exePath)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(exePath), "."+BinaryName+"-update-*")
	if err != nil {
		return fmt.Errorf("cannot write next to %s (try with sudo, or reinstall with the install script): %w", exePath, err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(binary); err != nil {
		tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Chmod(info.Mode().Perm() | 0o111); err != nil {
		tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, exePath); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}
