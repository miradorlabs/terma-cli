package main

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"sort"
	"time"

	"github.com/miradorlabs/terma-cli/e2e"
)

// A night's snapshot of a build is put together from what the night captured of it: its
// profile (the model request, the probes, the out-of-band captures), its field census, and
// the capability checks the history holds of it. It is merged into the build's published
// snapshot section by section: a section the night did not capture keeps what an earlier
// night did, and says when.

// HistoryEntry is a capability check's result, as docs/compat/history.json keeps it.
type HistoryEntry struct {
	Harness    string    `json:"harness"`
	Version    string    `json:"version"`
	Platform   string    `json:"platform"`
	Capability string    `json:"capability"`
	Result     string    `json:"result"`
	LastRun    time.Time `json:"last_run"`
	Why        string    `json:"why,omitempty"`
}

// CensusRun is a build a census scenario ran, as report/census.json says.
type CensusRun struct {
	Harness string `json:"harness"`
	Version string `json:"version"`
	Failed  bool   `json:"failed,omitempty"`
}

// night is everything one run of refgen reads.
type night struct {
	profiles []e2e.Profile
	fields   []e2e.FieldRow
	census   []CensusRun
	history  []HistoryEntry
	labels   map[string]string // capability id → label
	// attempted are builds a run set out to capture: one it captured nothing of is still
	// published, partial, so the hourly check does not set out to capture it again.
	attempted []buildID
	now       time.Time
}

type buildID struct{ harness, version string }

// builds are the builds the night has anything of, by harness then version.
func (n night) builds() []buildID {
	seen := map[buildID]bool{}
	for _, p := range n.profiles {
		seen[buildID{p.Harness, p.Version}] = true
	}
	for _, f := range n.fields {
		seen[buildID{f.Harness, f.Version}] = true
	}
	for _, h := range n.history {
		if h.Result == "pass" || h.Result == "fail" {
			seen[buildID{h.Harness, h.Version}] = true
		}
	}
	for _, id := range n.attempted {
		seen[id] = true
	}
	out := slices.Collect(maps.Keys(seen))
	sort.Slice(out, func(i, j int) bool {
		if out[i].harness != out[j].harness {
			return out[i].harness < out[j].harness
		}
		return versionLess(out[i].version, out[j].version)
	})
	return out
}

// profileOf is the night's profile of a build: its profile scenarios' merged, the macOS one
// first, so a snapshot reads one platform's evidence where it can.
func (n night) profileOf(id buildID) (e2e.Profile, bool) {
	var ps []e2e.Profile
	for _, p := range n.profiles {
		if p.Harness == id.harness && p.Version == id.version {
			ps = append(ps, normalize(p))
		}
	}
	if len(ps) == 0 {
		return e2e.Profile{}, false
	}
	sort.SliceStable(ps, func(i, j int) bool {
		if (ps[i].Platform == "darwin/arm64") != (ps[j].Platform == "darwin/arm64") {
			return ps[i].Platform == "darwin/arm64"
		}
		return ps[i].Scenario < ps[j].Scenario
	})
	merged := ps[0]
	for _, p := range ps[1:] {
		if p.Platform != merged.Platform {
			continue
		}
		// A scenario beside the plain turn (its egress, its telemetry) takes a first turn of
		// its own: the plain turn's is the first turn, the others' behaviour probes are kept.
		for _, r := range p.Requests {
			if r.Probe == "first-turn" && hasFirstTurn(merged) {
				continue
			}
			merged.Requests = append(merged.Requests, r)
		}
		merged.Egress = append(merged.Egress, p.Egress...)
		merged.Failed = merged.Failed || p.Failed
		for k, v := range p.Help {
			if merged.Help == nil {
				merged.Help = map[string]string{}
			}
			merged.Help[k] = v
		}
		for k, v := range p.Probes {
			if merged.Probes == nil {
				merged.Probes = map[string]json.RawMessage{}
			}
			merged.Probes[k] = v
		}
		for k, v := range p.Files {
			if merged.Files == nil {
				merged.Files = map[string]json.RawMessage{}
			}
			merged.Files[k] = v
		}
		if len(merged.Binary) == 0 {
			merged.Binary = p.Binary
		}
		if len(merged.Command) == 0 {
			merged.Command = p.Command
		}
	}
	return merged, true
}

// tonight is the snapshot of a build from the night's evidence alone.
func (n night) tonight(id buildID) *Snapshot {
	s := &Snapshot{SchemaVersion: schemaVersion, Harness: id.harness, Version: id.version, CapturedAt: n.now}
	if p, ok := n.profileOf(id); ok {
		s.Platform = p.Platform
		s.Capture = Capture{Command: p.Command, Partial: p.Failed,
			Notes: []string{"Model requests were answered by a local fixture provider, not the vendor's API; nothing was sent to a model."}}
		deriveRequestSections(s, p)
		deriveBehavior(s, p)
		deriveCLI(s, p)
		deriveOutOfBand(s, p)
		deriveTelemetryProbe(s, p)
	}
	n.deriveTelemetry(s)
	n.deriveSupport(s)
	if len(present(s)) == 0 && slices.Contains(n.attempted, id) {
		s.Capture = Capture{Partial: true, Notes: []string{"This build's capture failed; the next nightly run tries it again."}}
	}
	return s
}

