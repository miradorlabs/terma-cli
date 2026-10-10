package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"
)

// tarball is a tree as GitHub serves one: every path under one folder.
func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: "codex-rust-v0/" + name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The builds of a fake Codex: 0.161.0 sends a persistence metric and tags a snapshot metric
// with "state"; 0.162.0 still defines the metric, drops the turn's phase field, and adds a key.
func fakeCodex(t *testing.T) map[string][]byte {
	metrics := `const APPEND_METRIC: &str = "codex.rollout.persistence.append";
`
	snapshot := `fn record(state: &'static str) {
    let tags = [("version", "v2"), ("state", state)];
    metrics.counter("codex.shell_snapshot.command", 1, &tags);
}
`
	doctor := `fn check() {
    let state = probe();
    report("state", state);
}
`
	return map[string][]byte{
		"rust-v0.161.0": tarball(t, map[string]string{
			"codex-rs/rollout/src/persistence_metrics.rs": metrics,
			"codex-rs/exec-server/src/telemetry.rs":       snapshot,
			"codex-rs/cli/src/doctor.rs":                  doctor,
			"codex-rs/core/src/turn.rs":                   "fn run() {\n    info_span!(\"turn\", codex.turn.phase = \"compaction\");\n}\n",
		}),
		"rust-v0.162.0": tarball(t, map[string]string{
			"codex-rs/rollout/src/persistence_metrics.rs": metrics,
			"codex-rs/exec-server/src/telemetry.rs":       snapshot,
			"codex-rs/cli/src/doctor.rs":                  doctor,
			"codex-rs/core/src/turn.rs":                   "fn run() {\n    info_span!(\"turn\");\n}\n",
			// Tests and comments name things too: neither counts.
			"codex-rs/core/tests/suite/sku.rs":   `assert_eq!(attr("product_sku"), "codex");`,
			"codex-rs/core/src/mcp.rs":           "// product_sku is bounded\nfn sku() {\n    tags.push((\"product_sku\", sku));\n}\n",
			"codex-rs/core/src/mcp_tests.rs":     `"product_sku"`,
			"codex-cli/bin/codex.js":             `"product_sku"`,
			"codex-rs/core/src/unrelated_let.rs": "fn x() {\n    let product_sku = 1;\n}\n",
		}),
	}
}

func withSource(t *testing.T, builds map[string][]byte) {
	t.Helper()
	prev := openSource
	t.Cleanup(func() { openSource = prev })
	openSource = func(repo, tag string) (io.ReadCloser, error) {
		if repo != "openai/codex" {
			t.Errorf("read %s", repo)
		}
		b, ok := builds[tag]
		if !ok {
			return nil, errors.New("404 Not Found")
		}
		return io.NopCloser(bytes.NewReader(b)), nil
	}
}

