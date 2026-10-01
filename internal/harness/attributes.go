package harness

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// The resource attributes Terma stamps on what an agent emits.
const (
	// AttrServiceName is set explicitly so a documented filter survives a changed agent default.
	AttrServiceName = "service.name"

	// AttrEnduserID attributes a trace to a person, from git's email.
	AttrEnduserID = "enduser.id"

	// AttrProjectID records the project an agent was connected against, where its
	// config can carry resource attributes; otherwise the connect journal holds it.
	AttrProjectID = "mirador.project.id"
)

// ServiceNamer is an agent that reports under a service.name other than its own name.
type ServiceNamer interface {
	ServiceName() string
}

// ServiceName is the service.name h reports under.
func ServiceName(h Harness) string {
	if n, ok := h.(ServiceNamer); ok {
		return n.ServiceName()
	}
	return h.Name()
}

// GitEmail returns git's global user.email, or "": it labels a machine-wide config, so a
// checkout's local address would be stamped on every other directory's sessions.
func GitEmail(ctx context.Context) string {
	path, err := exec.LookPath("git")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, path, "config", "--global", "--get", "user.email").Output()
	if err != nil {
		// No fallback to the local value: a wrong owner is worse than none.
		return ""
	}
	return strings.TrimSpace(string(out))
}
