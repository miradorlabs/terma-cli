// Package servinghost tells whether an agent's base URL names an endpoint terma does not
// recognise: a gateway, a proxy or a local model server, rather than a model API's own host
// or a named cloud's.
package servinghost

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// Private is terma.serving.backend for calls sent to raw: private when raw names a host that
// is not a model API's own (Anthropic's, OpenAI's) or a named cloud's (Bedrock, Vertex,
// Azure), "" when it is one of those, or raw is empty or names no host. Never the host itself.
func Private(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" || recognised(host) {
		return ""
	}
	return semconv.TermaServingBackendPrivate
}

// recognised reports a model API's own host or a named cloud's.
func recognised(host string) bool {
	under := func(domains ...string) bool {
		for _, d := range domains {
			if host == d || strings.HasSuffix(host, "."+d) {
				return true
			}
		}
		return false
	}
	switch {
	case under("anthropic.com", "openai.com", "chatgpt.com"):
		return true
	// Vertex AI, Claude on Google Cloud and their Private Service Connect endpoints.
	case under("googleapis.com"):
		return true
	// Foundry's and Azure OpenAI's resources. A customer's own host on Azure (a VM's
	// cloudapp.azure.com, API Management's azure-api.net) is a gateway.
	case under(azureModelDomains...):
		return true
	// Bedrock's and Claude Platform on AWS's endpoints, named by their service; anything else on
	// AWS (a load balancer, API Gateway) is the customer's own.
	case under("amazonaws.com", "amazonaws.com.cn", "api.aws") && awsModelService.MatchString(host):
		return true
	}
	return false
}

// azureModelDomains are where Azure serves its model resources.
var azureModelDomains = []string{
	"openai.azure.com", "services.ai.azure.com", "cognitiveservices.azure.com", "models.ai.azure.com",
	"openai.azure.us", "services.ai.azure.us", "cognitiveservices.azure.us",
	"openai.azure.cn", "services.ai.azure.cn", "cognitiveservices.azure.cn",
}

// awsModelService is a host whose service label is Bedrock's or Claude Platform on AWS's,
// behind an optional VPC endpoint id.
var awsModelService = regexp.MustCompile(`^(?:vpce-[a-z0-9-]+\.)?(?:bedrock(?:-runtime|-mantle|-agent-runtime)?|aws-external-anthropic)(?:-fips)?\.`)
