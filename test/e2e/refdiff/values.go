package refdiff

// Values is every difference between a and b, two values decoded from JSON, under path: maps
// key by key, a list of things with names by name, a list of words as a set, a long text line
// by line (a unified diff in TextDiff). It is the comparison a snapshot's sections get, without
// a harness's rules on top: what changed, unclassified and untitled, for another reference (the
// Model Reference) to classify as it reads its own data.
func Values(path []string, a, b any) []Change {
	var out []Change
	diffValue(path, a, b, &out)
	return out
}

// TextDiff is the unified diff from a to b, as a text change carries it, and what it added and
// removed.
func TextDiff(a, b string) (string, TextStats) {
	return unifiedDiff(a, b)
}
