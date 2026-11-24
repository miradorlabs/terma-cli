package antigravity

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
)

// folderRig is pathRig with what a folder may hold: a collected checkout with an excluded
// submodule, one with a collected nested clone, a folder in no repository above an excluded
// checkout, and a folder of the collected checkout holding a link to the excluded one.
type folderRig struct {
	pathRig
	// withSubmodule holds the excluded submodule module; withCollected the collected clone
	// nested; above, in no repository, holds an excluded checkout; links holds into, a link to
	// a folder of the excluded checkout.
	withSubmodule, module, withCollected, nestedCollected, above, links, into string
}

func newFolderRig(t *testing.T) folderRig {
	t.Helper()
	g := folderRig{pathRig: newPathRig(t)}
	g.withSubmodule, g.withCollected = hookruntest.InitRepo(t), hookruntest.InitRepo(t)
	// A submodule: its .git a file naming its git directory in the superproject's modules,
	// whose origin the policy does not admit.
	g.module = filepath.Join(g.withSubmodule, "mod")
	hookruntest.WriteFile(t, g.withSubmodule, ".git/modules/mod/HEAD", "ref: refs/heads/main\n")
	hookruntest.WriteFile(t, g.withSubmodule, ".git/modules/mod/config",
		"[remote \"origin\"]\n\turl = https://github.com/acme/submodule-excluded.git\n")
	hookruntest.WriteFile(t, g.withSubmodule, "mod/.git", "gitdir: ../.git/modules/mod\n")
	hookruntest.WriteFile(t, g.withSubmodule, "mod/file.txt", "made-up file\n")
	hookruntest.WriteFile(t, g.withSubmodule, "docs/file.txt", "made-up file\n")
	// A nested clone the policy collects.
	g.nestedCollected = filepath.Join(g.withCollected, "vendor", "ok")
	mustInit(t, g.nestedCollected, "https://github.com/acme/nested-collected.git")
	// A folder in no repository above an excluded checkout.
	g.above = filepath.Join(g.none, "projects")
	mustInit(t, filepath.Join(g.above, "excluded-inner"), "https://github.com/acme/inner-excluded.git")
	g.links = filepath.Join(g.collected, "links")
	g.into = filepath.Join(g.links, "into-excluded")
	sub := filepath.Join(g.excluded, "sub")
	hookruntest.WriteFile(t, sub, "file.txt", "made-up file\n")
	if err := os.Symlink(sub, g.into); err != nil {
		g.into = "" // no symlinks here: the link rows are skipped
	}
	for _, root := range []string{g.withSubmodule, g.withCollected} {
		g.pol.Repositories = append(g.pol.Repositories, hookruntest.Admitting(root).Repositories...)
	}
	g.pol.Repositories = append(g.pol.Repositories, "github.com/acme/nested-collected")
	return g
}

