package antigravity

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

// fileURI is the file:// URI of an absolute path, with host as its host.
func fileURI(host, path string) string {
	slashed := filepath.ToSlash(path)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed // file:///C:/dir on Windows
	}
	return (&url.URL{Scheme: "file", Host: host, Path: slashed}).String()
}

// argsOf is v as a call's arguments.
func argsOf(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// jsonText is v encoded as JSON, as a tool may carry it inside a string.
func jsonText(t *testing.T, v any) string { return string(argsOf(t, v)) }

// A path is judged under its other spellings: a file:// URI, under any key; an object key that
// is absolute, starts with "~" or is a file:// URI; a value inside a string that holds JSON.
func TestAntigravityJudgesAPathUnderAnySpelling(t *testing.T) {
	g := newPathRig(t)
	excluded, collected := filepath.Join(g.excluded, "file.txt"), filepath.Join(g.collected, "file.txt")
	relative, err := filepath.Rel(g.collected, excluded)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args any
		want bool
	}{
		// A file:// URI.
		{"a file URI of an excluded file", map[string]any{"uri": fileURI("", excluded)}, true},
		{"a file URI of a collected file", map[string]any{"uri": fileURI("", collected)}, false},
		{"a file URI under a file key", map[string]any{"Path": fileURI("", excluded)}, true},
		{"a file URI on localhost, excluded", map[string]any{"uri": fileURI("localhost", excluded)}, true},
		{"a file URI on localhost, collected", map[string]any{"uri": fileURI("localhost", collected)}, false},
		{"a file URI on another host", map[string]any{"uri": fileURI("build-box", collected)}, true},
		{"a file URI in capitals, excluded", map[string]any{"uri": "FILE" + strings.TrimPrefix(fileURI("", excluded), "file")}, true},
		{"a file URI in capitals, collected", map[string]any{"uri": "FILE" + strings.TrimPrefix(fileURI("", collected), "file")}, false},
		{"a file URI of a folder holding an excluded clone", map[string]any{"uri": fileURI("", g.collected)}, true},
		{"a file URI in a list", map[string]any{"uris": []any{fileURI("", collected), fileURI("", excluded)}}, true},
		{"an https URL whose path names an excluded file", map[string]any{"url": "https://example.com" + filepath.ToSlash(excluded)}, false},
		// An object key.
		{"an excluded file as a key", map[string]any{"edits": map[string]any{excluded: "made-up"}}, true},
		{"a collected file as a key", map[string]any{"edits": map[string]any{collected: "made-up"}}, false},
		{"a file URI of an excluded file as a key", map[string]any{"edits": map[string]any{fileURI("", excluded): 1}}, true},
		{"a path from the home folder as a key", map[string]any{"edits": map[string]any{"~/notes.txt": 1}}, true},
		{"a relative path that climbs, as a key", map[string]any{"edits": map[string]any{relative: 1}}, false},
		// A string that holds JSON.
		{"an object in a string, excluded", map[string]any{"payload": jsonText(t, map[string]any{"File": excluded})}, true},
		{"an object in a string, collected", map[string]any{"payload": jsonText(t, map[string]any{"File": collected})}, false},
		{"a list in a string, excluded", map[string]any{"payload": "  " + jsonText(t, []any{collected, excluded}) + "\n"}, true},
		{"a key in a string, excluded", map[string]any{"payload": jsonText(t, map[string]any{excluded: true})}, true},
		{"a string in a string in a string", map[string]any{"payload": jsonText(t, map[string]any{"inner": jsonText(t, map[string]any{"inner": jsonText(t, []any{excluded})})})}, true},
		{"three strings deep, collected", map[string]any{"payload": jsonText(t, map[string]any{"inner": jsonText(t, map[string]any{"inner": jsonText(t, []any{collected})})})}, false},
		{"four strings deep", map[string]any{"payload": jsonText(t, map[string]any{"a": jsonText(t, map[string]any{"b": jsonText(t, map[string]any{"c": jsonText(t, []any{"made-up"})})})})}, true},
		{"a string that does not parse as JSON", map[string]any{"payload": "{" + excluded}, false},
		{"a list that does not parse as JSON", map[string]any{"payload": "[" + excluded + " made-up"}, false},
	} {
		if got := newAntigravityPaths(g.pol).refuses(g.collected, argsOf(t, tc.args)); got != tc.want {
			t.Errorf("%s: refused %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The count of values judged runs across strings that hold JSON.
func TestAntigravityCountsPathsInsideJSONStrings(t *testing.T) {
	g := newPathRig(t)
	file := filepath.Join(g.collected, "file.txt")
	for _, n := range []int{antigravityPathsPerCall, antigravityPathsPerCall + 1} {
		list := make([]string, n-1)
		for i := range list {
			list[i] = file
		}
		args := argsOf(t, map[string]any{"Path": file, "payload": jsonText(t, list)})
		want := n > antigravityPathsPerCall
		if got := newAntigravityPaths(g.pol).refuses(g.collected, args); got != want {
			t.Errorf("%d collected paths, all but one in a JSON string: refused %v, want %v", n, got, want)
		}
	}
}

// A file:// URI names the absolute path it holds, or none terma can place.
func TestAntigravityFileURI(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "a b", "file.txt")
	for _, tc := range []struct {
		name, s, want string
		uri           bool
	}{
		{"no host", fileURI("", abs), abs, true},
		{"localhost", fileURI("localhost", abs), abs, true},
		{"another host", fileURI("build-box", abs), "", true},
		{"a port", fileURI("localhost:8080", abs), "", true},
		{"no path", "file://", "", true},
		{"two lines", fileURI("", abs) + "\nmade-up", "", false},
		{"https", "https://example.com/file.txt", "", false},
		{"a word", "file", "", false},
	} {
		got, uri := antigravityFileURI(tc.s)
		if got != tc.want || uri != tc.uri {
			t.Errorf("%s: %q, %v; want %q, %v", tc.name, got, uri, tc.want, tc.uri)
		}
	}
}
