package antigravity

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// What was said in a conversation: the prompt, the reply, a tool's arguments and its result.
// Which of it a hook may send is the policy's, the developer's and rule 3.3's
// (content_paths.go); this file holds what is sent once it may be: agy's envelope and T3
// Code's wrapper taken off a prompt, and the cuts.

// antigravityContentLimit bounds one content value, in bytes, the bound terma cuts at
// everywhere; a marker counts inside it.
const antigravityContentLimit = 16 << 10

// antigravityArgumentValueLimit is what each long string in a large call's arguments is cut
// to, so the arguments stay an object whose short values (a path, a command) survive.
const antigravityArgumentValueLimit = 2 << 10

// agy's envelope around the client's text in a USER_INPUT line.
const (
	agyRequestOpen  = "<USER_REQUEST>\n"
	agyRequestClose = "\n</USER_REQUEST>"
	agyMetadataOpen = "<ADDITIONAL_METADATA>"
)

// T3 Code's wrapper around a thread's prompt: its instructions, the request, its runtime notes.
const (
	t3InstructionsOpen  = "<t3_code_instructions>"
	t3InstructionsClose = "</t3_code_instructions>"
	t3RequestOpen       = "<user_request>"
	t3RequestClose      = "</user_request>"
	t3RuntimeOpen       = "<runtime_info>"
	t3RuntimeClose      = "</runtime_info>"
)

// antigravityPrompt is the text a USER_INPUT line's content holds as the person's prompt:
// agy's envelope off, then T3 Code's wrapper off. ok is false when the content opens with
// T3's wrapper and the split is unsure: the prompt is then sent without text, so nothing of
// T3's is stored as the person's words.
func antigravityPrompt(content string) (text string, ok bool) {
	return t3Request(agyRequest(content))
}

// agyRequest is what lies between the first `<USER_REQUEST>\n` and the last
// `\n</USER_REQUEST>` followed by `<ADDITIONAL_METADATA>` or the end of the content, both
// after white space; the content whole when it does not have that shape.
func agyRequest(content string) string {
	start := strings.Index(content, agyRequestOpen)
	if start < 0 {
		return content
	}
	body := content[start+len(agyRequestOpen):]
	for end := len(body); end > 0; {
		i := strings.LastIndex(body[:end], agyRequestClose)
		if i < 0 {
			break
		}
		after := strings.TrimLeft(body[i+len(agyRequestClose):], " \t\r\n")
		if after == "" || strings.HasPrefix(after, agyMetadataOpen) {
			return body[:i]
		}
		end = i
	}
	return content
}

// t3Request takes T3 Code's wrapper off a prompt, reading its structure only: a text that
// starts with `<t3_code_instructions>`, then after the first `</t3_code_instructions>` and
// white space `<user_request>`, and ends with `</user_request>`, optionally followed by one
// `<runtime_info>` block, is the request between those tags, less one leading and one
// trailing newline. The closing tag is the last one that fits, so a request that quotes it is
// not cut short. A text that does not start with the opening tag is not T3's and is the
// prompt as it is; one that does but breaks the rest is unsure (ok false).
func t3Request(s string) (string, bool) {
	if !strings.HasPrefix(s, t3InstructionsOpen) {
		return s, true
	}
	i := strings.Index(s, t3InstructionsClose)
	if i < 0 {
		return "", false
	}
	rest := strings.TrimLeft(s[i+len(t3InstructionsClose):], " \t\r\n")
	body, found := strings.CutPrefix(rest, t3RequestOpen)
	if !found {
		return "", false
	}
	for end := len(body); end > 0; {
		k := strings.LastIndex(body[:end], t3RequestClose)
		if k < 0 {
			break
		}
		if t3RequestTail(body[k+len(t3RequestClose):]) {
			req := strings.TrimPrefix(body[:k], "\n")
			return strings.TrimSuffix(req, "\n"), true
		}
		end = k
	}
	return "", false
}

// t3RequestTail reports whether what follows T3's closing request tag is white space, or one
// runtime_info block and white space.
func t3RequestTail(tail string) bool {
	tail = strings.TrimSpace(tail)
	if tail == "" {
		return true
	}
	return strings.HasPrefix(tail, t3RuntimeOpen) && strings.HasSuffix(tail, t3RuntimeClose) &&
		strings.Count(tail, t3RuntimeOpen) == 1 && strings.Count(tail, t3RuntimeClose) == 1
}

// cutText is a prompt's or a reply's text at most antigravityContentLimit bytes, on a UTF-8
// boundary, and whether it was cut (terma.message.truncated).
func cutText(s string) (string, bool) {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= antigravityContentLimit {
		return s, false
	}
	return hookrun.TruncateBytes(s, antigravityContentLimit), true
}

// cutResult is a tool's result at most antigravityContentLimit bytes, ending `…[N chars]`
// when cut, N the characters dropped. agy's own cut notice inside the text is left as it is.
func cutResult(s string) string {
	return cutMarked(strings.ToValidUTF8(s, "�"), antigravityContentLimit)
}

// cutMarked is s at most limit bytes, on a UTF-8 boundary, ending `…[N chars]` when cut, N
// the characters dropped; the ending counts inside the limit.
func cutMarked(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	keep := limit
	for range 4 {
		kept := hookrun.TruncateBytes(s, keep)
		marker := "…[" + strconv.Itoa(utf8.RuneCountInString(s[len(kept):])) + " chars]"
		if len(kept)+len(marker) <= limit {
			return kept + marker
		}
		keep = limit - len(marker)
	}
	// The marker's digits settle within a few rounds; this only guards the loop.
	return hookrun.TruncateBytes(s, max(limit-32, 0)) + "…"
}

// cutArguments is a tool call's arguments as gen_ai.tool.call.arguments: the object whole
// when its JSON is at most antigravityContentLimit bytes; else with every string value over
// antigravityArgumentValueLimit cut to it with the `…[N chars]` ending, still an object, so a
// path or a command survives; else that JSON as a string, cut with the same ending. ok is
// false for arguments that are not a JSON object.
func cutArguments(raw json.RawMessage) (any, bool) {
	var args map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &args) != nil || args == nil {
		return nil, false
	}
	b, err := marshalArguments(args)
	if err != nil {
		return nil, false
	}
	if len(b) <= antigravityContentLimit {
		return args, true
	}
	shortened, _ := shortenStrings(args).(map[string]any)
	if b, err = marshalArguments(shortened); err != nil {
		return nil, false
	}
	if len(b) <= antigravityContentLimit {
		return shortened, true
	}
	return cutMarked(string(b), antigravityContentLimit), true
}

// shortenStrings is v with every string over antigravityArgumentValueLimit bytes cut to it.
func shortenStrings(v any) any {
	switch t := v.(type) {
	case string:
		return cutMarked(t, antigravityArgumentValueLimit)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = shortenStrings(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = shortenStrings(x)
		}
		return out
	}
	return v
}

// marshalArguments is v's JSON as it leaves: compact, with no HTML escaping to swell it.
func marshalArguments(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
