package refdiff

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// fsProbe is a files profile as a snapshot holds it, from JSON.
func fsProbe(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// What changed in the filesystem section, said in a line each: paths read and written, commands
// started, programs looked for, decoys touched, sockets, and macOS's own; and what is no change:
// counts, a run's temporary names, what builds either side have and a run missed, a platform
// one build was not traced on.
func TestFilesystemChanges(t *testing.T) {
	a := fsProbe(t, `{"platform": "linux/amd64", "tracer": "strace",
	  "paths": {
	    "<home>/.claude.json": {"ops": ["read"], "phases": ["startup"]},
	    "<repo>/CLAUDE.md": {"ops": ["probe"], "phases": ["startup"]},
	    "<claude-config>/statsig": {"ops": ["write"], "phases": ["startup"]},
	    "<claude-config>/.claude.json.lock": {"ops": ["create"], "phases": ["startup"]},
	    "<claude-config>/cache/<n>.json": {"ops": ["write"], "phases": ["exit"]},
	    "<home>/.config/sometimes": {"ops": ["read"], "phases": ["exit"]}
	  },
	  "exec": {
	    "git status --porcelain": {"argv": ["git", "status", "--porcelain"], "phases": ["startup"], "count": 2, "by": "claude",
	      "parents": ["bash -c x"], "role": "tool"},
	    "uname -s": {"argv": ["uname", "-s"], "phases": ["startup"], "count": 1, "by": "sh"},
	    "rg --files": {"argv": ["rg", "--files"], "phases": ["startup"], "count": 1, "by": "claude"},
	    "/bin/sh -c [ -x 'terma' ] && 'terma' hook --user stop || true": {"argv": ["/bin/sh", "-c", "…"], "phases": ["exit"], "count": 1, "by": "claude"},
	    "terma hook --user stop": {"argv": ["terma", "hook", "--user", "stop"], "phases": ["exit"], "count": 1, "by": "sh"},
	    "bash -c -l S=1\necho old": {"argv": ["bash", "-c", "-l", "S=1\necho old"], "phases": ["turn"], "count": 1, "by": "claude"}
	  },
	  "lookups": ["git", "node"],
	  "decoys": {"<home>/.aws/credentials": "not touched", "<home>/.ssh/id_ed25519": "stat", "<home>/.ssh/agent.sock": "not touched"},
	  "sockets": {"listening": {}, "connections": {"tcp <provider>": {"role": "provider", "phases": ["turn"], "count": 3}},
	              "unix": {}, "dns": []},
	  "dropped": {"libraries": 300}}`)
	b := fsProbe(t, `{"platform": "linux/amd64", "tracer": "strace",
	  "paths": {
	    "<home>/.claude.json": {"ops": ["read", "write"], "phases": ["startup", "exit"]},
	    "<home>/.gitconfig": {"ops": ["read"], "phases": ["startup"]},
	    "<home>/.zshrc": {"ops": ["write"], "phases": ["turn"]},
	    "<claude-config>/statsig": {"ops": ["write"], "phases": ["turn"], "programs": ["git"]},
	    "<claude-config>/cache/<n>.json": {"ops": ["read"], "phases": ["exit"]},
	    "<tmp>/claude-<id>-cwd": {"ops": ["write"], "phases": ["turn"]}
	  },
	  "exec": {
	    "git status --porcelain": {"argv": ["git", "status", "--porcelain"], "phases": ["startup", "exit"], "count": 5, "by": "claude",
	      "parents": [], "role": "harness", "purpose": "reads the repository's state for the model's context"},
	    "uname -s": {"argv": ["uname", "-s"], "phases": ["startup"], "count": 1, "by": "sh", "parents": ["sh -c uname -s"],
	      "role": "harness", "purpose": "detects the platform and installed package managers"},
	    "git log -n 5": {"argv": ["git", "log", "-n", "5"], "phases": ["startup"], "count": 1, "by": "claude"},
	    "bash -c -l S=1\necho new": {"argv": ["bash", "-c", "-l", "S=1\necho new"], "phases": ["turn"], "count": 1, "by": "claude"}
	  },
	  "lookups": ["git", "rg"],
	  "decoys": {"<home>/.aws/credentials": "read", "<home>/.ssh/id_ed25519": "stat", "<home>/.ssh/agent.sock": "connected", "<home>/.npmrc": "read"},
	  "sockets": {"listening": {"tcp 127.0.0.1:<ephemeral>": {"phases": ["startup"], "count": 1}, "tcp 0.0.0.0:7777": {"phases": ["turn"], "count": 1}},
	              "connections": {"tcp <provider>": {"role": "provider", "phases": ["turn"], "count": 9}, "tcp <remote>:443": {"phases": ["startup"], "count": 1}},
	              "unix": {"/var/run/docker.sock": {"role": "docker", "ops": ["connect"], "phases": ["startup"], "count": 1}, "<tmp>/cc-socks/<n>.sock": {"ops": ["bind"], "phases": ["startup"], "count": 1}},
	              "dns": ["statsig.anthropic.com"]},
	  "dropped": {"libraries": 400},
	  "unobserved": {"paths": ["<home>/.config/sometimes"]}}`)
	mac := fsProbe(t, `{"platform": "darwin/arm64", "tracer": "eslogger", "paths": {}, "exec": {}, "lookups": [], "decoys": {},
	  "sockets": {"listening": {}, "connections": {}, "unix": {}, "dns": []},
	  "macos": {"xpc": {"com.apple.securityd.xpc": {"phases": ["startup"], "count": 1}, "com.apple.cfprefsd.agent": {"phases": ["startup"], "count": 1}},
	            "launch_items": {"<home>/Library/LaunchAgents/com.example.plist": {"phases": ["exit"], "count": 1}},
	            "tcc": {}, "task_ports": {"/System/Library/CoreServices/Finder.app/Contents/MacOS/Finder": {"phases": ["turn"], "count": 1}}}}`)
	macBefore := fsProbe(t, `{"platform": "darwin/arm64", "tracer": "eslogger", "paths": {}, "exec": {}, "lookups": [], "decoys": {},
	  "sockets": {"listening": {}, "connections": {}, "unix": {}, "dns": []},
	  "macos": {"xpc": {}, "launch_items": {}, "tcc": {}, "task_ports": {}}}`)
	from := &Snapshot{Harness: "claude", Version: "1.0.0", Filesystem: map[string]any{"linux/amd64": a, "darwin/arm64": macBefore}}
	to := &Snapshot{Harness: "claude", Version: "1.0.1", Filesystem: map[string]any{"linux/amd64": b, "darwin/arm64": mac,
		"linux/arm64": a}} // a platform the older build was not traced on: no change
	d := New(from, to, nil, time.Now())
	byID := map[string]Change{}
	for _, c := range d.Changes {
		byID[c.ID] = c
		if c.Category != "filesystem" {
			t.Errorf("change %s in category %s", c.ID, c.Category)
		}
	}
	const L = "filesystem.linux/amd64."
	const M = "filesystem.darwin/arm64."
	for id, want := range map[string]struct{ title, impact string }{
		L + "paths.<home>/.claude.json.ops.write":                                            {"Now writes ~/.claude.json (linux/amd64)", "notable"},
		L + "paths.<home>/.gitconfig":                                                        {"Now reads ~/.gitconfig (linux/amd64)", "informational"},
		L + "paths.<home>/.zshrc":                                                            {"Now writes ~/.zshrc (linux/amd64)", "notable"},
		L + "paths.<repo>/CLAUDE.md":                                                         {"No longer looks for <repo>/CLAUDE.md (linux/amd64)", "informational"},
		L + "paths.<claude-config>/cache/<n>.json.ops.read":                                  {"Now reads <claude-config>/cache/<n>.json (linux/amd64)", "informational"},
		L + "paths.<claude-config>/cache/<n>.json.ops.write":                                 {"No longer writes <claude-config>/cache/<n>.json (linux/amd64)", "informational"},
		L + "exec.git log -n 5":                                                              {"New command at startup: `git log -n 5` (linux/amd64)", "notable"},
		L + "exec.rg --files":                                                                {"Command no longer run: `rg --files` (linux/amd64)", "informational"},
		L + "exec.git status --porcelain.phases.exit":                                        {"`git status --porcelain` now also runs at exit (linux/amd64)", "informational"},
		L + "exec.git status --porcelain.role":                                               {"`git status --porcelain` now runs as the build's own doing (was part of the tool call) (linux/amd64)", "notable"},
		L + "lookups.rg":                                                                     {"Now looks for the program rg on the PATH (linux/amd64)", "notable"},
		L + "lookups.node":                                                                   {"No longer looks for the program node on the PATH (linux/amd64)", "informational"},
		L + "decoys.<home>/.aws/credentials":                                                 {"Reads ~/.aws/credentials (a planted decoy) (linux/amd64)", "notable"},
		L + "decoys.<home>/.ssh/agent.sock":                                                  {"Connects to the decoy ssh-agent socket ~/.ssh/agent.sock (linux/amd64)", "notable"},
		L + "sockets.listening.tcp 127.0.0.1:<ephemeral>":                                    {"Now listens on 127.0.0.1, an ephemeral port (tcp) (linux/amd64)", "notable"},
		L + "sockets.listening.tcp 0.0.0.0:7777":                                             {"Now listens on 0.0.0.0:7777 (tcp) (linux/amd64)", "notable"},
		L + "sockets.connections.tcp <remote>:443":                                           {"Now connects to <remote>:443 (tcp) (linux/amd64)", "notable"},
		L + "sockets.unix./var/run/docker.sock":                                              {"Now connects to the Unix socket /var/run/docker.sock (docker) (linux/amd64)", "notable"},
		L + "sockets.dns.statsig.anthropic.com":                                              {"Now looks up statsig.anthropic.com (linux/amd64)", "notable"},
		M + "macos.xpc.com.apple.securityd.xpc":                                              {"Now connects to the XPC service com.apple.securityd.xpc (darwin/arm64)", "notable"},
		M + "macos.xpc.com.apple.cfprefsd.agent":                                             {"Now connects to the XPC service com.apple.cfprefsd.agent (darwin/arm64)", "informational"},
		M + "macos.launch_items.<home>/Library/LaunchAgents/com.example.plist":               {"Adds a launch item (runs at login): ~/Library/LaunchAgents/com.example.plist (darwin/arm64)", "notable"},
		M + "macos.task_ports./System/Library/CoreServices/Finder.app/Contents/MacOS/Finder": {"Takes the task port of another process: /System/Library/CoreServices/Finder.app/Contents/MacOS/Finder (darwin/arm64)", "notable"},
	} {
		c, ok := byID[id]
		if !ok {
			t.Errorf("no change %s among %v", id, ids(d.Changes))
			continue
		}
		if c.Title != want.title || c.Impact != want.impact {
			t.Errorf("%s = %q (%s), want %q (%s)", id, c.Title, c.Impact, want.title, want.impact)
		}
	}
	// No change: counts and when a path was touched or by which helper, what was dropped, a
	// decoy planted in one build alone, a run's temporary names (a lock, a temporary file),
	// what the builds either side have and this run missed, a platform one build lacks; a
	// command's parents and purpose, and a role one build was traced too early to have; terma's
	// hooks, which a build traced before the clean room ran.
	for _, id := range []string{
		L + "exec./bin/sh -c [ -x 'terma' ] && 'terma' hook --user stop || true", L + "exec.terma hook --user stop",
		L + "exec.git status --porcelain.parents", L + "exec.git status --porcelain.purpose", L + "exec.uname -s",
		L + "exec.uname -s.role", L + "exec.uname -s.parents", L + "exec.uname -s.purpose",
		L + "paths.<claude-config>/statsig", L + "paths.<claude-config>/statsig.phases.turn",
		L + "exec.git status --porcelain.count", L + "dropped.libraries", L + "decoys.<home>/.npmrc",
		L + "decoys.<home>/.ssh/id_ed25519", L + "paths.<claude-config>/.claude.json.lock", L + "paths.<tmp>/claude-<id>-cwd",
		L + "paths.<home>/.config/sometimes", L + "exec.bash -c -l S=1\necho old", L + "sockets.connections.tcp <provider>",
	} {
		if c, ok := byID[id]; ok {
			t.Errorf("%s is no change: %+v", id, c)
		}
	}
	for _, c := range d.Changes {
		if len(c.Path) > 1 && c.Path[1] == "linux/arm64" {
			t.Errorf("a platform one build was not traced on is no change: %s", c.ID)
		}
	}
	// A socket named by a process id is the same socket build to build.
	if c := byID[L+"sockets.unix.<tmp>/cc-socks/<n>.sock"]; c.Title != "Now listens on the Unix socket <tmp>/cc-socks/<n>.sock (linux/amd64)" {
		t.Errorf("pid-named socket = %+v", c)
	}
	// A command whose script changed is one change, its text diff.
	if c := byID[L+"exec.bash -c -l S=1\necho new"]; c.Op != "changed" || c.TextDiff == nil ||
		!strings.HasPrefix(c.Title, "Command changed: `bash -c -l S=1 …` (+") {
		t.Errorf("changed command = %+v", c)
	}
	// A decoy the build touches reads first among the notable changes.
	if h := Highlights(d, 2); len(h) == 0 || h[0] != "Reads ~/.aws/credentials (a planted decoy) (linux/amd64)" && h[0] != "Connects to the decoy ssh-agent socket ~/.ssh/agent.sock (linux/amd64)" {
		t.Errorf("highlights = %v", h)
	}
}

// A macOS trace's paths differ between two runs of one build (what its background work reached
// before it exited), so they are not compared; its programs are.
func TestFilesystemMacOSPathsNotComparedOffline(t *testing.T) {
	before := fsProbe(t, `{"platform": "darwin/arm64", "tracer": "eslogger",
	  "paths": {"<claude-config>/logs": {"ops": ["probe"], "phases": ["startup"]}},
	  "exec": {}, "lookups": [], "decoys": {}, "sockets": {"listening": {}, "connections": {}, "unix": {}, "dns": []}}`)
	after := fsProbe(t, `{"platform": "darwin/arm64", "tracer": "eslogger",
	  "paths": {"<claude-config>/todos": {"ops": ["probe"], "phases": ["startup"]}},
	  "exec": {"uname -s": {"argv": ["uname", "-s"], "phases": ["startup"], "count": 1, "by": "claude"}},
	  "lookups": [], "decoys": {}, "sockets": {"listening": {}, "connections": {}, "unix": {}, "dns": []}}`)
	d := New(&Snapshot{Harness: "claude", Version: "1.0.0", Filesystem: map[string]any{"darwin/arm64": before}},
		&Snapshot{Harness: "claude", Version: "1.0.1", Filesystem: map[string]any{"darwin/arm64": after}}, nil, time.Now())
	var ids []string
	for _, c := range d.Changes {
		ids = append(ids, c.ID)
	}
	if len(ids) != 1 || ids[0] != "filesystem.darwin/arm64.exec.uname -s" {
		t.Errorf("changes = %q, want only the new command", ids)
	}
}

// A macOS build's exit reaches its trace or not: a command run only at exit, or a command's exit
// phase, is no change there.
func TestFilesystemMacOSExitNotComparedOffline(t *testing.T) {
	probe := func(exec string) map[string]any {
		return fsProbe(t, `{"platform": "darwin/arm64", "tracer": "eslogger", "paths": {}, "lookups": [], "decoys": {},
		  "sockets": {"listening": {}, "connections": {}, "unix": {}, "dns": []}, "exec": `+exec+`}`)
	}
	before := probe(`{"git status": {"argv": ["git", "status"], "phases": ["startup", "exit"], "by": "codex"},
	  "git log": {"argv": ["git", "log"], "phases": ["exit"], "by": "codex", "role": "tool"},
	  "rm -f x": {"argv": ["rm", "-f", "x"], "phases": ["exit"], "by": "zsh"}}`)
	after := probe(`{"git status": {"argv": ["git", "status"], "phases": ["startup"], "by": "codex"},
	  "git log": {"argv": ["git", "log"], "phases": ["exit"], "by": "zsh", "role": "harness"},
	  "uname -s": {"argv": ["uname", "-s"], "phases": ["startup"], "by": "codex"}}`)
	d := New(&Snapshot{Harness: "codex", Version: "1.0.0", Filesystem: map[string]any{"darwin/arm64": before}},
		&Snapshot{Harness: "codex", Version: "1.0.1", Filesystem: map[string]any{"darwin/arm64": after}}, nil, time.Now())
	var ids []string
	for _, c := range d.Changes {
		ids = append(ids, c.ID)
	}
	if len(ids) != 1 || ids[0] != "filesystem.darwin/arm64.exec.uname -s" {
		t.Errorf("changes = %q, want only the new command", ids)
	}
}
