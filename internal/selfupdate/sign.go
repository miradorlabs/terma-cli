package selfupdate

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// SignatureName is the release asset holding the signatures of checksums.txt, which names
// every archive's digest: signing it, with the release's tag, signs the release. One
// signature per line, so a release made while a key is being rotated carries both keys'.
const SignatureName = "checksums.txt.sig"

// maxSignature bounds the signature file: a few lines of base64.
const maxSignature = 4096

// releaseKeys are the ed25519 public keys a release may be signed with, hex, newest first.
// To rotate: a release lists the new key beside the old and is signed by both (both seeds
// in TERMA_SIGNING_KEY), and so is every release until no build that lacks the new key is
// still updating; then the old key and its signature go. A key to retire at once goes with
// the release that replaces it, and a build that lacks the new key is reinstalled.
var releaseKeys = []string{
	"cb6646bf0ec98128bcab6bd5a4b92d0ca4ade2d9e3f17e12e06e33725240667b", // 2026-10-07
}

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

// message is what is signed: the release's tag before the file, so a signed file cannot be
// published again under another tag as a later release.
func message(tag string, data []byte) []byte {
	return append([]byte("terma release "+tag+"\n"), data...)
}

// Sign returns one line of the signature file for data under tag: its ed25519 signature,
// base64. Lines from several keys concatenate into one file.
func Sign(priv ed25519.PrivateKey, tag string, data []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, message(tag, data))) + "\n")
}

// Verify checks sig, a signature file, against data under tag: any line by any of keys.
func Verify(keys []ed25519.PublicKey, tag string, data, sig []byte) error {
	if len(sig) > maxSignature {
		return ErrUnsigned
	}
	msg := message(tag, data)
	for line := range bytes.Lines(sig) {
		raw, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(line)))
		if err != nil || len(raw) != ed25519.SignatureSize {
			continue
		}
		for _, k := range keys {
			if ed25519.Verify(k, msg, raw) {
				return nil
			}
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
