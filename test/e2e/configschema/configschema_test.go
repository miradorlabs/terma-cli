package configschema

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

// want checks that opts has each option as given, and no option missing describes one wanted.
func want(t *testing.T, opts Options, wanted map[string]Option) {
	t.Helper()
	for path, w := range wanted {
		got, ok := opts[path]
		if !ok {
			t.Errorf("no option %s; have %v", path, slices.Sorted(func(yield func(string) bool) {
				for k := range opts {
					if !yield(k) {
						return
					}
				}
			}))
			continue
		}
		if !reflect.DeepEqual(got, w) {
			g, _ := json.Marshal(got)
			x, _ := json.Marshal(w)
			t.Errorf("%s = %s, want %s", path, g, x)
		}
	}
}

// A JSON schema's $refs, allOf and oneOf are resolved; a table of named keys is <name>; a
// list of tables' keys are under []; a null default is none.
func TestFromJSONSchema(t *testing.T) {
	schema := `{
	  "type": "object",
	  "definitions": {
	    "SandboxMode": {"type": "string", "enum": ["read-only", "workspace-write"]},
	    "Approval": {"oneOf": [{"type": "string", "enum": ["never"]}, {"type": "object", "properties": {"granular": {"type": "boolean"}}}]},
	    "Server": {"type": "object", "properties": {"command": {"type": "string", "description": "What to run."}, "env": {"type": "object", "additionalProperties": {"type": "string"}}}},
	    "Loop": {"type": "object", "properties": {"next": {"$ref": "#/definitions/Loop"}}}
	  },
	  "properties": {
	    "sandbox_mode": {"allOf": [{"$ref": "#/definitions/SandboxMode"}], "description": "Sandbox mode to use.", "default": "read-only"},
	    "approval_policy": {"$ref": "#/definitions/Approval"},
	    "mcp_servers": {"type": "object", "additionalProperties": {"$ref": "#/definitions/Server"}, "default": {}},
	    "notify": {"type": ["array", "null"], "items": {"type": "string"}, "default": null},
	    "hooks": {"type": "array", "items": {"type": "object", "properties": {"matcher": {"type": "string"}}}},
	    "old": {"type": "boolean", "deprecated": true},
	    "models": {"type": "object", "patternProperties": {"^.*$": {"type": "integer"}}},
	    "loop": {"$ref": "#/definitions/Loop"}
	  }
	}`
	opts, err := FromJSONSchema([]byte(schema))
	if err != nil {
		t.Fatal(err)
	}
	want(t, opts, map[string]Option{
		"sandbox_mode":                  {Type: "string", Description: "Sandbox mode to use.", Default: "read-only", Enum: []any{"read-only", "workspace-write"}},
		"approval_policy":               {Type: "object|string", Enum: []any{"never"}},
		"approval_policy.granular":      {Type: "boolean"},
		"mcp_servers":                   {Type: "object", Default: map[string]any{}},
		"mcp_servers.<name>.command":    {Type: "string", Description: "What to run."},
		"mcp_servers.<name>.env.<name>": {Type: "string"},
		"notify":                        {Type: "array|null"},
		"hooks[].matcher":               {Type: "string"},
		"old":                           {Type: "boolean", Deprecated: true},
		"models.<name>":                 {Type: "integer"},
		"loop.next":                     {Type: "object"},
	})
	if _, ok := opts["loop.next.next.next.next"]; ok {
		t.Error("a path deeper than MaxDepth")
	}
}

