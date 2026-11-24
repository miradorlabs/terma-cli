package antigravity

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The fixtures here are made-up text in the shapes agy and T3 Code write.

// t3Wrapped is a prompt in T3 Code's wrapper, as a thread sends it.
func t3Wrapped(request string) string {
	return "<t3_code_instructions>\nmade-up instructions with <app> and <cmd> tags\n</t3_code_instructions>\n" +
		"<user_request>\n" + request + "\n</user_request>\n<runtime_info>\nmade-up runtime notes\n</runtime_info>"
}

// agyWrapped is a client's text in agy's envelope, as the transcript holds it.
func agyWrapped(text string) string {
	return "<USER_REQUEST>\n" + text + "\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nmade-up metadata\n</ADDITIONAL_METADATA>"
}

func TestAntigravityPromptTakesAgysEnvelopeOff(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, content, want string }{
		{"envelope", agyWrapped("fix the made-up bug"), "fix the made-up bug"},
		{"settings change after", agyWrapped("a prompt") + "\n<USER_SETTINGS_CHANGE>\nmade-up\n</USER_SETTINGS_CHANGE>", "a prompt"},
		{"no metadata", "<USER_REQUEST>\nonly a request\n</USER_REQUEST>", "only a request"},
		{"quotes the closing tag", agyWrapped("say \n</USER_REQUEST> twice"), "say \n</USER_REQUEST> twice"},
		{"multi-line", agyWrapped("line one\nline two"), "line one\nline two"},
		{"no envelope", "a bare made-up prompt", "a bare made-up prompt"},
		{"unclosed", "<USER_REQUEST>\nnever closed", "<USER_REQUEST>\nnever closed"},
	} {
		if got, ok := antigravityPrompt(tc.content); !ok || got != tc.want {
			t.Errorf("%s: %q, %v; want %q", tc.name, got, ok, tc.want)
		}
	}
}

