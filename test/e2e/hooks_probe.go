package e2e

import (
	"bytes"
	"compress/gzip"
	"debug/elf"
	"debug/macho"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// The hooks probe: every hook event a harness build knows, read from the build itself, and
// what it hands a hook when one fires. The probe registers a recording hook for every event
// the build knows (the test binary itself, started as one: RunHookRecorder), in place of
// terma's, and runs turns that make as many of them fire as it cheaply can. The recorder
// saves each run's event, stdin, arguments, working directory, environment and timing; the
// probe records, per event, whether it fired, its payload's key paths and their kinds, the
// environment variables the harness set for it, the directory it ran in and whether it takes
// a matcher. A second pass has the recorder answer the main control points (exit 2 with a
// message, a JSON decision, context to add) and records what the build did with each.
//
// Whether an event takes a matcher is measured, not read: each event has two recording hooks,
// one matching everything and one whose matcher matches nothing. An event that runs the
// second ignores matchers.

// hookRecorderArg is the first argument that starts the test binary as the recording hook.
const hookRecorderArg = "terma-hook-recorder"

// The recording hook's two registrations of an event: one whose matcher matches everything (or
// that has none), and one whose matcher matches nothing.
const (
	hookMatchAll  = "all"
	hookMatchNone = "none"
)

// hookNoMatch is a matcher no tool, source or trigger is named: as a plain name and as a
// regular expression, it matches nothing a harness compares it with.
const hookNoMatch = "TERMA_NO_MATCH"

// HookCall is one run of the recording hook, as it saved it.
type HookCall struct {
	// Event is the event the hook was registered for; Variant which of its two registrations
	// ran (hookMatchAll, hookMatchNone).
	Event   string `json:"event"`
	Variant string `json:"variant"`
	// Stdin is what the harness wrote to the hook's standard input.
	Stdin string `json:"stdin"`
	// Extra are the arguments the harness added after the registered command's own.
	Extra []string          `json:"extra,omitempty"`
	Cwd   string            `json:"cwd"`
	Env   map[string]string `json:"env"`
	Start time.Time         `json:"start"`
	End   time.Time         `json:"end"`
}

// HookControl is how the recording hook answers an event's runs in a control pass: what it
// prints and the status it exits with. Times is how many of the event's runs it answers so,
// the rest answered as by default (nothing printed, status 0); 0 is every one.
type HookControl struct {
	Exit   int    `json:"exit,omitempty"`
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
	Times  int    `json:"times,omitempty"`
}

// RunHookRecorder is the recording hook, where the test binary was started as one (its first
// argument hookRecorderArg, then the recorder's folder, the event and the variant): it saves
// the run, answers as the folder's control.json says, and exits. It returns where the binary
// was started as anything else.
func RunHookRecorder() {
	if len(os.Args) < 5 || os.Args[1] != hookRecorderArg {
		return
	}
	os.Exit(recordHook(os.Args[2], os.Args[3], os.Args[4], os.Args[5:], os.Stdin, os.Stdout, os.Stderr))
}

// recordHook saves one run of the recording hook in dir and answers it; it returns the status
// to exit with.
func recordHook(dir, event, variant string, extra []string, stdin io.Reader, stdout, stderr io.Writer) int {
	call := HookCall{Event: event, Variant: variant, Extra: extra, Start: time.Now().UTC(), Env: map[string]string{}}
	in, _ := io.ReadAll(io.LimitReader(stdin, 16<<20))
	call.Stdin = string(in)
	call.Cwd, _ = os.Getwd()
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		call.Env[k] = v
	}
	call.End = time.Now().UTC()
	data, _ := json.Marshal(call)
	calls := filepath.Join(dir, "calls")
	_ = os.MkdirAll(calls, 0o755)
	name := fmt.Sprintf("%020d-%d-%s-%s.json", call.Start.UnixNano(), os.Getpid(), event, variant)
	_ = os.WriteFile(filepath.Join(calls, name), data, 0o644)
	if variant != hookMatchAll {
		return 0
	}
	var controls map[string]HookControl
	raw, err := os.ReadFile(filepath.Join(dir, "control.json"))
	if err != nil || json.Unmarshal(raw, &controls) != nil {
		return 0
	}
	c, ok := controls[event]
	if !ok || !claimHookControl(dir, event, c.Times) {
		return 0
	}
	_, _ = io.WriteString(stdout, c.Stdout)
	_, _ = io.WriteString(stderr, c.Stderr)
	return c.Exit
}

