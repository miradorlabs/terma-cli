package selfupdate

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

// testSigner makes this test's releases signed by a key only it trusts, and returns
// the signer for the release files it serves.
func testSigner(t *testing.T) func([]byte) []byte {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(TrustOnly(pub))
	return func(data []byte) []byte { return Sign(priv, data) }
}

func TestSignatureVerifies(t *testing.T) {
	sign := testSigner(t)
	data := []byte("abc  terma_Darwin_arm64.tar.gz\n")
	sig := sign(data)
	if err := Verify(data, sig); err != nil {
		t.Fatalf("a signature by a trusted key: %v", err)
	}
	if err := Verify([]byte("abd  terma_Darwin_arm64.tar.gz\n"), sig); err == nil {
		t.Fatal("a changed checksums file verified")
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if err := Verify(data, Sign(other, data)); err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("a signature by an untrusted key: %v", err)
	}
	for _, bad := range []string{"", "garbage", "terma release signature, key x\nnot-base64!\n"} {
		if err := Verify(data, []byte(bad)); err == nil {
			t.Errorf("%q verified", bad)
		}
	}
}

// The key built into the binary is well-formed; a typo would make every update fail.
func TestReleaseKeyIsAnEd25519PublicKey(t *testing.T) {
	pub, err := base64.StdEncoding.DecodeString(releaseKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		t.Fatalf("releaseKey is not a base64 ed25519 public key: %v", err)
	}
	if KeyID(pub) != "5c4bb9d9babad561" {
		t.Fatalf("releaseKey's id is %s; the comment beside it names another key", KeyID(pub))
	}
}
