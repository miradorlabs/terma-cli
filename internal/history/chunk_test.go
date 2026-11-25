package history

import (
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/runs"
)

// A record newer than the resume cap is left for a later run, not counted as withheld by
// a Mixed cutoff.
func TestFilterMixedLogsCountsOnlyMixedDrops(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)
	seg := makeNativeSegment("claude", "s-resume", "/work/a", "EARLY", t0)
	later := makeNativeSegment("claude", "s-resume", "/work/a", "ADDED_SINCE", t0.Add(2*time.Hour))
	seg.NativeLogs.ResourceLogs = append(seg.NativeLogs.ResourceLogs, later.NativeLogs.ResourceLogs...)

	kept, dropped := FilterMixedLogs(seg.NativeLogs, runs.Snapshot{}, nil, time.Time{}, false, t0.Add(time.Hour))
	if kept != 1 || dropped != 0 {
		t.Errorf("resume cap alone: kept %d, dropped %d; want 1 and 0", kept, dropped)
	}
	seg = makeNativeSegment("claude", "s-resume", "/work/a", "AFTER_CUTOFF", t0)
	if kept, dropped := FilterMixedLogs(seg.NativeLogs, runs.Snapshot{}, nil, t0.Add(-time.Minute), true, time.Time{}); kept != 0 || dropped != 1 {
		t.Errorf("Mixed cutoff: kept %d, dropped %d; want 0 and 1", kept, dropped)
	}
}
