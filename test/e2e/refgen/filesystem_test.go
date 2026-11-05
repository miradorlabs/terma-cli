package main

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/e2e"
)

// filesRow is a files profile's row of a build on a platform.
func filesRow(version, platform string, paths map[string]any) e2e.Profile {
	probe, _ := json.Marshal(map[string]any{"platform": platform, "tracer": "strace", "paths": paths,
		"exec": map[string]any{
			"git status": map[string]any{"argv": []string{"git", "status"}, "phases": []string{"startup"}, "count": 1, "by": "claude",
				"parents": []string{}, "role": "harness", "purpose": "reads the repository's state for the model's context"},
			"cat": map[string]any{"argv": []string{"cat"}, "phases": []string{"turn"}, "count": 1, "by": "zsh",
				"parents": []string{"/bin/zsh -c -l SNAPSHOT_FILE=/tmp/TestX/001/home/s.sh"}, "role": "harness"}},
		"lookups": []string{}, "decoys": map[string]any{"<home>/.aws/credentials": "not touched"},
		"sockets": map[string]any{"listening": map[string]any{"tcp 127.0.0.1:7777": map[string]any{"phases": []string{"startup"}, "count": 1}},
			"connections": map[string]any{}, "unix": map[string]any{}, "dns": []string{}}})
	return e2e.Profile{Harness: "claude", Version: version, Scenario: "TestClaudeProfileFiles", Platform: platform,
		Placeholders: map[string]string{"/tmp/TestX/001/home": "<home>"},
		Requests:     []e2e.ProviderRequest{{Probe: "filesystem", Method: "POST", Path: "/v1/messages", Body: json.RawMessage(`{"model":"m","tools":[{"name":"Bash"}],"messages":[]}`)}},
		Probes:       map[string]json.RawMessage{"filesystem": probe}}
}

// A files profile is published by platform in the filesystem section, never as the build's
// first turn; a night that traced one platform keeps another's from before; and what builds
// either side have and a build's run missed is marked unobserved.
func TestFilesystemSection(t *testing.T) {
	first := claudeProfile("1.0.0", "Be brief.", []string{"Bash", "Read"}, "", "")
	linux := filesRow("1.0.0", "linux/amd64", map[string]any{"/tmp/TestX/001/home/.claude.json": map[string]any{"ops": []string{"read"}, "phases": []string{"startup"}}})
	s := night{now: time.Now(), profiles: []e2e.Profile{first, linux}}.tonight(buildID{"claude", "1.0.0"})
	if s.Platform != first.Platform || len(s.Tools) != 2 {
		t.Errorf("the files row took the first turn's place: platform %s, tools %v", s.Platform, s.Tools)
	}
	if s.Behavior != nil && s.Behavior["filesystem"] != nil {
		t.Errorf("the files profile is no behaviour: %v", s.Behavior)
	}
	fs, _ := s.Filesystem["linux/amd64"].(map[string]any)
	paths, _ := fs["paths"].(map[string]any)
	if paths["<home>/.claude.json"] == nil {
		t.Fatalf("filesystem = %v", s.Filesystem)
	}
	// Each command's parents, role and purpose are published, its parents' literals in their
	// placeholders as the exec list's keys are.
	exec, _ := fs["exec"].(map[string]any)
	git, _ := exec["git status"].(map[string]any)
	if git["role"] != "harness" || git["purpose"] != "reads the repository's state for the model's context" || !reflect.DeepEqual(git["parents"], []any{}) {
		t.Errorf("git status = %v", git)
	}
	if cat, _ := exec["cat"].(map[string]any); !reflect.DeepEqual(cat["parents"], []any{"/bin/zsh -c -l SNAPSHOT_FILE=<home>/s.sh"}) {
		t.Errorf("cat = %v", cat)
	}
	// A fixed port is no run's loopback address.
	if l, _ := fs["sockets"].(map[string]any)["listening"].(map[string]any); l["tcp 127.0.0.1:7777"] == nil {
		t.Errorf("listening = %v", l)
	}
	// A build only its files profile saw is published with it alone.
	only := night{now: time.Now(), profiles: []e2e.Profile{filesRow("1.0.5", "linux/amd64", nil)}}.tonight(buildID{"claude", "1.0.5"})
	if only.Filesystem["linux/amd64"] == nil || only.Prompt != nil {
		t.Errorf("files-only build = %+v", only)
	}

	// Another night traced macOS alone: Linux's is kept.
	mac := filesRow("1.0.0", "darwin/arm64", map[string]any{})
	tonight := night{now: time.Now(), profiles: []e2e.Profile{mac}}.tonight(buildID{"claude", "1.0.0"})
	merged := merge(s, tonight, time.Now())
	if merged.Filesystem["linux/amd64"] == nil || merged.Filesystem["darwin/arm64"] == nil {
		t.Errorf("merged platforms = %v", fsPlatforms(merged.Filesystem))
	}

	// Three builds: the middle one's run missed a path both others touched.
	by := map[string]*Snapshot{}
	for v, p := range map[string][]string{"1": {"<home>/a", "<home>/b"}, "2": {"<home>/a"}, "3": {"<home>/a", "<home>/b"}} {
		paths := map[string]any{}
		for _, k := range p {
			paths[k] = map[string]any{"ops": []any{"read"}}
		}
		by[v] = &Snapshot{Filesystem: map[string]any{"linux/amd64": map[string]any{"paths": paths}}}
	}
	markUnobservedFilesystem([]string{"1", "2", "3"}, by)
	got := by["2"].Filesystem["linux/amd64"].(map[string]any)["unobserved"]
	if !reflect.DeepEqual(got, map[string]any{"paths": []string{"<home>/b"}}) {
		t.Errorf("unobserved = %v", got)
	}
	if by["1"].Filesystem["linux/amd64"].(map[string]any)["unobserved"] != nil {
		t.Error("the first build has nothing before it to miss")
	}
}

func fsPlatforms(m map[string]any) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}
