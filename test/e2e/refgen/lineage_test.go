package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// lineageSnap is a build with the given commands (each helped, with its options), settings and
// variables; nil leaves a part not observed.
func lineageSnap(version string, cmds map[string][]string, settings []string, env []string) *Snapshot {
	s := &Snapshot{SchemaVersion: schemaVersion, Harness: "codex", Version: version, CapturedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	if cmds != nil {
		cs := map[string]any{}
		for name, flags := range cmds {
			opts := map[string]any{}
			for _, f := range flags {
				opts[f] = map[string]any{"names": []any{f}}
			}
			cs[name] = map[string]any{"helped": true, "options": opts}
		}
		s.CLI = map[string]any{"commands": cs}
	}
	if settings != nil || env != nil {
		s.Config = map[string]any{"source": "codex-rs/core/config.schema.json at rust-v" + version, "env_source": "the variables its source reads"}
		if settings != nil {
			opts := map[string]any{}
			for _, k := range settings {
				opts[k] = map[string]any{"type": "string"}
			}
			s.Config["options"] = opts
		}
		if env != nil {
			s.Config["env_vars"] = stringsAny(env)
		}
	}
	return s
}

func lineageFrom(snaps ...*Snapshot) *Lineage {
	byVersion := map[string]*Snapshot{}
	var versions []string
	for _, s := range snaps {
		byVersion[s.Version] = s
		versions = append(versions, s.Version)
	}
	return lineageOf("codex", versions, byVersion)
}

func str(s string) *string { return &s }

func TestLineageRanges(t *testing.T) {
	l := lineageFrom(
		lineageSnap("1.0.0", map[string][]string{"codex": {"--old"}}, []string{"model"}, []string{"CODEX_HOME"}),
		lineageSnap("1.1.0", map[string][]string{"codex": {"--old", "--new"}, "codex exec": {"--json"}}, []string{"model"}, nil),
		// Not observed: no CLI, no config. It neither ends a range nor starts one.
		lineageSnap("1.2.0", nil, nil, nil),
		lineageSnap("1.3.0", map[string][]string{"codex": {"--new"}, "codex exec": {"--json"}}, []string{"model", "approval"}, []string{"CODEX_HOME"}),
		lineageSnap("1.4.0", map[string][]string{"codex": {"--new", "--old"}}, []string{"approval"}, []string{"CODEX_HOME"}),
	)
	if l.Window != (LineageWindow{Oldest: "1.0.0", Newest: "1.4.0", Builds: 5}) {
		t.Errorf("window = %+v", l.Window)
	}
	cases := map[string]LineageItem{
		// In the oldest build: "or earlier". Gone at 1.3.0 and back at 1.4.0: two ranges.
		"cli.commands.codex.options.--old": {Kind: kindOption, Ranges: []LineageRange{
			{Since: "1.0.0", Until: str("1.1.0"), Removed: str("1.3.0")}, {Since: "1.4.0"}}},
		"cli.commands.codex.options.--new": {Kind: kindOption, SinceBounded: true, Ranges: []LineageRange{{Since: "1.1.0"}}},
		"cli.commands.codex exec":          {Kind: kindSubcommand, SinceBounded: true, Ranges: []LineageRange{{Since: "1.1.0", Until: str("1.3.0"), Removed: str("1.4.0")}}},
		// 1.2.0 observed nothing: the range runs through it.
		"config.options.model":    {Kind: kindSetting, Ranges: []LineageRange{{Since: "1.0.0", Until: str("1.3.0"), Removed: str("1.4.0")}}},
		"config.options.approval": {Kind: kindSetting, SinceBounded: true, Ranges: []LineageRange{{Since: "1.3.0"}}},
		// 1.1.0 read no variables and 1.2.0 nothing: neither ends the range.
		"config.env_vars.CODEX_HOME": {Kind: kindEnvVar, Ranges: []LineageRange{{Since: "1.0.0"}}},
	}
	for id, want := range cases {
		got := l.Items[id]
		if got == nil {
			t.Errorf("%s: missing", id)
			continue
		}
		if !reflect.DeepEqual(jsonOf(got), jsonOf(&want)) {
			t.Errorf("%s = %s, want %s", id, jsonOf(got), jsonOf(&want))
		}
	}
	if _, ok := l.Items["cli.commands.codex"]; ok {
		t.Error("the root command is no subcommand")
	}
}

// A command whose help was not read says nothing of its options; one gone, where the command
// that listed it was read, is gone with them.
func TestLineageUnhelpedCommand(t *testing.T) {
	a := lineageSnap("1.0.0", map[string][]string{"codex": nil, "codex exec": {"--json"}}, nil, nil)
	b := lineageSnap("1.1.0", map[string][]string{"codex": nil, "codex exec": nil}, nil, nil)
	b.CLI["commands"].(map[string]any)["codex exec"].(map[string]any)["helped"] = false
	c := lineageSnap("1.2.0", map[string][]string{"codex": nil, "codex exec": {"--json"}}, nil, nil)
	d := lineageSnap("1.3.0", map[string][]string{"codex": nil}, nil, nil)
	l := lineageFrom(a, b, c, d)
	got := l.Items["cli.commands.codex exec.options.--json"]
	want := []LineageRange{{Since: "1.0.0", Until: str("1.2.0"), Removed: str("1.3.0")}}
	if got == nil || !reflect.DeepEqual(got.Ranges, want) {
		t.Errorf("--json = %+v", got)
	}
}

// A setting a minified reading missed (a nested key, a top-level one marked unobserved) is not
// observed: no range ends there.
func TestLineageApproximateSettings(t *testing.T) {
	snap := func(v string, keys ...string) *Snapshot {
		s := lineageSnap(v, nil, keys, nil)
		s.Config["source"] = "the settings schema its bundle declares (zod, read from minified code)"
		return s
	}
	a := snap("1.0.0", "hooks", "hooks.matcher", "model")
	b := snap("1.1.0", "hooks")
	b.Config["unobserved"] = []any{"model"}
	c := snap("1.2.0", "hooks", "hooks.matcher", "model")
	l := lineageFrom(a, b, c)
	for _, id := range []string{"config.options.hooks.matcher", "config.options.model"} {
		if got := l.Items[id]; got == nil || len(got.Ranges) != 1 || got.Ranges[0].Until != nil {
			t.Errorf("%s = %+v", id, got)
		}
	}
}

// A render keeps the sources the lineage published before found, and a source makes an item
// the builds leave open bounded.
func TestLineageKeepsSources(t *testing.T) {
	dir := t.TempDir()
	versions := []string{"1.0.0", "1.1.0"}
	byVersion := map[string]*Snapshot{
		"1.0.0": lineageSnap("1.0.0", map[string][]string{"codex": {"--old"}}, nil, nil),
		"1.1.0": lineageSnap("1.1.0", map[string][]string{"codex": {"--old", "--new"}}, nil, nil),
	}
	if err := writeLineage(dir, "codex", versions, byVersion, nil); err != nil {
		t.Fatal(err)
	}
	l, err := readLineage(dir)
	if err != nil || l == nil {
		t.Fatal(l, err)
	}
	src := &LineageSource{Tag: "rust-v0.42.0", Version: "0.42.0", Commit: "abc", URL: "https://github.com/openai/codex/commit/abc",
		SourceLocation: SourceLocation{File: "codex-rs/cli/src/main.rs", Line: 12, Permalink: "https://github.com/openai/codex/blob/rust-v0.42.0/codex-rs/cli/src/main.rs#L12"},
		Verified:       "absent at rust-v0.41.0, present at rust-v0.42.0"}
	l.Items["cli.commands.codex.options.--old"].Source = src
	if err := writeJSON(filepath.Join(dir, lineageFile), l); err != nil {
		t.Fatal(err)
	}
	// A later render, with no source to read, keeps it.
	byVersion["1.2.0"] = lineageSnap("1.2.0", map[string][]string{"codex": {"--old", "--new"}}, nil, nil)
	if err := writeLineage(dir, "codex", append(versions, "1.2.0"), byVersion, nil); err != nil {
		t.Fatal(err)
	}
	l, _ = readLineage(dir)
	got := l.Items["cli.commands.codex.options.--old"]
	if got.Source == nil || got.Source.Commit != "abc" || !got.SinceBounded || got.Ranges[0].Since != "0.42.0" {
		t.Errorf("--old = %s", jsonOf(got))
	}
	if l.Items["cli.commands.codex.options.--new"].Source != nil {
		t.Error("a source appeared from nowhere")
	}
}

// The same history is the same file, byte for byte, however many times it is rendered.
func TestLineageDeterministic(t *testing.T) {
	dir := t.TempDir()
	byVersion := map[string]*Snapshot{}
	var versions []string
	for _, v := range []string{"1.0.0", "1.0.1", "1.1.0"} {
		byVersion[v] = lineageSnap(v, map[string][]string{"codex": {"--a", "--b", "--c"}, "codex exec": {"--json"}, "codex mcp": nil},
			[]string{"model", "a.b", "c"}, []string{"Z", "A", "M"})
		versions = append(versions, v)
	}
	var first []byte
	for i := range 3 {
		if err := writeLineage(dir, "codex", versions, byVersion, nil); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(dir, lineageFile))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = data
		} else if !bytes.Equal(first, data) {
			t.Fatalf("render %d differs:\n%s\nvs\n%s", i, first, data)
		}
	}
	if bytes.Contains(first, []byte("generated_at")) {
		t.Error("a lineage says when it was made")
	}
}