// A bundle's zod settings: builders by the bundle's names, a thunk per key, a memo's arrow,
// a nested object's keys dotted, a feature's gated settings among the object's own.
func TestFromBundle(t *testing.T) {
	bundle := `var S0=Xn("ZodString",(e,t)=>{});function o(e){return sn(S0,e)}` +
		`var B0=Xn("ZodBoolean",(e,t)=>{});function O(e){return sn(B0,e)}` +
		`var N0=Xn("ZodNumber",(e,t)=>{});function A(e){return sn(N0,e)}` +
		`var J0=Xn("ZodObject",(e,t)=>{});function d(e,t){let r={type:"object",shape:e??{}};return new J0(r)}` +
		`var E0=Xn("ZodEnum",(e,t)=>{});function G(e,t){return new E0({type:"enum",entries:e})}` +
		`var R0=Xn("ZodRecord",(e,t)=>{});function ue(e,t,r){return new R0({type:"record",valueType:t})}` +
		`var MODES=["default","plan"],Pm=p(()=>d({mode:G([...MODES]).describe("Mode")}));` +
		`function ba(){return{$schema:()=>o().optional().describe("JSON Schema reference for Claude Code settings"),` +
		`cleanupPeriodDays:()=>A().int().positive().optional().describe("Days to keep (default: 30)."),` +
		`permissions:()=>Pm().optional().describe("Permissions"),` +
		`env:()=>ue(o(),o()).optional(),` +
		`breakReminder:()=>d({enabled:O().optional().default(!1)}).optional(),` +
		`voice:{buildGate:()=>!0,shape:()=>({voiceEnabled:O().optional().describe("Voice")})},` +
		`...x.FLAG&&{xaaIdp:()=>d({issuer:o().url()}).optional()}}}`
	at := -1
	for i := range bundle {
		if bundle[i:min(len(bundle), i+9)] == "{$schema:" {
			at = i
		}
	}
	opts := FromBundle([]byte(bundle), at)
	want(t, opts, map[string]Option{
		"$schema":               {Type: "string", Description: "JSON Schema reference for Claude Code settings"},
		"cleanupPeriodDays":     {Type: "integer", Description: "Days to keep (default: 30)."},
		"permissions":           {Type: "object", Description: "Permissions"},
		"permissions.mode":      {Type: "string", Description: "Mode", Enum: []any{"default", "plan"}},
		"env.<name>":            {Type: "string"},
		"breakReminder.enabled": {Type: "boolean", Default: false},
		"voiceEnabled":          {Type: "boolean", Description: "Voice"},
		"xaaIdp.issuer":         {Type: "string"},
	})
	if _, ok := opts["voice"]; ok {
		t.Error("a gated group read as a setting of its own")
	}
}

// Effect Schema across modules: a namespace import's member, an imported constant, a struct
// with a rest, a union of a boolean and a literal, a @deprecated description.
func TestFromTSModules(t *testing.T) {
	files := map[string][]byte{
		"packages/core/src/v1/config/config.ts": []byte(`export * as ConfigV1 from "./config"
import { Schema } from "effect"
import { PositiveInt } from "../../schema"
import { ConfigAgentV1 } from "./agent"

const LogLevel = Schema.Literals(["DEBUG", "INFO"]).annotate({ identifier: "LogLevel" })

export const Info = Schema.Struct({
  logLevel: Schema.optional(LogLevel).annotate({ description: "Log level" }),
  autoupdate: Schema.optional(Schema.Union([Schema.Boolean, Schema.Literal("notify")])),
  autoshare: Schema.optional(Schema.Boolean).annotate({ description: "@deprecated Use 'share' field instead." }),
  agent: Schema.optional(Schema.Record(Schema.String, ConfigAgentV1.Info)),
  tool_output: Schema.optional(Schema.Struct({ max_lines: Schema.optional(PositiveInt) })),
}).annotate({ identifier: "Config" })
`),
		"packages/core/src/v1/config/agent.ts": []byte(`export * as ConfigAgentV1 from "./agent"
import { Schema } from "effect"
export const Info = Schema.Struct({
  model: Schema.optional(Schema.String),
  steps: Schema.optional(Schema.Number).annotate({ description: "Maximum steps" }),
})
`),
		"packages/core/src/schema.ts": []byte(`export const PositiveInt = Schema.Int.check(Schema.isGreaterThan(0))` + "\n"),
	}
	opts := FromTSModules(files, "packages/core/src/v1/config/config.ts", "Info")
	want(t, opts, map[string]Option{
		"logLevel":              {Type: "string", Description: "Log level", Enum: []any{"DEBUG", "INFO"}},
		"autoupdate":            {Type: "boolean|string", Enum: []any{"notify"}},
		"autoshare":             {Type: "boolean", Description: "@deprecated Use 'share' field instead.", Deprecated: true},
		"agent.<name>.model":    {Type: "string"},
		"agent.<name>.steps":    {Type: "number", Description: "Maximum steps"},
		"tool_output.max_lines": {Type: "integer"},
	})
}

