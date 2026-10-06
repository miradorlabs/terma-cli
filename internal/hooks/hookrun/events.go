package hookrun

// The event names and attribute keys terma sends are internal/semconv's, generated from the
// Weaver registry in semconv/registry: that registry is the contract with the platform.
// What is here never leaves the machine under its own name.

// AttrProjectID carries the developer's team on a spooled event; delivery routes by it and
// strips it, since the project leaves as the resource's mirador.project.id.
const AttrProjectID = "project_id"

// Values of terma.evidence.status and the other *.status attributes; the harness package
// adds "missing" and "unreadable".
const (
	StatusPresent     = "present"
	StatusUnavailable = "unavailable"
)

// UnknownValue replaces an agent's word outside the vocabulary terma forwards, so it never arrives as free text.
const UnknownValue = "unknown"