// claimHookControl reports whether this run of event is one of the first times its control
// answers (every one where times is 0): each run claims the next of times marker files.
func claimHookControl(dir, event string, times int) bool {
	if times <= 0 {
		return true
	}
	claims := filepath.Join(dir, "claims")
	_ = os.MkdirAll(claims, 0o755)
	for n := 1; n <= times; n++ {
		f, err := os.OpenFile(filepath.Join(claims, fmt.Sprintf("%s-%d", event, n)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_ = f.Close()
			return true
		}
	}
	return false
}

// HookRecorderCommand is the command line a harness's hook entry runs to record event's
// variant in dir: the test binary, started as the recording hook. Each variant's command
// differs, so a harness that runs each distinct command once still runs both.
func HookRecorderCommand(dir, event, variant string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return shellWord(exe) + " " + hookRecorderArg + " " + shellWord(dir) + " " + event + " " + variant, nil
}

// shellWord is s quoted for a POSIX shell.
func shellWord(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// HookCallsIn are the runs the recorder saved in dir, in the order they started.
func HookCallsIn(dir string) []HookCall {
	entries, _ := os.ReadDir(filepath.Join(dir, "calls"))
	var out []HookCall
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, "calls", e.Name()))
		if err != nil {
			continue
		}
		var c HookCall
		if json.Unmarshal(data, &c) == nil {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// HooksProbe is what the "hooks" probe records of a build.
type HooksProbe struct {
	// Supported are the hook events the build knows, sorted; Source where they were read:
	// "binary" (its executable or package), "source" (its public source at its tag), or "seed"
	// (the suite's own list, where neither named any).
	Supported []string                   `json:"supported"`
	Source    string                     `json:"source"`
	Events    map[string]*HookEventShape `json:"events"`
	// NotFired says, for each event the build knows that no run fired, why.
	NotFired map[string]string `json:"not_fired,omitempty"`
}

// HookEventShape is what the probe saw of one event.
type HookEventShape struct {
	Fired bool `json:"fired"`
	Count int  `json:"count"`
	// Keys are the dotted key paths of the event's payloads, four deep, a list's items' under
	// "[]"; Kinds the kind of value each held (text, number, bool, list, map, null), kinds
	// joined with "|" where runs differed.
	Keys  []string          `json:"keys"`
	Kinds map[string]string `json:"kinds"`
	// Stdin is what the hook's standard input held: "json", "text" or "empty".
	Stdin string `json:"stdin,omitempty"`
	// Env are the environment variables the harness set for the hook, the ones it was started
	// with left out; EnvPaths the value of each that is one path in the sandbox, as the first
	// run had it, which normalization makes a placeholder ("<repo>").
	Env      []string          `json:"env"`
	EnvPaths map[string]string `json:"env_paths,omitempty"`
	// Cwd is the directory the event's first run ran in.
	Cwd string `json:"cwd,omitempty"`
	// Args are how many arguments the harness added to the hook's command line, where it added
	// any.
	Args int `json:"args,omitempty"`
	// Matcher says whether the event takes a matcher: false where a hook whose matcher matches
	// nothing ran too. Absent where it did not fire.
	Matcher *bool `json:"matcher,omitempty"`
	// Control is what the build did with the hook's answers, by control point ("exit_2",
	// "json_decision", "additional_context"), where a control pass tried them.
	Control map[string]string `json:"control,omitempty"`
}

// hookPayloadDepth is how deep a payload's key paths go.
const hookPayloadDepth = 4

// hookShellVariables are what the shell a harness runs a hook in sets of itself: not the
// harness's.
var hookShellVariables = map[string]bool{"PWD": true, "OLDPWD": true, "SHLVL": true, "_": true}

// HooksProbeOf shapes calls: started is the environment the harness was started with, and root
// the sandbox folder a path value must be under to be kept.
func HooksProbeOf(supported []string, source string, calls []HookCall, started []string, root string) HooksProbe {
	base := map[string]string{}
	for _, kv := range started {
		k, v, _ := strings.Cut(kv, "=")
		base[k] = v
	}
	p := HooksProbe{Supported: sortedUnique(slices.Clone(supported)), Source: source, Events: map[string]*HookEventShape{}}
	if p.Supported == nil {
		p.Supported = []string{}
	}
	ignored := map[string]bool{}
	type acc struct {
		keys  map[string]map[string]bool
		env   map[string]bool
		paths map[string]string
		stdin map[string]bool
	}
	accs := map[string]*acc{}
	for _, c := range calls {
		if c.Variant == hookMatchNone {
			ignored[c.Event] = true
			continue
		}
		s := p.Events[c.Event]
		a := accs[c.Event]
		if s == nil {
			s = &HookEventShape{Keys: []string{}, Kinds: map[string]string{}, Env: []string{}}
			p.Events[c.Event] = s
			a = &acc{keys: map[string]map[string]bool{}, env: map[string]bool{}, paths: map[string]string{}, stdin: map[string]bool{}}
			accs[c.Event] = a
		}
		s.Fired = true
		s.Count++
		s.Args = max(s.Args, len(c.Extra))
		var payload any
		switch {
		case strings.TrimSpace(c.Stdin) == "":
			a.stdin["empty"] = true
		case json.Unmarshal([]byte(c.Stdin), &payload) == nil:
			a.stdin["json"] = true
			payloadKinds(payload, "", 0, a.keys)
		default:
			a.stdin["text"] = true
		}
		for k, v := range c.Env {
			if hookShellVariables[k] || strings.HasPrefix(k, "__CF") {
				continue
			}
			if was, ok := base[k]; ok && was == v {
				continue
			}
			a.env[k] = true
			// One path, not a list of them (a PATH the harness put its own folder first in), as
			// the event's first run had it.
			_, seen := a.paths[k]
			if !seen && root != "" && !strings.ContainsRune(v, filepath.ListSeparator) && (v == root || strings.HasPrefix(v, root+string(filepath.Separator))) {
				a.paths[k] = v
			}
		}
		if s.Cwd == "" {
			s.Cwd = c.Cwd
		}
	}
	for event, s := range p.Events {
		a := accs[event]
		for k, kinds := range a.keys {
			s.Keys = append(s.Keys, k)
			s.Kinds[k] = strings.Join(slices.Sorted(maps.Keys(kinds)), "|")
		}
		sort.Strings(s.Keys)
		s.Env = slices.Sorted(maps.Keys(a.env))
		if len(a.paths) > 0 {
			s.EnvPaths = a.paths
		}
		s.Stdin = strings.Join(slices.Sorted(maps.Keys(a.stdin)), "|")
		takes := !ignored[event]
		s.Matcher = &takes
	}
	return p
}

// payloadKinds adds the key paths of v under prefix, depth keys deep so far, with each one's
// kind, to kinds. An object of more keys than a record has is a map: its keys are one "*".
func payloadKinds(v any, prefix string, depth int, kinds map[string]map[string]bool) {
	see := func(path string, x any) {
		if kinds[path] == nil {
			kinds[path] = map[string]bool{}
		}
		kinds[path][jsonKind(x)] = true
	}
	switch x := v.(type) {
	case map[string]any:
		if depth >= hookPayloadDepth {
			return
		}
		join := func(k string) string {
			if prefix == "" {
				return k
			}
			return prefix + "." + k
		}
		if len(x) > maxRecordKeys {
			path := join("*")
			for _, item := range x {
				see(path, item)
				payloadKinds(item, path, depth+1, kinds)
			}
			return
		}
		for k, item := range x {
			see(join(k), item)
			payloadKinds(item, join(k), depth+1, kinds)
		}
	case []any:
		for _, item := range x {
			if _, ok := item.(map[string]any); ok {
				payloadKinds(item, prefix+"[]", depth, kinds)
			}
		}
	}
}

// jsonKind names the kind of a JSON value.
func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "text"
	case float64, json.Number:
		return "number"
	case bool:
		return "bool"
	case []any:
		return "list"
	case map[string]any:
		return "map"
	}
	return "unknown"
}

