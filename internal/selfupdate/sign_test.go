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

// testSign is data's signature file by the test signer.
func testSign(data []byte) []byte {
	_, priv := testSigner()
	return Sign(priv, data)
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
	sig := testSign(data)
	if err := Verify(testKeys(), data, sig); err != nil {
		t.Fatal(err)
	}
	other, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		keys []ed25519.PublicKey
		data []byte
		sig  []byte
	}{
		"another key":  {[]ed25519.PublicKey{other}, data, sig},
		"no key":       {nil, data, sig},
		"changed data": {testKeys(), []byte("abd  terma_Linux_x86_64.tar.gz\n"), sig},
		"not base64":   {testKeys(), data, []byte("not a signature\n")},
		"too short":    {testKeys(), data, []byte("YWJj\n")},
		"empty":        {testKeys(), data, nil},
	} {
		if err := Verify(tc.keys, tc.data, tc.sig); !errors.Is(err, ErrUnsigned) {
			t.Errorf("%s: Verify = %v, want ErrUnsigned", name, err)
		}
	}
	// A key among several, in any position, is enough.
	if err := Verify(append([]ed25519.PublicKey{other}, testKeys()...), data, sig); err != nil {
		t.Fatalf("second key: %v", err)
	}
	if _, err := ParseKey("abc"); err == nil {
		t.Fatal("a short key parsed")
	}
}
