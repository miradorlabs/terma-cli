package e2e

import (
	"bytes"
	"encoding/json"
	"maps"
	"regexp"
	"slices"

	"github.com/miradorlabs/terma-cli/e2e/configschema"
)

// What a closed harness's bundle says of its configuration: Claude Code's settings schema, the
// zod object it validates settings.json with, and the Copilot CLI's settings registry, the list
// its settings dialog and `copilot config` work from; and for either, the environment
// variables its code reads. Both are read out of minified JavaScript: they are what the bundle
// declares, read without running it, and BinaryFacts marks them heuristic.

// claudeSettingsAnchor is the description of the first key of Claude Code's settings schema.
var claudeSettingsAnchor = []byte("JSON Schema reference for Claude Code settings")

// ClaudeSettingsIn are the options of the Claude Code settings schema bundled in data: the
// object of zod schemas whose first key is $schema, each key's type, description, default and
// values, nested objects' keys dotted. None where the bundle has no such object as text (a
// build compiled to bytecode keeps only its strings).
func ClaudeSettingsIn(data []byte) configschema.Options {
	var best configschema.Options
	for i := 0; ; {
		j := bytes.Index(data[i:], claudeSettingsAnchor)
		if j < 0 {
			break
		}
		at := i + j
		i = at + len(claudeSettingsAnchor)
		// The object opens just before its first key: `{$schema:` or `{"$schema":`.
		lo := max(0, at-400)
		start := -1
		for _, key := range [][]byte{[]byte("{$schema:"), []byte(`{"$schema":`)} {
			if k := bytes.LastIndex(data[lo:at], key); k >= 0 && lo+k > start {
				start = lo + k
			}
		}
		if start < 0 {
			continue
		}
		if opts := configschema.FromBundle(data, start); len(opts) > len(best) {
			best = opts
		}
	}
	return best
}

// copilotRegistryRE is the start of the Copilot CLI's settings registry: a JSON array of
// {"path","leafKind","description"} entries embedded in its script.
var copilotRegistryRE = regexp.MustCompile(`\[\{"path":"[^"]+","leafKind":"`)

// CopilotSettingsIn are the options of the Copilot CLI settings registry in js, its script:
// each setting's path, kind, description and, for a choice, its values.
func CopilotSettingsIn(js []byte) configschema.Options {
	var best configschema.Options
	for _, loc := range copilotRegistryRE.FindAllIndex(js, -1) {
		var entries []struct {
			Path        string   `json:"path"`
			LeafKind    string   `json:"leafKind"`
			Description string   `json:"description"`
			Options     []string `json:"options"`
		}
		dec := json.NewDecoder(bytes.NewReader(js[loc[0]:]))
		if dec.Decode(&entries) != nil {
			continue
		}
		opts := configschema.Options{}
		for _, e := range entries {
			o := configschema.Option{Type: copilotKinds[e.LeafKind], Description: e.Description}
			if o.Type == "" {
				o.Type = e.LeafKind
			}
			for _, v := range e.Options {
				o.Enum = append(o.Enum, v)
			}
			opts[e.Path] = o
		}
		if len(opts) > len(best) {
			best = opts
		}
	}
	return best
}

// copilotKinds are the registry's kinds of leaf as an option's type.
var copilotKinds = map[string]string{"boolean": "boolean", "string": "string", "number": "number", "enum": "string",
	"string-array": "array", "json": "", "record-of-object": "object", "record-of-boolean": "object"}

// JSEnvReadsIn are the environment variables a JavaScript bundle reads: `process.env.X` and
// `process.env["X"]`, and, since a bundle mostly reads its own variables through a name it
// keeps process.env under (`a.CLAUDE_CODE_ENTRYPOINT`), any property that starts with one of
// prefixes, the harness's own.
func JSEnvReadsIn(js []byte, prefixes []string) []string {
	seen := map[string]bool{}
	envName := func(at int) string {
		end := at
		for end < len(js) && (isEnvChar(js[end]) || js[end] >= 'a' && js[end] <= 'z') {
			end++
		}
		if end == at || js[at] >= '0' && js[at] <= '9' {
			return ""
		}
		return string(js[at:end])
	}
	marker := []byte("process.env")
	for i := 0; ; {
		j := bytes.Index(js[i:], marker)
		if j < 0 {
			break
		}
		at := i + j + len(marker)
		i = at
		switch {
		case at+1 < len(js) && js[at] == '.':
			if name := envName(at + 1); name != "" {
				seen[name] = true
			}
		case at+2 < len(js) && js[at] == '[' && (js[at+1] == '"' || js[at+1] == '\'' || js[at+1] == '`'):
			if name := envName(at + 2); name != "" && at+2+len(name) < len(js) && js[at+2+len(name)] == js[at+1] {
				seen[name] = true
			}
		}
	}
	for _, p := range prefixes {
		dotted := []byte("." + p)
		for i := 0; ; {
			j := bytes.Index(js[i:], dotted)
			if j < 0 {
				break
			}
			at := i + j
			i = at + len(dotted)
			if at == 0 || !isJSIdent(js[at-1]) {
				continue // a property of an expression's value, not a name's: no env
			}
			end := at + 1
			for end < len(js) && isEnvChar(js[end]) {
				end++
			}
			if end < len(js) && isJSIdent(js[end]) {
				continue // runs into lowercase: an identifier, not a variable
			}
			if name := string(js[at+1 : end]); plausibleEnvVar(name, p) {
				seen[name] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

func isEnvChar(c byte) bool { return c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' }

func isJSIdent(c byte) bool {
	return isEnvChar(c) || c >= 'a' && c <= 'z' || c == '$' || c >= 0x80
}
