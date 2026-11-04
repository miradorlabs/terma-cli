package e2e

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Code signatures. On macOS each downloaded build's signature is read before it first runs:
// who signed it (the Developer ID and team), with which certificate, and what Gatekeeper says
// of it now, accepted, notarized, or rejected because the certificate was revoked. A build
// whose certificate was revoked is killed at launch (Codex's builds signed before OpenAI's
// certificate was revoked), so it is re-signed ad hoc to run here, which erases what it was
// signed with: that is why it is read first, and kept beside the build
// (<binary>.signing.json). RecordSigning puts it in the build's profile.

// Signing is what a build's code signature said when it was checked.
type Signing struct {
	// Format is "developer-id", "adhoc", "unsigned", or "other" (signed by another authority).
	Format          string    `json:"format"`
	Authority       []string  `json:"authority,omitempty"`
	TeamID          string    `json:"team_id,omitempty"`
	Identifier      string    `json:"identifier,omitempty"`
	SignedAt        string    `json:"signed_at,omitempty"` // the signature's secure timestamp
	HardenedRuntime bool      `json:"hardened_runtime,omitempty"`
	Certificate     *CertInfo `json:"certificate,omitempty"`
	// Status is Gatekeeper's verdict when checked: "valid", "revoked", "rejected" or
	// "unsigned"; Assessment its words.
	Status     string    `json:"status"`
	Notarized  bool      `json:"notarized,omitempty"`
	Assessment string    `json:"assessment,omitempty"`
	CheckedAt  time.Time `json:"checked_at"`
	// Resigned marks a build re-signed ad hoc so that it would run here.
	Resigned bool `json:"resigned,omitempty"`
}

