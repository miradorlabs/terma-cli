package servinghost

import "testing"

// Each provider's own hosts, its private-endpoint and sovereign ones included, are told
// apart; a look-alike or any other host is Other, and a URL with no host has no kind.
func TestOf(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]Kind{
		"https://api.anthropic.com":                                      Anthropic,
		"https://API.Anthropic.com.:443/v1":                              Anthropic,
		"https://api.openai.com/v1":                                      OpenAI,
		"https://bedrock-runtime.eu-west-1.amazonaws.com":                Bedrock,
		"https://bedrock-runtime-fips.us-gov-west-1.amazonaws.com":       Bedrock,
		"https://bedrock-runtime.cn-north-1.amazonaws.com.cn":            Bedrock,
		"https://bedrock.us-east-1.amazonaws.com":                        Bedrock,
		"https://vpce-0abc.bedrock-runtime.us-east-1.vpce.amazonaws.com": Bedrock,
		"https://bedrock-runtime.eu-central-1.amazonaws.eu":              Bedrock,
		"https://bedrock-mantle.us-east-1.api.aws/anthropic":             Bedrock,
		"https://aiplatform.googleapis.com":                              Vertex,
		"https://us-east5-aiplatform.googleapis.com/v1":                  Vertex,
		"https://aiplatform.europe-west4.rep.googleapis.com":             Vertex,
		"https://my-psc.p.googleapis.com":                                GooglePSC,
		"https://aws-external-anthropic.us-east-1.api.aws":               AnthropicAWS,
		"https://claude.googleapis.com":                                  AnthropicGoogleCloud,
		"https://claude.us-east5.rep.googleapis.com":                     AnthropicGoogleCloud,
		"https://claude.example.googleapis.com.evil.example":             Other,
		"https://my-resource.services.ai.azure.com/anthropic":            Azure,
		"https://my-resource.openai.azure.com/openai":                    Azure,
		"https://my-resource.cognitiveservices.azure.com":                Azure,
		"https://my-resource.openai.azure.us/openai":                     Azure,
		"https://api.anthropic.com.evil.example":                         Other,
		"https://bedrock-runtime.us-east-1.amazonaws.com.evil.example":   Other,
		"https://openai.azure.com.evil.example":                          Other,
		"https://apim.azure-api.net/anthropic":                           Other,
		"https://console.anthropic.com":                                  Other,
		"http://localhost:4000":                                          Other,
	} {
		if got, ok := Of(raw); !ok || got != want {
			t.Errorf("%q: got %v, %v; want %v", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"api.anthropic.com", "://", ""} {
		if got, ok := Of(raw); ok {
			t.Errorf("%q names no host, got %v", raw, got)
		}
	}
}
