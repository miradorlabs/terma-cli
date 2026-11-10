package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/e2e/configschema"
)

// What a harness build's executable says of itself, read from its bytes, never by running it:
// its size and digest, the runtime it was built on, the SDKs bundled in it, the environment
// variables it names and, for Claude Code and the Copilot CLI, the options of its settings
// schema and the variables its JavaScript reads (settingsfacts.go). Size and digest are facts;
// everything read out of strings or minified code is a best effort, and Heuristic names those
// fields: a variable named in an error message is listed though nothing reads it, and one
// built from parts at run time is not.

// BinaryFacts are what RecordBinaryFacts records of a build.
type BinaryFacts struct {
	// Form is "executable" (one native or compiled file) or "package" (a script and its
	// package directory: size is the directory's, digest the script's).
	Form string `json:"form"`
	// Runtime is what the build runs on: "bun 1.4.3", "node" (a package; Engines is what its
	// package.json asks), "python 3.12.4", or "rust" (RustcCommit the toolchain's commit).
	Runtime     string `json:"runtime,omitempty"`
	NodeCompat  string `json:"node_compat,omitempty"` // the Node version a Bun build reports
	Engines     string `json:"engines,omitempty"`
	RustcCommit string `json:"rustc_commit,omitempty"`
	// SDK are bundled SDKs and libraries by name: npm packages, Python distributions, Rust crates.
	SDK       map[string]string `json:"sdk,omitempty"`
	SizeBytes int64             `json:"size_bytes"`
	SHA256    string            `json:"sha256"`
	EnvVars   []string          `json:"env_vars,omitempty"`
	// EnvVarsUncertain are names seen only running into the text after them, whose last
	// character may be the next string's (EnvVarsIn).
	EnvVarsUncertain []string `json:"env_vars_uncertain,omitempty"`
	// EnvSource is how EnvVars were read: "reads" (the names the bundle's code reads, a
	// JavaScript bundle's process.env), or empty for names found in its strings.
	EnvSource    string   `json:"env_source,omitempty"`
	SettingsKeys []string `json:"settings_keys,omitempty"`
	// Settings are the options of the settings schema the bundle declares, by dotted path
	// (Claude Code's zod schema, the Copilot CLI's settings registry).
	Settings configschema.Options `json:"settings,omitempty"`
	// Heuristic names the fields read out of strings rather than stated by the build.
	Heuristic []string `json:"heuristic,omitempty"`
}

// agentOf is the harness whose agent harness runs: a desktop app's (claude-desktop runs Claude
// Code, codex-desktop Codex), else harness itself.
func agentOf(harness string) string {
	if agent, ok := strings.CutSuffix(harness, "-desktop"); ok {
		return agent
	}
	return harness
}

// envPrefixes are the environment variable prefixes each harness's own variables carry.
var envPrefixes = map[string][]string{
	"claude":   {"ANTHROPIC_", "CLAUDE_", "DISABLE_", "ENABLE_", "MCP_", "OTEL_", "BASH_"},
	"codex":    {"CODEX_", "OPENAI_", "OTEL_", "AZURE_"},
	"gemini":   {"GEMINI_", "GOOGLE_", "OTEL_"},
	"opencode": {"OPENCODE_", "OTEL_"},
	"pi":       {"PI_"},
	"omp":      {"OMP_", "PI_", "OTEL_"},
	"hermes":   {"HERMES_", "OPENAI_", "OPENROUTER_", "ANTHROPIC_"},
	"dsh":      {"DSH_", "DEEPSEEK_", "OTEL_"},
	"muse":     {"MUSE_", "TBH_", "META_", "OTEL_"},
	"copilot":  {"COPILOT_", "GH_", "GITHUB_", "OTEL_"},
}

