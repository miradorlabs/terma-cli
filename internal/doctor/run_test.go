package doctor

import (
	"context"
	"testing"
	"time"
)

// A failed binary check makes the backend check inconclusive before it flushes anything.
func TestBackendCheckWaitsForTheBinary(t *testing.T) {
	binary := Check{Status: Warn, Fix: "replace the stale binary"}
	check := BackendCheck(context.Background(), Probes{}, "p", "sha", binary, Progress{})
	if check.Status != Warn || !check.Inconclusive || check.Fix != binary.Fix {
		t.Fatalf("must identify the binary mismatch before flushing or polling: %+v", check)
	}
}

// The round-trip polls, noting each poll, over a commitLogWindow either side of its start.
func TestWaitForCommitPollsAWindowAroundTheCommit(t *testing.T) {
	started := time.Now()
	var polls, notes int
	p := Probes{CommitRecorded: func(_ context.Context, projectID, sha string, from, to time.Time) (bool, error) {
		polls++
		if projectID != "p" || sha != "sha" {
			t.Errorf("read %s %s", projectID, sha)
		}
		if from.After(started) || to.Before(started) || to.Sub(from) != 2*commitLogWindow {
			t.Errorf("window %v..%v does not bracket the commit by %v", from, to, commitLogWindow)
		}
		return polls == 2, nil
	}}
	found, err := waitForCommit(context.Background(), p, "p", "sha", Progress{Note: func(string) { notes++ }})
	if err != nil || !found || polls != 2 || notes != polls {
		t.Fatalf("found %v, %v after %d polls and %d notes; want the second poll to find it, noted", found, err, polls, notes)
	}
}
