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
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The hooks probe: every hook event a harness build knows, read from the build itself, and
// what it hands a hook when one fires. The probe registers a recording hook for every event
// the build knows (the test binary itself, started as one: RunHookRecorder), in place of
// terma's, and runs what makes each of them fire. The recorder saves each run's event, stdin,
// arguments, working directory, environment and timing; the probe records, per event, whether
// it fired, its payload's key paths and their kinds, one payload whole with its values, the
// environment variables the harness set for it and their values, the directory it ran in and
// whether it takes a matcher. What it publishes of values is scrubbed (hookScrubber): no
// secret, PATH, path or name of the machine's. A second pass has the recorder answer the main
// control points (exit 2 with a message, a JSON decision, context to add) and records what the
// build did with each.
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
//
// Started with hookMCPArg instead, it is the probe's MCP server (serveElicitingMCP).
func RunHookRecorder() {
	if len(os.Args) >= 2 && os.Args[1] == hookMCPArg {
		os.Exit(serveElicitingMCP(os.Stdin, os.Stdout))
	}
	if len(os.Args) < 5 || os.Args[1] != hookRecorderArg {
		return
	}
	os.Exit(recordHook(os.Args[2], os.Args[3], os.Args[4], os.Args[5:], os.Stdin, os.Stdout, os.Stderr))
}

// hookMCPArg is the first argument that starts the test binary as the hooks probe's MCP server.
const hookMCPArg = "terma-hook-mcp"

// hookMCPTool is the probe's MCP server's one tool, which asks the user a question.
const hookMCPTool = "ask"

// hookMCPElicitation is the id of the question the probe's MCP server asks.
const hookMCPElicitation = "terma-elicit"

