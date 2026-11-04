// Package servinghost recognises the hosts each cloud provider serves models from, as Claude
// Code 2.1.314 does (its own-host table): regional, FIPS, private-endpoint and sovereign-cloud
// hosts included. Any other host is a gateway, a proxy or a local server.
package servinghost

import "regexp"

const (
	awsRegion  = `[a-z]{2,4}(?:-[a-z]{2,5})?-[a-z]+-\d+`
	gcpRegion  = `[a-z]+(?:-[a-z]+\d+)?`
	awsDomains = `(?:amazonaws\.(?:com(?:\.cn)?|eu)|api\.aws|c2s\.ic\.gov|sc2s\.sgov\.gov|cloud\.adc-e\.uk|csp\.hci\.ic\.gov)`
)

var (
	bedrock = regexp.MustCompile(`^(?:vpce-[a-z0-9-]+\.)?bedrock(?:-runtime|-mantle)?(?:-fips)?\.` + awsRegion + `\.(?:vpce\.)?` + awsDomains + `$`)
	vertex  = regexp.MustCompile(`^(?:aiplatform|` + gcpRegion + `-aiplatform|aiplatform\.` + gcpRegion + `\.rep|[a-z0-9-]+\.p)\.googleapis\.com$`)
	azure   = regexp.MustCompile(`^[a-z0-9-]{2,64}\.(?:services\.ai|cognitiveservices|openai)\.azure\.(?:com|us|cn)$`)
)

// Bedrock reports whether host is Amazon Bedrock's, its Mantle endpoint's included.
func Bedrock(host string) bool { return bedrock.MatchString(host) }

// Vertex reports whether host is Google Vertex AI's.
func Vertex(host string) bool { return vertex.MatchString(host) }

// Azure reports whether host is an Azure AI resource's: Foundry's and Azure OpenAI's.
func Azure(host string) bool { return azure.MatchString(host) }