// hookSeeds are the hook events the suite knows each harness's builds to have had: a cross-check
// of what is read from a build, and what the probe registers where nothing is.
var hookSeeds = map[string][]string{
	"claude": {"PreToolUse", "PostToolUse", "PostToolUseFailure", "Notification", "UserPromptSubmit", "SessionStart", "SessionEnd",
		"Stop", "StopFailure", "SubagentStart", "SubagentStop", "PreCompact", "PostCompact", "PermissionRequest", "PermissionDenied"},
	"codex": {"PreToolUse", "PermissionRequest", "PostToolUse", "PreCompact", "PostCompact", "SessionStart", "SessionEnd",
		"UserPromptSubmit", "SubagentStart", "SubagentStop", "Stop"},
	"gemini": {"BeforeTool", "AfterTool", "BeforeAgent", "AfterAgent", "BeforeModel", "AfterModel", "BeforeToolSelection",
		"SessionStart", "SessionEnd", "PreCompress", "Notification"},
	"copilot": {"preToolUse", "postToolUse", "userPromptSubmitted", "sessionStart", "sessionEnd", "errorOccurred", "agentStop", "subagentStop"},
}

// HookEventsOf are the hook events b knows, read from its build, and where: "binary" or "seed"
// (hookSeeds, where the build names none it can be read for). A build that has no hook system
// at all names none, and is "binary" with none.
func HookEventsOf(b Binary) ([]string, string, error) {
	path, err := filepath.EvalSymlinks(b.Path)
	if err != nil {
		return nil, "", err
	}
	var events []string
	switch b.Harness {
	case "claude":
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, "", err
		}
		events = ClaudeHookEventsIn(data)
	case "codex":
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, "", err
		}
		events = CodexHookEventsIn(data)
		if len(events) == 0 && !bytes.Contains(data, []byte("hook_event_name")) {
			return []string{}, "binary", nil // no hook system yet
		}
	case "gemini":
		root := filepath.Dir(path)
		for dir := root; dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
			if fileExists(filepath.Join(dir, "package.json")) {
				root = dir
				break
			}
		}
		events = GeminiHookEventsIn(sourceOf(root, []string{".js", ".mjs", ".cjs"}, false))
	case "copilot":
		native := npmPlatformBinary(path)
		if native == "" {
			native = path
		}
		js, err := seaScript(native)
		if err != nil {
			return seedEvents(b.Harness), "seed", err
		}
		events = CopilotHookEventsIn(js)
	}
	if len(events) == 0 {
		return seedEvents(b.Harness), "seed", nil
	}
	return events, "binary", nil
}

