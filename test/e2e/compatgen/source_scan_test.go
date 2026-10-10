package main

import (
	"testing"
)

// A Slack section cut where no line ends is not cut inside a link.
func TestClipKeepsLinksWhole(t *testing.T) {
	s := renderLinks(slackEscape("`k` · "+link("https://github.com/x#L1", "x.rs:1")), true)
	if got := clip(s, len("`k` · <https://git")); got != "`k` · \n…" {
		t.Errorf("clip = %q", got)
	}
}

// A test module's raw strings hold braces that are text; a constant's string may be on the
// line after it; an import of the constant is not its use.
func TestSourceScanReadsRustAsItIs(t *testing.T) {
	files := map[string]string{
		"codex-rs/auth/src/util.rs":          "#[cfg(test)]\nmod tests {\n    const BODY: &str = r#\"{\n        \"a\": {\"b\": 1}\n    }\"#;\n    fn t() { emit(\"codex.only_in_a_test\"); }\n}\n\nfn real() {\n    emit(\"codex.after_tests\");\n}\n",
		"codex-rs/otel/src/metrics/names.rs": "pub const SPAWN_PHASE_METRIC: &str =\n    \"codex.multi_agent.spawn.phase\";\n",
		"codex-rs/otel/src/spawn.rs":         "use crate::metrics::SPAWN_PHASE_METRIC;\n\nfn spawn() {\n    let tags = [(\"phase\", p)];\n    metrics.record(SPAWN_PHASE_METRIC, d, &tags);\n}\n",
	}
	withSource(t, map[string][]byte{"rust-v0.162.0": tarball(t, files)})
	d := Drift{Harnesses: []HarnessDrift{{Harness: "codex", Name: "Codex CLI", Version: "0.162.0", Previous: "0.162.0",
		NewSurfaces: []SurfaceChange{{Surface: "logs/codex.only_in_a_test"}, {Surface: "logs/codex.after_tests"}},
		Added:       []FieldChange{{Surface: "metrics/codex.multi_agent.spawn.phase", Key: "phase"}}}}}
	linkSources(&d, nil)
	h := d.Harnesses[0]
	if s := h.NewSurfaces[0].Source; s != nil && len(s.Refs) > 0 {
		t.Errorf("a name in a test module's raw-string test was linked: %+v", s)
	}
	if s := h.NewSurfaces[1].Source; s == nil || len(s.Refs) != 1 || s.Refs[0].Line != 10 {
		t.Errorf("the code after the test module: %+v", s)
	}
	if s := h.Added[0].Source; s == nil || len(s.Refs) != 1 || s.Refs[0].Path != "codex-rs/otel/src/spawn.rs" || s.Refs[0].Line != 4 {
		t.Errorf("phase, beside the use of a two-line constant: %+v", s)
	}
}

// Comments are no build's code: a name left in a trailing or a block comment is not still in
// the source; a string that holds "//" or "/*" is still a string.
func TestSourceScanSkipsComments(t *testing.T) {
	files := map[string]string{
		"codex-rs/otel/src/a.rs": "fn a() {\n    emit(\"codex.live\"); // was \"codex.trailing\"\n    /* \"codex.block_one\"\n       \"codex.block_two\" */ emit(\"codex.after_block\");\n" +
			"    let url = \"https://x/*y\"; emit(\"codex.after_url\");\n    let q = '\"'; emit(\"codex.after_char\");\n}\n",
	}
	withSource(t, map[string][]byte{"rust-v0.162.0": tarball(t, files)})
	var ss []SurfaceChange
	for _, name := range []string{"live", "trailing", "block_one", "block_two", "after_block", "after_url", "after_char"} {
		ss = append(ss, SurfaceChange{Surface: "logs/codex." + name})
	}
	d := Drift{Harnesses: []HarnessDrift{{Harness: "codex", Name: "Codex CLI", Version: "0.162.0", Previous: "0.162.0", NewSurfaces: ss}}}
	linkSources(&d, nil)
	for i, want := range []bool{true, false, false, false, true, true, true} {
		s := d.Harnesses[0].NewSurfaces[i]
		if got := s.Source != nil && len(s.Source.Refs) > 0; got != want {
			t.Errorf("%s linked %v, want %v", s.Surface, got, want)
		}
	}
}

// Rust nests block comments: a name inside the outer one, after an inner one closes, is
// still comment.
func TestSourceScanNestsBlockComments(t *testing.T) {
	files := map[string]string{"codex-rs/a.rs": "fn a() {\n    /* outer /* inner */ emit(\"codex.dead\"); */ emit(\"codex.live\");\n}\n"}
	withSource(t, map[string][]byte{"rust-v0.162.0": tarball(t, files)})
	d := Drift{Harnesses: []HarnessDrift{{Harness: "codex", Name: "Codex CLI", Version: "0.162.0", Previous: "0.162.0",
		NewSurfaces: []SurfaceChange{{Surface: "logs/codex.dead"}, {Surface: "logs/codex.live"}}}}}
	linkSources(&d, nil)
	h := d.Harnesses[0]
	if s := h.NewSurfaces[0].Source; s != nil && len(s.Refs) > 0 {
		t.Errorf("a name inside a nested comment was linked: %+v", s)
	}
	if s := h.NewSurfaces[1].Source; s == nil || len(s.Refs) != 1 {
		t.Errorf("the code after the nested comment: %+v", s)
	}
}
