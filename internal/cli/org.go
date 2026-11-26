package cli

type organization struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role,omitempty"`
	// Type is "personal" for a person's own workspace and "shared" for every other
	// organization, empty from a server that predates it. Only Type tells them apart: a
	// shared organization may be named like the personal one.
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
