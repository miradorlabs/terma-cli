package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// tarball is a tree as GitHub serves one: every path under one folder.
func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
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
		"`codex.turn.phase` on `traces/turn` · gone from 0.162.0's source, was [turn.rs:2](https://github.com/openai/codex/blob/rust-v0.161.0/codex-rs/core/src/turn.rs#L2)",
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
