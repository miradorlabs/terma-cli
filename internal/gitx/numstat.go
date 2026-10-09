package gitx

import (
	"strconv"
	"strings"
)

// parseCommitDiff separates the raw status records from numstat. The raw format
// distinguishes renames from copies, while numstat preserves the existing line deltas.
func parseCommitDiff(block string) (files []FileStat, sources []string) {
	block = strings.TrimPrefix(block, "\x00")
	block = strings.TrimPrefix(block, "\n")
	for strings.HasPrefix(block, ":") {
		header, rest, ok := strings.Cut(block, "\x00")
		if !ok {
			return nil, sources
		}
		fields := strings.Fields(header)
		if len(fields) != 5 {
			return nil, sources
		}
		status := fields[4]
		old, rest, ok := strings.Cut(rest, "\x00")
		if !ok {
			return nil, sources
		}
		if strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C") {
			if strings.HasPrefix(status, "R") {
				sources = append(sources, old)
			}
			_, rest, ok = strings.Cut(rest, "\x00")
			if !ok {
				return nil, sources
			}
		}
		block = rest
	}
	return parseNumstat(block), sources
}

// parseNumstat reads the `--numstat -z` block. A rename spans three records (counts
// with an empty path, old path, new path) and is recorded under the new path; paths
// are raw, so nothing trims them.
func parseNumstat(block string) []FileStat {
	// The NUL and newline after the --format record are framing, not the first path.
	block = strings.TrimPrefix(block, "\x00")
	block = strings.TrimPrefix(block, "\n")
	records := strings.Split(block, "\x00")
	var out []FileStat
	for i := 0; i < len(records); i++ {
		rec := records[i]
		if rec == "" {
			continue
		}
		addedField, rest, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		deletedField, path, ok := strings.Cut(rest, "\t")
		if !ok {
			continue
		}
		if path == "" {
			// Rename or copy: the next two records are the old and the new path.
			if i+2 >= len(records) {
				break
			}
			path = records[i+2]
			i += 2
		}
		if path == "" {
			continue
		}
		added, addedOK := parseCount(addedField)
		deleted, deletedOK := parseCount(deletedField)
		out = append(out, FileStat{
			Path:    path,
			Added:   added,
			Deleted: deleted,
			Binary:  !addedOK || !deletedOK,
		})
	}
	return out
}

// parseCount reads one numstat count; ok is false for git's "-" or anything not a number.
func parseCount(field string) (int, bool) {
	n, err := strconv.Atoi(field)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