// CertInfo is the signing certificate: the leaf of the signature's chain.
type CertInfo struct {
	Subject   string    `json:"subject"`
	Serial    string    `json:"serial"`
	SHA256    string    `json:"sha256"`
	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`
}

// signingSidecar is where a build's signing is kept.
func signingSidecar(path string) string { return path + ".signing.json" }

// machO reports a Mach-O executable (thin or universal): a script or a JavaScript bundle has
// no code signature of its own.
func machO(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [4]byte
	if _, err := f.Read(magic[:]); err != nil {
		return false
	}
	switch hex.EncodeToString(magic[:]) {
	case "cffaedfe", "cefaedfe", "feedfacf", "feedface", "cafebabe", "bebafeca":
		return true
	}
	return false
}

// inspectSigning reads path's signature and asks Gatekeeper of it, running nothing of it.
func inspectSigning(path string) (*Signing, error) {
	if runtime.GOOS != "darwin" || !machO(path) {
		return nil, nil
	}
	if _, err := exec.LookPath("codesign"); err != nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s := &Signing{CheckedAt: time.Now().UTC()}
	out, err := exec.CommandContext(ctx, "codesign", "-dvvv", path).CombinedOutput()
	text := string(out)
	switch {
	case strings.Contains(text, "not signed at all"):
		s.Format, s.Status = "unsigned", "unsigned"
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("codesign %s: %v: %s", path, err, text)
	}
	for line := range strings.SplitSeq(text, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "Authority":
			s.Authority = append(s.Authority, value)
		case "TeamIdentifier":
			if value != "not set" {
				s.TeamID = value
			}
		case "Identifier":
			s.Identifier = value
		case "Timestamp":
			s.SignedAt = value
		case "CodeDirectory v":
			s.HardenedRuntime = strings.Contains(value, "(runtime)") || strings.Contains(value, "runtime")
		case "Signature":
			if value == "adhoc" {
				s.Format = "adhoc"
			}
		}
	}
	switch {
	case s.Format == "adhoc":
	case len(s.Authority) > 0 && strings.HasPrefix(s.Authority[0], "Developer ID Application"):
		s.Format = "developer-id"
	default:
		s.Format = "other"
	}
	if s.Format != "adhoc" {
		s.Certificate = leafCertificate(ctx, path)
	}
	// Gatekeeper asks whether the certificate still stands (it reaches Apple's OCSP service):
	// revocation is a fact of when it is asked, not of the build.
	out, _ = exec.CommandContext(ctx, "spctl", "-a", "-vv", "-t", "exec", path).CombinedOutput()
	// A command line tool is no app to run from the Finder: Gatekeeper rejects it as such, the
	// code valid. Asked as an installer would, it says what it says of the signature.
	if strings.Contains(string(out), "does not seem to be an app") {
		out, _ = exec.CommandContext(ctx, "spctl", "-a", "-vv", "-t", "install", path).CombinedOutput()
	}
	s.Assessment = strings.TrimSpace(strings.ReplaceAll(string(out), path+": ", ""))
	switch a := s.Assessment; {
	case strings.Contains(a, "REVOKED"):
		s.Status = "revoked"
	case strings.Contains(a, "accepted"):
		s.Status = "valid"
		s.Notarized = strings.Contains(a, "Notarized")
	default:
		s.Status = "rejected"
	}
	return s, nil
}

// leafCertificate is the certificate path is signed with, nil where it cannot be read.
func leafCertificate(ctx context.Context, path string) *CertInfo {
	dir, err := os.MkdirTemp("", "terma-cert")
	if err != nil {
		return nil
	}
	defer os.RemoveAll(dir)
	prefix := filepath.Join(dir, "cert")
	if err := exec.CommandContext(ctx, "codesign", "-d", "--extract-certificates="+prefix, path).Run(); err != nil {
		return nil
	}
	der, err := os.ReadFile(prefix + "0")
	if err != nil {
		return nil
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil
	}
	sum := sha256.Sum256(der)
	return &CertInfo{Subject: cert.Subject.CommonName, Serial: strings.ToUpper(cert.SerialNumber.Text(16)),
		SHA256: hex.EncodeToString(sum[:]), NotBefore: cert.NotBefore.UTC(), NotAfter: cert.NotAfter.UTC()}
}

// prepareDownload reads the signing of a build written at check, keeps it beside path, and
// puts the build at path to run: as it came, or re-signed ad hoc where its certificate was
// revoked, so that macOS runs it. macOS remembers by path a file Gatekeeper rejected, and holds
// whatever is put there at launch, however it is signed: so the build is checked at another
// path, and path is one Gatekeeper never assessed.
func prepareDownload(check, path string) error {
	s, err := inspectSigning(check)
	if err != nil {
		return err
	}
	if s != nil && s.Status == "revoked" {
		bin, err := os.ReadFile(check)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, bin, 0o755); err != nil {
			return err
		}
		for _, args := range [][]string{{"--remove-signature", path}, {"-s", "-", path}} {
			if out, err := exec.Command("codesign", args...).CombinedOutput(); err != nil {
				os.Remove(path)
				return fmt.Errorf("re-sign %s: %v: %s", path, err, out)
			}
		}
		s.Resigned = true
		os.Remove(check)
	} else if err := os.Rename(check, path); err != nil {
		return err
	}
	if s == nil {
		return nil
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(signingSidecar(path), append(data, '\n'), 0o644)
}

// SigningOf is what b's signature said: as kept when it was downloaded, or read now from a
// build downloaded before signatures were kept, or found installed. Nil off macOS, and for a
// build that is no Mach-O executable.
func SigningOf(b Binary) (*Signing, error) {
	path, err := filepath.EvalSymlinks(b.Path)
	if err != nil {
		path = b.Path
	}
	data, err := os.ReadFile(signingSidecar(b.Path))
	if err == nil {
		var s Signing
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, err
		}
		return &s, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if native := npmPlatformBinary(path); native != "" {
		path = native // an npm loader's platform executable (the Copilot CLI's) is what is signed
	}
	return inspectSigning(path)
}

// RecordSigning puts b's signing in t's profile of b, as the "signing" probe.
func RecordSigning(t *testing.T, b Binary) {
	t.Helper()
	s, err := SigningOf(b)
	if err != nil {
		t.Logf("signing of %s %s: %v", b.Harness, b.Version, err)
		return
	}
	if s != nil {
		RecordProbe(t, b, "signing", s)
	}
}

// writeBuild writes a downloaded build to path, prepared to run.
func writeBuild(path string, bin []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	check := path + ".download"
	if err := os.WriteFile(check, bin, 0o755); err != nil {
		return err
	}
	if err := prepareDownload(check, path); err != nil {
		os.Remove(check)
		os.Remove(path)
		return err
	}
	return nil
}
