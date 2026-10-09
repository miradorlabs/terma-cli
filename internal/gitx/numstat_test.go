package gitx

import (
	"slices"
	"testing"
)

func TestCommitDiffRetiresRenameSourcesButNotCopySources(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"R100", "C100"} {
		t.Run(status, func(t *testing.T) {
			// Embedded tabs/newlines are paths, not raw-status or numstat framing.
			const old, dest = "old\t\tname\n.txt", "new\tname\n.txt"
			block := "\x00\n:100644 100644 abcd abcd " + status + "\x00" + old + "\x00" + dest + "\x00" +
				":100644 100644 abcd efab M\x00plain.txt\x00" +
				"0\t0\t\x00" + old + "\x00" + dest + "\x00" + "1\t2\tplain.txt\x00"
			files, sources := parseCommitDiff(block)
			want := []FileStat{{Path: dest}, {Path: "plain.txt", Added: 1, Deleted: 2}}
			if !slices.Equal(files, want) {
				t.Fatalf("file statistics changed: got %+v, want %+v", files, want)
			}
			var wantSources []string
			if status[0] == 'R' {
				wantSources = []string{old}
			}
			if !slices.Equal(sources, wantSources) {
				t.Fatalf("retirement sources: got %v, want %v", sources, wantSources)
			}
			commit := Commit{Files: files, RenameSources: sources}
			if !slices.Equal(commit.Paths(), []string{dest, "plain.txt"}) {
				t.Fatalf("reported paths changed: %v", commit.Paths())
			}
			if !slices.Equal(commit.RetiredPaths(), append([]string{dest, "plain.txt"}, wantSources...)) {
				t.Fatalf("retired paths: %v", commit.RetiredPaths())
			}
		})
	}
}
