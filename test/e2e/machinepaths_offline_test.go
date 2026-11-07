package e2e

import "testing"

// The machine's paths are named the same whichever runner image, checkout and user took the
// capture; a build's own paths and a vendor's examples are not the machine's.
func TestMachinePathsOffline(t *testing.T) {
	for in, want := range map[string]string{
		"/opt/homebrew/bin/git config user.name":                                            "git config user.name",
		"/home/linuxbrew/.linuxbrew/bin/rg --files":                                         "rg --files",
		"/usr/local/Cellar/node/23.1.0/bin/node":                                            "node",
		"/opt/homebrew/lib/node_modules/npm/bin/npm-cli.js":                                 "npm-cli.js",
		"/opt/homebrew/Cellar/git/2.47.0/libexec/git-core/git-remote-https":                 "<machine>/git-remote-https",
		"HOMEBREW_PREFIX=/opt/homebrew":                                                     "HOMEBREW_PREFIX=<machine>",
		"/Applications/Xcode_26.6.app/Contents/Developer/usr/bin/xcodebuild -license check": "<xcode>/usr/bin/xcodebuild -license check",
		"sh -c /Applications/Xcode_26.6.app/Contents/Developer/usr/bin/xcodebuild -sdk /Applications/Xcode-beta.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk -find git 2> /dev/null": "sh -c <xcode>/usr/bin/xcodebuild -sdk <xcode>/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk -find git 2> /dev/null",
		"/Applications/Xcode_16.4.app": "<xcode-app>",
		"/Users/runner/work/terma-cli/terma-cli/test/e2e/versions/codex-desktop/26.1029.70936/ChatGPT.app/Contents/Resources/tectonic/tectonic": "<app>/Contents/Resources/tectonic/tectonic",
		"/Users/<user>/work/terma-cli/terma-cli/test/e2e/versions/codex-desktop/<version>/ChatGPT.app/Contents/MacOS/codex":                     "<app>/Contents/MacOS/codex",
		"/Users/runner/work/terma-cli/terma-cli/go.mod":              "<checkout>/go.mod",
		"/home/runner/work/terma-cli/terma-cli":                      "<checkout>",
		"/Users/<user>/Library/Logs/Claude-3p/cli-diagnostics.jsonl": "<home>/Library/Logs/Claude-3p/cli-diagnostics.jsonl",
		"/Users/runner/work/_temp/x":                                 "<home>/work/_temp/x",
		"/home/runner/.cache":                                        "<home>/.cache",
		"--config=/Users/someone/.config/x":                          "--config=<home>/.config/x",
		// A path wherever it starts: in a URL, after a word, as a firmlink spells it.
		"file:///Users/runner/x":                                              "file://<home>/x",
		"file:///opt/homebrew/bin/git":                                        "file://git",
		"file:///opt/homebrew/share/doc/index.html":                           "file://<machine>/index.html",
		"/System/Volumes/Data/Users/runner/x":                                 "<home>/x",
		"/System/Volumes/Data/opt/homebrew/bin/node":                          "node",
		"/System/Volumes/Data/Applications/Xcode_26.6.app/Contents/Developer": "<xcode>",
		"cwd=/c/Users/runner/x":                                               "cwd=/c<home>/x",
		"/private/var/folders/nc/1hy9ccx54nlbmblcm7_lnp3m0000gn/T/x.sock":     "<tmp>/x.sock",
		"/var/folders/nc/1hy9ccx54nlbmblcm7_lnp3m0000gn/C/com.apple.x":        "<tmp>/../C/com.apple.x",
		"/private/var/folders/.git/info/exclude":                              "/private/var/folders/.git/info/exclude",
		"/private/var/folders/CLAUDE.md":                                      "/private/var/folders/CLAUDE.md",
		// Neither the machine's nor anyone's home.
		"/Users/Shared/Library":                    "/Users/Shared/Library",
		"/home/claude/.claude/remote/.oauth_token": "/home/claude/.claude/remote/.oauth_token",
		"/usr/local/bin/rg":                        "/usr/local/bin/rg",
		"<repo>/opt/homebrew-ish":                  "<repo>/opt/homebrew-ish",
		"git status":                               "git status",
	} {
		if got := MachinePaths(in); got != want {
			t.Errorf("MachinePaths(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestMachineLeakOffline(t *testing.T) {
	for in, want := range map[string]string{
		"git config":                                         "",
		"/Users/Shared/x":                                    "",
		"/Users/Shared/x /Users/runner/y":                    "/Users/",
		"<xcode>/usr/bin/xcodebuild":                         "",
		"/opt/homebrew/bin/git":                              "/opt/homebrew",
		"/Applications/Xcode_26.6.app":                       "/Applications/Xcode",
		"/home/claude/.claude/remote/x":                      "",
		"/home/runner/.cache":                                "/home/runner",
		"file:///Users/runner/x":                             "/Users/",
		"/var/folders/nc/1hy9ccx54nlbmblcm7_lnp3m0000gn/T/x": "/var/folders/nc/1hy9ccx54nlbmblcm7_lnp3m0000gn/T",
		"/private/var/folders/CLAUDE.md":                     "",
	} {
		if got := MachineLeak(in); got != want {
			t.Errorf("MachineLeak(%q) = %q, want %q", in, got, want)
		}
	}
}
