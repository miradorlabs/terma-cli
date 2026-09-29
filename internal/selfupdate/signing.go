package selfupdate

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Releases are signed: the release workflow signs checksums.txt with an ed25519 key
// (scripts/sign), and this build trusts the public keys below. checksums.txt names the
// SHA-256 of every archive and of policy.json, so one signature covers everything the
// updater installs or obeys. A checksum alone protects against a damaged download, not
// a replaced release: whoever can publish the archive can publish its checksum.
//
// Two keys may be trusted at once so the signing key can be rotated: a release signed
// with the old key ships a build that trusts the new one, and the next release is signed
// with the new key.

// trustedKeys are the base64 ed25519 public keys a release signature may verify under.
// A variable so tests can sign with a key of their own.
var trustedKeys = []string{
	releaseKey,
}

// SignatureSuffix names a file's signature asset: checksums.txt.sig.
const SignatureSuffix = ".sig"

// signatureHeader starts every signature file, naming the key that made it.
const signatureHeader = "terma release signature, key "

// ErrUnsigned means a release carries no signature for its checksums.
var ErrUnsigned = errors.New("release is not signed")

// KeyID is a short, stable name for a public key: the first 8 bytes of its SHA-256.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// Sign returns the signature file for data under priv.
func Sign(priv ed25519.PrivateKey, data []byte) []byte {
	sig := ed25519.Sign(priv, data)
	pub, _ := priv.Public().(ed25519.PublicKey)
	return []byte(signatureHeader + KeyID(pub) + "\n" + base64.StdEncoding.EncodeToString(sig) + "\n")
}

// Verify checks that sigFile is a signature of data under one of the trusted keys.
func Verify(data, sigFile []byte) error {
	header, body, ok := bytes.Cut(sigFile, []byte("\n"))
	if !ok || !strings.HasPrefix(string(header), signatureHeader) {
		return errors.New("malformed release signature")
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(body)))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("malformed release signature")
	}
	for _, encoded := range trustedKeys {
		pub, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			continue
		}
		if ed25519.Verify(ed25519.PublicKey(pub), data, sig) {
			return nil
		}
	}
	return fmt.Errorf("release signature from key %s does not verify under a key this build trusts",
		strings.TrimPrefix(string(header), signatureHeader))
}

// TrustOnly makes pub the only trusted release key until restore is called. It exists
// for tests outside this package that serve releases of their own.
func TrustOnly(pub ed25519.PublicKey) (restore func()) {
	saved := trustedKeys
	trustedKeys = []string{base64.StdEncoding.EncodeToString(pub)}
	return func() { trustedKeys = saved }
}
