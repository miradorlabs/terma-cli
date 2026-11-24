package codex

import (
	"regexp"

	"github.com/miradorlabs/terma-cli/internal/agents/internal/tomledit"
)

// tomlFile is Codex's config.toml, edited by splicing only the `[otel]` table's text so
// hand-written comments, order and other tables survive byte for byte. A splice that
// changes how anything outside `[otel]` reads is refused, not written.
type tomlFile = tomledit.File

const otelTable = "otel"

// anyHeaderRE matches a table header line.
var anyHeaderRE = regexp.MustCompile(`^\s*\[`)

func loadTOML(path string) (*tomlFile, error) { return tomledit.Load(path, otelTable) }

func renderTOMLValue(v any) (string, error) { return tomledit.Render(v) }

func parseTOMLValue(text string) (any, error) { return tomledit.Parse(text) }

func quoteTOMLKey(k string) string { return tomledit.QuoteKey(k) }

func quoteTOMLString(s string) string { return tomledit.QuoteString(s) }

func tomlTypeName(v any) string { return tomledit.TypeName(v) }

// unmarshalTOMLLenient relies on go-toml ignoring keys the struct does not name: the
// files hold far more than the otel table.
func unmarshalTOMLLenient(data []byte, v any) error { return tomledit.Unmarshal(data, v) }
