package cli

type organization struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role,omitempty"`
}

type listOrganizationsResponse struct {
	Organizations []organization `json:"organizations"`
}
