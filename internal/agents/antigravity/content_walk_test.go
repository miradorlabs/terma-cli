package antigravity

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
)

// Every value in a call's arguments that looks like a path is judged, under any key and at any
// depth; a relative one under a key known to name a path, or one that climbs or names
// something in the cwd (TestAntigravityJudgesARelativePathUnderAnUnknownKey).
func TestAntigravityRefusesAPathUnderAnyKey(t *testing.T) {
	g := newPathRig(t)
	q := func(path string) string { return `"` + hookruntest.InJSON(path) + `"` }
	excluded, collected := filepath.Join(g.excluded, "file.txt"), filepath.Join(g.collected, "file.txt")
	relative, err := filepath.Rel(g.collected, excluded)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, args string
		want       bool
	}{
		{"an unknown key naming an excluded file", `{"File":` + q(excluded) + `}`, true},
		{"an unknown key naming a collected file", `{"File":` + q(collected) + `}`, false},
		{"a list of paths, one excluded", `{"TargetDirectories":[` + q(g.collected) + `,` + q(g.excluded) + `]}`, true},
		{"a list of collected paths", `{"TargetDirectories":[` + q(g.sub) + `,` + q(g.other) + `]}`, false},
		{"a nested object", `{"options":{"source":{"File":` + q(excluded) + `}}}`, true},
		{"an object in a list", `{"edits":[{"File":` + q(collected) + `},{"File":` + q(excluded) + `}]}`, true},
		{"a path from the home folder", `{"File":"~/x"}`, true},
		{"the home folder", `{"Root":"~"}`, true},
		{"a multi-line value that starts with a path", `{"Text":` + q(excluded+"\nmade-up text") + `}`, false},
		{"a value longer than a path", `{"Text":` + q(excluded+"/"+strings.Repeat("x", antigravityPathLimit)) + `}`, false},
		{"text that looks like a path", `{"Replacement":"/api/v1/users"}`, false},
		{"a relative path that climbs, under an unknown key", `{"Text":` + q(relative) + `}`, true},
		{"the same relative path under a file key", `{"Path":` + q(relative) + `}`, true},
		{"a number and a bool", `{"Count":1,"Recursive":true,"Nothing":null}`, false},
		// What is not a path is not refused by accident: a shell command's text, also one that
		// starts with a program's absolute path, a URL, and a relative path inside the checkout.
		{"a shell command", `{"CommandLine":"go test ./... && ls -la internal/agents"}`, false},
		{"a shell command naming an excluded file", `{"CommandLine":` + q("cat "+excluded) + `}`, false},
		{"a shell command that starts with a program's path", `{"CommandLine":"/usr/bin/env go version"}`, false},
		{"a URL", `{"Url":"https://example.com/docs/page?x=1"}`, false},
		{"a URL under a file key's sibling", `{"Path":"file.txt","Source":"https://example.com/a/b"}`, false},
		{"a relative path inside the checkout", `{"Path":"sub/dir/new-file.txt"}`, false},
		{"a relative folder inside the checkout", `{"SearchPath":"sub"}`, false},
		// A folder in no repository is refused under any key, as under a folder key: an MCP
		// filesystem tool lists or searches one under "path". A file there, and a path that is
		// not there yet, follow the files' answer.
		{"a folder in no repository under an unknown key", `{"TargetDirectories":[` + q(g.none) + `]}`, true},
		{"a folder in no repository under a file key", `{"path":` + q(g.none) + `,"pattern":"made-up"}`, true},
		{"a file in no repository under an unknown key", `{"File":` + q(filepath.Join(g.none, "file.txt")) + `}`, false},
		{"a path not there yet in no repository", `{"File":` + q(filepath.Join(g.none, "new", "file.txt")) + `}`, false},
		{"a folder in no repository under a folder key, nested", `{"options":{"SearchPath":` + q(g.none) + `}}`, true},
	} {
		if got := newAntigravityPaths(g.pol).refuses(g.collected, json.RawMessage(tc.args)); got != tc.want {
			t.Errorf("%s: refused %v, want %v", tc.name, got, tc.want)
		}
	}
	defer func(was bool) { antigravityShowsFilesInNoRepository = was }(antigravityShowsFilesInNoRepository)
	antigravityShowsFilesInNoRepository = false
	if !newAntigravityPaths(g.pol).refuses(g.collected, json.RawMessage(`{"File":`+q(filepath.Join(g.none, "file.txt"))+`}`)) {
		t.Error("a file in no repository under an unknown key was shown with the answer withholding it")
	}
}

// A hook places each resolved folder once: two files in one folder, and the hook and the
// reader judging the same arguments, look it up once.
func TestAntigravityPathsPlaceAFolderOnce(t *testing.T) {
	g := newPathRig(t)
	hookruntest.WriteFile(t, g.collected, "second.txt", "made-up file\n")
	q := func(path string) string { return `"` + hookruntest.InJSON(path) + `"` }
	args := json.RawMessage(`{"Files":[` + q(filepath.Join(g.collected, "file.txt")) + `,` + q(filepath.Join(g.collected, "second.txt")) + `]}`)
	p := newAntigravityPaths(g.pol)
	if p.refuses(g.collected, args) {
		t.Fatal("two collected files were refused")
	}
	if p.placed != 1 {
		t.Errorf("two files in one folder placed %d folders, want 1", p.placed)
	}
	// The reader judges the call line's copy of the same arguments.
	p.refuses(g.collected, args)
	if p.placed != 1 {
		t.Errorf("judging the same arguments again placed %d folders, want 1", p.placed)
	}
	p.refuses(g.collected, json.RawMessage(`{"Path":`+q(filepath.Join(g.other, "file.txt"))+`}`))
	if p.placed != 2 {
		t.Errorf("a file in another folder: %d folders placed, want 2", p.placed)
	}
}
