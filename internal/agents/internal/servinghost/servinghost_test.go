package servinghost

import "testing"

func TestPrivate(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"":                                       "",
		"   ":                                    "",
		"not a url":                              "",
		"https://api.anthropic.com":              "",
		"https://API.Anthropic.com./v1":          "",
		"https://api.openai.com/v1":              "",
		"https://chatgpt.com/backend-api/codex":  "",
		"https://my-res.openai.azure.com/openai": "",
		"https://my-res.services.ai.azure.com/anthropic":                     "",
		"https://bedrock-runtime.us-east-1.amazonaws.com":                    "",
		"https://us-east5-aiplatform.googleapis.com":                         "",
		"https://cliproxy.mesh.mccue.party":                                  "private",
		"https://cliproxy.mesh.mccue.party/v1":                               "private",
		"http://localhost:4000":                                              "private",
		"http://127.0.0.1:8317/v1":                                           "private",
		"https://gw.azure-api.net/anthropic":                                 "private",
		"https://litellm-123.us-east-1.elb.amazonaws.com":                    "private",
		"https://api.anthropic.com.evil.example":                             "private",
		"https://notanthropic.com":                                           "private",
		"https://gateway.eastus.cloudapp.azure.com/v1":                       "private",
		"https://bedrock-proxy-1.us-east-1.elb.amazonaws.com":                "private",
		"https://abc.execute-api.us-east-1.amazonaws.com/bedrock":            "private",
		"https://vpce-0a1b-xyz.bedrock-runtime.us-east-1.vpce.amazonaws.com": "",
		"https://bedrock-runtime-fips.us-gov-west-1.amazonaws.com":           "",
		"https://aws-external-anthropic.us-east-1.api.aws":                   "",
		"https://claude.googleapis.com":                                      "",
	} {
		if got := Private(raw); got != want {
			t.Errorf("Private(%q) = %q, want %q", raw, got, want)
		}
	}
}
