package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Environment variable names come out of a JavaScript bundle, and out of a Rust build's packed
// literals, without the constants, prefixes and stray bytes beside them.
func TestEnvVarsInOffline(t *testing.T) {
	js := `if(process.env.CLAUDE_CODE_USE_BEDROCK)x();let a=process.env;a.ANTHROPIC_DEFAULT_OPUS_MODEL=o.pin;` +
		`"set ANTHROPIC_API_KEY to use the API";e.CLAUDE_CODE_ENTRYPOINT="cli";var CLAUDE_CODE_TOOL_NAMES = {};` +
		`K[K["CLAUDE_CODE_EVENT_ID"] = 3] = "CLAUDE_CODE_EVENT_ID";process.env["CLAUDE_CODE_SHELL"]=1;` +
		"`${CLAUDE_CODE_}`;X_CLAUDE_CODE_NOT;FOO_DISABLE_TELEMETRY;DISABLE_AUTOUPDATER;" +
		`MCP_TIMEOUT"MCP_TIMEOUT3";BASH_VERSION;BASH_MAX_OUTPUT_LENGTH;`
	names, uncertain := EnvVarsIn([]byte(js), envPrefixes["claude"])
	want := []string{"ANTHROPIC_API_KEY", "ANTHROPIC_DEFAULT_OPUS_MODEL", "BASH_MAX_OUTPUT_LENGTH", "CLAUDE_CODE_ENTRYPOINT",
		"CLAUDE_CODE_SHELL", "CLAUDE_CODE_USE_BEDROCK", "DISABLE_AUTOUPDATER", "MCP_TIMEOUT"}
	if !slices.Equal(names, want) || len(uncertain) != 0 {
		t.Errorf("JavaScript: names %v, uncertain %v\nwant %v", names, uncertain, want)
	}

	rust := "\x00CODEX_API_KEYCODEX_ACCESS_TOKEN\x00CODEX_CA_CERTIFICATESSL_CERT_FILE\x00CODEX_HOME\x00" +
		"OPENAI_API_KEYT3B\x00CODEX_SESSION_IDLoading\x00CODEX_SQLITE_HOMEfailed to open\x00CODEX_SQLITE_HOME\x00"
	names, uncertain = EnvVarsIn([]byte(rust), envPrefixes["codex"])
	if want := []string{"CODEX_ACCESS_TOKEN", "CODEX_API_KEY", "CODEX_CA_CERTIFICATE", "CODEX_HOME", "CODEX_SQLITE_HOME", "OPENAI_API_KEYT3B"}; !slices.Equal(names, want) {
		t.Errorf("Rust: names %v, want %v", names, want)
	}
	if !slices.Equal(uncertain, []string{"CODEX_SESSION_IDL"}) {
		t.Errorf("Rust: uncertain %v", uncertain)
	}
	if names, _ := EnvVarsIn([]byte("OPENAI_API_KEY"), envPrefixes["pi"]); len(names) != 0 {
		t.Errorf("a short prefix inside a name was taken for one: %v", names)
	}
}

// Claude Code's settings keys are the run of zod thunks around the anchor, and no other.
func TestSettingsKeysInOffline(t *testing.T) {
	desc := `.optional().describe("a description")`
	bundle := `x={getter:()=>A()}` + string(make([]byte, 9000)) +
		`S=A().object({$schema:()=>A().literal("x")` + desc + `,apiKeyHelper:()=>A().string()` + desc +
		`,cleanupPeriodDays:()=>A().int()` + desc + `,permissions:()=>A().object({allow:A().array()})` + desc + `})` +
		string(make([]byte, 9000)) + `,remote:()=>A()`
	got := SettingsKeysIn([]byte(bundle), "cleanupPeriodDays")
	if want := []string{"$schema", "apiKeyHelper", "cleanupPeriodDays", "permissions"}; !slices.Equal(got, want) {
		t.Errorf("settings keys = %v, want %v", got, want)
	}
	if SettingsKeysIn([]byte(bundle), "nope") != nil {
		t.Error("keys without the anchor")
	}
}