// sdkPackages are the npm packages and Python distributions worth naming where installed.
var sdkPackages = []string{"@anthropic-ai/sdk", "@anthropic-ai/claude-agent-sdk", "openai", "@openai/agents", "@google/genai",
	"@modelcontextprotocol/sdk", "ai", "@ai-sdk/provider", "@opentelemetry/api", "@opentelemetry/sdk-node",
	"@github/copilot-sdk", "anthropic", "httpx", "litellm", "mcp", "opentelemetry-api", "pydantic"}

// rustCrates are the crates whose versions a Rust build's paths say.
var rustCrates = []string{"tokio", "reqwest", "rustls", "hyper", "h2", "tonic", "rmcp", "opentelemetry", "opentelemetry_sdk",
	"opentelemetry-otlp", "sqlx-core", "tokio-tungstenite", "eventsource-stream"}

// RecordBinaryFacts records what b's executable says of itself in t's profile of b. A build
// whose facts cannot be read is logged, not failed: facts are evidence, not a contract.
func RecordBinaryFacts(t *testing.T, b Binary) {
	t.Helper()
	facts, err := BinaryFactsOf(b)
	if err != nil {
		t.Logf("binary facts of %s %s: %v", b.Harness, b.Version, err)
		return
	}
	RecordBinary(t, b, facts)
}

