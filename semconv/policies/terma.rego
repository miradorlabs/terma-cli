package after_resolution

import rego.v1

# terma's own rules for its registry, on top of Weaver's. input.registry holds only what
# this registry defines; the events' attribute references reach the dependencies' too.

# The keys standard OpenTelemetry registries own: terma only references them.
reserved_prefixes := ["gen_ai.", "vcs.", "user.", "session.", "error.", "service.", "host."]

# The one key outside terma.* that terma defines, the platform's routing key.
declared_exceptions := {"mirador.project.id"}

# Keys that carry what was said or a tool's input and output.
content_keys := {
	"terma.message.text", "terma.approval.reason", "terma.session.title",
	"gen_ai.tool.call.arguments", "gen_ai.tool.call.result", "terma.repository.root",
}

unit_suffixes := [".count", "_ms", "_percent", "_minutes", "_tokens"]

finding(id, key, message) := {
	"id": id,
	"message": message,
	"level": "violation",
	"context": {"attribute_key": key},
}

# Every key terma sends: its own and those its events and groups reference.
used_attributes contains attr if {
	some attr in input.registry.attributes
}

used_attributes contains attr if {
	some event in input.registry.events
	some attr in event.attributes
}

used_attributes contains attr if {
	some group in input.registry.attribute_groups
	some attr in group.attributes
}

used_keys contains attr.key if {
	some attr in used_attributes
}

# 1. What this registry defines is terma's.
deny contains finding("terma_prefix", attr.key, sprintf("Attribute '%s' must start with 'terma.': only mirador.project.id is excepted.", [attr.key])) if {
	some attr in input.registry.attributes
	not startswith(attr.key, "terma.")
	not attr.key in declared_exceptions
}

# 2. A standard registry's namespace is referenced, never defined here.
deny contains finding("reserved_namespace", attr.key, sprintf("Attribute '%s' is in a namespace a dependency owns: reference it instead.", [attr.key])) if {
	some attr in input.registry.attributes
	some prefix in reserved_prefixes
	startswith(attr.key, prefix)
}

# 3. No key is both a value and a namespace.
deny contains finding("leaf_and_namespace", leaf, sprintf("Attribute '%s' is also the namespace of '%s'.", [leaf, other])) if {
	some leaf in used_keys
	some other in used_keys
	startswith(other, concat("", [leaf, "."]))
}

# 4. Every attribute says what it is, every enum what it holds, every measure its unit.
deny contains finding("brief_required", attr.key, sprintf("Attribute '%s' needs a brief.", [attr.key])) if {
	some attr in input.registry.attributes
	trim_space(object.get(attr, "brief", "")) == ""
}

deny contains finding("enum_members", attr.key, sprintf("Enum attribute '%s' lists no members.", [attr.key])) if {
	some attr in input.registry.attributes
	is_object(attr.type)
	count(object.get(attr.type, "members", [])) == 0
}

deny contains finding("unit_named", attr.key, sprintf("Attribute '%s' is a count, duration or percentage: name its unit in the key or the brief.", [attr.key])) if {
	some attr in input.registry.attributes
	measure(attr)
	not unit_named(attr)
}

measure(attr) if {
	attr.type in {"int", "double"}
	regex.match(`count|duration|percent|minutes`, attr.key)
}

measure(attr) if {
	attr.type in {"int", "double"}
	regex.match(`(?i)\bhow (many|long|much|full)\b`, attr.brief)
}

unit_named(attr) if {
	some suffix in unit_suffixes
	endswith(attr.key, suffix)
}

unit_named(attr) if {
	regex.match(`(?i)\b(milliseconds|seconds|minutes|tokens|from 0 to 100|lines|files|sessions|messages|tool calls|loops|steps)\b`, attr.brief)
}

# 5. Content is marked as content wherever it is defined or referenced.
deny contains finding("content_marked", attr.key, sprintf("Attribute '%s' carries content: annotate it with content.", [attr.key])) if {
	some attr in used_attributes
	attr.key in content_keys
	not object.get(object.get(attr, "annotations", {}), "content", null) in {"prompts", "tool"}
}