// A dict of defaults: types from the defaults, descriptions from the comments before a key
// and after it on its line, a computed value with no default.
func TestFromPythonDict(t *testing.T) {
	src := `import os

DEFAULT_CONFIG = {
    "model": "",
    # Turn cap. null = unlimited.
    "agent": {
        "max_turns": None,
        "max_size": 128,  # LRU entry cap
        "ratio": 0.75,
        "big": 2 * 1024,
        "verbose": False,
        "toolsets": ["hermes-cli"],
        "aux": _aux(120),
    },
    **EXTRA,
}
`
	opts := FromPythonDict([]byte(src), "DEFAULT_CONFIG")
	want(t, opts, map[string]Option{
		"model":           {Type: "string", Default: ""},
		"agent":           {Type: "object", Description: "Turn cap. null = unlimited."},
		"agent.max_turns": {},
		"agent.max_size":  {Type: "integer", Description: "LRU entry cap", Default: float64(128)},
		"agent.ratio":     {Type: "number", Default: 0.75},
		"agent.big":       {Type: "integer", Default: float64(2048)},
		"agent.verbose":   {Type: "boolean", Default: false},
		"agent.toolsets":  {Type: "array", Default: []any{"hermes-cli"}},
		"agent.aux":       {},
	})
}

// A config catalog's interfaces: each plugin's fields under its package's name.
func TestFromConfigCatalog(t *testing.T) {
	md := "<!-- BEGIN GENERATED config-catalog:@deepseek-ai/dsh-acp -->\n## x\n\n```ts config-catalog\n" +
		"/** Plugin config. */\nexport interface AcpConfig {\n  /** Provider route. */\n  provider?: string\n" +
		"  /** Page size. */\n  pageSize: Volatile<number>\n  mode?: 'native' | 'ptc'\n  agents: {\n    /** Agent id. */\n    id: string\n  }[]\n}\n" +
		"export interface Other {\n  x: string\n}\n```\n<!-- END GENERATED config-catalog:@deepseek-ai/dsh-acp -->\n"
	opts := FromConfigCatalog([]byte(md))
	want(t, opts, map[string]Option{
		"dsh-acp":          {Type: "object", Description: "Plugin config."},
		"dsh-acp.provider": {Type: "string", Description: "Provider route."},
		"dsh-acp.pageSize": {Type: "number", Description: "Page size."},
		"dsh-acp.mode":     {Type: "string", Enum: []any{"native", "ptc"}},
		"dsh-acp.agents":   {Type: "array"},
	})
	if _, ok := opts["dsh-acp.x"]; ok {
		t.Error("a referenced type read as the config")
	}
}

// Registered settings: each call's id, type, values (a constant's), default and description.
func TestFromRegistry(t *testing.T) {
	files := map[string][]byte{
		"src/edit/settings.ts": []byte(`import { register } from "../config/registry"
const EDIT_MODES = ["replace", "patch"] as const

export const cfgEditMode = register({
	id: "edit.mode",
	type: "enum",
	values: EDIT_MODES,
	default: "patch",
	ui: { tab: "files", description: "Select the edit tool variant" },
});

/** Per-model edit variant. Config-file only. */
export const cfgEditModelVariants = register({
	id: "edit.modelVariants",
	type: "record",
	default: {},
});

for (const k of KEYWORDS) register({ id: ` + "`magic.${k}`" + `, type: "boolean", default: true });
`),
	}
	opts := FromRegistry(files, "register")
	want(t, opts, map[string]Option{
		"edit.mode":          {Type: "string", Description: "Select the edit tool variant", Default: "patch", Enum: []any{"replace", "patch"}},
		"edit.modelVariants": {Type: "object", Description: "Per-model edit variant. Config-file only.", Default: map[string]any{}},
	})
	if len(opts) != 2 {
		t.Errorf("options = %v, want two (an id built at run time is none)", opts)
	}
}

// A Rust struct's serde fields, an old ConfigToml's.
func TestFromRustStruct(t *testing.T) {
	src := `#[derive(Deserialize)]
pub struct ConfigToml {
    /// Optional override of model selection.
    pub model: Option<String>,
    /// Sandbox mode to use.
    pub sandbox_mode: Option<SandboxMode>,
    #[serde(rename = "notify")]
    pub notify_command: Option<Vec<String>>,
    #[serde(default)]
    pub mcp_servers: HashMap<String, McpServerConfig>,
    pub project_doc_max_bytes: Option<usize>,
}
`
	opts := FromRustStruct([]byte(src), "ConfigToml")
	want(t, opts, map[string]Option{
		"model":                 {Type: "string", Description: "Optional override of model selection."},
		"sandbox_mode":          {Description: "Sandbox mode to use."},
		"notify":                {Type: "array"},
		"mcp_servers":           {Type: "object"},
		"project_doc_max_bytes": {Type: "integer"},
	})
}
