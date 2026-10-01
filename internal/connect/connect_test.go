package connect

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

func asked(answer bool, err error, n *int) Steps {
	return Steps{Confirm: func(string) (bool, error) { *n++; return answer, err }}
}

// A conflict outside the file terma writes is refused even with --force; one inside it
// needs --force; an advisory one never gates.
func TestGateRefusesWhatItCannotClear(t *testing.T) {
	var n int
	outside := []harness.Conflict{{Key: "OTEL_EXPORTER_OTLP_ENDPOINT", Scope: "shell"}}
	if err := gate(&bytes.Buffer{}, "Agent", outside, Options{Force: true}, asked(true, nil, &n), "", "q"); err == nil || !strings.Contains(err.Error(), "does not change") {
		t.Fatalf("an unclearable conflict passed: %v", err)
	}
	inside := []harness.Conflict{{Key: "endpoint", Clearable: true}}
	if err := gate(&bytes.Buffer{}, "Agent", inside, Options{}, asked(true, nil, &n), " in this repository", "q"); err == nil || !strings.Contains(err.Error(), "settings in this repository that would") {
		t.Fatalf("a clearable conflict passed without --force: %v", err)
	}
	if err := gate(&bytes.Buffer{}, "Agent", inside, Options{Force: true}, asked(true, nil, &n), "", "q"); err != nil {
		t.Fatalf("--force did not clear: %v", err)
	}
	advisory := []harness.Conflict{{Key: "profile", Advisory: true}}
	if err := gate(&bytes.Buffer{}, "Agent", advisory, Options{AssumeYes: true}, asked(true, nil, &n), "", "q"); err != nil {
		t.Fatalf("an advisory conflict gated: %v", err)
	}
	if n != 1 {
		t.Fatalf("asked %d times, want once (--yes and refusals never ask)", n)
	}
}

// A declined question writes nothing and is not a failure; a failed one is.
func TestGateDeclinedIsCancelled(t *testing.T) {
	var n int
	out := &bytes.Buffer{}
	if err := gate(out, "Agent", nil, Options{}, asked(false, nil, &n), "", "q"); !errors.Is(err, errCancelled) || !strings.Contains(out.String(), "Nothing was written") {
		t.Fatalf("declined: %v, %q", err, out)
	}
	boom := errors.New("no terminal")
	if err := gate(&bytes.Buffer{}, "Agent", nil, Options{}, asked(false, boom, &n), "", "q"); !errors.Is(err, boom) {
		t.Fatalf("a failed question: %v", err)
	}
}
