package boundary

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// releaseTag is the build tag that leaves development-only overrides out of a release.
const releaseTag = "release"

// TestReleaseBuildsSetTheReleaseTag holds every build goreleaser makes, which are what
// install.sh, Homebrew, npm and `terma update` install, to the release tag.
func TestReleaseBuildsSetTheReleaseTag(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Builds []struct {
			ID   string   `yaml:"id"`
			Tags []string `yaml:"tags"`
		} `yaml:"builds"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Builds) == 0 {
		t.Fatal(".goreleaser.yaml has no builds")
	}
	for _, b := range cfg.Builds {
		if !slices.Contains(b.Tags, releaseTag) {
			t.Errorf("goreleaser build %q does not set the %q build tag (tags %v)", b.ID, releaseTag, b.Tags)
		}
	}
}

// TestTheReleaseTagLeavesOutThePolicyStub runs internal/config's release-only test under
// the release tag: a release build never reads TERMA_POLICY_STUB.
func TestTheReleaseTagLeavesOutThePolicyStub(t *testing.T) {
	t.Parallel()
	const test = "TestReleaseBuildIgnoresThePolicyStub"
	root := repoRoot(t)
	// go test caches by what this process reads, not the go command it runs: read what the
	// release test builds from here, as listPackages does, or a change is answered from the cache.
	list := exec.Command("go", "list", "-deps", "-test", "-tags", releaseTag, "-json", "./internal/config/")
	list.Dir = root
	listed, err := list.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	files := []string{filepath.Join(root, "go.mod"), filepath.Join(root, "go.sum")}
	dec := json.NewDecoder(bytes.NewReader(listed))
	for dec.More() {
		var p struct {
			Dir, Name            string
			Standard             bool
			GoFiles, TestGoFiles []string
		}
		if err := dec.Decode(&p); err != nil {
			t.Fatal(err)
		}
		// The generated test main lives in the build cache, outside the module.
		if p.Standard || p.Name == "main" {
			continue
		}
		for _, name := range slices.Concat(p.GoFiles, p.TestGoFiles) {
			files = append(files, filepath.Join(p.Dir, name))
		}
	}
	if !slices.Contains(files, filepath.Join(root, "internal", "config", "policystub_release_test.go")) {
		t.Fatalf("go list -tags %s does not build the release test: %q", releaseTag, files)
	}
	for _, f := range files {
		if _, err := os.ReadFile(f); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("go", "test", "-count=1", "-tags", releaseTag, "-run", "^"+test+"$", "-v", "./internal/config/")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: "+test) {
		t.Fatalf("go test -tags %s: %v\n%s", releaseTag, err, out)
	}
}

// TestOnlyConfigReadsThePolicyStub keeps TERMA_POLICY_STUB behind config.PolicyStub, the
// one reader the release tag turns off: no other code reads the variable itself.
func TestOnlyConfigReadsThePolicyStub(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	fset := token.NewFileSet()
	for _, p := range listPackages(t) {
		// Files another platform or build tag compiles too: the gate itself is a build tag.
		for _, name := range slices.Concat(p.GoFiles, p.IgnoredGoFiles) {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(p.Dir, name)
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			rel, _ := filepath.Rel(root, path)
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				if v, _ := strconv.Unquote(lit.Value); v == "TERMA_POLICY_STUB" && filepath.ToSlash(rel) != "internal/config/policy.go" {
					t.Errorf("%s names TERMA_POLICY_STUB; read it through config.PolicyStub", filepath.ToSlash(rel))
				}
				return true
			})
		}
	}
}
