package main

import (
	"encoding/json"
	"path"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/e2e"
	"github.com/miradorlabs/terma-cli/e2e/configschema"
	"github.com/miradorlabs/terma-cli/e2e/sourcelink"
)

// A build's configuration: every option it reads and every environment variable, as its
// config section has them (reference/SCHEMA.md). A harness whose source is public has them
// from the source at its release's tag, read at refgen time with -source (no capture needed):
// its configuration schema where it keeps one, and the variables its code reads. A closed one
// has them from its binary facts: the settings schema its bundle declares, the variables its
// JavaScript reads. config.source says where the options came from, without the build's tag,
// so that two builds read the same way compare option by option.

// configSources are where each public harness keeps the definition of its options, tried in
// order: the first that reads as any option is the build's.
var configSources = map[string][]configSource{
	"codex": {
		{file: "codex-rs/core/config.schema.json", read: jsonSchema},
		{
			// Before the schema was generated: the fields of the struct config.toml deserializes into.
			what: "the ConfigToml struct",
			want: func(p string) bool {
				return strings.HasPrefix(p, "codex-rs/") && (strings.HasSuffix(p, "config_toml.rs") || strings.HasSuffix(p, "config/mod.rs") ||
					strings.HasSuffix(p, "core/src/config.rs"))
			},
			read: func(files map[string][]byte) (configschema.Options, string) {
				for _, p := range slices.Sorted(keys(files)) {
					if opts := configschema.FromRustStruct(files[p], "ConfigToml"); len(opts) > 0 {
						return opts, "ConfigToml in " + p
					}
				}
				return nil, ""
			},
		},
	},
	"gemini": {{file: "schemas/settings.schema.json", read: jsonSchema}},
	"pi":     {{file: "packages/coding-agent/schemas/settings.schema.json", read: jsonSchema}},
	"opencode": {{
		what: "the Effect Schema of its config (ConfigV1.Info)",
		want: func(p string) bool {
			return strings.HasSuffix(p, ".ts") && !strings.Contains(p, ".test.") &&
				(strings.HasPrefix(p, "packages/core/src/") || strings.HasPrefix(p, "packages/opencode/src/config/"))
		},
		read: func(files map[string][]byte) (configschema.Options, string) {
			// Where the CLI's config schema has been: core's v1 config, and before it the CLI's own.
			for _, root := range []string{"packages/core/src/v1/config/config.ts", "packages/opencode/src/config/config.ts"} {
				if opts := configschema.FromTSModules(files, root, "Info"); len(opts) > 0 {
					return opts, "the schema Info in " + root
				}
			}
			return nil, ""
		},
	}},
	"omp": {{
		what: "its register() calls",
		want: func(p string) bool {
			return strings.HasPrefix(p, "packages/coding-agent/src/") && strings.HasSuffix(p, ".ts") && !strings.Contains(p, ".test.")
		},
		read: func(files map[string][]byte) (configschema.Options, string) {
			return configschema.FromRegistry(files, "register"), "the settings packages/coding-agent/src registers"
		},
	}},
	"dsh": {{file: "docs/config-catalog.md", read: func(files map[string][]byte) (configschema.Options, string) {
		return configschema.FromConfigCatalog(files["docs/config-catalog.md"]), "docs/config-catalog.md (each plugin's config interface)"
	}}},
	"hermes": {{
		what: "DEFAULT_CONFIG",
		want: func(p string) bool { return p == "hermes_cli/config_defaults.py" || p == "hermes_cli/config.py" },
		read: func(files map[string][]byte) (configschema.Options, string) {
			for _, p := range []string{"hermes_cli/config_defaults.py", "hermes_cli/config.py"} {
				if opts := configschema.FromPythonDict(files[p], "DEFAULT_CONFIG"); len(opts) > 0 {
					return opts, "DEFAULT_CONFIG in " + p
				}
			}
			return nil, ""
		},
	}},
}

// configSource is one place a harness may keep its options: a file, or the files want takes,
// and how they read.
type configSource struct {
	file string
	what string
	want func(path string) bool
	read func(files map[string][]byte) (configschema.Options, string)
}

func jsonSchema(files map[string][]byte) (configschema.Options, string) {
	for _, p := range slices.Sorted(keys(files)) {
		if opts, err := configschema.FromJSONSchema(files[p]); err == nil && len(opts) > 0 {
			return opts, p
		}
	}
	return nil, ""
}

