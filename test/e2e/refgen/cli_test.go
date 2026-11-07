package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/e2e/refdiff"
)

// Commander's two columns, wrapped; an option whose head is too long for its column; notes.
const commanderHelp = `Usage: claude [options] [command] [prompt]

Claude Code - starts an interactive session by default, use -p/--print for
non-interactive output

Arguments:
  prompt                                Your prompt

Options:
  --add-dir <directories...>            Additional directories to allow tool
                                        access to
  --allowedTools, --allowed-tools <tools...>
      Comma or space-separated list of tool names to allow (e.g. "Bash(git *)
      Edit")
  --output-format <format>              Output format (only works with --print)
                                        (choices: "text", "json", "stream-json",
                                        default: "text")
  -p, --print                           Print response and exit
  -h, --help                            Display help for command

Commands:
  mcp                                   Configure and manage MCP servers
  doctor                                Check the health of your Claude Code
                                        installation
`

// clap's long form: names a line, the description below it, notes in brackets.
const clapHelp = `Run Codex non-interactively

Usage: codex exec [OPTIONS] [PROMPT]
       codex exec [OPTIONS] <COMMAND> [ARGS]

Commands:
  resume  Resume a previous session by id or pick the most recent with --last
  help    Print this message or the help of the given subcommand(s)

Arguments:
  [PROMPT]
          Initial instructions for the agent. If not provided as an argument (or if ` + "`-`" + ` is used),
          instructions are read from stdin

Options:
  -c, --config <key=value>
          Override a configuration value that would otherwise be loaded from config.toml.
          Use a dotted path to override nested values.

          Examples: - ` + "`-c model=\"o3\"`" + `

      --oss
          Use open-source provider

  -s, --sandbox <SANDBOX_MODE>
          Select the sandbox policy to use when executing model-generated shell commands

          [possible values: read-only, workspace-write, danger-full-access]

      --color <COLOR>
          Specifies color settings for use in the output

          [default: auto]
          [possible values: always, never, auto]
`

func TestItemizeCommander(t *testing.T) {
	cmds := itemizeCLI("claude", map[string]string{"--help": commanderHelp})
	c := cmds["claude"]
	if c == nil || !c.Helped || c.Usage[0] != "Usage: claude [options] [command] [prompt]" {
		t.Fatalf("claude = %+v", c)
	}
	if c.Description != "Claude Code - starts an interactive session by default, use -p/--print for non-interactive output" {
		t.Errorf("description = %q", c.Description)
	}
	want := map[string]cliOption{
		"--add-dir":       {Names: []string{"--add-dir"}, Value: "<directories...>", Description: "Additional directories to allow tool access to"},
		"--allowedTools":  {Names: []string{"--allowedTools", "--allowed-tools"}, Value: "<tools...>", Description: `Comma or space-separated list of tool names to allow (e.g. "Bash(git *) Edit")`},
		"--output-format": {Names: []string{"--output-format"}, Value: "<format>", Description: "Output format (only works with --print)", Default: "text", Choices: []string{"text", "json", "stream-json"}},
		"--print":         {Names: []string{"-p", "--print"}, Description: "Print response and exit"},
		"--help":          {Names: []string{"-h", "--help"}, Description: "Display help for command"},
	}
	for k, w := range want {
		if got := c.Options[k]; got == nil || !reflect.DeepEqual(*got, w) {
			t.Errorf("%s = %+v, want %+v", k, got, w)
		}
	}
	if len(c.Options) != len(want) {
		t.Errorf("options = %v", c.Options)
	}
	if c.Arguments["prompt"] == nil || c.Arguments["prompt"].Description != "Your prompt" {
		t.Errorf("arguments = %v", c.Arguments)
	}
	if cmds["claude mcp"] == nil || cmds["claude mcp"].Description != "Configure and manage MCP servers" || cmds["claude mcp"].Helped ||
		cmds["claude doctor"].Description != "Check the health of your Claude Code installation" {
		t.Errorf("listed commands = %+v %+v", cmds["claude mcp"], cmds["claude doctor"])
	}
}

