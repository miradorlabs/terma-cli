package cmd

import (
	"testing"

	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// Login discards legacy machine-wide project selections; repositories own them.
func TestApplyLoginClearsLegacyProject(t *testing.T) {
	t.Run("different org clears the remembered project", func(t *testing.T) {
		p := &config.Profile{
			OrganizationID: "org-old",
			ProjectID:      "proj-old",
			ProjectName:    "Old Project",
		}
		applyLogin(p, &auth.Credential{OrganizationID: "org-new"}, "New Org")

		if p.ProjectID != "" || p.ProjectName != "" {
			t.Errorf("project should be cleared on org change, got %q/%q", p.ProjectID, p.ProjectName)
		}
		if p.OrganizationID != "org-new" || p.OrganizationName != "New Org" {
			t.Errorf("organization should be updated, got %q/%q", p.OrganizationID, p.OrganizationName)
		}
	})

	t.Run("same org clears the legacy project", func(t *testing.T) {
		p := &config.Profile{
			OrganizationID: "org-1",
			ProjectID:      "proj-1",
			ProjectName:    "Project One",
		}
		applyLogin(p, &auth.Credential{OrganizationID: "org-1"}, "Org One")

		if p.ProjectID != "" || p.ProjectName != "" {
			t.Errorf("legacy project should be cleared at login, got %q/%q", p.ProjectID, p.ProjectName)
		}
	})

	t.Run("first login into an empty profile sets the org", func(t *testing.T) {
		p := &config.Profile{}
		applyLogin(p, &auth.Credential{OrganizationID: "org-1"}, "Org One")

		if p.OrganizationID != "org-1" || p.OrganizationName != "Org One" {
			t.Errorf("organization should be recorded, got %q/%q", p.OrganizationID, p.OrganizationName)
		}
	})
}
