package config

// A profile is one sign-in's selections: its organizations, the team selected in each,
// and the agents recorded. Several profiles share one installation (`terma config`), and
// one machine collects for every team any of them selected.

// Profile is the non-secret half of a profile: where to talk to and what is selected.
type Profile struct {
	// Environment pins a hidden built-in environment; empty means production.
	Environment string `json:"environment,omitempty"`
	// Endpoint overrides; empty means the environment's defaults.
	APIURL           string `json:"api_url,omitempty"`
	AuthURL          string `json:"auth_url,omitempty"`
	AppURL           string `json:"app_url,omitempty"`
	OTLPURL          string `json:"otlp_url,omitempty"`
	OrganizationID   string `json:"organization_id,omitempty"`
	OrganizationName string `json:"organization_name,omitempty"`
	// Harnesses lists the agents and launch surfaces `terma setup` recorded; a preference, not a connection.
	Harnesses []string `json:"harnesses,omitempty"`
	// Team is the team `terma setup` selected, whose policy (PoliciesDir) the hooks apply.
	Team string `json:"team,omitempty"`
	// Teams is the team selected in each organization this profile set up, by
	// organization id: what this machine collects for (routing.Collection). Team is the
	// current organization's entry.
	Teams map[string]string `json:"teams,omitempty"`
}

// SelectOrganization records the account scope, never a repository's project. Switching
// organization keeps the team selected in the one left, so the machine goes on collecting
// for it, and brings back the team selected in the new one before, if any.
func (p *Profile) SelectOrganization(id, name string) {
	if p.OrganizationID != id {
		p.SelectTeam(p.Team) // a profile from before Teams were recorded has its team here alone
		p.OrganizationName = ""
		p.Team = p.Teams[id]
	}
	p.OrganizationID = id
	if name != "" {
		p.OrganizationName = name
	}
}

// SelectTeam records team as the current organization's, replacing the one selected
// there before: one team per organization is collected for.
func (p *Profile) SelectTeam(team string) {
	p.Team = team
	if p.OrganizationID == "" || team == "" {
		return
	}
	if p.Teams == nil {
		p.Teams = map[string]string{}
	}
	p.Teams[p.OrganizationID] = team
}

// Collects reports whether team is selected in some organization of this profile.
func (p *Profile) Collects(team string) bool {
	if team == "" {
		return false
	}
	if p.Team == team && p.OrganizationID != "" {
		return true
	}
	for org, t := range p.Teams {
		if org != "" && t == team {
			return true
		}
	}
	return false
}

// CollectedTeams is the team selected in each organization, the current one's included:
// a profile from before Teams were recorded has its one.
func (p *Profile) CollectedTeams() map[string]string {
	teams := make(map[string]string, len(p.Teams)+1)
	for org, team := range p.Teams {
		if org != "" && team != "" {
			teams[org] = team
		}
	}
	if p.OrganizationID != "" && p.Team != "" {
		teams[p.OrganizationID] = p.Team
	}
	return teams
}
