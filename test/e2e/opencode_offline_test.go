package e2e

import (
	"runtime"
	"strings"
	"testing"
)

// Each OpenCode major resolves from its own package: "lastN" and "checked" pick per
// major, the installed and named builds once.
func TestOpenCodeVersionSpecSplitsByMajor(t *testing.T) {
	t.Parallel()
	for spec, want := range map[string]string{
		"installed,last3":       "last3",
		"installed,1.19.0":      "",
		"checked, last2,2.0.30": "checked,last2",
	} {
		if got := releaseParts(spec); got != want {
			t.Errorf("releaseParts(%q) = %q, want %q", spec, got, want)
		}
	}
	if opencodeMajor("1.19.2") != 1 || opencodeMajor("2.0.30") != 2 || opencodeMajor("x") != 0 {
		t.Error("opencodeMajor misreads a version")
	}
	if !opencode2(Binary{Version: "2.0.30"}) || opencode2(Binary{Version: "1.19.2"}) {
		t.Error("opencode2 misreads a build")
	}
	one, two := opencodeTarball("1.19.2"), opencodeTarball("2.0.30")
	if !strings.Contains(one, "/opencode-"+runtime.GOOS+"-") || !strings.HasSuffix(one, "-1.19.2.tgz") {
		t.Errorf("OpenCode 1 tarball = %s", one)
	}
	if !strings.Contains(two, "/@opencode/cli-"+runtime.GOOS+"-") || !strings.HasSuffix(two, "-2.0.30.tgz") {
		t.Errorf("OpenCode 2 tarball = %s", two)
	}
}