func TestAntigravityPromptTakesT3sWrapperOff(t *testing.T) {
	t.Parallel()
	delegated := "[T3 Code: the agent in T3 thread \"made-up\" sent this message, not the user.]\n\nAct as a made-up sub-agent.\n\nThe task."
	for _, tc := range []struct {
		name, content, want string
		ok                  bool
	}{
		// The probe's shape: T3's bracket line stays, it is the platform's to read.
		{"the probe's shape", agyWrapped(t3Wrapped(delegated)), delegated, true},
		{"no runtime info", agyWrapped(strings.TrimSuffix(t3Wrapped("typed"), "\n<runtime_info>\nmade-up runtime notes\n</runtime_info>")), "typed", true},
		{"quotes the closing tag", agyWrapped(t3Wrapped("explain </user_request> to me")), "explain </user_request> to me", true},
		{"runtime info quoting the tag", agyWrapped(strings.Replace(t3Wrapped("ask"), "made-up runtime notes", "notes </user_request>", 1)), "ask", true},
		{"renamed inner tag", agyWrapped(strings.ReplaceAll(t3Wrapped("lost"), "user_request>", "user_req>")), "", false},
		{"renamed closing instructions", agyWrapped(strings.Replace(t3Wrapped("lost"), "</t3_code_instructions>", "</t3_instructions>", 1)), "", false},
		{"trailing text after", agyWrapped(t3Wrapped("lost") + "\nmore"), "", false},
		{"plain prompt naming the tags later", agyWrapped("why does <t3_code_instructions> wrap <user_request>?"), "why does <t3_code_instructions> wrap <user_request>?", true},
		// A byte order mark and white space before the opening tag are skipped.
		{"after a byte order mark", agyWrapped("\uFEFF" + t3Wrapped("typed")), "typed", true},
		{"after white space", agyWrapped(" \n\t" + t3Wrapped("typed")), "typed", true},
		{"after a mark and white space, broken", agyWrapped("\uFEFF\n" + strings.ReplaceAll(t3Wrapped("lost"), "user_request>", "user_req>")), "", false},
	} {
		got, ok := antigravityPrompt(tc.content)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: %q, %v; want %q, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
	// A renamed opening tag is not T3's wrapper: the whole text goes, cut at the bound.
	renamed := strings.Replace(t3Wrapped("typed")+strings.Repeat("x", antigravityContentLimit), "<t3_code_instructions>", "<t3_instructions>", 1)
	got, ok := antigravityPrompt(agyWrapped(renamed))
	if text, cut := cutText(got); !ok || !strings.HasPrefix(text, "<t3_instructions>") || !cut || len(text) != antigravityContentLimit {
		t.Errorf("renamed opening tag: ok %v, cut %v, %d bytes", ok, cut, len(text))
	}
}

func TestAntigravityCutsTextAtTheBound(t *testing.T) {
	t.Parallel()
	at := strings.Repeat("a", antigravityContentLimit)
	if got, cut := cutText(at); cut || got != at {
		t.Errorf("a text at the bound was cut")
	}
	if got, cut := cutText(at + "b"); !cut || got != at {
		t.Errorf("a text past the bound: %d bytes, cut %v", len(got), cut)
	}
	// A two-byte character across the edge is dropped whole.
	edge := strings.Repeat("a", antigravityContentLimit-1) + "é"
	if got, cut := cutText(edge); !cut || len(got) != antigravityContentLimit-1 || !utf8.ValidString(got) {
		t.Errorf("multi-byte edge: %d bytes, cut %v, valid %v", len(got), cut, utf8.ValidString(got))
	}
}

func TestAntigravityCutsAResultWithItsEnding(t *testing.T) {
	t.Parallel()
	at := strings.Repeat("r", antigravityContentLimit)
	if cutResult(at) != at {
		t.Error("a result at the bound was cut")
	}
	for _, s := range []string{
		at + strings.Repeat("r", 5000),
		strings.Repeat("é", antigravityContentLimit),  // two bytes each
		strings.Repeat("日本", antigravityContentLimit), // three bytes each
	} {
		got := cutResult(s)
		kept, marker, found := strings.Cut(got, "…[")
		if !found || len(got) > antigravityContentLimit || !utf8.ValidString(got) || !strings.HasSuffix(marker, " chars]") {
			t.Errorf("cut %d bytes to %d: %q", len(s), len(got), got[max(0, len(got)-20):])
			continue
		}
		n, _ := strconv.Atoi(strings.TrimSuffix(marker, " chars]"))
		if utf8.RuneCountInString(kept)+n != utf8.RuneCountInString(s) {
			t.Errorf("kept %d characters and named %d dropped, of %d", utf8.RuneCountInString(kept), n, utf8.RuneCountInString(s))
		}
	}
}

func TestAntigravityCutsArguments(t *testing.T) {
	t.Parallel()
	small := json.RawMessage(`{"Path":"/w/a.go","Overwrite":true,"Content":"made-up <code> & more"}`)
	got, ok := cutArguments(small)
	if m, isMap := got.(map[string]any); !ok || !isMap || m["Content"] != "made-up <code> & more" || m["Overwrite"] != true {
		t.Fatalf("small arguments: %#v, %v", got, ok)
	}
	// Arguments whose JSON is exactly at the bound go whole.
	atBound := strings.Repeat("b", antigravityContentLimit-len(`{"Content":""}`))
	got, ok = cutArguments(json.RawMessage(`{"Content":"` + atBound + `"}`))
	if m, isMap := got.(map[string]any); !ok || !isMap || m["Content"] != atBound {
		t.Fatalf("arguments at the bound were cut")
	}
	// A 1 MiB write keeps its path and stays an object; its content is cut and marked.
	content := strings.Repeat("w", 1<<20)
	big, _ := json.Marshal(map[string]any{"Path": "/w/big.txt", "Content": content, "Overwrite": true})
	got, ok = cutArguments(big)
	m, isMap := got.(map[string]any)
	if !ok || !isMap || m["Path"] != "/w/big.txt" || m["Overwrite"] != true {
		t.Fatalf("a 1 MiB write: %T, %v", got, ok)
	}
	cut, _ := m["Content"].(string)
	if len(cut) > antigravityArgumentValueLimit || !strings.HasSuffix(cut, " chars]") || !strings.Contains(cut, "…[") {
		t.Errorf("the write's content is %d bytes, ends %q", len(cut), cut[max(0, len(cut)-20):])
	}
	if b, _ := marshalArguments(m); len(b) > antigravityContentLimit {
		t.Errorf("the write's arguments are %d bytes", len(b))
	}
	// Too many values to fit even shortened: a string, cut at the bound with the ending.
	many := map[string]any{}
	for i := range 20 {
		many["Value"+strconv.Itoa(i)] = strings.Repeat("v", 4<<10)
	}
	raw, _ := json.Marshal(many)
	got, ok = cutArguments(raw)
	if s, isString := got.(string); !ok || !isString || len(s) > antigravityContentLimit || !strings.HasSuffix(s, " chars]") {
		t.Errorf("arguments past the bound shortened: %T, %v", got, ok)
	}
	for _, raw := range []string{``, `"a string"`, `[1,2]`, `null`, `{bad`} {
		if _, ok := cutArguments(json.RawMessage(raw)); ok {
			t.Errorf("arguments %q are not an object", raw)
		}
	}
}

// A line's time is its created_at, but never later than the hook's real time.
func TestAntigravityLineTimeIsNeverAfterTheHook(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 16, 52, 0, 0, time.UTC)
	for _, tc := range []struct {
		created string
		want    time.Time
	}{
		{"2026-11-20T16:51:47Z", time.Date(2026, 11, 20, 16, 51, 47, 0, time.UTC)},
		{"2026-11-20T16:52:00Z", now},
		{"2026-11-20T17:00:00Z", now},
		{"not a time", now},
	} {
		if got := antigravityLineTime(antigravityStep{CreatedAt: tc.created}, now); !got.Equal(tc.want) {
			t.Errorf("%s: %v, want %v", tc.created, got, tc.want)
		}
	}
}
