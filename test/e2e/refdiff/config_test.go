package refdiff

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func configSnapshot(t *testing.T, version, config string) *Snapshot {
	t.Helper()
	s := &Snapshot{SchemaVersion: SchemaVersion, Harness: "codex", Version: version}
	if err := json.Unmarshal([]byte(config), &s.Config); err != nil {
		t.Fatal(err)
	}
	return s
}

// Options compare key by key: each kind of change is its own line, and its impact says what
// someone relying on the harness would want to know of.
func TestConfigOptionsDiff(t *testing.T) {
	a := configSnapshot(t, "0.120.0", `{"source": "codex-rs/core/config.schema.json at rust-v0.120.0",
	  "env_source": "the variables its source reads", "env_vars": ["CODEX_HOME", "CODEX_API_KEY"],
	  "options": {
	    "model": {"type": "string", "description": "Model to use."},
	    "zsh_path": {"type": "string"},
	    "history.persistence": {"type": "string", "default": "save-all", "enum": ["save-all", "none"]},
	    "approval_policy": {"type": "string", "enum": ["untrusted", "on-request", "never"]},
	    "personality": {"type": "string"},
	    "code_mode": {"type": "boolean"},
	    "hooks": {"type": "object"},
	    "hooks.<name>": {"type": "array"},
	    "hooks.<name>[].command": {"type": "string"}
	  }}`)
	b := configSnapshot(t, "0.159.1", `{"source": "codex-rs/core/config.schema.json at rust-v0.159.1",
	  "env_source": "the variables its source reads", "env_vars": ["CODEX_HOME", "CODEX_ACCESS_TOKEN"],
	  "options": {
	    "model": {"type": "string", "description": "The model to use."},
	    "history.persistence": {"type": "string", "default": "none", "enum": ["save-all", "none", "summary"]},
	    "approval_policy": {"type": "string", "enum": ["on-request", "never"]},
	    "personality": {"type": "string", "deprecated": true},
	    "code_mode": {"type": "boolean|object"},
	    "sandbox_workspace_write.network_access": {"type": "boolean", "default": false},
	    "tui.theme": {"type": "string"},
	    "hooks": {"type": "object"},
	    "hooks.<name>": {"type": "array"}
	  }}`)
	d := New(a, b, nil, time.Time{})
	got := map[string]string{}
	for _, c := range d.Changes {
		got[c.Title] = c.Impact
		if c.Category != "config" {
			t.Errorf("%s: category %s", c.Title, c.Category)
		}
	}
	for title, impact := range map[string]string{
		"New setting: sandbox_workspace_write.network_access": "notable",
		"New setting: tui.theme":                              "informational",
		"Setting no longer read: zsh_path":                    "notable",
		"Default of history.persistence: save-all → none":     "notable",
		"Setting history.persistence gained value summary":    "informational",
		"Setting approval_policy lost value untrusted":        "notable",
		"Type of code_mode: boolean → boolean|object":         "notable",
		"Setting personality deprecated":                      "notable",
		"model description changed (+17 −13 chars)":           "informational",
		"New environment variable: CODEX_ACCESS_TOKEN":        "informational",
		"Environment variable no longer read: CODEX_API_KEY":  "informational",
	} {
		if g, ok := got[title]; !ok {
			t.Errorf("no change %q in %v", title, got)
		} else if g != impact {
			t.Errorf("%q impact %s, want %s", title, g, impact)
		}
	}
	for title := range got {
		if strings.Contains(title, "read from") {
			t.Errorf("a build's own tag read as a change: %s", title)
		}
		if strings.Contains(title, "hooks.<name>[].command") {
			t.Errorf("a table whose keys were not read reads as emptied: %s", title)
		}
	}
	if c := changeOf(d, "config.options.model.description"); c == nil || c.TextDiff == nil {
		t.Error("a description's change is no text diff")
	}
}

// Options read from different kinds of source, or variables read differently, are not compared
// one by one: the change is where they were read from.
func TestConfigSourcesDiffer(t *testing.T) {
	a := configSnapshot(t, "1", `{"env_vars": ["A", "B_STRINGS_ONLY"], "settings_keys": ["model"],
	  "options": {"model": {"type": "string"}}, "source": "ConfigToml in codex-rs/core/src/config.rs at rust-v1"}`)
	b := configSnapshot(t, "2", `{"env_vars": ["A"], "env_source": "the variables its source reads",
	  "options": {"model": {"type": "string"}, "tui.theme": {"type": "string"}}, "source": "codex-rs/core/config.schema.json at rust-v2"}`)
	d := New(a, b, nil, time.Time{})
	var titles []string
	for _, c := range d.Changes {
		titles = append(titles, c.Title)
	}
	want := []string{
		"Configuration options read from: ConfigToml in codex-rs/core/src/config.rs → codex-rs/core/config.schema.json",
		"Environment variables read from: the names its strings hold → the variables its source reads",
	}
	if strings.Join(titles, "\n") != strings.Join(want, "\n") {
		t.Errorf("changes =\n%s\nwant\n%s", strings.Join(titles, "\n"), strings.Join(want, "\n"))
	}
}

func changeOf(d *Diff, id string) *Change {
	for i := range d.Changes {
		if d.Changes[i].ID == id {
			return &d.Changes[i]
		}
	}
	return nil
}