// mustInit makes dir a clone with origin as its remote.
func mustInit(t *testing.T, dir, origin string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", origin}} {
		if _, err := gitx.Git(context.Background(), dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	hookruntest.WriteFile(t, dir, "file.txt", "made-up file\n")
}

// A folder is judged by what it is and what it holds, never by the key that names it: one in
// no repository is refused, and so is one in a collected checkout that holds a checkout the
// policy does not collect. A path not there yet is a file, but under a folder key.
func TestAntigravityRefusesAFolderByWhatItHolds(t *testing.T) {
	g := newFolderRig(t)
	q := func(path string) string { return `"` + hookruntest.InJSON(path) + `"` }
	type row struct {
		name, args string
		want       bool
	}
	rows := []row{
		// An MCP filesystem tool names its folder under "path".
		{"list_directory of a folder in no repository", `{"path":` + q(g.none) + `}`, true},
		{"search_files above an excluded checkout, in no repository", `{"path":` + q(g.above) + `,"pattern":"x"}`, true},
		{"a folder of the checkout holding nothing excluded, under path", `{"path":` + q(g.sub) + `}`, false},
		{"a folder of the checkout holding nothing excluded, under an unknown key", `{"Root":` + q(g.sub) + `}`, false},
		{"the checkout's root, holding an excluded clone", `{"path":` + q(g.collected) + `}`, true},
		{"the checkout's root, under an unknown key", `{"Root":` + q(g.collected) + `}`, true},
		{"the folder above the excluded clone", `{"path":` + q(filepath.Join(g.collected, "vendor")) + `}`, true},
		{"a checkout's root, holding an excluded submodule", `{"path":` + q(g.withSubmodule) + `}`, true},
		{"a folder of it beside the submodule", `{"path":` + q(filepath.Join(g.withSubmodule, "docs")) + `}`, false},
		{"the excluded submodule itself", `{"path":` + q(g.module) + `}`, true},
		{"a checkout's root, holding a collected clone", `{"path":` + q(g.withCollected) + `}`, false},
		{"the collected clone itself", `{"path":` + q(g.nestedCollected) + `}`, false},
		// A path not there yet: a file, but under a folder key.
		{"a file not there yet in the checkout", `{"path":` + q(filepath.Join(g.collected, "new", "file.txt")) + `}`, false},
		{"a relative file not there yet in the checkout", `{"Path":"new/file.txt"}`, false},
		{"a folder not there yet in the checkout, under a folder key", `{"SearchPath":` + q(filepath.Join(g.collected, "new")) + `}`, false},
		{"a file not there yet in an excluded checkout", `{"path":` + q(filepath.Join(g.excluded, "new", "file.txt")) + `}`, true},
		{"a folder not there yet in no repository, under a folder key", `{"DirectoryPath":` + q(filepath.Join(g.none, "new")) + `}`, true},
		{"a folder not there yet in no repository, under another key", `{"path":` + q(filepath.Join(g.none, "new")) + `}`, false},
		// A file under a folder key is a file.
		{"a file in no repository under a folder key", `{"SearchPath":` + q(filepath.Join(g.none, "file.txt")) + `}`, false},
	}
	if g.into != "" {
		rows = append(rows,
			row{"a link to a folder of an excluded checkout", `{"path":` + q(g.into) + `}`, true},
			row{"a folder holding that link, which the walk does not follow", `{"path":` + q(g.links) + `}`, false},
		)
	}
	for _, r := range rows {
		if got := newAntigravityPaths(g.pol).refuses(g.collected, json.RawMessage(r.args)); got != r.want {
			t.Errorf("%s: refused %v, want %v", r.name, got, r.want)
		}
	}

	// Global mode collects everything: no folder rule refuses.
	global := config.Policy{Mode: config.ModeGlobal}
	for _, dir := range []string{g.none, g.above, g.collected, g.withSubmodule, filepath.Join(g.none, "new")} {
		for _, key := range []string{"path", "DirectoryPath"} {
			if newAntigravityPaths(global).refuses(g.collected, json.RawMessage(`{"`+key+`":`+q(dir)+`}`)) {
				t.Errorf("global mode refused %s=%s", key, dir)
			}
		}
	}
}

// A folder whose walk passes the bound is refused: what the rest holds is unknown. Not in
// global mode, which walks nothing.
func TestAntigravityRefusesAFolderPastTheWalksBound(t *testing.T) {
	g := newPathRig(t)
	for _, d := range []string{"a", "b", "c"} {
		if err := os.MkdirAll(filepath.Join(g.sub, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	args := json.RawMessage(`{"path":"` + hookruntest.InJSON(g.sub) + `"}`)
	if newAntigravityPaths(g.pol).refuses(g.collected, args) {
		t.Fatal("a folder under the bound was refused")
	}
	defer func(was int) { gitx.NestedWalkLimit = was }(gitx.NestedWalkLimit)
	gitx.NestedWalkLimit = 3
	if !newAntigravityPaths(g.pol).refuses(g.collected, args) {
		t.Error("a folder past the bound was shown")
	}
	if newAntigravityPaths(config.Policy{Mode: config.ModeGlobal}).refuses(g.collected, args) {
		t.Error("global mode refused a folder past the bound")
	}
}

// A hook walks a folder once: the hook and the reader judging the same arguments, and two
// values naming it, look it up once.
func TestAntigravityPathsWalkAFolderOnce(t *testing.T) {
	g := newPathRig(t)
	q := `"` + hookruntest.InJSON(g.sub) + `"`
	args := json.RawMessage(`{"path":` + q + `,"Root":` + q + `}`)
	p := newAntigravityPaths(g.pol)
	p.refuses(g.collected, args)
	p.refuses(g.collected, args)
	if p.walked != 1 {
		t.Errorf("one folder walked %d times", p.walked)
	}
}

// Under a key terma does not know, a relative value is a path when it climbs out of the cwd or
// names something there; other relative text, a command line among it, is not judged.
func TestAntigravityJudgesARelativePathUnderAnUnknownKey(t *testing.T) {
	g := newPathRig(t)
	sep := string(filepath.Separator)
	for _, tc := range []struct {
		name, value   string
		judged, wantR bool
	}{
		{"climbing into an excluded checkout", "../" + filepath.Base(g.excluded) + "/file.txt", true, true},
		{"climbing with the system's separator", ".." + sep + filepath.Base(g.excluded) + sep + "file.txt", true, true},
		{"a file there", "sub/file.txt", true, false},
		{"a folder there holding nothing excluded", "sub", true, false},
		// The cost of reading a word as a path: a folder there holding an excluded clone.
		{"a folder there holding an excluded clone", "vendor", true, true},
		{"a word that names nothing there", "beta", false, false},
		{"a command line", "cd ../x && ls", false, false},
		{"a segment that only starts with two dots", "..x/file.txt", false, false},
		{"two lines", "sub/file.txt\nmade-up", false, false},
	} {
		raw, err := json.Marshal(map[string]any{"Query": tc.value})
		if err != nil {
			t.Fatal(err)
		}
		p := newAntigravityPaths(g.pol)
		if got := p.refuses(g.collected, raw); got != tc.wantR {
			t.Errorf("%s: refused %v, want %v", tc.name, got, tc.wantR)
		}
		if judged := p.placed > 0; judged != tc.judged {
			t.Errorf("%s: judged %v, want %v", tc.name, judged, tc.judged)
		}
	}
}
