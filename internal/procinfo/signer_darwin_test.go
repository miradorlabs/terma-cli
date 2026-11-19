package procinfo

import (
	"os"
	"testing"
)

// The test binary, as go test links it: signed ad hoc, so valid, with no team.
func TestSignerOfThisProcess(t *testing.T) {
	s, ok := Signer(os.Getpid())
	if !ok {
		t.Fatal("no signature for this process")
	}
	if !s.Valid || s.Platform || s.TeamID != "" || s.Identifier == "" || s.Start == 0 {
		t.Fatalf("Signer(self) = %+v; want valid, not Apple's, no team, an identifier and a start", s)
	}
	again, _ := Signer(os.Getpid())
	if again.Start != s.Start {
		t.Fatalf("start changed between asks: %d then %d", s.Start, again.Start)
	}
}

// launchd is Apple's.
func TestSignerOfLaunchd(t *testing.T) {
	s, ok := Signer(1)
	if !ok || !s.Platform || !s.Valid || s.Identifier != "com.apple.xpc.launchd" {
		t.Fatalf("Signer(1) = %+v, %v; want Apple's launchd", s, ok)
	}
}

func TestSignerOfNoProcess(t *testing.T) {
	if _, ok := Signer(999_999); ok {
		t.Fatal("a signature for a process that does not exist")
	}
}