// BinaryFactsOf reads b's facts.
func BinaryFactsOf(b Binary) (BinaryFacts, error) {
	path, err := filepath.EvalSymlinks(b.Path)
	if err != nil {
		return BinaryFacts{}, err
	}
	if native := npmPlatformBinary(path); native != "" {
		path = native // the loader only starts it
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return BinaryFacts{}, err
	}
	sum := sha256.Sum256(data)
	facts := BinaryFacts{Form: "executable", SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	corpus := data
	if bytes.HasPrefix(data, []byte("#!")) {
		facts.Form = "package"
		corpus = facts.readPackage(path, data)
	} else {
		facts.readExecutable(data)
	}
	harness := agentOf(b.Harness)
	facts.EnvVars, facts.EnvVarsUncertain = EnvVarsIn(corpus, envPrefixes[harness])
	facts.readSettings(harness, path, corpus)
	if len(facts.EnvVars) > 0 {
		facts.Heuristic = append(facts.Heuristic, "env_vars")
	}
	if harness == "claude" {
		if keys := SettingsKeysIn(corpus, "cleanupPeriodDays"); len(keys) > 0 {
			facts.SettingsKeys = keys
			facts.Heuristic = append(facts.Heuristic, "settings_keys")
		}
	}
	if len(facts.Settings) > 0 {
		facts.Heuristic = append(facts.Heuristic, "settings")
	}
	return facts, nil
}

// readSettings reads what a closed harness's JavaScript declares of its configuration: the
// options of its settings schema, and the environment variables its code reads, in place of
// those its strings name. The Copilot CLI's script is packed in its executable.
func (f *BinaryFacts) readSettings(harness, path string, corpus []byte) {
	var js []byte
	switch harness {
	case "claude":
		js = corpus
		f.Settings = ClaudeSettingsIn(js)
	case "copilot":
		script, err := seaScript(path)
		if err != nil {
			return
		}
		js = script
		f.Settings = CopilotSettingsIn(js)
	default:
		return
	}
	if names := JSEnvReadsIn(js, envPrefixes[harness]); len(names) > 0 {
		f.EnvVars, f.EnvVarsUncertain, f.EnvSource = names, nil, "reads"
	}
}

var (
	bunUARE = regexp.MustCompile(`bun/(\d+\.\d+\.\d+) npm/\S* node/(v\d+\.\d+\.\d+)`)
	bunRE   = regexp.MustCompile(`Bun v(\d+\.\d+\.\d+)`)
	rustcRE = regexp.MustCompile(`/rustc/([0-9a-f]{40})/`)
	// nodeReleaseRE is the URL of a Node build's headers, its version in it.
	nodeReleaseRE = regexp.MustCompile(`nodejs\.org/download/release/(v\d+\.\d+\.\d+)/node-`)
	crateRE       = regexp.MustCompile(`/([a-z0-9_-]+)-(\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)/src/`)
	jsSDKRE       = regexp.MustCompile("getUserAgent\\(\\)\\{return`([A-Za-z]+)/JS \\$\\{([\\w$]+)\\}`")
	semverLit     = `="(\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)"`
)

// jsSDKNames are the npm packages behind the user agents bundled SDK clients send. A bundle's
// platform clients (AnthropicBedrock/JS, …) name the version of the SDK they are bundled with,
// not their own package's, so only the SDKs themselves are named.
var jsSDKNames = map[string]string{"Anthropic": "@anthropic-ai/sdk", "OpenAI": "openai"}

// readExecutable reads a compiled build's runtime and bundled SDKs.
func (f *BinaryFacts) readExecutable(data []byte) {
	if m := bunUARE.FindSubmatch(data); m != nil {
		f.Runtime, f.NodeCompat = "bun "+string(m[1]), string(m[2])
	} else if m := bunRE.FindSubmatch(data); m != nil {
		f.Runtime = "bun " + string(m[1])
	} else if commit := mostCommon(rustcRE, data, 1); commit != "" {
		f.Runtime, f.RustcCommit = "rust", commit
	} else if m := nodeReleaseRE.FindSubmatch(data); m != nil {
		// Node itself, its headers' download URL naming its version: a single executable
		// application (Copilot CLI's) carries its JavaScript packed, so strings see little of it.
		f.Runtime = "node " + string(m[1])
		if bytes.Contains(data, []byte("NODE_SEA_FUSE_")) {
			f.Runtime += " (single executable application)"
		}
	}
	if f.Runtime != "" {
		f.Heuristic = append(f.Heuristic, "runtime")
	}
	sdk := JSSDKsIn(data)
	if f.Runtime == "rust" {
		sdk = RustCratesIn(data)
	}
	if len(sdk) > 0 {
		f.SDK = sdk
		f.Heuristic = append(f.Heuristic, "sdk")
	}
}

// JSSDKsIn are the Stainless-generated SDK clients bundled in data, by package, each at the
// version its user agent names.
func JSSDKsIn(data []byte) map[string]string {
	out := map[string]string{}
	for _, m := range jsSDKRE.FindAllSubmatch(data, -1) {
		pkg := jsSDKNames[string(m[1])]
		if _, done := out[pkg]; done || pkg == "" {
			continue
		}
		re := regexp.MustCompile(`[^\w$]` + regexp.QuoteMeta(string(m[2])) + semverLit)
		if v := re.FindSubmatch(data); v != nil {
			out[pkg] = string(v[1])
		}
	}
	return out
}

// RustCratesIn are the versions of rustCrates in the source paths a Rust build keeps for its
// panics, the most named where several are linked.
func RustCratesIn(data []byte) map[string]string {
	counts := map[[2]string]int{}
	for _, m := range crateRE.FindAllSubmatch(data, -1) {
		counts[[2]string{string(m[1]), string(m[2])}]++
	}
	out := map[string]string{}
	best := map[string]int{}
	for k, n := range counts {
		if slices.Contains(rustCrates, k[0]) && n > best[k[0]] {
			out[k[0]], best[k[0]] = k[1], n
		}
	}
	return out
}

// mostCommon is the group of re matched most often in data.
func mostCommon(re *regexp.Regexp, data []byte, group int) string {
	counts := map[string]int{}
	for _, m := range re.FindAllSubmatch(data, -1) {
		counts[string(m[group])]++
	}
	best, n := "", 0
	for k, c := range counts {
		if c > n || (c == n && k < best) {
			best, n = k, c
		}
	}
	return best
}

// readPackage reads a script build's package: Node's from its package.json and installed
// dependencies, Python's from its virtualenv. It returns the package's source, for strings.
func (f *BinaryFacts) readPackage(script string, head []byte) []byte {
	// A shell launcher that only hands on to another script (`exec <script> "$@"`) is read as
	// that script: Hermes's ~/.local/bin/hermes starts the launcher in its checkout.
	for range 3 {
		shebang, _, _ := bytes.Cut(head, []byte("\n"))
		next := shellForwardRE.FindSubmatch(head)
		if next == nil || !bytes.Contains(shebang, []byte("sh")) || string(next[1]) == script {
			break
		}
		data, err := os.ReadFile(string(next[1]))
		if err != nil || !bytes.HasPrefix(data, []byte("#!")) {
			break
		}
		script, head = string(next[1]), data
	}
	shebang, _, _ := bytes.Cut(head, []byte("\n"))
	if bytes.Contains(shebang, []byte("python")) {
		return f.readPythonPackage(string(bytes.Fields(shebang[2:])[0]), filepath.Dir(script))
	}
	// A shell launcher that runs a virtualenv's Python on the app's source (Hermes's).
	if m := pythonLauncherRE.FindSubmatch(head); m != nil {
		return f.readPythonPackage(string(m[1]), filepath.Dir(string(m[2])))
	}
	// One that runs a Python with a bootstrap of its own that puts the app on sys.path (Hermes's
	// since 0.22: `exec <python> -I -c '…sys.path.insert(0, <root>)…'`).
	if m := pythonBootstrapRE.FindSubmatch(head); m != nil {
		if root := pythonBootstrapRootRE.FindSubmatch(head); root != nil {
			return f.readPythonPackage(string(m[1]), string(root[1]))
		}
	}
	f.Runtime = "node"
	root := filepath.Dir(script)
	for dir := root; dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if fileExists(filepath.Join(dir, "package.json")) {
			root = dir
			break
		}
	}
	var pkg struct {
		Engines map[string]string `json:"engines"`
	}
	if data, err := os.ReadFile(filepath.Join(root, "package.json")); err == nil {
		_ = json.Unmarshal(data, &pkg)
		f.Engines = pkg.Engines["node"]
	}
	f.SDK = map[string]string{}
	for _, name := range sdkPackages {
		if v := installedVersion(root, name); v != "" {
			f.SDK[name] = v
		}
	}
	maps.Copy(f.SDK, JSSDKsIn(sourceOf(root, []string{".js", ".mjs", ".cjs"}, true)))
	if len(f.SDK) == 0 {
		f.SDK = nil
	}
	f.SizeBytes = dirSize(root)
	return sourceOf(root, []string{".js", ".mjs", ".cjs"}, false)
}

