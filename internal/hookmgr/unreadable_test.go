package hookmgr

import (
	"os"
	"path/filepath"
	"testing"
)

// A file that exists and cannot be read is not an absent file: planned as a create, Apply
// would rename terma-only content over whatever the developer had there. A directory at
// the file's path fails to read on every platform. The agents' planners are held to the
// same in internal/agents/builtin.
func TestPlannersRefuseAFileTheyCannotRead(t *testing.T) {
	planners := []struct {
		path string
		plan func(root string) (Plan, error)
	}{
		{"lefthook.yml", func(root string) (Plan, error) { return planLefthook(root, "lefthook.yml", true) }},
		{".pre-commit-config.yaml", func(root string) (Plan, error) { return planPreCommit(root, true) }},
		{".husky/post-commit", func(root string) (Plan, error) { return planHusky(root, true) }},
		{ShimDir + "/post-commit", func(root string) (Plan, error) { return planShim(root, true) }},
	}
	for _, tc := range planners {
		t.Run(tc.path, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(tc.path)), 0o755); err != nil {
				t.Fatal(err)
			}
			plan, err := tc.plan(root)
			if err == nil {
				t.Fatalf("planned %d change(s) over a file that could not be read", len(plan.Changes))
			}
		})
	}
}

// Absent is still absent: the contract change must not turn a first install into an error.
func TestPlannersStillCreateWhatIsMissing(t *testing.T) {
	plan, err := PlanClaudeSettings(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 1 || plan.Changes[0].Before != nil {
		t.Fatalf("a missing settings file should be one create, got %+v", plan.Changes)
	}
}
