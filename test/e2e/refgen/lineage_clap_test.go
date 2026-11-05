package main

import (
	"slices"
	"testing"
)

// A command line spread over two crates, as Codex's is: a root that flattens a struct from
// another crate (by an alias) and a newtype around one, global options, hidden ones, a
// subcommand enum with renamed, hidden and nested commands, and a platform's type alias.
func TestClapIndex(t *testing.T) {
	files := map[string][]byte{
		"codex-rs/cli/Cargo.toml":    []byte("[package]\nname = \"codex-cli\"\n"),
		"codex-rs/common/Cargo.toml": []byte("[package]\nname = \"codex-common\"\n"),
		"codex-rs/common/src/lib.rs": []byte(`
mod overrides;
pub use overrides::CliConfigOverrides;
`),
		"codex-rs/common/src/overrides.rs": []byte(`
use clap::Args;

#[derive(Args, Debug)]
pub struct CliConfigOverrides {
    /// Override a value; "quoted // not a comment".
    #[arg(short = 'c', long = "config", value_name = "key=value", global = true)]
    pub raw_overrides: Vec<String>,
}

#[derive(Args, Debug)]
pub struct Shared {
    #[arg(long, short = 'm')]
    pub model: Option<String>,
    #[arg(long = "image", short = 'i')]
    pub images: Vec<PathBuf>,
}
`),
		"codex-rs/cli/src/main.rs": []byte(`
use clap::Parser;
use codex_common::CliConfigOverrides as Overrides;
use codex_common::Shared;

/// Codex CLI
#[derive(Debug, Parser)]
#[clap(author, version, bin_name = "codex")]
struct MultitoolCli {
    #[clap(flatten)]
    pub config_overrides: Overrides,

    #[clap(flatten)]
    shared: Wrapped,

    /* a block comment with #[arg(long)] in it */
    #[arg(long = "hidden-thing", hide = true)]
    hidden: bool,

    #[clap(subcommand)]
    subcommand: Option<Subcommand>,
}

struct Wrapped(Shared);

#[derive(Debug, clap::Subcommand)]
enum Subcommand {
    /// Run non-interactively.
    #[clap(visible_alias = "e")]
    Exec(ExecCli),
    McpServer,
    #[clap(name = "cloud", alias = "cloud-tasks")]
    CloudTasks(CloudCli),
    #[clap(hide = true)]
    Internal(ExecCli),
    Sandbox(SandboxCommand),
}

#[derive(Debug, Parser)]
#[command(version)]
struct ExecCli {
    #[arg(long = "json", default_value_t = false)]
    json: bool,
    #[arg(long)]
    skip_git_repo_check: bool,
}

#[derive(Debug, Parser)]
struct CloudCli {
    #[command(subcommand)]
    cmd: Option<CloudCommand>,
}

#[derive(Debug, clap::Subcommand)]
enum CloudCommand {
    Exec { #[arg(long)] env: String },
    List,
}

#[derive(Debug, Parser)]
struct SandboxCommand {
    #[command(flatten)]
    host: HostArgs,
}

#[cfg(target_os = "linux")]
type HostArgs = LinuxArgs;
#[cfg(target_os = "macos")]
type HostArgs = MacArgs;

#[derive(Debug, Parser)]
struct LinuxArgs {
    #[arg(long)]
    landlock: bool,
}

#[derive(Debug, Parser)]
struct MacArgs {
    #[arg(long)]
    log_denials: bool,
}
`),
	}
	idx := newRustTree(files).clapIndex("MultitoolCli", "codex")
	want := []string{
		"cli.commands.codex.options.--config", "cli.commands.codex.options.--model", "cli.commands.codex.options.--image",
		"cli.commands.codex.options.--help", "cli.commands.codex.options.--version",
		"cli.commands.codex exec", "cli.commands.codex exec.options.--json", "cli.commands.codex exec.options.--skip-git-repo-check",
		"cli.commands.codex exec.options.--config", "cli.commands.codex exec.options.--help", "cli.commands.codex exec.options.--version",
		"cli.commands.codex mcp-server", "cli.commands.codex mcp-server.options.--config", "cli.commands.codex mcp-server.options.--help",
		"cli.commands.codex cloud", "cli.commands.codex cloud.options.--config", "cli.commands.codex cloud.options.--help",
		"cli.commands.codex cloud exec", "cli.commands.codex cloud exec.options.--env", "cli.commands.codex cloud exec.options.--config",
		"cli.commands.codex cloud exec.options.--help",
		"cli.commands.codex cloud list", "cli.commands.codex cloud list.options.--config", "cli.commands.codex cloud list.options.--help",
		"cli.commands.codex sandbox", "cli.commands.codex sandbox.options.--log-denials", "cli.commands.codex sandbox.options.--config",
		"cli.commands.codex sandbox.options.--help",
	}
	var got []string
	for id := range idx {
		got = append(got, id)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("index =\n%q\nwant\n%q", got, want)
	}
	if loc := idx["cli.commands.codex.options.--config"]; loc.file != "codex-rs/common/src/overrides.rs" || loc.line != 7 {
		t.Errorf("--config at %+v", loc)
	}
	if loc := idx["cli.commands.codex exec"]; loc.file != "codex-rs/cli/src/main.rs" || loc.line != 30 {
		t.Errorf("exec at %+v", loc)
	}
}

func TestKebabCamel(t *testing.T) {
	for in, want := range map[string]string{"AppServer": "app-server", "McpServer": "mcp-server", "StdioToUds": "stdio-to-uds",
		"SendMessageV2": "send-message-v2", "Exec": "exec"} {
		if got := kebab(in); got != want {
			t.Errorf("kebab(%s) = %s, want %s", in, got, want)
		}
	}
	if got := camel("app-server"); got != "AppServer" {
		t.Errorf("camel = %s", got)
	}
}

func TestSettingLine(t *testing.T) {
	schema := []byte(`{
  "properties": {
    "enabled": { "type": "boolean" },
    "agents": {
      "properties": {
        "enabled": { "type": "boolean" },
        "config_file": { "type": "string" }
      }
    }
  }
}`)
	if got := settingLine(schema, "agents.enabled"); got != 6 {
		t.Errorf("agents.enabled at line %d", got)
	}
	if got := settingLine(schema, "enabled"); got != 3 {
		t.Errorf("enabled at line %d", got)
	}
	if got := settingLeaf("agents.<name>.config_file"); got != "config_file" {
		t.Errorf("leaf = %s", got)
	}
	if got := settingLeaf("mcp_servers.<name>"); got != "mcp_servers" {
		t.Errorf("leaf = %s", got)
	}
}
