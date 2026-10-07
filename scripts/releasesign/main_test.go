package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// keygen writes a private key only its owner can read and prints the public key; sign
// with that key makes a signature verify accepts with that public key, and refuses with
// another. The release workflow runs exactly this, with the key in TERMA_SIGNING_KEY.
func TestKeygenSignAndVerify(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "release.key")
	out := capture(t, func() error { return run([]string{"keygen", keyFile}) })
	_, pub, ok := strings.Cut(strings.TrimSpace(out), "public key: ")
	if !ok || len(pub) != 64 {
		t.Fatalf("keygen printed %q, want the public key", out)
	}
	if info, err := os.Stat(keyFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file %v, mode %v; want owner-only", err, info.Mode())
	}
	if err := run([]string{"keygen", keyFile}); err == nil {
		t.Fatal("keygen overwrote an existing key")
	}
	seed, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, strings.TrimSpace(string(seed))) {
		t.Fatalf("keygen printed the private key: %q", out)
	}
	data := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(data, []byte("abc  terma_Linux_x86_64.tar.gz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sig := data + ".sig"
	t.Setenv("TERMA_SIGNING_KEY", "")
	if err := run([]string{"sign", "v1.0.0", data, sig}); err == nil {
		t.Fatal("signed without a key")
	}
	t.Setenv("TERMA_SIGNING_KEY", string(seed))
	if err := run([]string{"sign", "v1.0.0", data, sig}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"verify", "v1.0.0", data, sig, pub}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"verify", "v1.0.1", data, sig, pub}); err == nil {
		t.Fatal("verified under another tag")
	}
	if err := run([]string{"verify", "v1.0.0", data, sig, strings.Repeat("00", 32)}); err == nil {
		t.Fatal("verified with another key")
	}
	// Two keys, while one is rotated in: a signature from each, either enough.
	second := filepath.Join(dir, "second.key")
	out = capture(t, func() error { return run([]string{"keygen", second}) })
	_, pub2, _ := strings.Cut(strings.TrimSpace(out), "public key: ")
	seed2, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMA_SIGNING_KEY", string(seed)+"\n"+string(seed2))
	if err := run([]string{"sign", "v1.0.0", data, sig}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{pub, pub2} {
		if err := run([]string{"verify", "v1.0.0", data, sig, k}); err != nil {
			t.Fatalf("key %s: %v", k[:8], err)
		}
	}
	if err := os.WriteFile(data, []byte("abd  terma_Linux_x86_64.tar.gz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"verify", "v1.0.0", data, sig, pub}); err == nil {
		t.Fatal("verified changed data")
	}
}

// capture runs fn with stdout captured, failing the test on an error.
func capture(t *testing.T, fn func() error) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	was := os.Stdout
	os.Stdout = w
	runErr := fn()
	os.Stdout = was
	_ = w.Close()
	out, _ := readAll(r)
	if runErr != nil {
		t.Fatal(runErr)
	}
	return out
}

func readAll(f *os.File) (string, error) {
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := f.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			return b.String(), nil
		}
	}
}
