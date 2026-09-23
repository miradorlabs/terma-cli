package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// The spool check exists because a hook never reports a failed append: the commit
// path swallows the error by design, so a queue nobody can write to is a machine
// whose commits go unbilled in silence. Doctor has to be the place that says so.
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
	// A directory where the queue file belongs: every read of the spool still
	// works, every append fails.
	if err := os.Mkdir(filepath.Join(spoolDir, "events.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}

	out, err := runTerma(t, "doctor", "--skip-commit")
	if err == nil {
		t.Fatalf("doctor must fail on a spool it cannot write:\n%s", out)
	}
	if !strings.Contains(out, "events cannot be written to the spool") {
		t.Fatalf("doctor should name the write failure, not the queue depth:\n%s", out)
	}
}
