package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"strings"
	"testing"
)

// `terma relay classify` answers as the relay's content policy treats each key, with the
// agents this terma knows: the e2e field census classifies every key it saw with it.
func TestRelayClassify(t *testing.T) {
	c := testApp.newRelayCommand()
	var out bytes.Buffer
	c.SetArgs([]string{"classify"})
	c.SetIn(strings.NewReader(`["from_mode", "prompt", "a.key.no.rule.names", "resource/service.name"]`))
	c.SetOut(&out)
	c.SetErr(io.Discard)
	if err := c.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	want := map[string]string{"from_mode": "safe", "prompt": "prompt", "a.key.no.rule.names": "unclassified", "resource/service.name": "safe"}
	if !maps.Equal(got, want) {
		t.Errorf("classify = %v, want %v", got, want)
	}
	c = testApp.newRelayCommand()
	c.SetArgs([]string{"classify"})
	c.SetIn(strings.NewReader("not json"))
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	if err := c.ExecuteContext(context.Background()); err == nil {
		t.Error("classify accepted input that is no JSON array")
	}
}
