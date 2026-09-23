package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
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
	if _, _, err := PickAsset(rel, "linux", "amd64"); err == nil {
		t.Fatal("expected a missing-asset error")
	}
	a, s, err := PickAsset(rel, "linux", "arm64")
	if err != nil || a.Name != "terma_Linux_arm64.tar.gz" || s.Name != "checksums.txt" {
		t.Fatalf("unexpected pick: %v %v %v", a, s, err)
	}
	if rel.Version() != "1.2.3" {
		t.Fatal("version should strip the v")
	}
}

func TestParseChecksums(t *testing.T) {
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

func TestApplyVerifiesChecksumAndSwapsBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no self-update on windows")
	}
	newBinary := []byte("#!/bin/sh\necho new\n")
	archive := archiveWith(t, "terma", newBinary)
	sum := sha256.Sum256(archive)
	assetName := AssetName(runtime.GOOS, runtime.GOARCH)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+Repo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		host := "http://" + r.Host
		_, _ = w.Write([]byte(`{"tag_name":"v9.0.0","assets":[{"name":"checksums.txt","browser_download_url":"` + host + `/sums"},{"name":"` + assetName + `","browser_download_url":"` + host + `/archive","size":` + "123" + `}]}`))
	})
	mux.HandleFunc("/sums", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(hex.EncodeToString(sum[:]) + "  " + assetName + "\n"))
	})
	mux.HandleFunc("/archive", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	exe := filepath.Join(t.TempDir(), "terma")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL, Version: "1.0.0"}
	rel, err := c.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !Newer("1.0.0", rel.TagName) {
		t.Fatal("expected a newer release")
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
	if info.Mode()&0o111 == 0 {
		t.Fatal("replacement lost the executable bit")
	}

	// A tampered archive is refused.
	mux.HandleFunc("/archive2", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("tampered")) })
	rel.Assets[1].URL = srv.URL + "/archive2"
	if _, err := c.Apply(context.Background(), rel, exe, nil); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected a checksum error, got %v", err)
	}
	if data, _ := os.ReadFile(exe); !bytes.Equal(data, newBinary) {
		t.Fatal("a refused update must leave the binary untouched")
	}
}

func TestNoticeUsesCache(t *testing.T) {
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
	if msg := c.Notice(context.Background(), dir, "1.0.0"); !strings.Contains(msg, "2.0.0") {
		t.Fatalf("expected a notice, got %q", msg)
	}
	if msg := c.Notice(context.Background(), dir, "1.0.0"); !strings.Contains(msg, "2.0.0") || calls != 1 {
		t.Fatalf("second notice should come from the cache (calls=%d): %q", calls, msg)
	}
	if msg := c.Notice(context.Background(), dir, "2.0.0"); msg != "" {
		t.Fatalf("up to date should be silent, got %q", msg)
	}
}

// A local build can never be told it is out of date, so it must not ask. It used to:
// the lookup ran first and Newer discarded the answer, which put a live request — three
// seconds of timeout when offline — behind every command of every dev build and test.
func TestNoticeAsksNothingForABuildThatIsNotARelease(t *testing.T) {
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
		if msg := c.Notice(context.Background(), t.TempDir(), version); msg != "" {
			t.Errorf("%q: got a notice: %q", version, msg)
		}
	}
	if calls != 0 {
		t.Fatalf("a build outside the release sequence made %d request(s)", calls)
	}
}
