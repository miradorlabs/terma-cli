//go:build !release

package config

// policyStubAllowed lets a development or test build read TERMA_POLICY_STUB.
const policyStubAllowed = true
