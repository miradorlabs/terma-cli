package selfupdate

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// SignatureName is the release asset holding the signature of checksums.txt, which names
// every archive's digest: signing it signs the release.
const SignatureName = "checksums.txt.sig"

// releaseKeys are the ed25519 public keys a release may be signed with, hex, newest first.
// A new key ships in a release signed with the old one, and the old key goes a release
// later, once every machine has taken the new one; a key to retire at once goes with the
// release that replaces it. The private key is the release workflow's TERMA_SIGNING_KEY.
var releaseKeys = []string{}

// builtinKeys are releaseKeys parsed; a bad entry fails the build's tests (TestBuiltinKeys).
var builtinKeys = func() []ed25519.PublicKey {
	var keys []ed25519.PublicKey
	for _, h := range releaseKeys {
		if k, err := ParseKey(h); err == nil {
			keys = append(keys, k)
		}
	}
	return keys
}()

// ErrUnsigned means the release carries no signature, or one no pinned key made.
var ErrUnsigned = errors.New("the release is not signed by a key this terma trusts; refusing to install")

// ParseKey reads a public key as releaseKeys hold it: 32 bytes, hex.
func ParseKey(h string) (ed25519.PublicKey, error) {
	k, err := hex.DecodeString(h)
	if err != nil || len(k) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("release key %q: want %d hex bytes", h, ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(k), nil
}

// Sign returns the signature file for data: the ed25519 signature, base64, one line.
func Sign(priv ed25519.PrivateKey, data []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, data)) + "\n")
}

// Verify checks sig, a signature file, against data with any of keys.
func Verify(keys []ed25519.PublicKey, data, sig []byte) error {
	raw, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(sig)))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return ErrUnsigned
	}
	for _, k := range keys {
		if ed25519.Verify(k, data, raw) {
			return nil
		}
	}
	return ErrUnsigned
}

// BuiltinKeys are the public keys built into this terma.
func BuiltinKeys() []ed25519.PublicKey { return builtinKeys }

// keys are the public keys this client accepts: its own, else the build's.
func (c *Client) keys() []ed25519.PublicKey {
	if c.ReleaseKeys != nil {
		return c.ReleaseKeys
	}
	return builtinKeys
}
