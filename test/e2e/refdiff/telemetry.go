package refdiff

// What a capture measured of a build's own telemetry: its field census (every surface the build
// sent, each with its keys) and its telemetry probe (the export's cadence, and each surface's
// metric type, unit and temporality or span parents). A capture can hold one without the other:
// the frequent check runs the probe alone, and an older capture predates the probe. So two
// builds' telemetry is compared only in the parts both captures measured. What one did not
// measure is not observed there: never a surface "no longer sent", nor an export "added".

// ProbeFields are what the telemetry probe adds to a surface.
var ProbeFields = []string{"type", "unit", "temporality", "monotonic", "parent", "parents", "root"}

// TelemetryMeasured says whether t holds a field census (a surface with its keys) and a probe's
// findings (the export, or a surface's probe fields).
func TelemetryMeasured(t map[string]any) (census, probe bool) {
	_, probe = t["export"]
	surfaces, _ := t["surfaces"].(map[string]any)
	for _, raw := range surfaces {
		s, _ := raw.(map[string]any)
		if _, ok := s["keys"]; ok {
			census = true
		}
		for _, f := range ProbeFields {
			if _, ok := s[f]; ok {
				probe = true
			}
		}
	}
	return census, probe
}

// comparableTelemetry are copies of a's and b's telemetry sections holding only what both
// captures measured: the census's keys where both have a census, the probe's findings where
// both ran the probe, and the surfaces both have where they measured them differently (the
// census sees every surface, the probe alone fewer).
func comparableTelemetry(a, b map[string]any) (map[string]any, map[string]any) {
	ac, ap := TelemetryMeasured(a)
	bc, bp := TelemetryMeasured(b)
	sameSurfaces := ac == bc
	strip := func(t, other map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range t {
			out[k] = v
		}
		if !(ap && bp) {
			delete(out, "export")
		} else if e, ok := t["export"].(map[string]any); ok {
			other, _ := other["export"].(map[string]any)
			out["export"] = comparableExport(e, other)
		}
		surfaces, _ := t["surfaces"].(map[string]any)
		others, _ := other["surfaces"].(map[string]any)
		if surfaces == nil {
			return out
		}
		kept := map[string]any{}
		for name, raw := range surfaces {
			if _, both := others[name]; !sameSurfaces && !both {
				continue
			}
			s, _ := raw.(map[string]any)
			c := map[string]any{}
			for k, v := range s {
				c[k] = v
			}
			if !(ac && bc) {
				delete(c, "keys")
			}
			if !(ap && bp) {
				for _, f := range ProbeFields {
					delete(c, f)
				}
			}
			kept[name] = c
		}
		out["surfaces"] = kept
		return out
	}
	return strip(a, b), strip(b, a)
}

// minIntervalBatches is how many exports of a signal its measured interval must rest on to be
// compared: one batch's wait carries any stall of the build's own, so an interval from one
// batch moves from run to run.
const minIntervalBatches = 2

// comparableExport is a copy of export e without what is the run's rather than the build's:
// how many batches arrived (interval_batches), and a signal's interval where e or other measured
// it from fewer than minIntervalBatches.
func comparableExport(e, other map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range e {
		out[k] = v
	}
	delete(out, "interval_batches")
	intervals, _ := e["interval_ms"].(map[string]any)
	if intervals == nil {
		return out
	}
	batches := func(x map[string]any, signal string) float64 {
		b, _ := x["interval_batches"].(map[string]any)
		n, _ := b[signal].(float64)
		return n
	}
	kept := map[string]any{}
	for signal, v := range intervals {
		if batches(e, signal) >= minIntervalBatches && batches(other, signal) >= minIntervalBatches {
			kept[signal] = v
		}
	}
	out["interval_ms"] = kept
	return out
}
