package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/e2e"
	"github.com/miradorlabs/terma-cli/e2e/sourcelink"
)

// A closed harness's config is its binary facts': the settings schema's options as
// config.options, the variables it reads, and where each came from. A build's binary scenario's
// facts are preferred to an earlier scenario's.
func TestConfigFromBinaryFacts(t *testing.T) {
	old, _ := json.Marshal(map[string]any{"form": "executable", "size_bytes": 1, "sha256": "x",
		"env_vars": []string{"CLAUDE_FROM_STRINGS"}, "settings_keys": []string{"model"}})
	facts, _ := json.Marshal(map[string]any{"form": "executable", "size_bytes": 1, "sha256": "x",
		"env_vars": []string{"CLAUDE_CODE_ENTRYPOINT"}, "env_source": "reads", "settings_keys": []string{"model"},
		"settings":  map[string]any{"model": map[string]any{"type": "string", "description": "Model"}},
		"heuristic": []string{"env_vars", "settings"}})
	main := claudeProfile("2.1.300", "Be brief.", []string{"Bash"}, "a-1", "")
	main.Binary = old
	binary := e2e.Profile{Harness: "claude", Version: "2.1.300", Scenario: "TestClaudeProfileBinary", Platform: "darwin/arm64", Binary: facts}
	n := night{now: time.Now(), profiles: []e2e.Profile{main, binary}}
	s := n.tonight(buildID{"claude", "2.1.300"})
	cfg := s.Config
	opts, _ := cfg["options"].(map[string]any)
	if opts["model"] == nil {
		t.Fatalf("config.options = %v", cfg)
	}
	if !strings.Contains(cfg["source"].(string), "zod") || cfg["env_source"] != "the variables its JavaScript reads" {
		t.Errorf("config sources = %v, %v", cfg["source"], cfg["env_source"])
	}
	if env, _ := json.Marshal(cfg["env_vars"]); string(env) != `["CLAUDE_CODE_ENTRYPOINT"]` {
		t.Errorf("env_vars = %s, want the binary scenario's", env)
	}
	if _, ok := s.Binary["settings"]; ok {
		t.Error("settings left in the binary section")
	}
}

// A public harness's config is its source's at the build's tag: its schema's options, the
// variables its code reads (in place of its strings'), each build read once.
func TestConfigFromSource(t *testing.T) {
	tarball := func(files map[string]string) []byte {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(zw)
		for name, body := range files {
			_ = tw.WriteHeader(&tar.Header{Name: "codex-v0/" + name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
			_, _ = tw.Write([]byte(body))
		}
		_ = tw.Close()
		_ = zw.Close()
		return buf.Bytes()
	}
	opened := 0
	lib := sourcelink.New("")
	defer lib.Close()
	lib.Open = func(repo, tag string) (io.ReadCloser, error) {
		opened++
		if tag != "rust-v0.159.1" {
			return nil, errors.New("404 Not Found")
		}
		return io.NopCloser(bytes.NewReader(tarball(map[string]string{
			"codex-rs/core/config.schema.json": `{"type":"object","properties":{"model":{"type":"string","description":"Model to use."}}}`,
			"codex-rs/core/src/home.rs":        "fn home() { std::env::var(\"CODEX_HOME\").ok(); }\n",
		}))), nil
	}
	bin, _ := json.Marshal(map[string]any{"form": "executable", "size_bytes": 1, "env_vars": []string{"CODEX_FROM_STRINGS"},
		"env_vars_uncertain": []string{"CODEX_X"}})
	p := e2e.Profile{Harness: "codex", Version: "0.159.1", Scenario: "TestCodexProfile", Platform: "darwin/arm64", Binary: bin}
	n := night{now: time.Now(), profiles: []e2e.Profile{p}, sources: &sourceConfigs{lib: lib, cache: map[buildID]map[string]any{}}}
	s := n.tonight(buildID{"codex", "0.159.1"})
	n.tonight(buildID{"codex", "0.159.1"})
	cfg := s.Config
	if cfg["source"] != "codex-rs/core/config.schema.json at rust-v0.159.1" {
		t.Errorf("source = %v", cfg["source"])
	}
	if o, _ := cfg["options"].(map[string]any)["model"].(map[string]any); o["description"] != "Model to use." {
		t.Errorf("options = %v", cfg["options"])
	}
	if env, _ := json.Marshal(cfg["env_vars"]); string(env) != `["CODEX_HOME"]` || cfg["env_vars_uncertain"] != nil {
		t.Errorf("env_vars = %s, uncertain %v: want the source's alone", env, cfg["env_vars_uncertain"])
	}
	if opened != 1 {
		t.Errorf("the build's source read %d times", opened)
	}
	// A build whose source cannot be read keeps what its binary said.
	p.Version = "0.1.0"
	n.profiles = []e2e.Profile{p}
	if s := n.tonight(buildID{"codex", "0.1.0"}); s.Config["options"] != nil || s.Config["env_vars"] == nil {
		t.Errorf("config of an unreadable source = %v", s.Config)
	}
}

func TestConfigNeedle(t *testing.T) {
	for path, want := range map[string]string{
		"config.options.sandbox_workspace_write.network_access": "network_access",
		"config.options.mcp_servers.<name>":                     "mcp_servers",
		"config.options.hooks.<name>[].matcher":                 "matcher",
		"config.env_vars.CODEX_HOME":                            "CODEX_HOME",
	} {
		parts := strings.SplitN(path, ".", 3)
		c := &Change{Category: "config", Path: parts}
		if got := needleOf(c); got != want {
			t.Errorf("needle of %s = %q, want %q", path, got, want)
		}
	}
}