// A finding about Codex links the lines that name it at its build; one gone says whether the
// new build's source still names it, or where the build before did; a key leads to the lines
// beside its surface, not to every line that says its name.
func TestSourceLinks(t *testing.T) {
	withSource(t, fakeCodex(t))
	d := Drift{Harnesses: []HarnessDrift{{Harness: "codex", Name: "Codex CLI", Version: "0.162.0", Previous: "0.161.0",
		Added:    []FieldChange{{Surface: "logs/codex.mcp", Key: "product_sku"}},
		Withheld: []FieldChange{{Surface: "metrics/codex.shell_snapshot.command", Key: "state"}},
		Removed:  []FieldChange{{Surface: "traces/turn", Key: "codex.turn.phase"}},
		Unseen:   []GoneSurface{{Surface: "metrics/codex.rollout.persistence.append"}, {Surface: "resource"}},
	}}}
	linkSources(&d)
	h := d.Harnesses[0]
	if h.Compare != "https://github.com/openai/codex/compare/rust-v0.161.0...rust-v0.162.0" {
		t.Errorf("compare = %q", h.Compare)
	}
	refs := func(s *SourceSays) string {
		if s == nil {
			return "nil"
		}
		var out []string
		for _, r := range s.Refs {
			out = append(out, r.Path+":"+itoa(r.Line))
		}
		for _, r := range s.Before {
			out = append(out, "before "+r.Path+":"+itoa(r.Line))
		}
		return strings.Join(out, " ")
	}
	for name, c := range map[string]struct {
		got  *SourceSays
		want string
	}{
		"added, outside tests, comments and lets": {h.Added[0].Source, "codex-rs/core/src/mcp.rs:3"},
		"a generic key beside its metric":         {h.Withheld[0].Source, "codex-rs/exec-server/src/telemetry.rs:2"},
		"a field gone from the source":            {h.Removed[0].Source, "before codex-rs/core/src/turn.rs:2"},
		"a surface still in the source":           {h.Unseen[0].Source, "codex-rs/rollout/src/persistence_metrics.rs:1 before codex-rs/rollout/src/persistence_metrics.rs:1"},
		"a surface the source cannot name":        {h.Unseen[1].Source, "nil"},
	} {
		if got := refs(c.got); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
	if got := h.Added[0].Source.Refs[0].URL; got != "https://github.com/openai/codex/blob/rust-v0.162.0/codex-rs/core/src/mcp.rs#L3" {
		t.Errorf("url = %s", got)
	}

	md := d.markdown()
	for _, want := range []string{
		"Codex CLI 0.162.0 (was 0.161.0, [diff](https://github.com/openai/codex/compare/rust-v0.161.0...rust-v0.162.0))",
		"`product_sku` on `logs/codex.mcp` · [mcp.rs:3](https://github.com/openai/codex/blob/rust-v0.162.0/codex-rs/core/src/mcp.rs#L3)",
		"`codex.turn.phase` on `traces/turn` · gone from 0.162.0's source, was in 0.161.0's: [turn.rs:2](https://github.com/openai/codex/blob/rust-v0.161.0/codex-rs/core/src/turn.rs#L2)",
		"`metrics/codex.rollout.persistence.append` · still in 0.162.0's source: [persistence_metrics.rs:1](",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("drift.md lacks %q:\n%s", want, md)
		}
	}
	data, _ := json.Marshal(d.slack())
	for _, want := range []string{
		`\u003chttps://github.com/openai/codex/compare/rust-v0.161.0...rust-v0.162.0|diff\u003e`,
		`\u003chttps://github.com/openai/codex/blob/rust-v0.162.0/codex-rs/core/src/mcp.rs#L3|mcp.rs:3\u003e`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("slack payload lacks %s: %s", want, data)
		}
	}
	if strings.ContainsAny(string(data), "\x00\x01\x02") {
		t.Error("a link marker reached Slack")
	}
}

// A build whose source cannot be read leaves its findings unlinked, and the digest says so;
// a harness whose source is not public, or a night with nothing to link, reads nothing.
func TestSourceLinksFailOpen(t *testing.T) {
	withSource(t, map[string][]byte{})
	d := Drift{Harnesses: []HarnessDrift{
		{Harness: "codex", Name: "Codex CLI", Version: "0.163.0", Previous: "0.162.0", Added: []FieldChange{{Surface: "logs/x", Key: "k"}}},
		{Harness: "claude", Name: "Claude Code", Version: "2.1.296", Previous: "2.1.295", Added: []FieldChange{{Surface: "logs/x", Key: "k"}}},
	}}
	linkSources(&d)
	if d.Harnesses[0].Added[0].Source != nil || d.Harnesses[1].Added[0].Source != nil || d.Harnesses[1].Compare != "" {
		t.Errorf("linked %+v", d.Harnesses)
	}
	if len(d.SourceErrors) != 1 || !strings.Contains(d.SourceErrors[0], "Codex CLI 0.163.0: 404") {
		t.Errorf("source errors %v", d.SourceErrors)
	}
	if !strings.Contains(d.markdown(), "_Source links left out: Codex CLI 0.163.0: 404 Not Found._") {
		t.Errorf("drift.md does not say the links were left out:\n%s", d.markdown())
	}
	opened := false
	openSource = func(string, string) (io.ReadCloser, error) { opened = true; return nil, errors.New("no") }
	quiet := Drift{Harnesses: []HarnessDrift{{Harness: "codex", Name: "Codex CLI", Version: "0.163.0", Previous: "0.162.0"}}}
	linkSources(&quiet)
	if opened || quiet.Harnesses[0].Compare == "" {
		t.Errorf("a night with nothing to link read the source (%v), or left out the comparison", opened)
	}
}

