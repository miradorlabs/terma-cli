package selfupdate

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"sync"
	"testing"
)

// testSigner is the release key the tests' fake releases are signed with.
var testSigner = sync.OnceValues(func() (ed25519.PublicKey, ed25519.PrivateKey) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return pub, priv
})

// testKeys are the keys a test's client accepts: the test signer's.
func testKeys() []ed25519.PublicKey {
	pub, _ := testSigner()
	return []ed25519.PublicKey{pub}
}

// testSign is data's signature file under tag by the test signer.
func testSign(tag string, data []byte) []byte {
	_, priv := testSigner()
	return Sign(priv, tag, data)
}

// Every key built into terma parses: a bad entry would leave the build trusting fewer keys
// than it lists, or none.
func TestBuiltinKeysParse(t *testing.T) {
	t.Parallel()
	if len(builtinKeys) != len(releaseKeys) {
		t.Fatalf("%d of %d release keys parse", len(builtinKeys), len(releaseKeys))
	}
	for _, h := range releaseKeys {
		if _, err := ParseKey(h); err != nil {
			t.Error(err)
		}
	}
}

// A signature verifies with the key that made it and with no other, and a signature file
// that is not one at all is refused rather than failing differently.
func TestSignAndVerify(t *testing.T) {
	t.Parallel()
	data := []byte("abc  terma_Linux_x86_64.tar.gz\n")
	sig := testSign("v1.2.3", data)
	if err := Verify(testKeys(), "v1.2.3", data, sig); err != nil {
		t.Fatal(err)
	}
	other, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		keys []ed25519.PublicKey
		tag  string
		data []byte
		sig  []byte
	}{
		"another key":  {[]ed25519.PublicKey{other}, "v1.2.3", data, sig},
		"no key":       {nil, "v1.2.3", data, sig},
		"changed data": {testKeys(), "v1.2.3", []byte("abd  terma_Linux_x86_64.tar.gz\n"), sig},
		"another tag":  {testKeys(), "v1.2.4", data, sig},
		"not base64":   {testKeys(), "v1.2.3", data, []byte("not a signature\n")},
		"too short":    {testKeys(), "v1.2.3", data, []byte("YWJj\n")},
		"empty":        {testKeys(), "v1.2.3", data, nil},
		"too long":     {testKeys(), "v1.2.3", data, append(sig, make([]byte, maxSignature)...)},
	} {
		if err := Verify(tc.keys, tc.tag, tc.data, tc.sig); !errors.Is(err, ErrUnsigned) {
			t.Errorf("%s: Verify = %v, want ErrUnsigned", name, err)
		}
	}
	// A key among several, in any position, is enough.
	if err := Verify(append([]ed25519.PublicKey{other}, testKeys()...), "v1.2.3", data, sig); err != nil {
		t.Fatalf("second key: %v", err)
	}
	if _, err := ParseKey("abc"); err == nil {
		t.Fatal("a short key parsed")
	}
}

// Rotating a key: a release signed by both the old and the new key verifies on a build that
// knows only the old one and on a build that knows only the new one, so no build is left
// unable to update whichever release it first sees; a line that is no signature is skipped.
func TestARotatingReleaseVerifiesOnOldAndNewBuilds(t *testing.T) {
	t.Parallel()
	oldPub, oldPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	newPub, newPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("abc  terma_Linux_x86_64.tar.gz\n")
	transition := append([]byte("\n"), Sign(oldPriv, "v1.3.0", data)...)
	transition = append(transition, "garbage line\r\n"...)
	transition = append(transition, Sign(newPriv, "v1.3.0", data)...)
	for name, keys := range map[string][]ed25519.PublicKey{
		"old build": {oldPub},
		"new build": {newPub},
		"both":      {newPub, oldPub},
	} {
		if err := Verify(keys, "v1.3.0", data, transition); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Once the old key's signature goes, the old build no longer verifies: it is reinstalled.
	if err := Verify([]ed25519.PublicKey{oldPub}, "v1.4.0", data, Sign(newPriv, "v1.4.0", data)); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("old build verified a release signed by the new key alone: %v", err)
	}
}