// pythonLauncherRE finds a launcher's `exec "<venv>/bin/python" "<app>/<script>"`.
var pythonLauncherRE = regexp.MustCompile(`exec\s+"([^"]*/bin/python[0-9.]*)"\s+"([^"]+)"`)

var (
	// pythonBootstrapRE finds a launcher's `exec <python> -I -c`, the interpreter's path as
	// shlex.join writes it (quoted only where it must be).
	pythonBootstrapRE = regexp.MustCompile(`(?m)^exec\s+'?(/[^'\s]*/python[0-9.]*)'?\s+-I\s+-c\s`)
	// pythonBootstrapRootRE finds the folder its bootstrap puts first on sys.path, its repr's
	// quotes inside the shell's ('"'"'…'"'"').
	pythonBootstrapRootRE = regexp.MustCompile(`sys\.path\.insert\(0,\s*(?:'"'"'|'|")(/[^'"]+)`)
	// shellForwardRE finds a shell launcher's `exec <script> "$@"`.
	shellForwardRE = regexp.MustCompile(`(?m)^exec\s+'?(/[^'\s]+)'?\s+"\$@"\s*$`)
)

// readPythonPackage reads a Python build: its virtualenv's version and distributions, and the
// source of the app (in app) and of what it installed.
func (f *BinaryFacts) readPythonPackage(python, app string) []byte {
	venv := filepath.Dir(filepath.Dir(python))
	if cfg, err := os.ReadFile(filepath.Join(venv, "pyvenv.cfg")); err == nil {
		for line := range strings.Lines(string(cfg)) {
			k, v, _ := strings.Cut(line, "=")
			if k = strings.TrimSpace(k); k == "version" || k == "version_info" {
				f.Runtime = "python " + strings.TrimSpace(v)
				break
			}
		}
	}
	sites, _ := filepath.Glob(filepath.Join(venv, "lib", "python*", "site-packages"))
	if len(sites) == 0 {
		return nil
	}
	site := sites[0]
	if f.Runtime == "" {
		// An interpreter of its own, no virtualenv (Hermes's store Python): its library's version.
		f.Runtime = "python " + strings.TrimPrefix(filepath.Base(filepath.Dir(site)), "python")
	}
	f.SDK = map[string]string{}
	infos, _ := filepath.Glob(filepath.Join(site, "*.dist-info"))
	for _, info := range infos {
		name, version, ok := strings.Cut(strings.TrimSuffix(filepath.Base(info), ".dist-info"), "-")
		if ok && slices.Contains(sdkPackages, strings.ToLower(strings.ReplaceAll(name, "_", "-"))) {
			f.SDK[strings.ToLower(strings.ReplaceAll(name, "_", "-"))] = version
		}
	}
	if len(f.SDK) == 0 {
		f.SDK = nil
	}
	f.SizeBytes = dirSize(venv)
	src := sourceOf(site, []string{".py"}, false)
	if !strings.HasPrefix(app, venv+string(filepath.Separator)) {
		f.SizeBytes = dirSize(app)
		src = sourceOfExcept(app, []string{".py"}, venv)
	}
	return src
}

