package pi

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

const relayAddr = "127.0.0.1:43180"

// relayIn is a Pi 1.6.1 on the PATH, an agent directory of its own, and a terma the
// extension can run.
func relayIn(t *testing.T) (dir, terma string) {
	t.Helper()
	dir = t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", dir)
	piOnPath(t, "1.6.1")
	terma = filepath.Join(t.TempDir(), "terma")
	if err := os.WriteFile(terma, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir, terma
}

func setUp(t *testing.T, hook ...string) agents.RelayResult {
	t.Helper()
	res, err := Agent{}.ConfigureRelay(context.Background(), agents.RelayConfig{Endpoint: "http://" + relayAddr, Token: "tok", HookCommand: hook})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// Pi's exporter is terma's extension, written into Pi's agent directory, private to this
// user, pointed at the relay; a Pi already running loads it at its next start.
func TestConfigureRelayWritesTheExtension(t *testing.T) {
	dir, terma := relayIn(t)
	res := setUp(t, terma, "hook")
	want := filepath.Join(dir, "extensions", "terma.ts")
	if !slices.Equal(res.Paths, []string{want}) || len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "T3 Code") {
		t.Fatalf("result %+v", res)
	}
	data, _ := os.ReadFile(want)
	for _, w := range []string{`"endpoint":"http://127.0.0.1:43180"`, `"hookCommand":["` + terma + `","hook"]`} {
		if !strings.Contains(string(data), w) {
			t.Errorf("extension lacks %s", w)
		}
	}
	if info, _ := os.Stat(want); info.Mode().Perm() != 0o600 {
		t.Errorf("extension is %v: it holds the relay's token", info.Mode().Perm())
	}
	if !strings.Contains(extensionTemplate, extensionConfigMarker) {
		t.Fatal("the raw template must stay inert, with its configuration line")
	}
}

// Doctor's checks, the first that applies: the extension missing, not this build's or
// pointed elsewhere (naming the agent directory it looked in); a terma it cannot run.
// Nothing where Pi is not on the PATH.
func TestRelayProblem(t *testing.T) {
	for _, c := range []struct {
		name    string
		prepare func(t *testing.T, dir, terma string) string // the address doctor asks about
		problem string
	}{
		{"set up", func(t *testing.T, _, terma string) string { setUp(t, terma, "hook"); return relayAddr }, ""},
		{"missing", func(*testing.T, string, string) string { return relayAddr }, "is missing, not this terma's or not pointed at the local relay (Pi loads it from "},
		{"another relay", func(t *testing.T, _, terma string) string { setUp(t, terma, "hook"); return "127.0.0.1:1" }, "not pointed at the local relay"},
		{"an earlier build", func(t *testing.T, dir, terma string) string {
			setUp(t, terma, "hook")
			path := filepath.Join(dir, "extensions", "terma.ts")
			data, _ := os.ReadFile(path)
			if err := os.WriteFile(path, append([]byte("// an earlier build\n"), data...), 0o600); err != nil {
				t.Fatal(err)
			}
			return relayAddr
		}, "not this terma's"},
		{"terma gone", func(t *testing.T, _, terma string) string { setUp(t, terma+"-gone", "hook"); return relayAddr }, "which is not there or cannot be run"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, terma := relayIn(t)
			addr := c.prepare(t, dir, terma)
			detail, fix, problem := Agent{}.RelayProblem(addr)
			pointed, known := Agent{}.RelayPointed(addr)
			if !known || pointed != (c.problem == "") || problem != (c.problem != "") || !strings.Contains(detail, c.problem) {
				t.Fatalf("pointed %v (known %v), problem %v %q %q", pointed, known, problem, detail, fix)
			}
			if problem && fix != "terma setup" {
				t.Errorf("fix %q", fix)
			}
			if c.name == "missing" && !strings.Contains(detail, output.TildePath(dir)) {
				t.Errorf("doctor does not name the agent directory %s: %q", dir, detail)
			}
			piOnPath(t, "")
			if _, _, problem := (Agent{}).RelayProblem(addr); problem {
				t.Error("a problem where Pi is not on the PATH")
			}
			if _, known := (Agent{}).RelayPointed(addr); known {
				t.Error("pointed is known where Pi is not on the PATH")
			}
		})
	}
}

func TestRelayTokenCurrent(t *testing.T) {
	_, terma := relayIn(t)
	if _, known := (Agent{}).RelayTokenCurrent("tok"); known {
		t.Error("a token is known with no extension")
	}
	setUp(t, terma, "hook")
	if matches, known := (Agent{}).RelayTokenCurrent("tok"); !matches || !known {
		t.Errorf("the relay's own token: matches %v, known %v", matches, known)
	}
	if matches, known := (Agent{}).RelayTokenCurrent("other"); matches || !known {
		t.Errorf("another token: matches %v, known %v", matches, known)
	}
}

// v1Extension is a file as the extension of wire version 1 left it: the same config line in
// another body.
func v1Extension(hook string) string {
	return "// Terma extension for Pi, wire version 1\n" +
		`const CONFIG: TermaConfig | null = {"version":1,"endpoint":"http://127.0.0.1:43180","headers":{"Authorization":"Bearer tok"},"includePrompts":true,"includeToolContent":true,"hookCommand":["` + hook + `","hook"]} /* terma:config */` + "\n" +
		"export default function (pi: any) {}\n"
}

// Refresh turns a version 1 extension into exactly this build's for the same relay, token
// and terma; and puts this terma in place of a bare name or a path that is gone.
func TestRefreshMachineRewritesTheExtension(t *testing.T) {
	self, err := procinfo.AbsExecutable()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		hook func(terma string) string
		want func(terma string) string
	}{
		{"a live terma", func(terma string) string { return terma }, func(terma string) string { return terma }},
		{"a bare terma", func(string) string { return "terma" }, func(string) string { return self }},
		{"a terma that is gone", func(terma string) string { return terma + "-gone" }, func(string) string { return self }},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, terma := relayIn(t)
			path := filepath.Join(dir, "extensions", "terma.ts")
			if got, changed, err := (Agent{}).RefreshMachine(); err != nil || changed || got != path {
				t.Fatalf("RefreshMachine(absent) = (%q, %v, %v)", got, changed, err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(v1Extension(c.hook(terma))), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, changed, err := (Agent{}).RefreshMachine(); err != nil || !changed {
				t.Fatalf("RefreshMachine(version 1) = (%v, %v)", changed, err)
			}
			data, _ := os.ReadFile(path)
			want, err := renderExtension(extensionConfig{Endpoint: "http://" + relayAddr, Headers: map[string]string{"Authorization": "Bearer tok"},
				IncludePrompts: true, IncludeToolContent: true, HookCommand: []string{c.want(terma), "hook"}})
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != want {
				t.Errorf("the refreshed extension is not this build's for the same configuration")
			}
			if _, changed, err := (Agent{}).RefreshMachine(); err != nil || changed {
				t.Fatalf("RefreshMachine(current) = (%v, %v)", changed, err)
			}
		})
	}
}
