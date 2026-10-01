package hookmgr

import (
	"os"
	"path/filepath"
	"testing"
)

// A file that exists and cannot be read is refused, not planned as a create over the developer's content.
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
