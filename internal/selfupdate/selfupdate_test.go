package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		current, candidate string
		want               bool
	}{
		{"1.0.0", "1.0.1", true},
		{"v1.0.0", "v1.1.0", true},
		{"1.2.3", "1.2.3", false},
		{"1.2.3", "1.2.2", false},
		{"1.2.3", "2.0.0-rc1", true},
		{"2.0.0-rc1", "2.0.0", true},
		{"dev", "9.9.9", false},
		{"1.2.4-next", "1.2.4", false},
	}
	for _, c := range cases {
		if got := Newer(c.current, c.candidate); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.current, c.candidate, got, c.want)
		}
	}
}

func TestAssetNaming(t *testing.T) {
	t.Parallel()
	if got := AssetName("darwin", "arm64"); got != "terma_Darwin_arm64.tar.gz" {
		t.Fatalf("got %s", got)
	}
	if got := AssetName("linux", "amd64"); got != "terma_Linux_x86_64.tar.gz" {
		t.Fatalf("got %s", got)
	}
	if got := AssetName("windows", "amd64"); got != "terma_Windows_x86_64.zip" {
		t.Fatalf("got %s", got)
	}
	rel := &Release{TagName: "v1.2.3", Assets: []Asset{{Name: "checksums.txt"}, {Name: "terma_Linux_arm64.tar.gz"}}}
	if _, _, _, err := PickAsset(rel, "linux", "arm64"); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("a release without %s = %v, want ErrUnsigned", SignatureName, err)
	}
	rel.Assets = append(rel.Assets, Asset{Name: SignatureName})
	if _, _, _, err := PickAsset(rel, "linux", "amd64"); err == nil {
		t.Fatal("expected a missing-asset error")
	}
	a, s, sig, err := PickAsset(rel, "linux", "arm64")
	if err != nil || a.Name != "terma_Linux_arm64.tar.gz" || s.Name != "checksums.txt" || sig.Name != SignatureName {
		t.Fatalf("unexpected pick: %v %v %v", a, s, err)
	}
	if rel.Version() != "1.2.3" {
		t.Fatal("version should strip the v")
	}
}

func TestParseChecksums(t *testing.T) {
	t.Parallel()
	sums := ParseChecksums(strings.NewReader("ABCD  terma_Linux_x86_64.tar.gz\nef01  terma_Darwin_arm64.tar.gz\nbad line\n"))
	if sums["terma_Linux_x86_64.tar.gz"] != "abcd" || sums["terma_Darwin_arm64.tar.gz"] != "ef01" || len(sums) != 2 {
		t.Fatalf("unexpected: %v", sums)
	}
}

func archiveWith(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func zipWith(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	_ = zw.Close()
	return buf.Bytes()
}

func TestApplyVerifiesChecksumAndSwapsBinary(t *testing.T) {
	t.Parallel()
	newBinary := []byte("#!/bin/sh\necho new\n")
	archive := archiveWith(t, "terma", newBinary)
	exeName := "terma"
	if runtime.GOOS == "windows" {
		archive = zipWith(t, "terma_Windows/terma.exe", newBinary)
		exeName = "terma.exe"
	}
	sum := sha256.Sum256(archive)
	assetName := AssetName(runtime.GOOS, runtime.GOARCH)
	sums := []byte(hex.EncodeToString(sum[:]) + "  " + assetName + "\n")

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+Repo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		host := "http://" + r.Host
		_, _ = w.Write([]byte(`{"tag_name":"v9.0.0","assets":[{"name":"checksums.txt","browser_download_url":"` + host + `/sums"},{"name":"` + SignatureName + `","browser_download_url":"` + host + `/sig"},{"name":"` + assetName + `","browser_download_url":"` + host + `/archive","size":` + "123" + `}]}`))
	})
	mux.HandleFunc("/sums", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(sums) })
	mux.HandleFunc("/sig", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(testSign(sums)) })
	// Signed by someone else: the checksums are right, and the release is still refused.
	other, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	mux.HandleFunc("/forged", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(Sign(otherPriv, sums)) })
	mux.HandleFunc("/archive", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	exe := filepath.Join(t.TempDir(), exeName)
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL, Version: "1.0.0", ReleaseKeys: testKeys()}
	rel, err := c.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !Newer("1.0.0", rel.TagName) {
		t.Fatal("expected a newer release")
	}
	// Before anything else: a forged signature, a signature from a key this terma does not
	// trust, and no signature asset at all are each refused, the binary untouched.
	signed := rel.Assets[1].URL
	rel.Assets[1].URL = srv.URL + "/forged"
	if _, err := c.Apply(context.Background(), rel, exe, nil); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("a release signed by another key: %v, want ErrUnsigned", err)
	}
	rel.Assets[1].URL = signed
	untrusting := &Client{HTTP: srv.Client(), BaseURL: srv.URL, Version: "1.0.0", ReleaseKeys: []ed25519.PublicKey{other}}
	if _, err := untrusting.Apply(context.Background(), rel, exe, nil); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("a release signed by a key this terma does not trust: %v, want ErrUnsigned", err)
	}
	unsigned := &Release{TagName: rel.TagName, Assets: rel.Assets[:1:1]}
	unsigned.Assets = append(unsigned.Assets, rel.Assets[2])
	if _, err := c.Apply(context.Background(), unsigned, exe, nil); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("a release without a signature: %v, want ErrUnsigned", err)
	}
	if data, _ := os.ReadFile(exe); string(data) != "old" {
		t.Fatalf("a refused release replaced the binary: %q", data)
	}
	got, err := c.Apply(context.Background(), rel, exe, nil)
	if err != nil || got != "9.0.0" {
		t.Fatalf("apply: %v (%s)", err, got)
	}
	data, _ := os.ReadFile(exe)
	if !bytes.Equal(data, newBinary) {
		t.Fatalf("binary not replaced: %q", data)
	}
	info, _ := os.Stat(exe)
	if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		t.Fatal("replacement lost the executable bit")
	}

	// A tampered archive is refused.
	mux.HandleFunc("/archive2", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("tampered")) })
	rel.Assets[2].URL = srv.URL + "/archive2"
	if _, err := c.Apply(context.Background(), rel, exe, nil); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected a checksum error, got %v", err)
	}
	if data, _ := os.ReadFile(exe); !bytes.Equal(data, newBinary) {
		t.Fatal("a refused update must leave the binary untouched")
	}
}

