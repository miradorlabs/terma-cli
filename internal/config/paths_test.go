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
