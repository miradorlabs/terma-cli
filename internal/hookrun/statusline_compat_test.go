package hookrun

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These are terminal byte streams, not strings to sanitize or reflow. Prefixing
// must not split ANSI/OSC sequences, normalize newlines, or drop trailing resets.
func TestStatusLineExoticOutputPreserved(t *testing.T) {
	cases := map[string][]byte{
		"empty":               {},
		"no-newline":          []byte("plain"),
		"multiline-crlf":      []byte("one\r\ntwo\r\n"),
		"leading-blank-lines": []byte("\n\nthird\n"),
		"sgr":                 []byte("\x1b[1;3;4;7;9mstyled\x1b[22;23;24;27;29m\x1b[0m"),
		"truecolor":           []byte("\x1b[38;2;12;34;56m\x1b[48;2;67;89;90mRGB\x1b[0m\n"),
		"256-color":           []byte("\x1b[38;5;208m\x1b[48;5;17mcolors\x1b[39;49m"),
		"powerline-unicode":   []byte("\x1b[44m λ main \x1b[0m 中文 👩🏽‍💻 é ▰▰▱\n"),
		"osc8-st":             []byte("\x1b]8;;https://example.test/path?q=a;b=c\x1b\\link\x1b]8;;\x1b\\"),
		"osc8-bel":            []byte("\x1b]8;;https://example.test\alink\x1b]8;;\a"),
		"cursor-controls":     []byte("\r\x1b[2K\x1b[4Gvalue\b!\x1b[s\x1b[2C.\x1b[u"),
		"tabs":                []byte("left\tright\n"),
		"binary":              {0, 0xff, 0x1b, '[', 'm', '\n'},
		"large":               bytes.Repeat([]byte("\x1b[32m✓\x1b[0m\n"), 10000),
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			for _, indicator := range []bool{false, true} {
				env, out, _ := statusEnv(t, quotaPayload)
				t.Setenv("NO_COLOR", "1")
				path := filepath.Join(t.TempDir(), "output with spaces.bin")
				if err := os.WriteFile(path, want, 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("TERMA_TEST_RENDER_OUTPUT", path)
				code := StatusLine(context.Background(), env, StatusLineOptions{Renderer: `cat "$TERMA_TEST_RENDER_OUTPUT"`, Indicator: indicator})
				expected := append([]byte(nil), want...)
				if indicator && len(want) > 0 {
					expected = append([]byte("t "), expected...)
				}
				if code != 0 || !bytes.Equal(out.Bytes(), expected) {
					t.Fatalf("indicator=%v code=%d output differs (%d vs %d bytes)", indicator, code, out.Len(), len(expected))
				}
			}
		})
	}
}

func TestStatusLineShellCompatibility(t *testing.T) {
	for name, renderer := range map[string]string{
		"literal-wrapper-name": `printf 'terma hook statusline'`,
		"pipeline":             `cat | tr a-z A-Z`,
		"quotes-and-multiline": "printf '%s\\n' \"it's quoted\"; cat <<'END'\n$HOME `literal` \\backslash\nEND\ncat",
		"env-cwd-and-width":    `printf '%s|%s|%s|%s\n' "$PWD" "$COLUMNS" "$TERM" "$TERMA_TEST_STYLE"; cat`,
		"early-close":          `printf done`,
		"stderr-and-failure":   `cat; printf '\033[31mwarning\033[0m\n' >&2; exit 17`,
		"chunked-escapes":      `printf '\033'; printf '[38;2;1;2;3m'; printf text; printf '\033[0m\n'; cat`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("NO_COLOR", "1")
			t.Setenv("COLUMNS", "73")
			t.Setenv("TERM", "xterm-256color")
			t.Setenv("TERMA_TEST_STYLE", "🎨 value with spaces")
			env, out, _ := statusEnv(t, quotaPayload)
			cwd, err := filepath.EvalSymlinks(env.Cwd)
			if err != nil {
				t.Fatal(err)
			}
			env.Cwd = cwd
			direct := exec.Command("sh", "-c", renderer)
			direct.Dir = env.Cwd
			direct.Stdin = strings.NewReader(quotaPayload)
			var expected, expectedErr, actualErr bytes.Buffer
			direct.Stdout = &expected
			direct.Stderr = &expectedErr
			_ = direct.Run()
			env.Stderr = &actualErr
			got := StatusLine(context.Background(), env, StatusLineOptions{Renderer: renderer, Indicator: true})
			want := expected.Bytes()
			if len(want) > 0 {
				want = append([]byte("t "), want...)
			}
			if got != direct.ProcessState.ExitCode() || !bytes.Equal(out.Bytes(), want) || actualErr.String() != expectedErr.String() {
				t.Fatalf("code %d/%d stdout %q/%q stderr %q/%q", got, direct.ProcessState.ExitCode(), out.Bytes(), want, actualErr.String(), expectedErr.String())
			}
		})
	}
}

func TestStatusLineOversizedInputPassesThroughWithIndicator(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	input := strings.Repeat("🦊", statusLineMaxInput/4+1)
	env, out, sp := statusEnv(t, input)
	if code := StatusLine(context.Background(), env, StatusLineOptions{Renderer: "cat", Indicator: true}); code != 0 || out.String() != "t "+input {
		t.Fatalf("code %d bytes %d", code, out.Len())
	}
	if events := spooledQuota(t, sp); len(events) != 0 {
		t.Fatal("oversized input was parsed")
	}
}

func TestStatusLineStartFailureStillCaptures(t *testing.T) {
	env, out, sp := statusEnv(t, quotaPayload)
	var errOut bytes.Buffer
	env.Stderr = &errOut
	code := StatusLine(context.Background(), env, StatusLineOptions{Renderer: "printf never", Shell: "/does/not/exist", Indicator: true})
	if code == 0 || out.Len() != 0 || errOut.Len() == 0 {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	if evs := spooledQuota(t, sp); len(evs) != 1 {
		t.Fatal(evs)
	}
}

func FuzzStatusLineIndicatorPreservesRendererBytes(f *testing.F) {
	for _, seed := range [][]byte{nil, []byte("plain"), []byte("\x1b[31mred\x1b[0m\n"), {0, 0xff, '\r', '\n'}} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		original := bytes.Clone(raw)
		got := withIndicator(raw)
		if !bytes.Equal(raw, original) {
			t.Fatal("mutated renderer bytes")
		}
		if len(raw) == 0 {
			if len(got) != 0 {
				t.Fatal("empty renderer became visible")
			}
			return
		}
		mark := []byte(indicatorMark())
		if !bytes.HasPrefix(got, mark) || !bytes.Equal(got[len(mark):], raw) {
			t.Fatal("renderer bytes changed")
		}
	})
}