func TestBundledVersionsOffline(t *testing.T) {
	js := "getUserAgent(){return`Anthropic/JS ${VE}`}x;var VE=\"0.127.0\",Q=\"solaris\";getUserAgent(){return`AnthropicBedrock/JS ${VE}`}"
	if got := JSSDKsIn([]byte(js)); len(got) != 1 || got["@anthropic-ai/sdk"] != "0.127.0" {
		t.Errorf("JS SDKs = %v", got)
	}
	rust := "/index.crates.io-1/tokio-1.52.3/src/a.rs\x00/index.crates.io-1/tokio-1.52.3/src/b.rs\x00/x/tokio-1.40.0/src/c.rs" +
		"/x/tokio-rustls-0.26.4/src/d.rs\x00/x/rustls-0.23.45/src/e.rs\x00/x/serde-1.0.0/src/f.rs"
	if got := RustCratesIn([]byte(rust)); len(got) != 2 || got["tokio"] != "1.52.3" || got["rustls"] != "0.23.45" {
		t.Errorf("Rust crates = %v", got)
	}
}

// A compiled build's facts: its digest and size, and the runtime its strings name.
func TestBinaryFactsOfOffline(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "claude")
	data := []byte("\xcf\xfa\xed\xfe bun/1.4.3 npm/? node/v26.3.0 darwin arm64\x00process.env.CLAUDE_CODE_X\x00")
	if err := os.WriteFile(exe, data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	f, err := BinaryFactsOf(Binary{Harness: "claude", Path: filepath.Join(dir, "link")})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if f.Form != "executable" || f.Runtime != "bun 1.4.3" || f.NodeCompat != "v26.3.0" || f.SizeBytes != int64(len(data)) ||
		f.SHA256 != hex.EncodeToString(sum[:]) || !slices.Equal(f.EnvVars, []string{"CLAUDE_CODE_X"}) ||
		!slices.Equal(f.Heuristic, []string{"runtime", "env_vars"}) {
		t.Errorf("facts = %+v", f)
	}

	// An npm package: its engines, installed SDKs, and its source's variables.
	pkg := filepath.Join(dir, "node_modules", "@x", "agent")
	writeFile(t, filepath.Join(pkg, "package.json"), `{"engines":{"node":">=20"}}`)
	writeFile(t, filepath.Join(pkg, "dist", "cli.js"), "#!/usr/bin/env node\nprocess.env.GEMINI_API_KEY\n")
	writeFile(t, filepath.Join(dir, "node_modules", "@google", "genai", "package.json"), `{"version":"2.21.0"}`)
	f, err = BinaryFactsOf(Binary{Harness: "gemini", Path: filepath.Join(pkg, "dist", "cli.js")})
	if err != nil {
		t.Fatal(err)
	}
	if f.Form != "package" || f.Runtime != "node" || f.Engines != ">=20" || f.SDK["@google/genai"] != "2.21.0" ||
		!slices.Equal(f.EnvVars, []string{"GEMINI_API_KEY"}) {
		t.Errorf("package facts = %+v", f)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestHelpCommandsOffline(t *testing.T) {
	commander := `Usage: claude [options] [command] [prompt]

Options:
  -d, --debug                Enable debug

Commands:
  agents [options]           Manage background agents
  attach <id>                Open a background session in this
                             terminal. <id> is the short id
  plugin|plugins             Manage plugins
  help [command]             display help for command
`
	clap := "Usage: codex [OPTIONS] [PROMPT]\n\nCommands:\n  exec        Run Codex non-interactively [aliases: e]\n  login       Manage login\n  help        Print this message\n\nArguments:\n  [PROMPT]\n"
	yargs := "Usage: gemini [options] [command]\n\nCommands:\n  gemini [query..]             Launch Gemini CLI  [default]\n  gemini mcp                   Manage MCP servers\n  gemini extensions <command>  Manage extensions\n"
	argparse := "usage: hermes [-h]\n              {chat,model,setup}\n              ...\n\npositional arguments:\n  {chat,model,setup}\n    chat    Interactive chat\n"
	for _, c := range []struct {
		help, program string
		want          []string
	}{
		{commander, "claude", []string{"agents", "attach", "plugin"}},
		{clap, "codex", []string{"exec", "login"}},
		{yargs, "gemini", []string{"mcp", "extensions"}},
		{argparse, "hermes", []string{"chat", "model", "setup"}},
		{"Usage: dsh [--profile] <name>\n\nOptions:\n  -V  version\n", "dsh", nil},
	} {
		if got := HelpCommands(c.help, c.program); !slices.Equal(got, c.want) {
			t.Errorf("%s: commands %v, want %v", c.program, got, c.want)
		}
	}
}