// Windows's release archive is a zip holding terma.exe; a zip without it is refused.
func TestExtractBinaryReadsWindowsZip(t *testing.T) {
	t.Parallel()
	got, err := extractBinaryFor("windows", zipWith(t, "terma_Windows_x86_64/terma.exe", []byte("exe")))
	if err != nil || string(got) != "exe" {
		t.Fatalf("extract: %q, %v", got, err)
	}
	if _, err := extractBinaryFor("windows", zipWith(t, "README.md", []byte("x"))); err == nil {
		t.Fatal("a zip without terma.exe was accepted")
	}
}

// On Windows the old executable steps aside to .old (replacing a previous one) first.
func TestSwapExecutableMovesTheRunningOneAsideOnWindows(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	exe, next := filepath.Join(dir, "terma.exe"), filepath.Join(dir, ".terma-update-1")
	_ = os.WriteFile(exe, []byte("old"), 0o755)
	_ = os.WriteFile(exe+".old", []byte("older"), 0o755)
	_ = os.WriteFile(next, []byte("new"), 0o755)
	if err := swapExecutable("windows", next, exe); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(exe); string(data) != "new" {
		t.Fatalf("terma.exe holds %q, want the new build", data)
	}
	if data, _ := os.ReadFile(exe + ".old"); string(data) != "old" {
		t.Fatalf("terma.exe.old holds %q, want the build that was running", data)
	}
}

func TestNoticeUsesCache(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+Repo+"/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"tag_name":"v2.0.0","assets":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL, Version: "1.0.0"}
	if msg := noticeOf(c, context.Background(), dir, "1.0.0"); !strings.Contains(msg, "2.0.0") {
		t.Fatalf("expected a notice, got %q", msg)
	}
	if msg := noticeOf(c, context.Background(), dir, "1.0.0"); !strings.Contains(msg, "2.0.0") || calls != 1 {
		t.Fatalf("second notice should come from the cache (calls=%d): %q", calls, msg)
	}
	if msg := noticeOf(c, context.Background(), dir, "2.0.0"); msg != "" {
		t.Fatalf("up to date should be silent, got %q", msg)
	}
}

// A local build never looks up the latest release: it can never be out of date.
func TestNoticeAsksNothingForABuildThatIsNotARelease(t *testing.T) {
	t.Parallel()
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+Repo+"/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"tag_name":"v2.0.0","assets":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}
	for _, version := range []string{"dev", "", "1.2.3-next", "v1.2.3-next"} {
		if msg := noticeOf(c, context.Background(), t.TempDir(), version); msg != "" {
			t.Errorf("%q: got a notice: %q", version, msg)
		}
	}
	if calls != 0 {
		t.Fatalf("a build outside the release sequence made %d request(s)", calls)
	}
}
