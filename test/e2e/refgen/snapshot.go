package main

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/e2e"
	"github.com/miradorlabs/terma-cli/e2e/refdiff"
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
	// published, partial, so the frequent check captures it again only after a while.
	attempted []buildID
	// unfinished are builds a job of the run set out to capture and did not finish (a platform's
	// capture that failed): published partial, whatever the other jobs took.
	unfinished []buildID
	now        time.Time
	sources    *sourceConfigs // what public builds' source says of their configuration; nil reads none
}

type buildID struct{ harness, version string }

// The notes a partial capture carries until a whole one replaces them.
const (
	failedNote     = "This build's capture failed; the frequent check tries it again, up to four attempts."
	unfinishedNote = "Part of this build's capture did not finish; the frequent check tries it again, up to four attempts."
)

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
		// A desktop app's checks name the build of its agent they ran, not a release of the app
		// (deriveSupport): they make no build of their own.
		if (h.Result == "pass" || h.Result == "fail") && agentOf(h.Harness) == h.Harness {
			seen[buildID{h.Harness, h.Version}] = true
		}
	}
	for _, id := range n.attempted {
		seen[id] = true
	}
	out := slices.Collect(maps.Keys(seen))
	slices.SortStableFunc(out, func(a, b buildID) int {
		if c := strings.Compare(a.harness, b.harness); c != 0 {
			return c
		}
		return refdiff.VersionCompare(a.version, b.version)
	})
	return out
}

// profileOf is the night's profile of a build: its profile scenarios' merged, the macOS one
// first, so a snapshot reads one platform's evidence where it can.
// profileOf is the night's profile of a build, whether its first turn was taken behind the
// recording proxy, and whether there is one.
func (n night) profileOf(id buildID) (e2e.Profile, bool, bool) {
	var ps []e2e.Profile
	for _, p := range n.profiles {
		// A files profile is its own platform's trace, never the first turn (filesystem.go).
		if p.Harness == id.harness && p.Version == id.version && !isFilesScenario(p) {
			ps = append(ps, normalize(p))
		}
	}
	if len(ps) == 0 {
		return e2e.Profile{}, false, false
	}
	sort.SliceStable(ps, func(i, j int) bool {
		if (ps[i].Platform == "darwin/arm64") != (ps[j].Platform == "darwin/arm64") {
			return ps[i].Platform == "darwin/arm64"
		}
		return ps[i].Scenario < ps[j].Scenario
	})
	// The first turn published is the one taken behind the recording proxy (the egress
	// scenario's), where every request but the model's is answered empty and the build runs on
	// the feature flags it ships with: the plain turn reaches the vendor's live flags, which a
	// server-side rollout changes from one night to the next. The plain turn's is kept as the
	// live-flags capture. Without a whole proxied turn, the plain turn's is the first turn.
	first := -1
	for i, p := range ps {
		if p.Platform == ps[0].Platform && strings.HasSuffix(p.Scenario, "ProfileEgress") && !p.Failed && hasFirstTurn(p) {
			first = i
			break
		}
	}
	src := first
	for i, p := range ps {
		if src < 0 && p.Platform == ps[0].Platform && hasFirstTurn(p) {
			src = i
		}
	}
	merged := ps[0]
	merged.Requests = nil
	for i, p := range ps {
		if p.Platform != merged.Platform {
			continue
		}
		for _, r := range p.Requests {
			if r.Probe == "first-turn" {
				switch {
				case i == src:
				case first >= 0 && strings.HasSuffix(p.Scenario, "Profile"):
					r.Probe = "live-flags"
				default:
					continue // another scenario's turn of its own (its telemetry's)
				}
			}
			merged.Requests = append(merged.Requests, r)
		}
		if i == 0 {
			continue
		}
		merged.Egress = append(merged.Egress, p.Egress...)
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
		for _, note := range p.Notes {
			if !slices.Contains(merged.Notes, note) {
				merged.Notes = append(merged.Notes, note)
			}
		}
	}
	// The command published is the one that took the turn published, not a probe's that sorts
	// before it (TestGeminiProbeHooks before TestGeminiProfileEgress).
	if src >= 0 && len(ps[src].Command) > 0 {
		merged.Command = ps[src].Command
	}
	preferBinaryScenario(&merged, ps)
	// A probe that failed says nothing of the first turn: only the turn published makes the
	// capture's sections whole or truncated (a failed probe leaves it partial: failedScenario).
	merged.Failed = src >= 0 && ps[src].Failed
	return merged, first >= 0, true
}