func seedEvents(harness string) []string { return slices.Clone(hookSeeds[harness]) }

var (
	jsStringArrayRE = regexp.MustCompile(`^\[(?:"[A-Za-z]+",)+"[A-Za-z]+"\]$`)
	jsQuotedRE      = regexp.MustCompile(`"([A-Za-z]+)"`)
)

// ClaudeHookEventsIn are the hook events of the Claude Code build in data: its settings
// schema's hook event enum, a JavaScript array of the events' names, the longest naming
// PreToolUse, SessionStart and Stop.
func ClaudeHookEventsIn(data []byte) []string {
	var best []string
	quoted := func(c byte) bool { return c == '"' || c == ',' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' }
	anchor := []byte(`"PreToolUse"`)
	for i := 0; ; {
		j := bytes.Index(data[i:], anchor)
		if j < 0 {
			break
		}
		at := i + j
		i = at + len(anchor)
		// The array around the anchor: back to its "[", on to its "]", over names alone.
		lo, hi := at, i
		for lo > 0 && at-lo < 4096 && quoted(data[lo-1]) {
			lo--
		}
		for hi < len(data) && hi-at < 4096 && quoted(data[hi]) {
			hi++
		}
		if lo == 0 || hi == len(data) || data[lo-1] != '[' || data[hi] != ']' {
			continue
		}
		m := data[lo-1 : hi+1]
		if !jsStringArrayRE.Match(m) || !bytes.Contains(m, []byte(`"SessionStart"`)) || !bytes.Contains(m, []byte(`"Stop"`)) {
			continue
		}
		var names []string
		for _, q := range jsQuotedRE.FindAllSubmatch(m, -1) {
			names = append(names, string(q[1]))
		}
		if len(names) > len(best) {
			best = names
		}
	}
	return sortedUnique(best)
}

var codexEventConstRE = regexp.MustCompile(`"hook_event_name":\s*\{\s*"const":\s*"([A-Za-z]+)"`)

// CodexHookEventsIn are the hook events of the Codex build in data: Codex embeds the JSON schema
// of each event's input (<event>.command.input), whose hook_event_name is a constant.
func CodexHookEventsIn(data []byte) []string {
	var out []string
	for _, m := range codexEventConstRE.FindAllSubmatch(data, -1) {
		out = append(out, string(m[1]))
	}
	return sortedUnique(out)
}

var geminiEventRE = regexp.MustCompile(`HookEventName\d*\["([A-Za-z]+)"\]\s*=\s*"[A-Za-z]+"`)

// GeminiHookEventsIn are the hook events of the Gemini CLI package whose source is src: its
// HookEventName enum, as TypeScript compiles one.
func GeminiHookEventsIn(src []byte) []string {
	var out []string
	for _, m := range geminiEventRE.FindAllSubmatch(src, -1) {
		out = append(out, string(m[1]))
	}
	return sortedUnique(out)
}

var copilotHookTypeRE = regexp.MustCompile(`"enum":\s*\[([^\]]*)\][^\]\[]{0,800}?"title":\s*"HookType"`)

// CopilotHookEventsIn are the hook events of the Copilot CLI build whose script is js: the
// HookType enum of its embedded RPC schema.
func CopilotHookEventsIn(js []byte) []string {
	var out []string
	for _, m := range copilotHookTypeRE.FindAllSubmatch(js, -1) {
		for _, q := range jsQuotedRE.FindAllSubmatch(m[1], -1) {
			out = append(out, string(q[1]))
		}
	}
	return sortedUnique(out)
}

// seaScript is the script a Node single executable application carries: its blob section holds
// a loader and the application gzipped, the largest stream in it.
func seaScript(path string) ([]byte, error) {
	var blob []byte
	if f, err := macho.Open(path); err == nil {
		defer f.Close()
		if s := f.Section("__NODE_SEA_BLOB"); s != nil {
			blob, _ = s.Data()
		}
	} else if f, err := elf.Open(path); err == nil {
		defer f.Close()
		for _, s := range f.Sections {
			if strings.Contains(s.Name, "NODE_SEA_BLOB") {
				blob, _ = s.Data()
			}
		}
	}
	if blob == nil {
		return nil, errors.New("no single executable application blob")
	}
	for i := 0; i < len(blob); {
		j := bytes.Index(blob[i:], []byte{0x1f, 0x8b, 0x08})
		if j < 0 {
			break
		}
		at := i + j
		i = at + 3
		zr, err := gzip.NewReader(bytes.NewReader(blob[at:]))
		if err != nil {
			continue
		}
		zr.Multistream(false)
		out, err := io.ReadAll(io.LimitReader(zr, 1<<30))
		if err == nil && len(out) > 4<<20 {
			return out, nil
		}
	}
	return nil, errors.New("no application script in the blob")
}
