package harness

// The resource attributes Terma stamps on what an agent emits.
const (
	// AttrServiceName is set explicitly so a documented filter survives a changed agent default.
	AttrServiceName = "service.name"

	// AttrEnduserID attributes a trace to a person, from git's email.
	AttrEnduserID = "enduser.id"
)
