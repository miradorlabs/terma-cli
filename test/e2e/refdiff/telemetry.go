package refdiff

import "maps"

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
	m := measuredByBoth{keys: ac && bc, probe: ap && bp, sameSurfaces: ac == bc}
	return m.strip(a, b), m.strip(b, a)
}

// measuredByBoth is what two captures both measured: the census's keys, the probe's findings,
// and whether they saw the same surfaces (both took the census, or neither did).
type measuredByBoth struct{ keys, probe, sameSurfaces bool }

// strip is a copy of telemetry t without what m says t's capture alone measured, beside other's.
func (m measuredByBoth) strip(t, other map[string]any) map[string]any {
	out := maps.Clone(t)
	if !m.probe {
		delete(out, "export")
	} else if e, ok := t["export"].(map[string]any); ok {
		o, _ := other["export"].(map[string]any)
		out["export"] = comparableExport(e, o)
	}
	surfaces, _ := t["surfaces"].(map[string]any)
	if surfaces == nil {
		return out
	}
	others, _ := other["surfaces"].(map[string]any)
	kept := map[string]any{}
	for name, raw := range surfaces {
		if _, both := others[name]; m.sameSurfaces || both {
			s, _ := raw.(map[string]any)
			kept[name] = m.stripSurface(s)
		}
	}
	out["surfaces"] = kept
	return out
}

// stripSurface is a copy of surface s with only what both captures measured.
func (m measuredByBoth) stripSurface(s map[string]any) map[string]any {
	c := maps.Clone(s)
	if !m.keys {
		delete(c, "keys")
	}
	if !m.probe {
		for _, f := range ProbeFields {
			delete(c, f)
		}
	}
	return c
}

// minIntervalBatches is how many exports of a signal its measured interval must rest on to be
// compared: one batch's wait carries any stall of the build's own, so an interval from one
// batch moves from run to run.
const minIntervalBatches = 2

// comparableExport is a copy of export e without what is the run's rather than the build's:
// how many batches arrived (interval_batches), and a signal's interval where e or other measured
// it from fewer than minIntervalBatches.
func comparableExport(e, other map[string]any) map[string]any {
	out := maps.Clone(e)
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
	// An interval neither side measured from enough batches is not observed: no interval_ms at
	// all, as on a side that measured none, and never an emptied one that reads as removed.
	if len(kept) == 0 {
		delete(out, "interval_ms")
	} else {
		out["interval_ms"] = kept
	}
	return out
}
