package sourcelink

import (
	"slices"
	"strings"
	"testing"
)

// The variables each language's code reads, by name or by a constant that holds one; a test, a
// test module, a comment and a variable only set name none.
func TestEnvReads(t *testing.T) {
	cases := []struct {
		lang  string
		files map[string]string
		want  []string
	}{
		{"rust", map[string]string{
			"codex-rs/core/src/home.rs": "const CODEX_HOME_ENV: &str = \"CODEX_HOME\";\n" +
				"fn home() { std::env::var(CODEX_HOME_ENV).ok(); env::var_os(\"CODEX_SANDBOX\"); }\n" +
				"// std::env::var(\"IN_COMMENT\")\n" +
				"fn child(c: &mut Command) { c.env(\"ONLY_SET\", \"1\"); }\n" +
				"const BUILT: &str = env!(\"CARGO_PKG_VERSION\");\n" +
				"#[arg(long, env = \"CODEX_API_KEY\")]\n" +
				"#[cfg(test)]\nmod tests {\n  fn t() { std::env::var(\"IN_TEST_MODULE\"); }\n}\n",
			"codex-rs/core/tests/it.rs": "fn t() { std::env::var(\"IN_TEST_FILE\"); }\n",
			"codex-rs/login/src/env.rs": "pub const KEY_ENV: &str = \"OPENAI_API_KEY\";\n" +
				"fn env_var_present(name: &str) -> bool {\n    std::env::var(name).is_ok()\n}\n" +
				"fn telemetry() { env_var_present(KEY_ENV); env_var_present(\"CODEX_REFRESH_URL\"); }\n",
			"codex-rs/rmcp-client/src/bin/test_stdio_server.rs": "fn main() { std::env::var(\"MCP_TEST_PID_FILE\"); }\n",
			"codex-rs/linux-sandbox/build.rs":                   "fn main() { std::env::var(\"TARGET\"); }\n",
			// A wrapper called with a constant from another module, by reference.
			"codex-rs/login/src/auth.rs": "pub const CODEX_ACCESS_TOKEN_ENV_VAR: &str = \"CODEX_ACCESS_TOKEN\";\n" +
				"fn read_non_empty_env_var(key: &str) -> Option<String> {\n    std::env::var(key).ok()\n}\n" +
				"fn token() { read_non_empty_env_var(&crate::auth::CODEX_ACCESS_TOKEN_ENV_VAR); }\n",
		}, []string{"CARGO_PKG_VERSION", "CODEX_ACCESS_TOKEN", "CODEX_API_KEY", "CODEX_HOME", "CODEX_REFRESH_URL", "CODEX_SANDBOX", "OPENAI_API_KEY"}},
		{"typescript", map[string]string{
			"packages/core/src/env.ts": "export const KEY_VAR = 'GEMINI_API_KEY';\n" +
				"const a = process.env.GEMINI_MODEL ?? process.env['GOOGLE_API_KEY'];\n" +
				"const b = process.env[KEY_VAR];\n" +
				"/* process.env.IN_COMMENT */\n",
			"packages/core/src/env.test.ts": "process.env.IN_TEST = '1';\n",
		}, []string{"GEMINI_API_KEY", "GEMINI_MODEL", "GOOGLE_API_KEY"}},
		{"python", map[string]string{
			"hermes_cli/config.py": "import os\nHOME_VAR = \"HERMES_HOME\"\n" +
				"home = os.getenv(HOME_VAR)\nkey = os.environ.get('OPENROUTER_API_KEY')\n" +
				"if \"HERMES_DEBUG\" in os.environ:\n    pass\nx = os.environ[\"HERMES_PROFILE\"]\n" +
				"# os.getenv(\"IN_COMMENT\")\n",
			"tests/test_config.py": "os.getenv(\"IN_TEST\")\n",
		}, []string{"HERMES_DEBUG", "HERMES_HOME", "HERMES_PROFILE", "OPENROUTER_API_KEY"}},
	}
	for _, c := range cases {
		t.Run(c.lang, func(t *testing.T) {
			withSource(t, "env-"+c.lang, Source{Repo: "o/env-" + c.lang, Tag: "v%s", Lang: c.lang})
			lib, _ := fakeLibrary(t, "", map[string][]byte{"o/env-" + c.lang + "@v1": tarball(t, c.files)})
			got, err := lib.EnvReads("env-"+c.lang, "1")
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("EnvReads = %v, want %v", got, c.want)
			}
		})
	}
}

// Files reads any file the caller asks for, shipped or not: a schema generated into docs/.
func TestFiles(t *testing.T) {
	withSource(t, "files", Source{Repo: "o/files", Tag: "v%s", Dirs: []string{"packages/"}, Lang: "typescript"})
	lib, _ := fakeLibrary(t, "", map[string][]byte{"o/files@v1": tarball(t, map[string]string{
		"schemas/settings.schema.json": "{}",
		"packages/core/src/a.ts":       "x",
	})})
	got, err := lib.Files("files", "1", func(p string) bool { return strings.HasSuffix(p, ".json") })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || string(got["schemas/settings.schema.json"]) != "{}" {
		t.Errorf("Files = %v", got)
	}
}
