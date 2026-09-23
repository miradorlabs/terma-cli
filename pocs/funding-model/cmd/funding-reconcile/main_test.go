package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/miradorlabs/terma-cli/pocs/funding-model/replay"
)

func TestRun(t *testing.T) {
	var out, errout bytes.Buffer
	if code := run(nil, &out, &errout); code != 2 || out.Len() != 0 {
		t.Fatal("missing inputs must fail without JSON")
	}
	p := "../../replay/testdata/"
	if code := run([]string{"-scope", p + "claude-scope.json", "-report", p + "claude.csv", "-capture", p + "capture.json"}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	var result replay.Result
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || len(result.Comparisons) != 2 {
		t.Fatalf("invalid output: %s", out.String())
	}
}