// A nested setting is no older than its setting: first seen with it where its start is known,
// it starts there too; one seen later, where nested settings were not read before, stays "or
// earlier".
func TestLineageNestedBounded(t *testing.T) {
	l := &Lineage{Items: map[string]*LineageItem{
		"config.options.marketplaces":              {Kind: kindSetting, Ranges: []LineageRange{{Since: "2.1.232"}}, SinceBounded: true},
		"config.options.marketplaces.<name>":       {Kind: kindSetting, Ranges: []LineageRange{{Since: "2.1.232"}}},
		"config.options.marketplaces.<name>.auto":  {Kind: kindSetting, Ranges: []LineageRange{{Since: "2.1.232"}}},
		"config.options.marketplaces.<name>.later": {Kind: kindSetting, Ranges: []LineageRange{{Since: "2.1.250"}}},
		"config.options.old.nested":                {Kind: kindSetting, Ranges: []LineageRange{{Since: "2.1.206"}}},
		"config.options.old":                       {Kind: kindSetting, Ranges: []LineageRange{{Since: "2.1.206"}}},
	}}
	boundNested(l)
	for id, want := range map[string]bool{
		"config.options.marketplaces.<name>": true, "config.options.marketplaces.<name>.auto": true,
		"config.options.marketplaces.<name>.later": false, "config.options.old.nested": false,
	} {
		if got := l.Items[id].SinceBounded; got != want {
			t.Errorf("%s since_bounded = %v, want %v", id, got, want)
		}
	}
	if lineageParent("cli.commands.codex exec.options.--json") != "cli.commands.codex exec" {
		t.Errorf("option parent = %q", lineageParent("cli.commands.codex exec.options.--json"))
	}
}
