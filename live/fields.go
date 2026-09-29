package live

import (
	"encoding/json"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
)

// The field registry is how a new harness release that starts carrying content somewhere
// new fails the canary instead of leaking. Every field that reaches Terma through the
// relay — each log attribute, span attribute, span event and its attributes, metric
// data point attribute, resource attribute, and a log body that is not just the event's
// name — is classified in golden/relay/<harness>-fields.json as "metadata" or
// "content":
//
//   - a field the registry does not know fails the newest build, in either mode, until
//     someone classifies it (and, when it is content, adds it to the relay's field sets —
//     internal/relay/content_registry_test.go holds the two together);
//   - a "content" field that reaches a project withholding content fails, whatever its
//     value.
//
// The value scan (leakedFields, planted markers) catches content in a field thought to be
// metadata; the registry catches a field no one has looked at. LIVE_UPDATE_GOLDEN=1
// records new fields: those the relay stripped from the withheld run are content, the
// rest metadata — review the diff before committing it.

// fieldKinds are the prefixes of registry keys.
const (
	fieldLog       = "log:"
	fieldLogBody   = "log-body:"
	fieldSpan      = "span:"
	fieldEvent     = "span-event:"
	fieldEventAttr = "span-event-attr:"
	fieldMetric    = "metric:"
	fieldResource  = "resource:"
)

// observedFields lists every field a run's evidence carries, as registry keys.
func observedFields(e telemetryEvidence) map[string]bool {
	out := map[string]bool{}
	resource := func(r map[string]string) {
		for k := range r {
			out[fieldResource+k] = true
		}
	}
	for _, r := range e.logs {
		for k := range r.Attrs {
			out[fieldLog+k] = true
		}
		if name := r.Attrs["event.name"]; r.Body != "" && r.Body != name && !strings.HasSuffix(name, "."+r.Body) && !strings.HasSuffix(r.Body, "."+name) {
			out[fieldLogBody+name] = true
		}
		resource(r.Resource)
	}
	for _, s := range e.spans {
		for k := range s.Attrs {
			out[fieldSpan+k] = true
		}
		for _, ev := range s.Proto.GetEvents() {
			out[fieldEvent+ev.GetName()] = true
			for k := range flatten(ev.GetAttributes()) {
				out[fieldEventAttr+k] = true
			}
		}
		resource(s.Resource)
	}
	for _, m := range e.metrics {
		for _, p := range m.Proto.GetSum().GetDataPoints() {
			for k := range flatten(p.Attributes) {
				out[fieldMetric+k] = true
			}
		}
		for _, p := range m.Proto.GetHistogram().GetDataPoints() {
			for k := range flatten(p.Attributes) {
				out[fieldMetric+k] = true
			}
		}
		for _, p := range m.Proto.GetGauge().GetDataPoints() {
			for k := range flatten(p.Attributes) {
				out[fieldMetric+k] = true
			}
		}
		resource(m.Resource)
	}
	return out
}

// fieldValues are the string values a registry field takes across the evidence (log,
// span and span event attributes; other kinds carry no content to blank).
func fieldValues(e telemetryEvidence, field string) []string {
	kind, key, _ := strings.Cut(field, ":")
	var out []string
	switch kind + ":" {
	case fieldLog:
		for _, r := range e.logs {
			if v, ok := r.Attrs[key]; ok {
				out = append(out, v)
			}
		}
	case fieldSpan:
		for _, s := range e.spans {
			if v, ok := s.Attrs[key]; ok {
				out = append(out, v)
			}
		}
	case fieldEventAttr:
		for _, s := range e.spans {
			for _, ev := range s.Proto.GetEvents() {
				if v, ok := flatten(ev.GetAttributes())[key]; ok {
					out = append(out, v)
				}
			}
		}
	}
	return out
}

func fieldRegistryPath(harness string) string { return goldenPath("relay/" + harness + "-fields") }

func loadFieldRegistry(t *testing.T, harness string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(fieldRegistryPath(harness))
	if os.IsNotExist(err) {
		return map[string]string{}
	}
	if err != nil {
		t.Fatal(err)
	}
	reg := map[string]string{}
	if err := json.Unmarshal(data, &reg); err != nil {
		t.Fatalf("%s: %v", fieldRegistryPath(harness), err)
	}
	return reg
}

// fieldRuns holds, per harness and build, what the content and withheld runs carried,
// so LIVE_UPDATE_GOLDEN can tell a stripped (content) field from a kept one.
var fieldRuns = struct {
	sync.Mutex
	m map[string]map[bool]map[string]bool
}{m: map[string]map[bool]map[string]bool{}}

// checkFieldRegistry holds a run's fields to the registry: every field classified, and
// no content field at a project that withholds it. strict (the newest build) fails; an
// older build only reports, since fields come and go between releases.
func checkFieldRegistry(t *testing.T, e telemetryEvidence, harness, build string, withheld, strict bool) {
	t.Helper()
	observed := observedFields(e)
	if os.Getenv("LIVE_UPDATE_GOLDEN") == "1" {
		recordFieldRun(t, harness, build, withheld, observed)
		return
	}
	reg := loadFieldRegistry(t, harness)
	report := t.Logf
	if strict {
		report = t.Errorf
	}
	var unknown []string
	for _, f := range slices.Sorted(maps.Keys(observed)) {
		class, ok := reg[f]
		switch {
		case !ok:
			unknown = append(unknown, f)
		case (class == "content" || class == "consent") && withheld:
			t.Errorf("%s: %s is %s and reached a project that withholds it", harness, f, class)
		case class == "redacted" && withheld:
			for _, v := range fieldValues(e, f) {
				if v != "" && v != "<REDACTED>" && v != "[REDACTED]" {
					t.Errorf("%s: %s reached a project that withholds content unblanked", harness, f)
					break
				}
			}
		case !slices.Contains([]string{"metadata", "content", "redacted", "consent"}, class):
			t.Errorf("%s: %s is classified %q; want metadata, content, redacted or consent", harness, f, class)
		}
	}
	if len(unknown) > 0 {
		report("%s: fields the registry does not classify (%s): %v — classify each as metadata or content in %s; "+
			"a content one also joins the relay's field sets", harness, relayMode(!withheld), unknown, fieldRegistryPath(harness))
		Note(t.Name(), "unclassified fields: "+joinStrings(unknown))
	}
}

// recordFieldRun collects a run for LIVE_UPDATE_GOLDEN and, once both modes of a build
// have run, merges what they show into the registry: a field the content run carried
// and the withheld run did not was stripped by the relay, so it is content; every other
// field is metadata. Fields already classified keep their classification.
func recordFieldRun(t *testing.T, harness, build string, withheld bool, observed map[string]bool) {
	t.Helper()
	fieldRuns.Lock()
	defer fieldRuns.Unlock()
	key := harness + "/" + build
	if fieldRuns.m[key] == nil {
		fieldRuns.m[key] = map[bool]map[string]bool{}
	}
	fieldRuns.m[key][withheld] = observed
	content, stripped := fieldRuns.m[key][false], fieldRuns.m[key][true]
	if content == nil || stripped == nil {
		return
	}
	reg := loadFieldRegistry(t, harness)
	added := 0
	for f := range content {
		if _, ok := reg[f]; ok {
			continue
		}
		reg[f] = "metadata"
		if !stripped[f] {
			reg[f] = "content"
		}
		added++
	}
	for f := range stripped {
		if _, ok := reg[f]; !ok {
			reg[f] = "metadata"
			added++
		}
	}
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fieldRegistryPath(harness), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("field registry %s: %d fields added (%d total)", harness, added, len(reg))
}
