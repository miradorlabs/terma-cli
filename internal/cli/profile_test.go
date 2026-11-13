package cli

import (
	"testing"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// selected is the organization and team the profile records, and how many sign-ins are stored.
func selected(t *testing.T) (org, team string, signIns int) {
	t.Helper()
	file, err := config.LoadFile(testApp.dir)
	if err != nil {
		t.Fatal(err)
	}
	if p := file.Profiles[config.DefaultProfile]; p != nil {
		org, team = p.OrganizationID, p.Team
	}
	creds, _ := auth.Credentials(testApp.dir, config.DefaultProfile)
	return org, team, len(creds)
}
