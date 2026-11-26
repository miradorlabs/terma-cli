package history

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/relay"
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

func chunkTestBatch(texts ...string) *logspb.LogsData {
	t0 := time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)
	out := &logspb.LogsData{}
	for i, text := range texts {
		seg := makeNativeSegment("claude", "s-chunk", "/work/a", text, t0.Add(time.Duration(i)*time.Second))
		out.ResourceLogs = append(out.ResourceLogs, seg.NativeLogs.ResourceLogs...)
	}
	return out
}

// chunkPrompts unmarshals each chunk and returns the prompts in order, with each chunk's size.
func chunkPrompts(t *testing.T, chunks []Chunk) (prompts []string, sizes []int) {
	t.Helper()
	for _, ch := range chunks {
		var msg logspb.LogsData
		if err := proto.Unmarshal(ch.Body, &msg); err != nil {
			t.Fatalf("unmarshal chunk: %v", err)
		}
		n := 0
		for _, rl := range msg.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				for _, lr := range sl.GetLogRecords() {
					prompts = append(prompts, logRecordAttrString(lr, "prompt"))
					n++
				}
			}
		}
		if n != ch.Records {
			t.Errorf("chunk says %d records, holds %d", ch.Records, n)
		}
		sizes = append(sizes, len(ch.Body))
	}
	return prompts, sizes
}

// Many small records come out in chunks at or under TargetChunkBytes, all of them, in order.
func TestChunkWriterKeepsChunksNearTarget(t *testing.T) {
	t.Parallel()
	var texts []string
	for i := range 3000 {
		texts = append(texts, fmt.Sprintf("%04d-%s", i, strings.Repeat("x", 1024)))
	}
	var chunks []Chunk
	w := NewChunkWriter(MaxRequestBytes, func(ch Chunk) error { chunks = append(chunks, ch); return nil })
	if err := w.Add(chunkTestBatch(texts...)); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	prompts, sizes := chunkPrompts(t, chunks)
	if !slices.Equal(prompts, texts) {
		t.Fatalf("records lost or reordered: got %d, want %d", len(prompts), len(texts))
	}
	if len(chunks) < 5 {
		t.Errorf("got %d chunks for ~3 MiB; want several", len(chunks))
	}
	for i, n := range sizes {
		if n > TargetChunkBytes {
			t.Errorf("chunk %d is %d bytes, over the %d-byte target", i, n, TargetChunkBytes)
		}
	}
}

// A record larger than the target but under the hard limit goes alone in its own chunk.
func TestChunkWriterSendsLargeRecordAlone(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("y", 1<<20)
	var chunks []Chunk
	w := NewChunkWriter(0, func(ch Chunk) error { chunks = append(chunks, ch); return nil })
	if err := w.Add(chunkTestBatch("before", big, "after")); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	prompts, sizes := chunkPrompts(t, chunks)
	if len(chunks) != 3 || !slices.Equal(prompts, []string{"before", big, "after"}) {
		t.Fatalf("got %d chunks, prompts in order %v; want 3 chunks, the large record alone", len(chunks), len(prompts))
	}
	if sizes[1] <= TargetChunkBytes || sizes[1] > MaxRequestBytes {
		t.Errorf("large chunk is %d bytes", sizes[1])
	}
}

// A single record over MaxRequestBytes is still a permanent error.
func TestChunkWriterRefusesRecordOverHardLimit(t *testing.T) {
	t.Parallel()
	w := NewChunkWriter(0, func(Chunk) error { return nil })
	err := w.Add(chunkTestBatch("small", strings.Repeat("z", MaxRequestBytes+1)))
	if _, ok := errors.AsType[*PermanentError](err); !ok {
		t.Fatalf("Add = %v; want *PermanentError", err)
	}
}

// Dry run chunks a session the way upload does, so its estimate is the bytes upload sends,
// over several target-sized requests.
func TestDryRunEstimateMatchesUploadedBytes(t *testing.T) {
	t.Parallel()
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	var segs []agents.HistorySegment
	for i := range 1500 {
		segs = append(segs, makeNativeSegment("claude", "s-big", repoA, fmt.Sprintf("%04d-%s", i, strings.Repeat("q", 1024)), now.Add(-48*time.Hour+time.Duration(i)*time.Second)))
	}
	var mu sync.Mutex
	var sent int64
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		sent += int64(len(body))
		requests++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	run := func(dryRun bool) Summary {
		t.Helper()
		src := &stubHistorySource{
			source:   "claude",
			files:    []agents.HistoryFile{{SourceSystem: "claude", SessionID: "s-big", ModTime: now.Add(-47 * time.Hour)}},
			segments: map[string][]agents.HistorySegment{"s-big": segs},
		}
		sum, err := Upload(context.Background(), Options{
			StateDir: t.TempDir(), Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
			State:   singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", true, true),
			Sources: []agents.HistorySource{src}, Filter: relay.NewContentFilter(nil), Holdings: stubHoldings{},
			Transport: &Transport{
				Version: "test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "acc1", tokens: []string{"tok"}}, BytesPerSec: 1 << 30,
			},
			EndpointFor: func(string) string { return srv.URL },
			DryRun:      dryRun,
			Now:         func() time.Time { return now },
		})
		if err != nil {
			t.Fatal(err)
		}
		return sum
	}
	dry := run(true)
	run(false)
	if len(dry.Planned) != 1 || dry.Planned[0].EstimatedBytes != sent {
		t.Fatalf("dry run planned %+v; upload sent %d bytes", dry.Planned, sent)
	}
	if requests < 3 {
		t.Errorf("upload made %d requests for ~1.5 MiB; want several", requests)
	}
}
