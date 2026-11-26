package config

import (
	"os"
	"testing"
)

// SetServerKeySignIn bumps ServerKeyGeneration only when the profile switches between the
// server key and a person, in the same config.json write as the switch; a profile written
// with no count reads as 0.
func TestServerKeySwitchesBumpTheGeneration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte(`{"active_profile":"default","profiles":{"default":{"server_key_sign_in":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	read := func() *Profile {
		t.Helper()
		f, err := LoadFile(dir)
		if err != nil {
			t.Fatal(err)
		}
		return f.Profiles[DefaultProfile]
	}
	if p := read(); p.ServerKeyGeneration != 0 {
		t.Fatalf("a profile with no count reads at %d, want 0", p.ServerKeyGeneration)
	}
	for _, step := range []struct {
		on   bool
		want uint64
	}{{true, 0}, {false, 1}, {false, 1}, {true, 2}, {false, 3}} {
		if err := UpdateProfile(dir, DefaultProfile, func(p *Profile) { p.SetServerKeySignIn(step.on, "") }); err != nil {
			t.Fatal(err)
		}
		if p := read(); p.ServerKeySignIn != step.on || p.ServerKeyGeneration != step.want {
			t.Fatalf("after setting %v: server key %v at %d, want %v at %d", step.on, p.ServerKeySignIn, p.ServerKeyGeneration, step.on, step.want)
		}
	}
}
