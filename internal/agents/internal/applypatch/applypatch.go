// Package applypatch reads the files an apply_patch envelope names, for the agents whose
// edit tool takes one.
package applypatch

import "strings"

// headers name a file; "Move to" is a rename's destination, recorded beside its source
// because the commit touches both.
var headers = []string{
	"*** Add File:",
	"*** Update File:",
	"*** Delete File:",
	"*** Move to:",
}

// Paths reads the envelope from raw text, since no agent gives it a structured field: the
// patch may be the tool's whole input or heredoc'd into a shell call.
func Paths(text string) []string {
	if !strings.Contains(text, "*** ") {
		return nil
	}
	var out []string
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		for _, header := range headers {
			rest, ok := strings.CutPrefix(line, header)
			if !ok {
				continue
			}
			if p := strings.TrimSpace(rest); p != "" {
				out = append(out, p)
			}
			break
		}
	}
	return out
}
