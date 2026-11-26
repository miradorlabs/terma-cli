package doctor

import (
	"testing"

	"github.com/miradorlabs/terma-cli/internal/remediation"
)

// A campaign's notice leads doctor's context, also without a configuration, and says
// nothing when it asks nothing of this machine.
func TestRemediationRowsLeadTheContext(t *testing.T) {
	t.Parallel()
	c := &remediation.Campaign{Title: "Update terma", Steps: []string{"terma update"}, MinimumClientVersion: "0.43.0"}
	rows := Context(Env{Remediation: remediation.Evaluate(c, "0.41.2", sessionOK)})
	if len(rows) != 3 || rows[0].Label != "Required" || rows[1] != (Row{Value: "Update terma"}) || rows[2] != (Row{Value: "1. terma update"}) {
		t.Fatalf("rows without a configuration = %+v", rows)
	}
	if rows := RemediationRows(remediation.Evaluate(c, "0.43.0", sessionOK)); rows != nil {
		t.Fatalf("a current machine got %+v", rows)
	}
	if rows := Context(Env{}); rows != nil {
		t.Fatalf("no configuration and no campaign = %+v", rows)
	}
}

func sessionOK() bool { return true }
