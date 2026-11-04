// Package servinghost tells which provider serves a base URL's host: the model APIs' own
// hosts and, as Claude Code 2.1.314 recognises them (its own-host table), each cloud
// provider's regional, FIPS, private-endpoint and sovereign-cloud hosts. Each agent maps a
// Kind to terma.serving.backend's value.
package servinghost

import (
	"net/url"
	"regexp"
	"strings"
)

// Kind is who serves a host.
type Kind int

// The kinds of host; Other is a gateway, a proxy or a local server.
const (
	Other Kind = iota
	Anthropic
	OpenAI
	Bedrock
	Vertex
	Azure
)

const (
	awsRegion  = `[a-z]{2,4}(?:-[a-z]{2,5})?-[a-z]+-\d+`
	gcpRegion  = `[a-z]+(?:-[a-z]+\d+)?`
	awsDomains = `(?:amazonaws\.(?:com(?:\.cn)?|eu)|api\.aws|c2s\.ic\.gov|sc2s\.sgov\.gov|cloud\.adc-e\.uk|csp\.hci\.ic\.gov)`
)

var (
	bedrock = regexp.MustCompile(`^(?:vpce-[a-z0-9-]+\.)?bedrock(?:-runtime|-mantle)?(?:-fips)?\.` + awsRegion + `\.(?:vpce\.)?` + awsDomains + `$`)
	vertex  = regexp.MustCompile(`^(?:aiplatform|` + gcpRegion + `-aiplatform|aiplatform\.` + gcpRegion + `\.rep|[a-z0-9-]+\.p)\.googleapis\.com$`)
	// Foundry's and Azure OpenAI's resources.
	azure = regexp.MustCompile(`^[a-z0-9-]{2,64}\.(?:services\.ai|cognitiveservices|openai)\.azure\.(?:com|us|cn)$`)
)

// Of is the kind of raw's host; false when raw names no host.
func Of(raw string) (Kind, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return Other, false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	switch {
	case host == "":
		return Other, false
	case host == "api.anthropic.com":
		return Anthropic, true
	case host == "api.openai.com":
		return OpenAI, true
	case bedrock.MatchString(host):
		return Bedrock, true
	case vertex.MatchString(host):
		return Vertex, true
	case azure.MatchString(host):
		return Azure, true
	}
	return Other, true
}