// sourceOfExcept is sourceOf root, the folder skip left out.
func sourceOfExcept(root string, exts []string, skip string) []byte {
	var buf bytes.Buffer
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || buf.Len() > maxSource || (d.IsDir() && (path == skip || d.Name() == "node_modules")) {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() && slices.Contains(exts, filepath.Ext(path)) {
			if data, err := os.ReadFile(path); err == nil {
				buf.Write(data)
				buf.WriteByte('\n')
			}
		}
		return nil
	})
	return buf.Bytes()
}

// npmPlatformBinary is the native executable an npm package's loader script starts, where the
// package ships one per platform beside it (@github/copilot's @github/copilot-darwin-arm64);
// "" where it does not.
func npmPlatformBinary(script string) string {
	dir := filepath.Dir(script)
	for ; dir != filepath.Dir(dir) && !fileExists(filepath.Join(dir, "package.json")); dir = filepath.Dir(dir) {
	}
	arch := map[string]string{"amd64": "x64"}[runtime.GOARCH]
	if arch == "" {
		arch = runtime.GOARCH
	}
	name := filepath.Base(dir)
	platform := filepath.Join(filepath.Dir(dir), name+"-"+runtime.GOOS+"-"+arch)
	for _, bin := range []string{name, filepath.Join("bin", name)} {
		if info, err := os.Stat(filepath.Join(platform, bin)); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return filepath.Join(platform, bin)
		}
	}
	return ""
}

// installedVersion is the version of npm package name as root resolves it.
func installedVersion(root, name string) string {
	for dir := root; dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		data, err := os.ReadFile(filepath.Join(dir, "node_modules", name, "package.json"))
		if err != nil {
			continue
		}
		var pkg struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(data, &pkg) == nil && pkg.Version != "" {
			return pkg.Version
		}
	}
	return ""
}

// maxSource bounds the source read of a package.
const maxSource = 512 << 20

// sourceOf is the files under root with one of exts, one after another; withDeps reads its
// node_modules too.
func sourceOf(root string, exts []string, withDeps bool) []byte {
	var buf bytes.Buffer
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || buf.Len() > maxSource {
			return filepath.SkipDir
		}
		if d.IsDir() && !withDeps && d.Name() == "node_modules" && path != root {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() && slices.Contains(exts, filepath.Ext(path)) {
			if data, err := os.ReadFile(path); err == nil {
				buf.Write(data)
				buf.WriteByte('\n')
			}
		}
		return nil
	})
	return buf.Bytes()
}

