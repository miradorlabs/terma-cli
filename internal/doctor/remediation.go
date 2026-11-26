package doctor

import "github.com/miradorlabs/terma-cli/internal/remediation"

// RemediationRows are a remediation campaign's notice among doctor's context: what it
// requires and the commands that do it, then the campaign's title and steps. None when it
// asks nothing of this machine. Context rows never count as a failed check.
func RemediationRows(n remediation.Notice) []Row {
	if !n.Warn() {
		return nil
	}
	rows := []Row{{"Required", n.Summary()}}
	for _, line := range n.Lines() {
		rows = append(rows, Row{Value: line})
	}
	return rows
}
