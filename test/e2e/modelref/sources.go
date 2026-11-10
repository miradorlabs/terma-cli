package main

import (
	"context"
	"strings"
	"time"
)

// Where the reference reads what it says of a model. A vendor's own sources come first: its API
// (where a key is configured) and its documentation, which is also what decides which models
// the reference follows, the ones the vendor lists. Two public catalogs (models.dev, OpenRouter)
// fill the facts a vendor's pages leave out, and the reference says where they disagree.

// observation is what one source says of one model.
type observation struct {
	source string
	vendor string
	id     string // the model's id as the source writes it
	url    string
	// listed says the source is the vendor's own list of its models (its docs' index, its API):
	// a model the vendor lists is one the reference follows.
	listed  bool
	facts   map[string]any
	docs    map[string]Page
	listing map[string]any // the vendor API's own object of the model
}

// sourceRank orders sources by how far the reference trusts what they say: a fact takes the
// value of the first source in this order that states it.
func sourceRank(source string) int {
	switch {
	case strings.HasSuffix(source, "-api"):
		return 0
	case strings.HasSuffix(source, "-docs"):
		return 1
	case source == "models.dev":
		return 2
	case source == "openrouter":
		return 3
	}
	return 4
}

// vendor is a company whose models the reference follows, and how to read each of its sources.
type vendor struct {
	id, name string
	docsURL  string // the page a person reads for its models
	// changelogURL is its API changelog, which notes are cut from (changelog.go).
	changelogURL string
	changelogMD  string // the same as Markdown
	// changelogDate reads a changelog heading as a date, "" for a heading that is none.
	changelogDate func(heading string, level int, st *dateState) string
	// openrouter and modelsdev are the vendor's id in each catalog.
	openrouter, modelsdev string
	// keyEnv is the variable its API key is read from.
	keyEnv string
	docs   func(ctx context.Context, c *client, now time.Time) ([]observation, error)
	api    func(ctx context.Context, c *client, key string) ([]observation, error)
	probe  func(ctx context.Context, c *client, key string, m *Snapshot) *Probes
}

// vendors are every vendor the reference follows, in the order it lists them.
var vendors = []*vendor{anthropic, openAI, google, xAI}

func vendorByID(id string) *vendor {
	for _, v := range vendors {
		if v.id == id {
			return v
		}
	}
	return nil
}

// Fact names. Counts are int64 tokens, dates YYYY-MM-DD (or YYYY-MM where a source names no
// day), lists lowercase words.
const (
	factName              = "name"
	factReleasedAt        = "released_at"
	factKnowledgeCutoff   = "knowledge_cutoff"
	factTrainingCutoff    = "training_cutoff"
	factContextWindow     = "context_window"
	factMaxInput          = "max_input"
	factMaxOutput         = "max_output"
	factInputModalities   = "input_modalities"
	factOutputModalities  = "output_modalities"
	factReasoning         = "reasoning"
	factReasoningEfforts  = "reasoning_efforts"
	factDefaultEffort     = "default_effort"
	factThinking          = "thinking"
	factToolCalling       = "tool_calling"
	factStructuredOutputs = "structured_outputs"
	factSampling          = "sampling_parameters"
	factOpenWeights       = "open_weights"
	factStatus            = "status"
	factDeprecatedAt      = "deprecated_at"
	factRetiresAt         = "retires_at"
	factRetiresNotBefore  = "retires_not_before"
	factAliases           = "aliases"
	factSnapshots         = "snapshots"
	factPlatformIDs       = "platform_ids"
	factEndpoints         = "endpoints"
	factFeatures          = "features"
	factUnsupported       = "unsupported_features"
	factTools             = "tools"
	factRegions           = "regions"
	factBatch             = "batch"
	factLatency           = "latency"
	factDescription       = "description"
)

// factOrder is the order facts are compared and shown in.
var factOrder = []string{factName, factDescription, factStatus, factReleasedAt, factKnowledgeCutoff, factTrainingCutoff,
	factContextWindow, factMaxInput, factMaxOutput, factInputModalities, factOutputModalities, factReasoning, factThinking,
	factReasoningEfforts, factDefaultEffort, factToolCalling, factStructuredOutputs, factSampling, factOpenWeights,
	factFeatures, factUnsupported, factTools, factEndpoints, factBatch, factLatency, factRegions, factAliases, factSnapshots,
	factPlatformIDs, factDeprecatedAt, factRetiresAt, factRetiresNotBefore}

// setFact puts v in facts unless it is empty: a source that does not state a fact leaves it
// unknown, never zero.
func setFact(facts map[string]any, key string, v any) {
	switch x := v.(type) {
	case nil:
		return
	case string:
		if strings.TrimSpace(x) == "" {
			return
		}
	case []string:
		if len(x) == 0 {
			return
		}
	case map[string]string:
		if len(x) == 0 {
			return
		}
	}
	facts[key] = v
}
