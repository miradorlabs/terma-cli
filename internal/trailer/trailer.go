// Package trailer stamps agent sessions into commit messages as git trailers.
//
// Telemetry is keyed by session id and GitHub by commit sha; the join between them
// has to travel in the one artifact that goes from the developer's machine to the
// remote — the commit message. A trailer survives rebase, amend, squash, and
// cherry-pick because it is part of the message, carries nothing but an opaque id,
// and follows the convention Co-authored-by already established:
//
//	Agent-Session-Id: 018f3a2c-...
//	Agent-Tool: claude-code/2.1.0
//
// Everything here is pure string manipulation so prepare-commit-msg can call it in
// well under its 50 ms budget with no I/O beyond reading and writing the message file.
package trailer

import (
	"regexp"
	"strings"
)

// The trailer keys. The Terma backend and the GitHub App parse commit messages for
// exactly these, so they are a contract with other repositories, not a spelling choice.
const (
	// KeySessionID names the agent session that produced the commit's changes.
	KeySessionID = "Agent-Session-Id"
	// KeyTool names the agent, as "<agent>/<version>" when the version is known.
	KeyTool = "Agent-Tool"
)

// Trailer is one stamped session. Tool is "<agent>/<version>" and may be empty.
type Trailer struct {
	SessionID string
	Tool      string
}

// trailerLine matches "Token: value" the way git's interpret-trailers does: a token of
// letters, digits and dashes, a colon, then the value.
var trailerLine = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9-]*):[ \t]*(.*)$`)

// scissors is git's cut line; everything below it is discarded by git and is never
// part of the message.
const scissors = " ------------------------ >8 ------------------------"

// Format renders the trailer lines for one session, in the order git will read them.
//
// Values are stripped of anything that would end the line they belong on. A trailer
// is a single line by definition, so a value carrying a newline does not produce a
// longer trailer — it produces whatever the attacker put after the newline, as its
// own line, indistinguishable from a real trailer to git, to `git interpret-trailers`,
// and to the Terma backend and GitHub App that parse these. This is the last point
// before the bytes reach a commit message, so the guarantee is made here rather than
// assumed of every caller.
func Format(t Trailer) []string {
	lines := []string{KeySessionID + ": " + oneLine(t.SessionID)}
	if tool := oneLine(t.Tool); tool != "" {
		lines = append(lines, KeyTool+": "+tool)
	}
	return lines
}

// oneLine collapses a value to the single line a trailer can hold, dropping CR and LF
// along with the leading and trailing space that would be left behind.
func oneLine(v string) string {
	if !strings.ContainsAny(v, "\r\n") {
		return v
	}
	return strings.TrimSpace(strings.NewReplacer("\r", "", "\n", "").Replace(v))
}

// Parse returns the sessions already stamped in message. A tool trailer is paired
// with the session trailer immediately preceding it.
func Parse(message, commentChar string) []Trailer {
	body, _ := splitComments(message, commentChar)
	var out []Trailer
	for line := range strings.SplitSeq(body, "\n") {
		m := trailerLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key, value := m[1], strings.TrimSpace(m[2])
		switch {
		case strings.EqualFold(key, KeySessionID) && value != "":
			out = append(out, Trailer{SessionID: value})
		case strings.EqualFold(key, KeyTool) && len(out) > 0 && out[len(out)-1].Tool == "":
			out[len(out)-1].Tool = value
		}
	}
	return out
}

// Stamp appends a trailer block for every session not already present and returns
// the new message. It reports false when nothing changed, so the caller can skip the
// write entirely.
//
// Layout rules follow git: trailers go in the final paragraph of the message body;
// if that paragraph is already a trailer block (every line "Token: value") the new
// lines join it, otherwise a blank line opens a new block. Comment lines and the
// scissors section stay below the trailers, where git expects them.
func Stamp(message string, sessions []Trailer, commentChar string) (string, bool) {
	present := map[string]bool{}
	for _, t := range Parse(message, commentChar) {
		present[t.SessionID] = true
	}
	var lines []string
	for _, s := range sessions {
		// An id that is not already a single clean line is not a real session id, and
		// a sanitized version of it would attribute the commit to something that does
		// not exist. Skip it: an unstamped commit is recoverable, a wrongly stamped
		// one is a false record.
		if s.SessionID == "" || s.SessionID != oneLine(s.SessionID) || present[s.SessionID] {
			continue
		}
		present[s.SessionID] = true
		lines = append(lines, Format(s)...)
	}
	if len(lines) == 0 {
		return message, false
	}

	body, tail := splitComments(message, commentChar)
	body = strings.TrimRight(body, " \t\r\n")

	var b strings.Builder
	if body == "" {
		// No subject yet (an empty template): the trailers still belong in the
		// message; git keeps them when the subject is written above.
		b.WriteString(strings.Join(lines, "\n"))
	} else {
		b.WriteString(body)
		if isTrailerBlock(lastParagraph(body)) {
			b.WriteString("\n")
		} else {
			b.WriteString("\n\n")
		}
		b.WriteString(strings.Join(lines, "\n"))
	}
	b.WriteString("\n")
	if tail != "" {
		b.WriteString(tail)
	}
	return b.String(), true
}

// splitComments separates the message proper from the trailing comment/scissors
// section git shows in the editor. Comments interleaved earlier in the message are
// left in place: only the trailing run (and anything after the scissors line) is
// moved aside so trailers land above it.
func splitComments(message, commentChar string) (body, tail string) {
	if commentChar == "" {
		commentChar = "#"
	}
	lines := strings.Split(message, "\n")
	cut := len(lines)
	// Scissors: everything from the cut line on is tail.
	for i, line := range lines {
		if strings.HasPrefix(line, commentChar) && strings.Contains(line, scissors) {
			cut = i
			break
		}
	}
	// Trailing comment / blank lines above the cut are tail as well.
	for cut > 0 {
		line := lines[cut-1]
		if strings.HasPrefix(line, commentChar) || strings.TrimSpace(line) == "" {
			cut--
			continue
		}
		break
	}
	body = strings.Join(lines[:cut], "\n")
	tail = strings.Join(lines[cut:], "\n")
	return body, tail
}

func lastParagraph(body string) []string {
	lines := strings.Split(body, "\n")
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	start := end
	for start > 0 && strings.TrimSpace(lines[start-1]) != "" {
		start--
	}
	return lines[start:end]
}

// isTrailerBlock reports whether every line is a trailer (or a folded continuation
// line), which is git's own test for the final block. A single-line paragraph is
// not a block: it is almost always the subject of a one-line message.
func isTrailerBlock(lines []string) bool {
	if len(lines) == 0 {
		return false
	}
	trailers := 0
	for _, line := range lines {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue // continuation of the previous trailer
		}
		if !trailerLine.MatchString(line) {
			return false
		}
		trailers++
	}
	return trailers > 0 && (len(lines) != 1 || !looksLikeSubject(lines[0]))
}

// looksLikeSubject guards the one ambiguous case: "fix: handle nil" parses as a
// trailer ("fix" then a value) but is a conventional-commit subject. Real trailer
// tokens are capitalized or contain a dash (Signed-off-by, Agent-Session-Id).
func looksLikeSubject(line string) bool {
	m := trailerLine.FindStringSubmatch(line)
	if m == nil {
		return true
	}
	token := m[1]
	return !strings.Contains(token, "-") && strings.ToLower(token) == token
}
