package main

import (
	"os"
	"path/filepath"
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
	uses, err := harnessUses(dir)
	if err != nil {
		t.Fatal(err)
	}
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
