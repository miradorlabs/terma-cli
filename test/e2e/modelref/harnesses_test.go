package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The newest build tracked may say nothing of models (a capture that failed; a test's build
// published by mistake, 9.9.9): what is current is what the newest build with models uses.
func TestHarnessUses(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "index.json"), `{"harnesses":[{"id":"claude","name":"Claude Code"}]}`)
	writeFile(t, filepath.Join(dir, "claude/index.json"), `{"versions":[{"version":"9.9.9"},{"version":"2.1.10"},{"version":"2.1.9"}]}`)
	writeFile(t, filepath.Join(dir, "claude/versions/2.1.9.json"),
		`{"model":{"default":"claude-opus-5-5","aliases":{"opus":"claude-opus-5-5"}},"behavior":{"compaction":{"model":"claude-opus-5-5"}}}`)
	writeFile(t, filepath.Join(dir, "claude/versions/2.1.10.json"),
		`{"model":{"default":"claude-fable-5-2","aliases":{"opus":"claude-opus-5-5[1m]"},"params":{"output_config":{"effort":"high"}}},"side_requests":[{"model":"claude-haiku-5-5"},{"model":"claude-haiku-5-5"}]}`)
	writeFile(t, filepath.Join(dir, "claude/versions/9.9.9.json"), `{"network":{}}`)
	builds, errs := readHarnessBuilds(context.Background(), harnessesAt(newClient(), dir), nil)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	uses := harnessUses(builds)
	find := func(model, role string) *HarnessUse {
		for _, u := range uses[norm(model)] {
			if u.Role == role {
				return &u
			}
		}
		return nil
	}
	fable := find("claude-fable-5-2", "default")
	if fable == nil || !fable.Current || fable.First != "2.1.10" || fable.Params == nil {
		t.Errorf("fable default = %+v", fable)
	}
	opus := find("claude-opus-5-5", "default")
	if opus == nil || opus.Current || opus.Last != "2.1.9" {
		t.Errorf("opus default = %+v", opus)
	}
	alias := find("claude-opus-5-5", "alias")
	if alias == nil || !alias.Current || alias.Label != "opus" || alias.Builds != 2 {
		t.Errorf("opus alias = %+v", alias)
	}
	if side := find("claude-haiku-5-5", "side_request"); side == nil || side.Builds != 1 {
		t.Errorf("haiku side request = %+v", side)
	}
	// A dated model is used by its undated name too.
	s := &Snapshot{ID: "claude-opus-5-5", Facts: map[string]any{}}
	if got := usesFor(uses, s); len(got) != 3 || got[0].Role != "default" {
		t.Errorf("usesFor = %+v", got)
	}
}

// A run reads only the builds it has not read before; a build a harness's index no longer lists
// goes; a harness whose index cannot be read keeps what was read of it.
func TestHarnessBuildsAreIncremental(t *testing.T) {
	var mu sync.Mutex // the join reads builds at once
	read := map[string]int{}
	files := map[string]string{
		"index.json":                  `{"harnesses":[{"id":"claude","name":"Claude Code"},{"id":"codex","name":"Codex CLI"}]}`,
		"claude/index.json":           `{"versions":[{"version":"2.1.9"},{"version":"2.1.10"}]}`,
		"claude/versions/2.1.9.json":  `{"model":{"default":"claude-opus-5-5"}}`,
		"claude/versions/2.1.10.json": `{"model":{"default":"claude-fable-5-2"}}`,
		"codex/index.json":            `{"versions":[{"version":"0.1.0"}]}`,
		"codex/versions/0.1.0.json":   `{"model":{"default":"gpt-6.1-sol"}}`,
	}
	src := func(_ context.Context, path string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		read[path]++
		if f, ok := files[path]; ok {
			return []byte(f), nil
		}
		if path == "codex/index.json" {
			return nil, errors.New("503")
		}
		return nil, fmt.Errorf("%s: %w", path, errNotFound)
	}
	first, errs := readHarnessBuilds(context.Background(), src, nil)
	if len(errs) > 0 || len(first.Harnesses["claude"].Builds) != 2 {
		t.Fatalf("first read: %+v %v", first, errs)
	}
	files["claude/index.json"] = `{"versions":[{"version":"2.1.10"},{"version":"2.1.11"}]}`
	files["claude/versions/2.1.11.json"] = `{"network":{}}`
	delete(files, "codex/index.json")
	second, errs := readHarnessBuilds(context.Background(), src, first)
	if len(errs) != 1 {
		t.Errorf("errors = %v, want the codex index's", errs)
	}
	if read["claude/versions/2.1.10.json"] != 1 || read["claude/versions/2.1.11.json"] != 1 {
		t.Errorf("builds read again: %v", read)
	}
	c := second.Harnesses["claude"].Builds
	if _, ok := c["2.1.9"]; ok || c["2.1.11"].Captured || !c["2.1.10"].Captured {
		t.Errorf("claude builds = %+v", c)
	}
	if second.Harnesses["codex"] == nil || len(second.Harnesses["codex"].Builds) != 1 {
		t.Errorf("an unreadable index lost codex's builds: %+v", second.Harnesses["codex"])
	}
}

// A build published partial (no models captured) and captured again later is read again, so
// its models count and it becomes the newest build that uses them; a missing harness index keeps
// what was read before.
func TestHarnessBuildCapturedAgainIsReadAgain(t *testing.T) {
	var mu sync.Mutex
	files := map[string]string{
		"index.json":                  `{"harnesses":[{"id":"claude","name":"Claude Code"}]}`,
		"claude/index.json":           `{"versions":[{"version":"2.1.9","captured_at":"2026-11-09T01:00:00Z"},{"version":"2.1.10","captured_at":"2026-11-10T01:00:00Z"}]}`,
		"claude/versions/2.1.9.json":  `{"model":{"default":"claude-opus-5-5"}}`,
		"claude/versions/2.1.10.json": `{"network":{}}`,
	}
	src := func(_ context.Context, path string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		if f, ok := files[path]; ok {
			return []byte(f), nil
		}
		return nil, fmt.Errorf("%s: %w", path, errNotFound)
	}
	first, _ := readHarnessBuilds(context.Background(), src, nil)
	if first.Harnesses["claude"].Builds["2.1.10"].Captured {
		t.Fatal("a partial build counted as captured")
	}
	files["claude/index.json"] = `{"versions":[{"version":"2.1.9","captured_at":"2026-11-09T01:00:00Z"},{"version":"2.1.10","captured_at":"2026-11-10T05:00:00Z"}]}`
	files["claude/versions/2.1.10.json"] = `{"model":{"default":"claude-fable-5-2"}}`
	second, errs := readHarnessBuilds(context.Background(), src, first)
	if len(errs) > 0 || !second.Harnesses["claude"].Builds["2.1.10"].Captured {
		t.Fatalf("a build captured again was not read again: %+v %v", second.Harnesses["claude"].Builds["2.1.10"], errs)
	}
	uses := harnessUses(second)
	if u := uses[norm("claude-fable-5-2")]; len(u) != 1 || !u[0].Current {
		t.Errorf("fable uses = %+v", u)
	}
	if u := uses[norm("claude-opus-5-5")]; len(u) != 1 || u[0].Current {
		t.Errorf("opus uses = %+v", u)
	}
	delete(files, "index.json")
	third, errs := readHarnessBuilds(context.Background(), src, second)
	if len(errs) != 1 || len(third.Harnesses["claude"].Builds) != 2 {
		t.Errorf("a missing index lost the builds: %+v %v", third.Harnesses, errs)
	}
}