func TestItemizeClap(t *testing.T) {
	cmds := itemizeCLI("codex", map[string]string{"exec --help": clapHelp})
	c := cmds["codex exec"]
	if c == nil || len(c.Usage) != 2 || c.Description != "Run Codex non-interactively" {
		t.Fatalf("exec = %+v", c)
	}
	if o := c.Options["--sandbox"]; o == nil || o.Value != "<SANDBOX_MODE>" || !reflect.DeepEqual(o.Names, []string{"-s", "--sandbox"}) ||
		!reflect.DeepEqual(o.Choices, []string{"read-only", "workspace-write", "danger-full-access"}) ||
		o.Description != "Select the sandbox policy to use when executing model-generated shell commands" {
		t.Errorf("--sandbox = %+v", o)
	}
	if o := c.Options["--color"]; o == nil || o.Default != "auto" || len(o.Choices) != 3 {
		t.Errorf("--color = %+v", o)
	}
	if o := c.Options["--config"]; o == nil || o.Description != "Override a configuration value that would otherwise be loaded from config.toml. Use a dotted path to override nested values.\nExamples: - `-c model=\"o3\"`" {
		t.Errorf("--config = %q", o.Description)
	}
	if o := c.Options["--oss"]; o == nil || o.Value != "" || o.Description != "Use open-source provider" {
		t.Errorf("--oss = %+v", o)
	}
	if c.Arguments["PROMPT"] == nil || cmds["codex exec resume"] == nil || cmds["codex exec help"] != nil {
		t.Errorf("arguments %v, commands %v", c.Arguments, cmds)
	}
}

// A narrower terminal wraps a description elsewhere: no change. A new allowed value, a moved
// default and a removed flag are each one.
func TestCLIChanges(t *testing.T) {
	wrapped := map[string]string{"exec --help": clapHelp}
	narrow := map[string]string{"exec --help": `Run Codex non-interactively

Usage: codex exec [OPTIONS] [PROMPT]
       codex exec [OPTIONS] <COMMAND> [ARGS]

Commands:
  resume  Resume a previous session by id or pick the most recent with
          --last
  help    Print this message or the help of the given subcommand(s)

Arguments:
  [PROMPT]
          Initial instructions for the agent. If not provided as an argument (or if ` + "`-`" + ` is
          used), instructions are read from stdin

Options:
  -c, --config <key=value>
          Override a configuration value that would otherwise be loaded from
          config.toml. Use a dotted path to override nested values.

          Examples: - ` + "`-c model=\"o3\"`" + `

      --oss
          Use open-source provider

  -s, --sandbox <SANDBOX_MODE>
          Select the sandbox policy to use when executing model-generated shell
          commands

          [possible values: read-only, workspace-write, danger-full-access]

      --color <COLOR>
          Specifies color settings for use in the output

          [default: auto]
          [possible values: always, never, auto]
`}
	snap := func(v string, help map[string]string) *Snapshot {
		cmds := map[string]any{}
		for k, c := range itemizeCLI("codex", help) {
			cmds[k] = jsonOf(c)
		}
		return &Snapshot{Harness: "codex", Version: v, CLI: map[string]any{"commands": cmds}}
	}
	if d := refdiff.New(snap("1", wrapped), snap("2", narrow), nil, time.Now()); len(d.Changes) != 0 {
		t.Errorf("rewrapping read as changes: %+v", d.Changes)
	}
	changed := map[string]string{"exec --help": clapHelp[:len(clapHelp)-len("auto]\n")] + "auto, ansi]\n"}
	changed["exec --help"] = strings.ReplaceAll(changed["exec --help"], "[default: auto]", "[default: never]")
	changed["exec --help"] = strings.ReplaceAll(changed["exec --help"], "      --oss\n          Use open-source provider\n\n", "")
	d := refdiff.New(snap("1", wrapped), snap("2", changed), nil, time.Now())
	titles := map[string]string{}
	for _, c := range d.Changes {
		titles[c.ID] = c.Title + " [" + c.Impact + "]"
	}
	for id, want := range map[string]string{
		"cli.commands.codex exec.options.--color.choices.ansi": "`codex exec --color` gained value `ansi` [notable]",
		"cli.commands.codex exec.options.--color.default":      "Default of `codex exec --color`: auto → never [notable]",
		"cli.commands.codex exec.options.--oss":                "Flag removed: `codex exec --oss` [notable]",
	} {
		if titles[id] != want {
			t.Errorf("%s = %q, want %q (all: %v)", id, titles[id], want, titles)
		}
	}
}
