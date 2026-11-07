package agents

import (
	"slices"
)

// Processes is an agent the opt-in sensor can recognise by its own process: the sensor
// starts a session at that process's exec and follows everything it starts.
type Processes interface {
	Agent
	// ProcessNames are the base names of the agent's executables, or of the script an
	// interpreter runs for it (an npm install runs `node …/<name>`).
	ProcessNames() []string
}

// ProcessNames is every name a known agent's process goes by, sorted.
func (r *Registry) ProcessNames() []string {
	var out []string
	for _, a := range r.With[Processes]() {
		for _, n := range a.ProcessNames() {
			if !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	slices.Sort(out)
	return out
}