// serveElicitingMCP is an MCP server on stdio (newline-delimited JSON-RPC) whose one tool asks
// the user a question before it answers: an elicitation/create request to the client, the tool's
// result saying how it was answered. It returns the status to exit with when its input closes.
func serveElicitingMCP(in io.Reader, out io.Writer) int {
	type message struct {
		ID     json.RawMessage `json:"id,omitempty"`
		Method string          `json:"method,omitempty"`
		Params json.RawMessage `json:"params,omitempty"`
		Result json.RawMessage `json:"result,omitempty"`
		Error  json.RawMessage `json:"error,omitempty"`
	}
	enc := json.NewEncoder(out)
	send := func(v map[string]any) {
		v["jsonrpc"] = "2.0"
		_ = enc.Encode(v)
	}
	// The tool call waiting for the answer to its question, by its request id.
	var calling json.RawMessage
	asked := 0
	dec := json.NewDecoder(in)
	for {
		var m message
		if err := dec.Decode(&m); err != nil {
			return 0
		}
		switch {
		case m.Method == "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(m.Params, &p)
			if p.ProtocolVersion == "" {
				p.ProtocolVersion = "2025-06-18"
			}
			send(map[string]any{"id": m.ID, "result": map[string]any{"protocolVersion": p.ProtocolVersion,
				"capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "terma-hooks-probe", "version": "1.0.0"}}})
		case m.Method == "tools/list":
			send(map[string]any{"id": m.ID, "result": map[string]any{"tools": []any{map[string]any{"name": hookMCPTool,
				"description": "Asks the user one question and returns the answer.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}}}}})
		case m.Method == "tools/call":
			calling = m.ID
			asked++
			send(map[string]any{"id": fmt.Sprintf("%s-%d", hookMCPElicitation, asked), "method": "elicitation/create", "params": map[string]any{
				"message": "TERMA hooks probe: which answer?",
				"requestedSchema": map[string]any{"type": "object", "properties": map[string]any{
					"answer": map[string]any{"type": "string", "title": "Answer", "description": "Any answer"}}, "required": []string{"answer"}}}})
		case m.Method == "" && strings.Contains(string(m.ID), hookMCPElicitation) && calling != nil:
			// The client's answer to the question: the tool's result says what it was.
			var r struct {
				Action string `json:"action"`
			}
			_ = json.Unmarshal(m.Result, &r)
			if r.Action == "" {
				r.Action = "error"
			}
			send(map[string]any{"id": calling, "result": map[string]any{"content": []any{map[string]any{"type": "text",
				"text": "TERMA_ELICITATION_" + strings.ToUpper(r.Action)}}}})
			calling = nil
		case m.Method == "ping":
			send(map[string]any{"id": m.ID, "result": map[string]any{}})
		case m.Method != "" && len(m.ID) > 0:
			send(map[string]any{"id": m.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
		}
	}
}

// HookMCPCommand is the command line of the hooks probe's MCP server, as an MCP configuration's
// command and args: the test binary, started as the server.
func HookMCPCommand() (string, []string, error) {
	exe, err := os.Executable()
	return exe, []string{hookMCPArg}, err
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
	// Method is how the probe made events fire (HooksProbeMethod): builds probed differently
	// fire different tools and terminals, so their payloads are not compared.
	Method int `json:"method,omitempty"`
	// Trust is, for a build that runs a hook only once the developer trusts it (Codex 0.129
	// on), whether the probe's hooks were: "trusted" (recorded as Codex records a developer's
	// review, at the hash Codex gave each) or "untrusted" (they could not be). Absent for a
	// build without hook trust.
	Trust string `json:"trust,omitempty"`
}

// HooksProbeMethod is the probe's current way of making events fire: 2 adds the task, MCP
// and worktree runs and the interactive session, and publishes example payloads.
const HooksProbeMethod = 2

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
	// Example is one of the event's payloads whole, with its values: the one with the most key
	// paths, the first of those. A secret's value is <redacted>, a path of the machine's outside
	// the sandbox <machine-path>, and a text longer than hookTextLimit is cut; normalization
	// then makes the sandbox's paths, ids and times placeholders. Text where the hook's standard
	// input was not JSON, and absent where it was empty.
	Example json.RawMessage `json:"example,omitempty"`
	// EnvValues are the values of Env as the event's first run had them, scrubbed as Example's
	// are: a variable whose name says it holds a secret <redacted>, a list of folders (PATH)
	// <path>.
	EnvValues map[string]string `json:"env_values,omitempty"`
}

// hookPayloadDepth is how deep a payload's key paths go.
const hookPayloadDepth = 4

// hookShellVariables are what the shell a harness runs a hook in sets of itself: not the
// harness's.
var hookShellVariables = map[string]bool{"PWD": true, "OLDPWD": true, "SHLVL": true, "_": true}

// HooksProbeOf shapes calls: started is the environment the harness was started with, and root
// the sandbox folder a path value must be under to be kept.
func HooksProbeOf(supported []string, source string, calls []HookCall, started []string, root string) HooksProbe {
	return hooksProbeOf(supported, source, calls, started, newHookScrubber(root, started, calls, machineOf()))
}

// hooksProbeOf is HooksProbeOf with the scrubber its examples and values go through.
func hooksProbeOf(supported []string, source string, calls []HookCall, started []string, scrub *hookScrubber) HooksProbe {
	root := scrub.root
	base := map[string]string{}
	for _, kv := range started {
		k, v, _ := strings.Cut(kv, "=")
		base[k] = v
	}
	p := HooksProbe{Supported: sortedUnique(slices.Clone(supported)), Source: source, Events: map[string]*HookEventShape{}, Method: HooksProbeMethod}
	if p.Supported == nil {
		p.Supported = []string{}
	}
	ignored := map[string]bool{}
	type acc struct {
		keys   map[string]map[string]bool
		env    map[string]bool
		paths  map[string]string
		values map[string]string
		stdin  map[string]bool
		// example is the payload with the most key paths so far, and how many it has.
		example json.RawMessage
		width   int
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
			a = &acc{keys: map[string]map[string]bool{}, env: map[string]bool{}, paths: map[string]string{}, values: map[string]string{},
				stdin: map[string]bool{}, width: -1}
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
			own := map[string]map[string]bool{}
			payloadKinds(payload, "", 0, own)
			if len(own) > a.width {
				a.example, a.width = marshalExample(scrub.value("", payload)), len(own)
			}
		default:
			a.stdin["text"] = true
			if a.width < 0 {
				a.example, a.width = marshalExample(scrub.text(c.Stdin)), 0
			}
		}
		for k, v := range c.Env {
			if hookShellVariables[k] || strings.HasPrefix(k, "__CF") {
				continue
			}
			if was, ok := base[k]; ok && was == v {
				continue
			}
			a.env[k] = true
			if _, seen := a.values[k]; !seen {
				a.values[k] = scrub.env(k, v)
			}
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
		s.Example = a.example
		if len(a.values) > 0 {
			s.EnvValues = a.values
		}
	}
	return p
}

// marshalExample is v as JSON, its <, > and & as they are: placeholders read as written.
func marshalExample(v any) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return nil
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n"))
}

// hookTextLimit is how long a text in a published example or variable may be: one longer is
// cut there, with a marker saying how much was cut.
const hookTextLimit = 2000

// machine is what names the machine a probe runs on, which nothing published may hold: the
// home folder the suite runs from, its user's name and the machine's name.
type machine struct {
	home, user, host string
}

// machineOf is the machine this process runs on.
func machineOf() machine {
	var m machine
	m.home, _ = os.UserHomeDir()
	if u, err := user.Current(); err == nil {
		m.user = u.Username
	}
	if m.user == "" {
		m.user = os.Getenv("USER")
	}
	m.host, _ = os.Hostname()
	return m
}

// hookScrubber makes what a hook was handed fit to publish: a secret's value <redacted>, the
// PATH <path>, a path of the machine's outside the sandbox <machine-path>, the machine's and
// its user's names placeholders, and a long text cut. The sandbox's own paths stay, for
// normalization to make placeholders of.
type hookScrubber struct {
	root    string
	machine machine
	// path is the PATH the harness was started with; secrets are the values of every variable,
	// the harness's or a hook's, whose name says it holds a secret.
	path    string
	secrets []string
}

// newHookScrubber is the scrubber of a probe whose sandbox is root, whose harness was started
// with started and whose hooks were handed calls, on m.
func newHookScrubber(root string, started []string, calls []HookCall, m machine) *hookScrubber {
	s := &hookScrubber{root: root, machine: m}
	seen := map[string]bool{}
	secret := func(k, v string) {
		if secretName(k) && len(v) >= 6 && !seen[v] {
			seen[v] = true
			s.secrets = append(s.secrets, v)
		}
	}
	for _, kv := range started {
		k, v, _ := strings.Cut(kv, "=")
		if k == "PATH" {
			s.path = v
		}
		secret(k, v)
	}
	for _, c := range calls {
		for k, v := range c.Env {
			secret(k, v)
		}
	}
	// Longest first, so a secret that holds another is replaced whole.
	sort.Slice(s.secrets, func(i, j int) bool { return len(s.secrets[i]) > len(s.secrets[j]) })
	return s
}

var (
	// secretWords are the words of a name that say its value is a secret.
	secretWords = map[string]bool{"token": true, "secret": true, "secrets": true, "password": true, "passwd": true, "key": true,
		"apikey": true, "auth": true, "authorization": true, "credential": true, "credentials": true, "cookie": true, "cookies": true,
		"signature": true, "bearer": true}
	// secretValueRE are values that are credentials whatever they are called.
	secretValueRE = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}|(?i:\bbearer\s+)[A-Za-z0-9._~+/=-]{8,}|\bgh[pousr]_[A-Za-z0-9]{20,}|\bgithub_pat_[A-Za-z0-9_]{20,}|\bxox[abposr]-[A-Za-z0-9-]{10,}|\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]+|\bAKIA[0-9A-Z]{16}\b`)
	// absPathRE is an absolute path in a text, up to the first character a path rarely holds.
	absPathRE = regexp.MustCompile(`(?:^|[^\w.~-])(/[^\s"'<>:;,|()\[\]{}` + "`" + `]+)`)
)

// secretName reports whether a variable or a payload's key named name holds a secret by its
// name: one of its words is token, secret, key, password and the like.
func secretName(name string) bool {
	return slices.ContainsFunc(nameWords(name), func(w string) bool { return secretWords[w] })
}

// nameWords are the words of a name, lower case: snake, kebab, dotted and camel case split.
func nameWords(name string) []string {
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(name)
	for i, r := range rs {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
			continue
		case unicode.IsUpper(r) && len(cur) > 0:
			// A capital starts a word after a lower-case letter (apiKey), or ends a run of
			// capitals before a lower-case letter (HTTPServer).
			prev := cur[len(cur)-1]
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || i+1 < len(rs) && unicode.IsLower(rs[i+1]) && unicode.IsUpper(prev) {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return words
}

// machineDirs are the machine's own folders: its users' homes, and where package managers
// install programs. A path under one is no evidence of the build's, and says what the machine
// has and who uses it.
var machineDirs = []string{"/Users/", "/home/", "/root/", "/Volumes/", "/nix/", "/snap/", "/opt/homebrew/", "/usr/local/Cellar/",
	"/usr/local/lib/node_modules/", "/usr/local/opt/", "/usr/local/bin/", "/home/linuxbrew/", "/opt/local/"}

// value is v, the value of key, scrubbed.
func (s *hookScrubber) value(key string, v any) any {
	switch x := v.(type) {
	case string:
		if key != "" && secretName(key) && x != "" {
			return "<redacted>"
		}
		return s.text(x)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = s.value(key, item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = s.value(k, item)
		}
		return out
	}
	return v
}

// env is the value v of the variable named k, scrubbed.
func (s *hookScrubber) env(k, v string) string {
	switch {
	case v == "":
		return v
	case secretName(k):
		return "<redacted>"
	case k == "PATH" || pathList(v):
		return "<path>"
	case RunPID(k, v):
		return "<pid>"
	}
	return s.text(v)
}

// RunPID reports whether the variable named k holds a process id the run picked
// (CLAUDE_PID=82807): its value changes every run, and says nothing of the build.
func RunPID(k, v string) bool {
	return (k == "PID" || strings.HasSuffix(k, "_PID")) && v != "" && strings.Trim(v, "0123456789") == ""
}

// pathList reports whether v is a list of folders: two or more absolute paths joined with the
// list separator.
func pathList(v string) bool {
	parts := strings.Split(v, string(filepath.ListSeparator))
	if len(parts) < 2 {
		return false
	}
	for _, p := range parts {
		if !strings.HasPrefix(p, "/") {
			return false
		}
	}
	return true
}

// text is t scrubbed: secrets, the PATH, the machine's paths and names, then cut.
func (s *hookScrubber) text(t string) string {
	for _, sec := range s.secrets {
		t = strings.ReplaceAll(t, sec, "<redacted>")
	}
	t = secretValueRE.ReplaceAllString(t, "<redacted>")
	if len(s.path) > 1 {
		t = strings.ReplaceAll(t, s.path, "<path>")
	}
	t = absPathRE.ReplaceAllStringFunc(t, func(m string) string {
		at := strings.IndexByte(m, '/')
		p := m[at:]
		if s.root != "" && (p == s.root || strings.HasPrefix(p, s.root+"/")) {
			return m
		}
		// macOS spells a temporary folder both ways; normalization makes either a placeholder.
		if strings.HasPrefix(p, "/private/var/") || strings.HasPrefix(p, "/var/") || strings.HasPrefix(p, "/tmp/") || strings.HasPrefix(p, "/private/tmp/") {
			return m
		}
		mine := s.machine.home != "" && len(s.machine.home) > 1 && (p == s.machine.home || strings.HasPrefix(p, s.machine.home+"/"))
		for _, d := range machineDirs {
			mine = mine || strings.HasPrefix(p, d)
		}
		if !mine {
			return m
		}
		return m[:at] + "<machine-path>"
	})
	for _, name := range []struct{ lit, with string }{{s.machine.host, "<host>"}, {s.machine.user, "<user>"}} {
		// A short name (a user "dev") would match words that are not it.
		if len(name.lit) >= 4 {
			t = strings.ReplaceAll(t, name.lit, name.with)
		}
	}
	if n := utf8.RuneCountInString(t); n > hookTextLimit {
		r := []rune(t)
		t = string(r[:hookTextLimit]) + fmt.Sprintf("…<cut: %d more characters>", n-hookTextLimit)
	}
	return t
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
