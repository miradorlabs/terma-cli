// Package trailer stamps agent sessions into commit messages as git trailers, the join
// between telemetry's session ids and a commit's sha; it is pure string manipulation.
package trailer

import (
	"regexp"
	"strings"
)

// The trailer keys, a contract with the backend and the GitHub App that parse them.
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

// Agent splits Tool into the agent, which with SessionID is the session's identity, and
// its version, which is not.
func (t Trailer) Agent() (agent, version string) {
	agent, version, _ = strings.Cut(t.Tool, "/")
	return agent, version
}

// key is the session's identity: one agent's id, whatever its version.
func (t Trailer) key() [2]string {
	agent, _ := t.Agent()
	return [2]string{agent, t.SessionID}
}

// trailerLine matches "Token: value" the way git's interpret-trailers does.
var trailerLine = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9-]*):[ \t]*(.*)$`)

// scissors is git's cut line; git discards everything below it.
const scissors = " ------------------------ >8 ------------------------"

// Format renders the trailer lines for one session, stripping line breaks from values:
// a newline would forge a trailer of the attacker's choosing.
func Format(t Trailer) []string {
	lines := []string{KeySessionID + ": " + oneLine(t.SessionID)}
	if tool := oneLine(t.Tool); tool != "" {
		lines = append(lines, KeyTool+": "+tool)
	}
	return lines
}

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

// Stamp appends a trailer for every session not already present, by agent and id, laid out
// as git does, and reports false when nothing changed.
func Stamp(message string, sessions []Trailer, commentChar string) (string, bool) {
	present := map[[2]string]bool{}
	for _, t := range Parse(message, commentChar) {
		present[t.key()] = true
	}
	var lines []string
	for _, s := range sessions {
		// A multi-line id is no real session; an unstamped commit beats a false record.
		if s.SessionID == "" || s.SessionID != oneLine(s.SessionID) || present[s.key()] {
			continue
		}
		present[s.key()] = true
		lines = append(lines, Format(s)...)
	}
	if len(lines) == 0 {
		return message, false
	}

	body, tail := splitComments(message, commentChar)
	body = strings.TrimRight(body, " \t\r\n")

	var b strings.Builder
	if body == "" {
		// An empty template: git keeps the trailers when the subject is written above.
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

// splitComments separates the message from its trailing comment and scissors section,
// so trailers land above it.
func splitComments(message, commentChar string) (body, tail string) {
	if commentChar == "" {
		commentChar = "#"
	}
	lines := strings.Split(message, "\n")
	cut := len(lines)
	for i, line := range lines {
		if strings.HasPrefix(line, commentChar) && strings.Contains(line, scissors) {
			cut = i
			break
		}
	}
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

// isTrailerBlock is git's test for the final block, except that a lone line that looks
// like a subject is not one.
func isTrailerBlock(lines []string) bool {
	if len(lines) == 0 {
		return false
	}
	trailers := 0
	for _, line := range lines {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		if !trailerLine.MatchString(line) {
			return false
		}
		trailers++
	}
	return trailers > 0 && (len(lines) != 1 || !looksLikeSubject(lines[0]))
}

// looksLikeSubject catches "fix: handle nil": real trailer tokens are capitalized or dashed.
func looksLikeSubject(line string) bool {
	m := trailerLine.FindStringSubmatch(line)
	if m == nil {
		return true
	}
	token := m[1]
	return !strings.Contains(token, "-") && strings.ToLower(token) == token
}
