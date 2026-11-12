package e2e

import (
	"slices"
	"testing"
)

// Claude Code's settings schema read out of a minified bundle: the object of zod schemas whose
// first key is $schema, by the builders the bundled zod defines.
func TestClaudeSettingsInOffline(t *testing.T) {
	bundle := []byte(`var S0=Xn("ZodString",(e,t)=>{});function o(e){return sn(S0,e)}` +
		`var B0=Xn("ZodBoolean",(e,t)=>{});function O(e){return sn(B0,e)}` +
		`function ba(){return{$schema:()=>o().optional().describe("JSON Schema reference for Claude Code settings"),` +
		`respectGitignore:()=>O().optional().describe("Whether file picker should respect .gitignore files")}}`)
	opts := ClaudeSettingsIn(bundle)
	if o := opts["respectGitignore"]; o.Type != "boolean" || o.Description != "Whether file picker should respect .gitignore files" {
		t.Errorf("respectGitignore = %+v (options %v)", o, opts)
	}
	if len(ClaudeSettingsIn([]byte(`"JSON Schema reference for Claude Code settings"`))) != 0 {
		t.Error("options from a bundle with no settings object")
	}
}

// The Copilot CLI's settings registry: each entry's path, kind, description and choices.
func TestCopilotSettingsInOffline(t *testing.T) {
	js := []byte(`x=1;[{"path":"banner","leafKind":"enum","description":"Frequency of the startup banner.","options":["always","once","never"]},` +
		`{"path":"sidebar.enabled","leafKind":"boolean","description":"Show the sidebar."},{"path":"allowedUrls","leafKind":"string-array","description":"URLs."}]{"canonicalKeys":[]}`)
	opts := CopilotSettingsIn(js)
	if o := opts["banner"]; o.Type != "string" || len(o.Enum) != 3 {
		t.Errorf("banner = %+v", o)
	}
	if opts["sidebar.enabled"].Type != "boolean" || opts["allowedUrls"].Type != "array" {
		t.Errorf("options = %v", opts)
	}
}

// The variables a bundle reads: through process.env, and its own through the name it keeps
// process.env under; not a constant's property, nor a name running into lowercase.
func TestJSEnvReadsInOffline(t *testing.T) {
	js := []byte(`let a=process.env.ANTHROPIC_API_KEY,b=process.env["HTTPS_PROXY"],c=e.CLAUDE_CODE_ENTRYPOINT;` +
		`d=x().CLAUDE_CODE_NOT_A_NAME;f=CLAUDE_CODE_NOT_READ;g=a.CLAUDE_CODE_PARTial;h=process.env.npm_config_user_agent`)
	got := JSEnvReadsIn(js, []string{"CLAUDE_", "ANTHROPIC_"})
	want := []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_ENTRYPOINT", "HTTPS_PROXY", "npm_config_user_agent"}
	if !slices.Equal(got, want) {
		t.Errorf("JSEnvReadsIn = %v, want %v", got, want)
	}
}
