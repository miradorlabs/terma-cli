package repohooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// pre-push's input is on stdin, which one reader consumes: the script copies it so the
// displaced hook and terma read the same bytes, whatever they are.
func TestPrePushScriptGivesBothReadersGitsInput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake terma and displaced hooks are sh scripts")
	}
	shells := []string{"sh"}
	if s := os.Getenv("TERMA_SHIM_SHELLS"); s != "" {
		shells = strings.Split(s, ",")
	}
	line := "refs/heads/main " + strings.Repeat("a", 40) + " refs/heads/main " + strings.Repeat("b", 40) + "\n"
	for _, sh := range shells {
		for _, tc := range []struct {
			what  string
			input string
			// prev is the displaced hook's exit status, -1 for none.
			prev int
			// terma is the fake terma's exit status, -1 for none installed.
			terma int
			// tmp is TMPDIR, "" for the test's own; "missing" names one that does not exist.
			tmp    string
			status int
			// prevGot and termaGot are the stdin each should have read; nil for not run.
			prevGot, termaGot *string
		}{
			{what: "both read the input", input: line + line, prev: 0, terma: 0, status: 0, prevGot: ptr(line + line), termaGot: ptr(line + line)},
			{what: "an empty input stays empty", input: "", prev: 0, terma: 0, status: 0, prevGot: ptr(""), termaGot: ptr("")},
			{what: "no final newline", input: strings.TrimSuffix(line, "\n"), prev: 0, terma: 0, status: 0,
				prevGot: ptr(strings.TrimSuffix(line, "\n")), termaGot: ptr(strings.TrimSuffix(line, "\n"))},
			{what: "terma alone", input: line, prev: -1, terma: 0, status: 0, termaGot: ptr(line)},
			{what: "the displaced hook's veto", input: line, prev: 3, terma: 0, status: 3, prevGot: ptr(line)},
			{what: "a failing terma", input: line, prev: 0, terma: 9, status: 0, prevGot: ptr(line), termaGot: ptr(line)},
			{what: "terma gone", input: line, prev: 0, terma: -1, status: 0, prevGot: ptr(line)},
			{what: "no temporary file", input: line, prev: 0, terma: 0, tmp: "missing", status: 0, prevGot: ptr(line)},
			{what: "no temporary file, the veto kept", input: line, prev: 4, terma: 0, tmp: "missing", status: 4, prevGot: ptr(line)},
		} {
			t.Run(sh+"/"+tc.what, func(t *testing.T) {
				dir, out := t.TempDir(), t.TempDir()
				record := func(who string, status int) string {
					return "#!/bin/sh\ncat > '" + filepath.Join(out, who) + "'\nexit " + strconv.Itoa(status) + "\n"
				}
				terma := filepath.Join(t.TempDir(), "terma")
				if tc.terma >= 0 {
					writeExec(t, terma, record("terma", tc.terma))
				}
				writeExec(t, filepath.Join(dir, prePush), script(prePush, terma))
				if tc.prev >= 0 {
					writeExec(t, filepath.Join(dir, prePush+preTermaSuffix), record("prev", tc.prev))
				}
				tmp := t.TempDir()
				if tc.tmp == "missing" {
					tmp = filepath.Join(tmp, "missing")
				}
				args := append(strings.Fields(sh), filepath.Join(dir, prePush), "origin", "https://example.com/r.git")
				cmd := exec.Command(args[0], args[1:]...)
				cmd.Dir, cmd.Env = t.TempDir(), append(os.Environ(), "TMPDIR="+tmp)
				cmd.Stdin = strings.NewReader(tc.input)
				got, err := cmd.CombinedOutput()
				if code := cmd.ProcessState.ExitCode(); code != tc.status {
					t.Fatalf("pre-push exited %d, want %d (%v)\n%s", code, tc.status, err, got)
				}
				if len(got) > 0 {
					t.Errorf("pre-push wrote %q", got)
				}
				for who, want := range map[string]*string{"prev": tc.prevGot, "terma": tc.termaGot} {
					data, err := os.ReadFile(filepath.Join(out, who))
					switch {
					case want == nil && err == nil:
						t.Errorf("%s ran, reading %q", who, data)
					case want != nil && err != nil:
						t.Errorf("%s did not run: %v", who, err)
					case want != nil && string(data) != *want:
						t.Errorf("%s read %q, want %q", who, data, *want)
					}
				}
				// The copy is removed whatever happened.
				if left, _ := filepath.Glob(filepath.Join(tmp, "terma-pre-push.*")); len(left) > 0 {
					t.Errorf("left %v behind", left)
				}
			})
		}
	}
}

func ptr(s string) *string { return &s }
