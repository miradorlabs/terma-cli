package cli

import "github.com/spf13/cobra"

type organization struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role,omitempty"`
}

type listOrganizationsResponse struct {
	Organizations []organization `json:"organizations"`
}

// pickOrganization prompts for one of orgs, current marked and kept by a bare Enter.
func pickOrganization(cmd *cobra.Command, orgs []organization, current string) (*organization, error) {
	labels := organizationKind.labels(orgs)
	return organizationKind.pick(cmd, orgs, func(o organization) pickRow {
		return pickRow{Label: labels[o.ID], Note: o.Role, Current: o.ID == current, Default: o.ID == current}
	})
}