func dirSize(root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// EnvVarsIn are the environment variable names in data that start with one of prefixes,
// sorted, and those it cannot be sure of. A name is a run of capitals, digits and underscores; a
// compiled Rust build packs its string literals end to end ("CODEX_API_KEYCODEX_HOME"), so a
// run is also split where a prefix of five or more characters, or a well-known variable
// (envSplitMarkers), starts after anything but an underscore. A name ending in an underscore is
// a prefix code builds names from, not a name, and is dropped; so is one that is another name
// and more not set off by an underscore ("…_CAPABILITIES3", the byte after it in a bundle's
// string table). A name seen only running into lowercase text may have the first capital of
// the next literal on its end ("CODEX_SESSION_IDLoad"): it is uncertain, unless the name
// without its last character is seen.
func EnvVarsIn(data []byte, prefixes []string) (names, uncertain []string) {
	if len(prefixes) == 0 {
		return nil, nil
	}
	isName := func(c byte) bool { return c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' }
	markers := append(slices.Clone(envSplitMarkers), prefixes...)
	sure, unsure := map[string]bool{}, map[string]bool{}
	// A name a bundle assigns (`var GEMINI_3_SET = {`, a TypeScript enum's
	// `Key["GEMINI_CLI_AGENT_ID"] = 112`) is a constant, unless it is also read from an env.
	declared, accessed := map[string]bool{}, map[string]bool{}
	for _, prefix := range prefixes {
		p := []byte(prefix)
		for i := 0; ; {
			j := bytes.Index(data[i:], p)
			if j < 0 {
				break
			}
			start := i + j
			i = start + len(p)
			if start > 0 && isName(data[start-1]) && (len(prefix) < 5 || data[start-1] == '_') {
				continue // inside another name
			}
			end := i
			for end < len(data) && isName(data[end]) {
				end++
			}
			name := string(data[start:end])
			cut := false
			for _, q := range markers {
				if len(q) < 5 && !slices.Contains(envSplitMarkers, q) {
					continue
				}
				for k := 1; k < len(name); {
					at := strings.Index(name[k:], q)
					if at < 0 {
						break
					}
					if name[k+at-1] != '_' {
						name, cut = name[:k+at], true
						break
					}
					k += at + 1
				}
			}
			if !plausibleEnvVar(name, prefix) || bashVariables[name] {
				continue
			}
			if !cut {
				switch {
				case envAccessed(data, start):
					accessed[name] = true
				case assigned(data, end) && (start == 0 || data[start-1] != '.'):
					// `obj.NAME = …` sets a property, an env's as often as not.
					declared[name] = true
				}
			}
			if !cut && end < len(data) && data[end] >= 'a' && data[end] <= 'z' {
				unsure[name] = true
			} else {
				sure[name] = true
			}
		}
	}
	// extends reports whether name is a name in set and more not set off by an underscore.
	extends := func(name string, set ...map[string]bool) bool {
		for cut := len(name) - 1; cut >= 4; cut-- {
			for _, s := range set {
				if name[cut] != '_' && s[name[:cut]] {
					return true
				}
			}
		}
		return false
	}
	for name := range declared {
		if !accessed[name] {
			delete(sure, name)
			delete(unsure, name)
		}
	}
	for name := range sure {
		if extends(name, sure) {
			delete(sure, name)
		}
	}
	for name := range unsure {
		if !sure[name] && !extends(name, sure, unsure) {
			uncertain = append(uncertain, name)
		}
	}
	sort.Strings(uncertain)
	return slices.Sorted(maps.Keys(sure)), uncertain
}

// envAccessed reports whether the name at start is read as an environment's: `env.NAME`,
// `env["NAME"]`, `environ.get("NAME"`, `getenv("NAME"`.
func envAccessed(data []byte, start int) bool {
	before := bytes.TrimRight(data[max(0, start-16):start], `"'`)
	for _, s := range []string{"env.", "env[", "environ[", "environ.get(", "getenv(", "getEnv(", "env.get("} {
		if bytes.HasSuffix(before, []byte(s)) {
			return true
		}
	}
	return false
}

// assigned reports whether the name ending at end is assigned: followed, past a closing quote
// and bracket, by `=` that is not `==` or `=>`.
func assigned(data []byte, end int) bool {
	i := end
	for i < len(data) && i < end+3 && (data[i] == '"' || data[i] == '\'' || data[i] == ']') {
		i++
	}
	for i < len(data) && data[i] == ' ' {
		i++
	}
	return i+1 < len(data) && data[i] == '=' && data[i+1] != '=' && data[i+1] != '>'
}

// envSplitMarkers are the starts of well-known variables a Rust build packs after its own
// ("CODEX_CA_CERTIFICATESSL_CERT_FILE"): a name is cut where one begins after anything but an
// underscore.
var envSplitMarkers = []string{"SSL_CERT_", "OLDPWD", "PWD", "GOOGLE_", "NODE_", "EXEC_", "HTTPS_", "HTTP_", "XDG_",
	"RUST_", "GIT_", "TMPDIR", "AWS_"}

// bashVariables are Bash's own, which a harness that runs Bash names for it.
var bashVariables = map[string]bool{"BASH_ALIASES": true, "BASH_ARGC": true, "BASH_ARGV": true, "BASH_ARGV0": true,
	"BASH_CMDS": true, "BASH_COMMAND": true, "BASH_COMPAT": true, "BASH_ENV": true, "BASH_EXECUTION_STRING": true,
	"BASH_LINENO": true, "BASH_LOADABLES_PATH": true, "BASH_MONOSECONDS": true, "BASH_REMATCH": true, "BASH_SOURCE": true,
	"BASH_SOURCE_PATH": true, "BASH_SUBSHELL": true, "BASH_TRAPSIG": true, "BASH_VERSINFO": true, "BASH_VERSION": true,
	"BASH_XTRACEFD": true}

func plausibleEnvVar(name, prefix string) bool {
	rest := strings.TrimPrefix(name, prefix)
	return len(rest) >= 2 && len(name) <= 64 && !strings.HasSuffix(name, "_") && !strings.Contains(name, "__") &&
		strings.ContainsAny(rest, "ABCDEFGHIJKLMNOPQRSTUVWXYZ")
}

// SettingsKeysIn are the top-level keys of a zod object schema bundled in data that has anchor
// among them: Claude Code declares its settings as `key:()=>schema.describe(…)` thunks, one
// after another, so the keys are the run of such thunks around the anchor, no two further
// apart than a description's length.
func SettingsKeysIn(data []byte, anchor string) []string {
	at := bytes.Index(data, []byte(anchor+":()=>"))
	if at < 0 {
		return nil
	}
	const window, maxGap = 400_000, 8_000
	lo, hi := max(0, at-window), min(len(data), at+window)
	re := regexp.MustCompile(`[{,]([A-Za-z_$][\w$]*):\(\)=>`)
	locs := re.FindAllSubmatchIndex(data[lo:hi], -1)
	idx := -1
	for i, l := range locs {
		if lo+l[2] == at {
			idx = i
		}
	}
	if idx < 0 {
		return nil
	}
	first, last := idx, idx
	for first > 0 && locs[first][0]-locs[first-1][1] <= maxGap {
		first--
	}
	for last < len(locs)-1 && locs[last+1][0]-locs[last][1] <= maxGap {
		last++
	}
	seen := map[string]bool{}
	for _, l := range locs[first : last+1] {
		seen[string(data[lo+l[2]:lo+l[3]])] = true
	}
	return slices.Sorted(maps.Keys(seen))
}
