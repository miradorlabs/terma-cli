package sourcelink

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Reading a build's files whole, and the environment variables its code reads. A
// configuration schema is the build's wherever its tree keeps it (Gemini CLI generates its
// settings schema into schemas/, DeepSeek Harness its catalog into docs/), so Files reads any
// file the caller asks for; EnvReads reads only the code that ships, as Find does.

// maxWhole bounds a file Files reads: a schema, not a bundle.
const maxWhole = 16 << 20

// Files are the files of a build of harness whose path from the top of its tree want takes,
// each whole, by that path.
func (l *Library) Files(harness, version string, want func(path string) bool) (map[string][]byte, error) {
	s, ok := Sources[harness]
	if !ok {
		return nil, fmt.Errorf("%s: %w", harness, ErrNotPublic)
	}
	l.mu.Lock()
	t, err := l.tree(s, version)
	l.mu.Unlock()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(t.file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		_, file, _ := strings.Cut(h.Name, "/")
		if h.Typeflag != tar.TypeReg || h.Size > maxWhole || !want(file) {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		out[file] = data
	}
}

// The ways code reads an environment variable by its name, each the name its first group.
var (
	// Rust: std::env::var("X"), env::var_os("X"), env!("X"), option_env!("X"), and clap's
	// #[arg(env = "X")].
	rustEnvReads = []*regexp.Regexp{
		regexp.MustCompile(`\benv::var(?:_os)?\(\s*"([^"]+)"`),
		regexp.MustCompile(`\b(?:option_)?env!\(\s*"([^"]+)"`),
		regexp.MustCompile(`#\[(?:arg|clap)\(.*\benv\s*=\s*"([^"]+)"`),
	}
	rustEnvConst = regexp.MustCompile(`\benv::var(?:_os)?\(\s*&?([A-Z][A-Z0-9_]*)\s*\)`)
	// TypeScript and JavaScript: process.env.X, process.env["X"], and Bun's, Deno's and Vite's.
	tsEnvReads = []*regexp.Regexp{
		regexp.MustCompile(`\b(?:process|Bun|Deno)\.env\.([A-Za-z_]\w*)`),
		regexp.MustCompile(`\bimport\.meta\.env\.([A-Za-z_]\w*)`),
		regexp.MustCompile(`\b(?:process|Bun)\.env\[\s*"([^"]+)"\s*\]`),
		regexp.MustCompile(`\bDeno\.env\.get\(\s*"([^"]+)"`),
	}
	tsEnvConst = regexp.MustCompile(`\b(?:process|Bun)\.env\[\s*([A-Z][A-Z0-9_]*)\s*\]`)
	// Python: os.environ.get("X"), os.environ["X"], os.getenv("X"), os.environ.setdefault("X",
	// "X" in os.environ.
	pyEnvReads = []*regexp.Regexp{
		regexp.MustCompile(`\benviron\.(?:get|setdefault|pop)\(\s*"([^"]+)"`),
		regexp.MustCompile(`\benviron\[\s*"([^"]+)"\s*\]`),
		regexp.MustCompile(`\bgetenv\(\s*"([^"]+)"`),
		regexp.MustCompile(`"([^"]+)"\s+(?:not\s+)?in\s+(?:os\.)?environ\b`),
	}
	pyEnvConst = regexp.MustCompile(`\b(?:environ\.get|getenv|environ\.setdefault)\(\s*([A-Z][A-Z0-9_]*)\s*[,)]`)

	envNameRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	constEnvRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]*[A-Z0-9]$`)
)

// EnvReads are the environment variables a build of harness's code reads, by name, sorted: a
// name its code passes to the language's way of reading one (std::env::var, process.env,
// os.environ), directly or as a constant that holds it. Tests and comments are not the
// build's code. A name built at run time is not read here.
func (l *Library) EnvReads(harness, version string) ([]string, error) {
	s, ok := Sources[harness]
	if !ok {
		return nil, fmt.Errorf("%s: %w", harness, ErrNotPublic)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t, err := l.tree(s, version)
	if err != nil {
		return nil, err
	}
	if t.env == nil {
		lang, err := languageOf(s.Lang)
		if err != nil {
			return nil, err
		}
		if t.env, err = read(l, t, func(r io.Reader) ([]string, error) { return scanEnvReads(r, s, lang) }); err != nil {
			return nil, err
		}
	}
	return slices.Clone(t.env), nil
}

var (
	// A function that reads the variable its parameter names (fn env_var_present(name: &str)
	// { std::env::var(name) … }) reads each name it is called with.
	rustEnvParam = regexp.MustCompile(`\benv::var(?:_os)?\(\s*&?[a-z_][a-z0-9_]*\s*\)`)
	tsEnvParam   = regexp.MustCompile(`\b(?:process|Bun)\.env\[\s*[a-z_$][\w$]*\s*\]`)
	pyEnvParam   = regexp.MustCompile(`\b(?:environ\.get|getenv|environ\.setdefault)\(\s*[a-z_][a-z0-9_]*\s*[,)]`)
	fnNameRE     = map[string]*regexp.Regexp{
		"rust":       regexp.MustCompile(`\bfn\s+([a-z_][a-z0-9_]*)`),
		"typescript": regexp.MustCompile(`(?:\bfunction\s+([A-Za-z_$][\w$]*)|([A-Za-z_$][\w$]*)\s*=\s*(?:async\s*)?\(|^\s*(?:async\s+)?([A-Za-z_$][\w$]*)\s*\()`),
		"python":     regexp.MustCompile(`\bdef\s+([a-z_][a-z0-9_]*)`),
	}
	// A call with a variable's name, or a constant, as its first argument: the constant maybe
	// by reference, or through its module (auth::CODEX_API_KEY_ENV_VAR, env.HOME_VAR).
	envCallRE = regexp.MustCompile(`\b([A-Za-z_$][\w$]*)\(\s*&?(?:"([A-Z][A-Z0-9_]+)"|(?:[A-Za-z_][\w]*(?:::|\.))*([A-Z][A-Z0-9_]+))\s*[,)]`)
)

// envTestish reports a file that is test support though no test (a test server's binary, a
// crate of test helpers), or a build script, whose variables the build does not read.
func envTestish(file string) bool {
	for _, seg := range strings.Split(file, "/") {
		seg = strings.ToLower(strings.TrimSuffix(seg, path.Ext(seg)))
		if strings.HasPrefix(seg, "test") || strings.HasSuffix(seg, "test") || strings.HasSuffix(seg, "tests") ||
			strings.Contains(seg, "test_support") || strings.Contains(seg, "test-support") || strings.Contains(seg, "testutil") ||
			strings.Contains(seg, "mock") || strings.Contains(seg, "fixture") {
			return true
		}
	}
	return path.Base(file) == "build.rs"
}

func scanEnvReads(r io.Reader, s Source, lang *language) ([]string, error) {
	reads, byConst := map[string]bool{}, map[string]bool{}
	consts := map[string]string{}
	wrappers := map[string]bool{}     // functions that read the variable their parameter names
	calls := map[string][][2]string{} // each function's calls with a name, or a constant
	patterns, constRead, paramRead := tsEnvReads, tsEnvConst, tsEnvParam
	switch s.Lang {
	case "rust":
		patterns, constRead, paramRead = rustEnvReads, rustEnvConst, rustEnvParam
	case "python":
		patterns, constRead, paramRead = pyEnvReads, pyEnvConst, pyEnvParam
	}
	fnName := fnNameRE[s.Lang]
	if fnName == nil {
		fnName = fnNameRE["typescript"]
	}
	code := func(file string) bool { return lang.code(file) && !lang.testFile(file) && !envTestish(file) }
	err := tarFiles(r, s, code, func(file string, r io.Reader) error {
		var tests testModule
		lex := newLexer(s.Lang, false)
		fn := ""
		return lines(file, r, func(_ int, raw string) {
			line, _ := lex.line(raw)
			if (lang.testModules && tests.skip(line)) || strings.TrimSpace(line) == "" {
				return
			}
			if lang.fnStart(line) {
				fn = ""
				if m := fnName.FindStringSubmatch(line); m != nil {
					fn = strings.Join(m[1:], "")
				}
			}
			if m := lang.constName.FindStringSubmatch(line); m != nil {
				if _, seen := consts[m[1]]; !seen {
					consts[m[1]] = m[2]
				}
			}
			for _, re := range patterns {
				for _, m := range re.FindAllStringSubmatch(line, -1) {
					reads[m[1]] = true
				}
			}
			for _, m := range constRead.FindAllStringSubmatch(line, -1) {
				byConst[m[1]] = true
			}
			// A wrapper says what it is in its name (env_var_present, getEnv): a function as
			// generic as get(key) reads other things by the names it is called with too.
			if fn != "" && strings.Contains(strings.ToLower(fn), "env") && paramRead.MatchString(line) {
				wrappers[fn] = true
			}
			for _, m := range envCallRE.FindAllStringSubmatch(line, -1) {
				if len(calls[m[1]]) < 4096 {
					calls[m[1]] = append(calls[m[1]], [2]string{m[2], m[3]})
				}
			}
		})
	})
	if err != nil {
		return nil, err
	}
	for name := range wrappers {
		for _, c := range calls[name] {
			if c[0] != "" {
				reads[c[0]] = true
			} else {
				byConst[c[1]] = true
			}
		}
	}
	for name := range byConst {
		// What a constant holds is a variable's name only where it is spelled as one.
		if v, ok := consts[name]; ok && constEnvRE.MatchString(v) {
			reads[v] = true
		}
	}
	out := []string{}
	for _, name := range slices.Sorted(maps.Keys(reads)) {
		if len(name) >= 2 && envNameRE.MatchString(name) {
			out = append(out, name)
		}
	}
	return out, nil
}
