//go:build release

package config

// policyStubAllowed keeps TERMA_POLICY_STUB out of a release build: whatever sets terma's
// environment there cannot stand in for the team's policy.
const policyStubAllowed = false
