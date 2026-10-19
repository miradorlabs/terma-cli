package boundary

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	// go test caches by what this process reads, not the go command it runs: read the
	// package here, as listPackages does, or a change to it is answered from the cache.
	files, err := filepath.Glob(filepath.Join(root, "internal", "config", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("internal/config: %v", err)
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
