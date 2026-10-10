package relay

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// pinnedCounterNames are the names, and the prefixes of the names, of every counter the
// relay keeps, as they leave the machine on the heartbeat (terma.relay.heartbeat.counter.
// <name>), where the platform reads them. Renaming one breaks that: change this list and the
// platform together. A prefix ends in "." or "_" and is completed with a signal or a reason.
var pinnedCounterNames = []string{
	"attributed_by_", "caught_by_default.", "dropped.ambiguous_process.",
	"dropped.ambiguous_process_at_exit.", "dropped.claimed_at_exit.", "dropped.encode_failed.",
	"dropped.no_key.", "dropped.no_key_at_exit.", "dropped.no_session_id.",
	"dropped.no_session_process.", "dropped.no_session_process_at_exit.",
	"dropped.no_session_trace.", "dropped.no_session_trace_at_exit.", "dropped.not_collected.",
	"dropped.not_collected_at_exit.", "dropped.outbox_expired.", "dropped.outbox_full.",
	"dropped.outbox_unreadable.", "dropped.outbox_write_failed.", "dropped.policy_repository.",
	"dropped.policy_signal.", "dropped.policy_signal_or_content.", "dropped.policy_widened.",
	"dropped.policy_widened_at_exit.", "dropped.process_running.",
	"dropped.process_running_at_exit.", "dropped.unclaimed_evicted.",
	"dropped.unclaimed_expired.", "dropped.unclaimed_expired_at_exit.",
	"dropped.unclaimed_overflow.", "dropped.uncovered_process.",
	"dropped.uncovered_process_at_exit.", "dropped.upstream_", "forwarded.", "heartbeats_failed",
	"heartbeats_sent", "held_parts", "process_index_full", "queued_at_exit.", "received.",
	"recovered_from_outbox", "refused_restarting", "refused_unauthorized", "refused_undecodable",
	"refused_unreadable", "released_after_hold", "sender_unresolved", "trace_index_full",
	"unclassified", "upstream_rejected.", "upstream_retries", "withheld_at_send_records",
	"withheld_content_records",
}

// The counters' names are read from this package's source: every literal stats.add name, every
// drop reason spelled as a literal (to stats.dropped, the outbox's drop, or a reason variable)
// or a why… constant, and the unclassified total. A name this package spells is pinned; one
// assembled from data at run time is not, and the patterns below must keep up.
func TestCounterNamesArePinned(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	add := regexp.MustCompile(`stats\.add\("([a-z_.]+)"`)
	drop := regexp.MustCompile(`stats\.dropped\([^,]+, "([a-z_]+)"(\+?)`)
	why := regexp.MustCompile(`\bwhy[A-Z][A-Za-z]*\s*=\s*"([a-z_]+)"`)
	// The outbox's drop(q, "outbox_full") and a reason held in a variable before it is dropped.
	reason := regexp.MustCompile(`\bdrop\(\w+, "([a-z_]+)"\)|\breason, records :?= "([a-z_]+)"`)
	total := regexp.MustCompile(`\bunclassifiedPrefix\s*=\s*"([a-z_]+)"`)
	found := map[string]bool{"received.": true, "forwarded.": true}
	var whys []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range add.FindAllSubmatch(src, -1) {
			found[string(m[1])] = true
		}
		for _, m := range drop.FindAllSubmatch(src, -1) {
			if len(m[2]) > 0 { // "upstream_"+detail: a prefix
				found["dropped."+string(m[1])] = true
				continue
			}
			found["dropped."+string(m[1])+"."] = true
		}
		for _, m := range reason.FindAllSubmatch(src, -1) {
			found["dropped."+string(m[1])+string(m[2])+"."] = true
		}
		for _, m := range total.FindAllSubmatch(src, -1) {
			found[string(m[1])] = true
		}
		for _, m := range why.FindAllSubmatch(src, -1) {
			found["dropped."+string(m[1])+"."] = true
			whys = append(whys, string(m[1]))
		}
	}
	// What a stopping relay held is dropped as <reason>_at_exit, or claimed_at_exit.
	for _, w := range append(whys, "claimed") {
		found["dropped."+w+"_at_exit."] = true
	}
	got := slices.Sorted(func(yield func(string) bool) {
		for k := range found {
			if !yield(k) {
				return
			}
		}
	})
	if !slices.Equal(got, pinnedCounterNames) {
		added, removed := setDiff(got, pinnedCounterNames), setDiff(pinnedCounterNames, got)
		t.Errorf("the relay's counter names changed: added %q, removed %q. They leave the machine on the heartbeat, where the platform reads them", added, removed)
	}
}
