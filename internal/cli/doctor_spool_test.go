package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// A hook swallows a failed append by design, so doctor is the place an unwritable spool
// gets said.
func TestDoctorFailsWhenTheSpoolCannotBeWritten(t *testing.T) {
	userSandbox(t)
	dir, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	spoolDir := filepath.Join(dir, "spool")
	if err := os.MkdirAll(spoolDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A directory where the queue file belongs: reads still work, every append fails.
	if err := os.Mkdir(filepath.Join(spoolDir, "events.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}

	out, err := runTerma(t, "doctor")
	if err == nil {
		t.Fatalf("doctor must fail on a spool it cannot write:\n%s", out)
	}
	if !strings.Contains(out, "events cannot be written to the spool") {
		t.Fatalf("doctor should name the write failure, not the queue depth:\n%s", out)
	}
}

// Scripts written before doctor stopped making a scratch commit still pass --skip-commit.
func TestDoctorStillAcceptsSkipCommit(t *testing.T) {
	userSandbox(t)
	out, _ := runTerma(t, "doctor", "--skip-commit")
	if strings.Contains(out, "unknown flag") {
		t.Fatalf("--skip-commit must still parse:\n%s", out)
	}
	if help, _ := runTerma(t, "doctor", "--help"); strings.Contains(help, "skip-commit") {
		t.Fatalf("--skip-commit should stay hidden:\n%s", help)
	}
}
