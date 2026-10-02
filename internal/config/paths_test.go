package config

import "testing"

func TestExcludedPathGlobs(t *testing.T) {
	p := Policy{ExcludePaths: []string{".env", "**/secrets/**", "private/*"}}
	for _, name := range []string{".env", "src/.env", "src/secrets/password.txt", "secrets/key", "private/key", "private/nested/file", `src\secrets\key`} {
		if !p.ExcludesPath(name, "") {
			t.Errorf("excluded path allowed: %s", name)
		}
	}
	if !p.ExcludesPath("/workspace/src/secrets/key", "/workspace") {
		t.Fatal("absolute path not made relative")
	}
	for _, name := range []string{"src/public.go", ".env.example", "my-secrets/key", "src/private.go"} {
		if p.ExcludesPath(name, "") {
			t.Errorf("allowed path excluded: %s", name)
		}
	}
	if !p.HasExcludedPath(map[string]any{"tool_input": `{"path":"secrets/key"}`}, "") {
		t.Fatal("JSON tool input bypassed exclusion")
	}
	if !p.ExcludesPath("/workspace/private/key", "") || !p.ExcludesPath(`C:\workspace\private\key`, "") {
		t.Fatal("absolute native path without a workspace bypassed exclusion")
	}
	if !p.HasExcludedPath(map[string]any{"cwd": "/workspace/private/nested"}, "") {
		t.Fatal("excluded working directory bypassed exclusion")
	}
	if (Policy{ExcludePaths: []string{"private/"}}).ExcludesPath("private/nested/key", "") == false {
		t.Fatal("directory exclusion with trailing slash bypassed")
	}
}

// A shell command naming an excluded file is excluded like a read of it, however the
// agent spells the command.
func TestShellCommandsHitPathExclusion(t *testing.T) {
	p := Policy{ExcludePaths: []string{"secrets/**", "*.pem"}}
	for name, value := range map[string]any{
		"codex exec argv":      map[string]any{"arguments": `{"command":["bash","-lc","cat secrets/app.env"],"workdir":"/w"}`},
		"codex exec_command":   map[string]any{"arguments": `{"cmd":"cat secrets/app.env | head","workdir":"/w"}`},
		"claude bash":          map[string]any{"tool_parameters": `{"bash_command":"cat","full_command":"cat 'secrets/app.env'"}`},
		"hook tool input":      map[string]any{"tool_input": map[string]any{"command": "grep KEY <secrets/app.env"}},
		"absolute":             map[string]any{"command": "cat /home/dev/repo/secrets/app.env"},
		"relative to workdir":  map[string]any{"command": "cat app.env", "workdir": "/home/dev/repo/secrets"},
		"parent directory":     map[string]any{"command": "cat ../secrets/app.env", "cwd": "/home/dev/repo/src"},
		"flag value":           map[string]any{"command": "openssl x509 --in=certs/server.pem"},
		"directory":            map[string]any{"command": "ls -la secrets"},
		"nested in a subshell": map[string]any{"cmd": "echo $(cat secrets/app.env)"},
	} {
		if !p.HasExcludedPath(value, "") {
			t.Errorf("%s: command naming an excluded file was not excluded", name)
		}
	}
	if !p.HasExcludedPathFrom(map[string]any{"command": "cat app.env"}, "/home/dev/repo", "/home/dev/repo/secrets") {
		t.Error("a relative word did not resolve against the hook's working directory")
	}
	for name, value := range map[string]any{
		"other file":       map[string]any{"command": "cat README.md"},
		"not a command":    map[string]any{"description": "cat secrets/app.env"},
		"similar name":     map[string]any{"command": "cat my-secrets.txt"},
		"prompt mentioned": map[string]any{"prompt": "please cat secrets/app.env"},
	} {
		if p.HasExcludedPath(value, "") {
			t.Errorf("%s: excluded without naming an excluded file", name)
		}
	}
}
