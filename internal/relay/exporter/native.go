package exporter

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

type native struct{ harness.Harness }

func (n native) Tool() string         { return n.Name() }
func (n native) Selections() []string { return []string{n.Name()} }
func (n native) Configure(_ context.Context, cfg Config) (Result, error) {
	return Result{}, n.Connect(harness.Exporter{
		Endpoint: cfg.Endpoint, APIKey: cfg.Token, Signals: harness.AllSignals,
		IncludePrompts: true, IncludeToolContent: true,
	}, true)
}

type claude struct{ native }

func (claude) Tool() string { return "claude-code" }

var (
	_ Exporter = native{}
	_ Exporter = claude{}
)
