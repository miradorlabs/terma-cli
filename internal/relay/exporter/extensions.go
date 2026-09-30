package exporter

import (
	"context"
	"fmt"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// extension exports inside the agent without setting OTEL_* in its environment.
type extension struct {
	name      string
	display   string
	configure func(context.Context, Config) (Result, error)
}

func (e extension) Name() string         { return e.name }
func (e extension) DisplayName() string  { return e.display }
func (e extension) Tool() string         { return e.name }
func (e extension) Selections() []string { return []string{e.name} }
func (e extension) Configure(ctx context.Context, cfg Config) (Result, error) {
	return e.configure(ctx, cfg)
}

func headers(cfg Config) map[string]string {
	return map[string]string{"Authorization": "Bearer " + cfg.Token}
}

func piConfig(cfg Config) harness.PiConfig {
	return harness.PiConfig{Endpoint: cfg.Endpoint, Headers: headers(cfg),
		IncludePrompts: true, IncludeToolContent: true, HookCommand: cfg.HookCommand}
}

func configurePi(_ context.Context, cfg Config) (Result, error) {
	path, err := harness.WritePiExtension(piConfig(cfg))
	return Result{Paths: []string{path}}, err
}

func configureOmp(_ context.Context, cfg Config) (Result, error) {
	// omp's native exporter initializes before extensions; using its environment
	// would also configure every tool it runs. The extension exports from events.
	path, err := harness.WriteOmpRelayExtension(piConfig(cfg))
	return Result{Paths: []string{path}}, err
}

func configureGemini(_ context.Context, cfg Config) (Result, error) {
	// Gemini has no headers setting, so the token rides the endpoint path.
	settings, ext, err := harness.ConnectGeminiRelay(cfg.Endpoint+"/"+cfg.Token, cfg.HookCommand)
	return Result{Paths: []string{settings, ext}}, err
}

func configureDsh(_ context.Context, cfg Config) (Result, error) {
	path, err := harness.WriteDshPlugin(harness.DshConfig{Endpoint: cfg.Endpoint, Headers: headers(cfg),
		IncludePrompts: true, IncludeToolContent: true, HookCommand: cfg.HookCommand})
	return Result{Paths: []string{path}}, err
}

func configureHermes(ctx context.Context, cfg Config) (Result, error) {
	path, err := harness.WriteHermesPlugin(harness.HermesConfig{Endpoint: cfg.Endpoint, Headers: headers(cfg),
		IncludePrompts: true, IncludeToolContent: true, HookCommand: cfg.HookCommand})
	result := Result{Paths: []string{path}}
	if err != nil {
		return result, err
	}
	if err := harness.EnableHermesPlugin(ctx); err != nil {
		result.Pending = true
		result.Notes = append(result.Notes, fmt.Sprintf("Hermes: the plugin is written (%s) but not enabled: %v.", path, err))
	}
	return result, nil
}

var _ Exporter = extension{}
