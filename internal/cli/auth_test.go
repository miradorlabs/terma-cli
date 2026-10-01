package cli

import (
	"testing"

	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
)

func TestApplyLoginUpdatesOrganization(t *testing.T) {
	p := &config.Profile{OrganizationID: "org-old", OrganizationName: "Old"}
	applyLogin(p, &auth.Credential{OrganizationID: "org-new"}, "New Org")
	if p.OrganizationID != "org-new" || p.OrganizationName != "New Org" {
		t.Fatalf("organization not updated: %+v", p)
	}
}
