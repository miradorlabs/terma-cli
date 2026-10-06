package harness

// Scope is where a connect writes: global holds the endpoint and credential, local only a
// repository's committed policy of what to ship.
type Scope string

// The two layers a connect can write.
const (
	ScopeGlobal Scope = "global"
	ScopeLocal  Scope = "local"
)