// tonight is the snapshot of a build from the night's evidence alone.
func (n night) tonight(id buildID) *Snapshot {
	s := &Snapshot{SchemaVersion: schemaVersion, Harness: id.harness, Version: id.version, CapturedAt: n.now}
	if p, proxied, ok := n.profileOf(id); ok {
		s.Platform = p.Platform
		s.Capture = Capture{Command: p.Command, Partial: p.Failed, Truncated: p.Failed,
			Notes: []string{"Model requests were answered by a local fixture provider, not the vendor's API; nothing was sent to a model."}}
		if proxied {
			s.Capture.Notes = append(s.Capture.Notes, "The first turn ran behind a local proxy that answers every other request empty: "+
				"the build used the feature flags it ships with. What live flags changed is under live.")
		}
		// How the scenario ran the harness where it could not run it as a user does (a desktop
		// app, driven without a person at it).
		s.Capture.Notes = append(s.Capture.Notes, p.Notes...)
		deriveRequestSections(s, p)
		deriveBehavior(s, p)
		deriveCLI(s, p)
		deriveOutOfBand(s, p)
		n.deriveSourceConfig(s)
		deriveTelemetryProbe(s, p)
	}
	n.deriveFilesystem(s, id)
	n.deriveTelemetry(s)
	n.deriveSupport(s)
	scrubMachine(s)
	if len(present(s)) == 0 && slices.Contains(n.attempted, id) {
		s.Capture = Capture{Partial: true, Notes: []string{failedNote}}
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
	var caps []refdiff.Capability
	pass, fail := 0, 0
	coreFailed := false
	// A desktop app's capability checks run the agent build the app runs, launched as the app
	// launches it (TestRelayClaudeDesktop runs the Claude Code Desktop pins): they are recorded by
	// that build's version, and join each release of the app that runs it. A snapshot of an app
	// that says no agent (published before the app's own profile) is the agent build's.
	version := s.Version
	if agentOf(s.Harness) != s.Harness {
		if v := agentVersion(s); v != "" {
			version = v
		}
	}
	for _, h := range n.history {
		if h.Harness != s.Harness || h.Version != version {
			continue
		}
		caps = append(caps, refdiff.Capability{ID: h.Capability, Result: h.Result, Platform: h.Platform, Label: n.labels[h.Capability], Why: h.Why})
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
	sort.SliceStable(caps, func(i, j int) bool { return caps[i].ID+caps[i].Platform < caps[j].ID+caps[j].Platform })
	s.Support = &refdiff.Support{Status: verdict(pass, fail, coreFailed), Capabilities: caps}
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
		if tonight.Capture.Truncated {
			// As below: a turn that failed publishes its census alone (withoutProbe).
			out.Telemetry, _ = withoutProbe(tonight.Telemetry).(map[string]any)
		}
		out.Capture.Sections = map[string]time.Time{}
		for _, sec := range present(&out) {
			out.Capture.Sections[sec] = now
		}
		return &out
	}
	out := *old
	if out.Capture.Sections == nil {
		out.Capture.Sections = map[string]time.Time{}
	}
	keepWhole := tonight.Capture.Truncated && !old.Capture.Truncated
	ot, tt := out.Tree(), tonight.Tree()
	if tt["filesystem"] != nil {
		tt["filesystem"] = mergeFilesystem(ot["filesystem"], tt["filesystem"])
	}
	if tt["telemetry"] != nil {
		tel := tt["telemetry"]
		if tonight.Capture.Truncated {
			// The probe's findings come from the profile's turn, which failed: a truncated probe
			// is published nowhere, over a whole page or a partial one. The census, which ran
			// apart, still counts.
			tel = withoutProbe(tel)
		}
		if tel == nil {
			tt["telemetry"] = ot["telemetry"] // nothing measured tonight: nothing replaced
		} else {
			tt["telemetry"] = mergeTelemetry(ot["telemetry"], tel)
		}
	}
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
		merged.Capture.Truncated = tonight.Capture.Truncated
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
	var out []string
	for _, sec := range sections {
		if sectionPresent(s, sec) {
			out = append(out, sec)
		}
	}
	return out
}

// sectionPresent reports whether s has section sec, as its JSON has it: a map or list that is
// not empty (they are left out empty), side_requests not null, support set. It reads the
// fields, not the snapshot's JSON: publishing asks it of every snapshot.
func sectionPresent(s *Snapshot, sec string) bool {
	switch sec {
	case "model":
		return len(s.Model) > 0
	case "prompt":
		return len(s.Prompt) > 0
	case "tools":
		return len(s.Tools) > 0
	case "caching":
		return len(s.Caching) > 0
	case "request":
		return len(s.Request) > 0
	case "side_requests":
		return s.SideRequests != nil
	case "behavior":
		return len(s.Behavior) > 0
	case "telemetry":
		return len(s.Telemetry) > 0
	case "hooks":
		return len(s.Hooks) > 0
	case "cli":
		return len(s.CLI) > 0
	case "config":
		return len(s.Config) > 0
	case "network":
		return len(s.Network) > 0
	case "storage":
		return len(s.Storage) > 0
	case "filesystem":
		return len(s.Filesystem) > 0
	case "binary":
		return len(s.Binary) > 0
	case "support":
		return s.Support != nil
	}
	return s.Tree()[sec] != nil
}

func hasFirstTurn(p e2e.Profile) bool {
	for _, r := range p.Requests {
		if r.Probe == "first-turn" {
			return true
		}
	}
	return false
}

// withoutProbe is telemetry section t without the probe's findings: its census, the surfaces with
// their keys alone; nil where t has no census, and so nothing left measured.
func withoutProbe(t any) any {
	m, _ := t.(map[string]any)
	if census, _ := refdiff.TelemetryMeasured(m); !census {
		return nil
	}
	out := maps.Clone(m)
	delete(out, "export")
	surfaces, _ := m["surfaces"].(map[string]any)
	kept := map[string]any{}
	for name, raw := range surfaces {
		s, _ := raw.(map[string]any)
		if keys, ok := s["keys"]; ok {
			kept[name] = map[string]any{"keys": keys}
		}
	}
	out["surfaces"] = kept
	return out
}

// mergeTelemetry is the telemetry section with what tonight measured over old's, and old's where
// tonight did not measure it (refdiff.TelemetryMeasured): a night with the census alone keeps
// the probe's export and metric types from before, and one with the probe alone (the frequent
// check) keeps every surface and its keys, and a surface tonight's probe did not see keeps the
// findings of the probe that did. Without this, a render with less evidence than the last took
// the published findings for gone.
func mergeTelemetry(old, tonight any) any {
	o, _ := old.(map[string]any)
	t, _ := tonight.(map[string]any)
	if o == nil || t == nil {
		return tonight
	}
	oc, op := refdiff.TelemetryMeasured(o)
	tc, tp := refdiff.TelemetryMeasured(t)
	keepKeys, keepProbe := oc && !tc, op && !tp
	if !keepKeys && !op {
		return tonight // old has nothing tonight did not measure
	}
	out := maps.Clone(t)
	if e, ok := o["export"]; ok && keepProbe {
		out["export"] = e
	}
	surfaces := map[string]any{}
	tonightSurfaces, _ := t["surfaces"].(map[string]any)
	oldSurfaces, _ := o["surfaces"].(map[string]any)
	for name, raw := range tonightSurfaces {
		// Which surfaces are sent is the census's to say, and it records a surface by its keys
		// alone: one the probe saw that the census did not (a metric without attributes) is
		// left out, as a night with both leaves it out (deriveTelemetry).
		if _, known := oldSurfaces[name]; keepKeys && !known {
			continue
		}
		s, _ := raw.(map[string]any)
		if s == nil {
			s = map[string]any{} // not an object: nothing measured of it, and never a nil map
		}
		surfaces[name] = maps.Clone(s)
	}
	for name, raw := range oldSurfaces {
		was, _ := raw.(map[string]any)
		s, ok := surfaces[name].(map[string]any)
		switch {
		case !ok && !keepKeys:
			continue // a surface the census did not see tonight is gone
		case !ok:
			// Only the probe ran tonight, and it sees fewer surfaces than the census: one it did
			// not see keeps all it had, its probe findings as well as its keys.
			surfaces[name] = maps.Clone(was)
			continue
		}
		if keys, ok := was["keys"]; ok && keepKeys {
			s["keys"] = keys
		}
		if !refdiff.Probed(s) {
			// Tonight's probe did not see the surface (or did not run): what its turn did not
			// make keeps the findings of the turn that did.
			for _, f := range refdiff.ProbeFields {
				if v, ok := was[f]; ok {
					s[f] = v
				}
			}
		}
	}
	out["surfaces"] = surfaces
	return out
}