func keys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// sourceConfigs reads builds' configuration from their source, each build once a run.
type sourceConfigs struct {
	lib   *sourcelink.Library
	cache map[buildID]map[string]any
}

func newSourceConfigs(l *sourceLinker) *sourceConfigs {
	if l == nil {
		return nil
	}
	return &sourceConfigs{lib: l.lib, cache: map[buildID]map[string]any{}}
}

// of is the config section a build's source says, nil where its source cannot be read.
func (c *sourceConfigs) of(harness, version string) map[string]any {
	id := buildID{harness, version}
	if cfg, ok := c.cache[id]; ok {
		return cfg
	}
	cfg := readSourceConfig(c.lib, harness, version)
	c.cache[id] = cfg
	return cfg
}

func readSourceConfig(lib *sourcelink.Library, harness, version string) map[string]any {
	env, err := lib.EnvReads(harness, version)
	if err != nil {
		return nil // the source could not be read: nothing is known of it
	}
	cfg := map[string]any{"env_vars": stringsAny(env), "env_source": "the variables its source reads"}
	tried := []string{}
	for _, src := range configSources[harness] {
		want := src.want
		if want == nil {
			want = func(p string) bool { return p == src.file }
		}
		files, err := lib.Files(harness, version, want)
		if err != nil || len(files) == 0 {
			tried = append(tried, cmp(src.file, src.what))
			continue
		}
		opts, from := src.read(files)
		if len(opts) == 0 {
			tried = append(tried, cmp(src.file, src.what))
			continue
		}
		cfg["options"] = optionsAny(opts)
		cfg["source"] = from + " at " + sourcelink.Sources[harness].TagOf(version)
		return cfg
	}
	if len(tried) > 0 {
		cfg["source"] = "none: the source has no " + strings.Join(tried, ", nor ")
	}
	return cfg
}

func cmp(a, b string) string {
	if a != "" {
		return path.Base(a)
	}
	return b
}

// deriveSourceConfig puts what a build's source says of its configuration in its config
// section, over what its binary's strings said: the variables its code reads in place of the
// names its strings hold.
func (n night) deriveSourceConfig(s *Snapshot) {
	if n.sources == nil || !sourcelink.Has(s.Harness) {
		return
	}
	cfg := n.sources.of(s.Harness, s.Version)
	if cfg == nil {
		return
	}
	if s.Config == nil {
		s.Config = map[string]any{}
	}
	delete(s.Config, "env_vars_uncertain")
	for k, v := range cfg {
		s.Config[k] = v
	}
}

// configFromBinary shapes the config a build's binary facts hold: its settings schema's
// options as config.options, and how its variables were read.
func configFromBinary(harness string, cfg map[string]any) {
	if opts, ok := cfg["settings"]; ok {
		cfg["options"] = opts
		delete(cfg, "settings")
		cfg["source"] = map[string]string{
			"claude":  "the settings schema its bundle declares (zod, read from minified code)",
			"copilot": "the settings registry its script embeds",
		}[harness]
		if cfg["source"] == "" {
			cfg["source"] = "the settings schema its binary declares"
		}
	}
	if how, ok := cfg["env_source"]; ok {
		if how == "reads" {
			cfg["env_source"] = "the variables its JavaScript reads"
		}
	} else if _, ok := cfg["env_vars"]; ok {
		cfg["env_source"] = "the names its strings hold"
	}
}

// preferBinaryScenario takes a build's binary facts from its binary scenario (…ProfileBinary)
// where it has one: that scenario re-reads a build's facts that an earlier capture read with
// less.
func preferBinaryScenario(merged *e2e.Profile, ps []e2e.Profile) {
	for _, p := range ps {
		if p.Platform == merged.Platform && strings.HasSuffix(p.Scenario, "ProfileBinary") && len(p.Binary) > 0 {
			merged.Binary = p.Binary
		}
	}
}

// configNeedle is the name a config change is about, as the source would spell it: an
// option's last key, a variable's name.
func configNeedle(c *Change) string {
	if len(c.Path) < 3 {
		return ""
	}
	name := c.Path[2]
	if c.Path[1] == "options" {
		parts := strings.Split(name, ".")
		for i := len(parts) - 1; i >= 0; i-- {
			if k := strings.TrimSuffix(parts[i], "[]"); k != "<name>" && k != "" {
				return k
			}
		}
		return ""
	}
	return name
}

func stringsAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// optionsAny is options as a snapshot's generic JSON holds them.
func optionsAny(opts configschema.Options) map[string]any {
	data, _ := json.Marshal(opts)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return out
}
