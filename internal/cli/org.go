package cli

import "github.com/spf13/cobra"

type organization struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role,omitempty"`
	// Type is "personal" for a person's own workspace and "team" for every other
	// organization, empty from a server that predates it. Only Type tells them apart: a
	// team may be named like the personal one.
	Type string `json:"type,omitempty"`
}

// note is what the picker says beside an organization: that it is the person's own
// workspace, else their role in it.
func (o organization) note() string {
	if o.Type == "personal" {
		return "personal"
	}
	return o.Role
}

type listOrganizationsResponse struct {
	Organizations []organization `json:"organizations"`
}

// pickOrganization prompts for one of orgs, current marked and kept by a bare Enter.
func pickOrganization(cmd *cobra.Command, orgs []organization, current string) (*organization, error) {
	labels := organizationKind.labels(orgs)
	return organizationKind.pick(cmd, orgs, func(o organization) pickRow {
		return pickRow{Label: labels[o.ID], Note: o.note(), Current: o.ID == current, Default: o.ID == current}
	})
}