// deriveTelemetry is the build's field census: every surface and key it exported, the kinds
// of value each carried, and what terma's relay does with it.
func (n night) deriveTelemetry(s *Snapshot) {
	surfaces := map[string]any{}
	for _, f := range n.fields {
		if f.Harness != s.Harness || f.Version != s.Version {
			continue
		}
		surf, _ := surfaces[f.Surface].(map[string]any)
		if surf == nil {
			surf = map[string]any{"keys": map[string]any{}}
			surfaces[f.Surface] = surf
		}
		surf["keys"].(map[string]any)[f.Key] = map[string]any{"kinds": f.Kinds, "class": f.Class}
	}
	if len(surfaces) == 0 {
		return
	}
	if s.Platform == "" && len(n.fields) > 0 {
		for _, f := range n.fields {
			if f.Harness == s.Harness && f.Version == s.Version {
				s.Platform = f.Platform
				break
			}
		}
	}
	if s.Telemetry == nil {
		s.Telemetry = map[string]any{}
	}
	// A probe's metric types and span parents join the census's surfaces.
	if probe, ok := s.Telemetry["surfaces"].(map[string]any); ok {
		for name, extra := range probe {
			if surf, ok := surfaces[name].(map[string]any); ok {
				if m, ok := extra.(map[string]any); ok {
					for k, v := range m {
						if k != "keys" {
							surf[k] = v
						}
					}
				}
			}
		}
	}
	s.Telemetry["surfaces"] = surfaces
	for _, c := range n.census {
		if c.Harness == s.Harness && c.Version == s.Version && c.Failed {
			s.Capture.Notes = append(s.Capture.Notes, "The field census of this build was partial: a census scenario failed.")
			break
		}
	}
}

// deriveTelemetryProbe takes what a telemetry probe recorded: metric types, span parents and
// export cadence, which the census does not see.
func deriveTelemetryProbe(s *Snapshot, p e2e.Profile) {
	raw, ok := p.Probes["telemetry"]
	if !ok {
		return
	}
	var v map[string]any
	if json.Unmarshal(raw, &v) != nil {
		return
	}
	s.Telemetry = v
}

// deriveSupport is terma's verdict on the build, from every capability check the history
// holds of it.
func (n night) deriveSupport(s *Snapshot) {
	var caps []any
	pass, fail := 0, 0
	coreFailed := false
	for _, h := range n.history {
		if h.Harness != s.Harness || h.Version != s.Version {
			continue
		}
		c := map[string]any{"id": h.Capability, "result": h.Result, "platform": h.Platform}
		if l := n.labels[h.Capability]; l != "" {
			c["label"] = l
		}
		if h.Why != "" {
			c["why"] = h.Why
		}
		caps = append(caps, c)
		switch h.Result {
		case "pass":
			pass++
		case "fail":
			fail++
			if h.Capability == "relay.telemetry" || h.Capability == "telemetry.native" {
				coreFailed = true
			}
		}
	}
	if len(caps) == 0 {
		return
	}
	sort.SliceStable(caps, func(i, j int) bool {
		a, b := caps[i].(map[string]any), caps[j].(map[string]any)
		return a["id"].(string)+a["platform"].(string) < b["id"].(string)+b["platform"].(string)
	})
	s.Support = map[string]any{"status": verdict(pass, fail, coreFailed), "capabilities": caps}
}

func verdict(pass, fail int, coreFailed bool) string {
	switch {
	case pass+fail == 0:
		return "untested"
	case coreFailed:
		return "unsupported"
	case fail > 0:
		return "degraded"
	}
	return "supported"
}

// merge is old with what tonight captured: each section tonight has replaces old's, and says
// when; the rest keep old's. A night's partial capture (a failed profile scenario) replaces
// nothing a whole one took.
func merge(old, tonight *Snapshot, now time.Time) *Snapshot {
	if old == nil {
		out := *tonight
		out.Capture.Sections = map[string]time.Time{}
		for _, sec := range present(tonight) {
			out.Capture.Sections[sec] = now
		}
		return &out
	}
	out := *old
	if out.Capture.Sections == nil {
		out.Capture.Sections = map[string]time.Time{}
	}
	keepWhole := tonight.Capture.Partial && !old.Capture.Partial
	ot, tt := out.Tree(), tonight.Tree()
	took := false
	for _, sec := range present(tonight) {
		if keepWhole && sec != "support" && sec != "telemetry" && ot[sec] != nil {
			continue
		}
		// The same again is no new capture: when a section was captured stays when it last
		// changed, so a night that only confirms what was known moves nothing.
		if reflect.DeepEqual(ot[sec], tt[sec]) {
			continue
		}
		ot[sec] = tt[sec]
		out.Capture.Sections[sec] = now
		// The history's verdict is no capture of the build.
		took = took || sec != "support"
	}
	data, _ := json.Marshal(ot)
	var merged Snapshot
	_ = json.Unmarshal(data, &merged)
	merged.Capture.Sections = out.Capture.Sections
	if len(tonight.Capture.Command) > 0 && !keepWhole {
		merged.Capture.Command = tonight.Capture.Command
		merged.Capture.Partial = tonight.Capture.Partial
		merged.Capture.Notes = tonight.Capture.Notes
		merged.Platform = tonight.Platform
	}
	if merged.Platform == "" {
		merged.Platform = tonight.Platform
	}
	if took {
		merged.CapturedAt = now
	}
	return &merged
}

// present are the sections s has, in the schema's order.
func present(s *Snapshot) []string {
	t := s.Tree()
	var out []string
	for _, sec := range sections {
		if t[sec] != nil {
			out = append(out, sec)
		}
	}
	return out
}

func hasFirstTurn(p e2e.Profile) bool {
	for _, r := range p.Requests {
		if r.Probe == "first-turn" {
			return true
		}
	}
	return false
}