// A Slack section cut where no line ends is not cut inside a link.
func TestClipKeepsLinksWhole(t *testing.T) {
	s := renderLinks(slackEscape("`k` · "+link("https://github.com/x#L1", "x.rs:1")), true)
	if got := clip(s, len("`k` · <https://git")); got != "`k` · \n…" {
		t.Errorf("clip = %q", got)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// What the real Codex tree does that a small one does not: a generic key named in hundreds
// of lines before the one beside its metric, a metric named by a constant, test modules
// inside source files, and a generic tag dropped from its metric but said elsewhere.
func TestSourceLinksOnARealShapedTree(t *testing.T) {
	before := map[string]string{
		"codex-rs/otel/src/metrics/names.rs": "pub const TOOL_CALL_METRIC: &str = \"codex.tool.call\";\n",
		"codex-rs/otel/src/events/telemetry.rs": "fn tool_call(name: &str) {\n    let mut tags = vec![];\n    tags.push((\"tool\", name));\n" +
			"    self.counter(TOOL_CALL_METRIC, 1, &tags);\n}\n",
		"codex-rs/exec/src/snapshot.rs": "fn record(state: &str) {\n    let tags = [(\"state\", state)];\n    metrics.counter(\"codex.shell_snapshot.command\", 1, &tags);\n}\n",
		// A metric only a test module still names: no build sends it.
		"codex-rs/proxy/src/policy.rs": "fn decide() {}\n\n#[cfg(test)]\nmod tests {\n    const LEGACY: &str = \"codex.proxy.block\";\n    fn t() { if x { y() } }\n}\n\nfn after_tests() {\n    emit(\"codex.proxy.allow\");\n}\n",
	}
	// "tool" and "state" said in 120 files that sort before the ones that matter.
	for i := range 120 {
		before[fmt.Sprintf("codex-rs/a%03d/src/lib.rs", i)] = "fn f() {\n    report(\"tool\", \"state\");\n}\n"
	}
	after := maps.Clone(before)
	after["codex-rs/exec/src/snapshot.rs"] = "fn record() {\n    metrics.counter(\"codex.shell_snapshot.command\", 1, &[]);\n}\n"
	withSource(t, map[string][]byte{"rust-v0.161.0": tarball(t, before), "rust-v0.162.0": tarball(t, after)})
	d := Drift{Harnesses: []HarnessDrift{{Harness: "codex", Name: "Codex CLI", Version: "0.162.0", Previous: "0.161.0",
		Added:   []FieldChange{{Surface: "metrics/codex.tool.call", Key: "tool"}},
		Removed: []FieldChange{{Surface: "metrics/codex.shell_snapshot.command", Key: "state"}},
		Unseen:  []GoneSurface{{Surface: "logs/codex.proxy.block"}, {Surface: "logs/codex.proxy.allow"}},
	}}}
	linkSources(&d)
	h := d.Harnesses[0]
	md := d.markdown()
	for _, want := range []string{
		// Beside the constant that names its metric, past the 120 other lines that say it.
		"`tool` on `metrics/codex.tool.call` · [telemetry.rs:3](",
		"`state` on `metrics/codex.shell_snapshot.command` · gone from 0.162.0's source, was in 0.161.0's: [snapshot.rs:2](",
		"`logs/codex.proxy.block` · named in neither build's source",
		"`logs/codex.proxy.allow` · still in 0.162.0's source: [policy.rs:10](",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("drift.md lacks %q:\n%s", want, md)
		}
	}
	if s := h.Added[0].Source; s == nil || s.More != 0 {
		t.Errorf("tool: %+v, want only the line beside its metric", s)
	}
}

// The build before unread, a field still in the new build's source beside its surface still
// says so; one named only away from its surface is left unlinked, not called gone.
func TestSourceLinksWithoutTheBuildBefore(t *testing.T) {
	withSource(t, map[string][]byte{"rust-v0.162.0": tarball(t, map[string]string{
		"codex-rs/exec/src/snapshot.rs": "fn record(state: &str) {\n    let tags = [(\"state\", state)];\n    metrics.counter(\"codex.shell_snapshot.command\", 1, &tags);\n}\n",
		"codex-rs/cli/src/doctor.rs":    "fn check() {\n    report(\"mode\", m);\n}\n",
		"codex-rs/core/src/turn.rs":     "fn run() {\n    span(\"turn\");\n}\n",
	})})
	d := Drift{Harnesses: []HarnessDrift{{Harness: "codex", Name: "Codex CLI", Version: "0.162.0", Previous: "0.161.0",
		Removed: []FieldChange{{Surface: "metrics/codex.shell_snapshot.command", Key: "state"}, {Surface: "traces/turn", Key: "mode"}},
	}}}
	linkSources(&d)
	h := d.Harnesses[0]
	if s := h.Removed[0].Source; s == nil || len(s.Refs) != 1 || !strings.Contains(s.note(allLinks), "still in 0.162.0's source") {
		t.Errorf("state: %+v", s)
	}
	if h.Removed[1].Source != nil {
		t.Errorf("mode, named only away from its span, was given a verdict: %+v", h.Removed[1].Source)
	}
	if len(d.SourceErrors) != 1 || !strings.Contains(string(mustJSON(d.slack())), "Source links left out: Codex CLI 0.161.0") {
		t.Errorf("source errors %v not said in Slack", d.SourceErrors)
	}
}

// A section the links would push past Slack's limit drops them, and keeps every finding;
// a section with room keeps one link a finding.
func TestSlackDropsLinksBeforeFindings(t *testing.T) {
	says := &SourceSays{Version: "0.162.0", Refs: []SourceRef{
		{Path: "codex-rs/a.rs", Line: 1, URL: "https://github.com/openai/codex/blob/rust-v0.162.0/codex-rs/" + strings.Repeat("d/", 80) + "a.rs#L1"},
		{Path: "codex-rs/b.rs", Line: 2, URL: "https://github.com/openai/codex/blob/rust-v0.162.0/codex-rs/b.rs#L2"}}}
	h := HarnessDrift{Harness: "codex", Name: "Codex CLI", Version: "0.162.0", Previous: "0.161.0"}
	for i := range 12 {
		h.Added = append(h.Added, FieldChange{Surface: "logs/codex.api_request", Key: fmt.Sprint("key_", i), Source: says})
	}
	h.Withheld = []FieldChange{{Surface: "logs/codex.api_request", Key: "the_one_to_act_on", Source: says}}
	big := string(mustJSON(Drift{Harnesses: []HarnessDrift{h}}.slack()))
	if !strings.Contains(big, "the_one_to_act_on") || strings.Contains(big, "a.rs:1") {
		t.Errorf("a section too long with links: %s", big)
	}
	h.Added = h.Added[:1]
	small := string(mustJSON(Drift{Harnesses: []HarnessDrift{h}}.slack()))
	if !strings.Contains(small, "|a.rs:1") || strings.Contains(small, "|b.rs:2") || !strings.Contains(small, "+1 more") {
		t.Errorf("a section with room: %s", small)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// A key as common as "model", named nowhere beside its surface, is left unlinked rather than
// linked to lines that have nothing to do with it; one named in a few lines is linked.
func TestSourceLinksLeaveACommonKeyUnplaced(t *testing.T) {
	files := map[string]string{"codex-rs/otel/src/macros.rs": "macro_rules! log_event {\n    () => { info!(model = %m) };\n}\n"}
	for i := range 10 {
		files[fmt.Sprintf("codex-rs/a%d/src/lib.rs", i)] = "fn f() {\n    report(\"model\", m);\n}\n"
	}
	files["codex-rs/mcp/src/sku.rs"] = "fn sku() {\n    tags.push((\"product_sku\", s));\n}\n"
	withSource(t, map[string][]byte{"rust-v0.162.0": tarball(t, files)})
	d := Drift{Harnesses: []HarnessDrift{{Harness: "codex", Name: "Codex CLI", Version: "0.162.0", Previous: "0.162.0",
		Added: []FieldChange{{Surface: "logs/codex.sse_event", Key: "model"}, {Surface: "logs/codex.mcp", Key: "product_sku"}}}}}
	linkSources(&d)
	if s := d.Harnesses[0].Added[0].Source; s != nil {
		t.Errorf("model linked by its name alone: %+v", s)
	}
	if s := d.Harnesses[0].Added[1].Source; s == nil || len(s.Refs) != 1 {
		t.Errorf("product_sku: %+v", s)
	}
}
