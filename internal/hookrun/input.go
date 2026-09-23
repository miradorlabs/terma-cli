package hookrun

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// maxHookInput bounds the payload a harness writes to a hook's stdin. Real payloads are
// a few kilobytes; the bound is there so a runaway writer cannot make a hook hold the
// agent up.
const maxHookInput = 4 << 20

// readHookInput decodes one harness payload into T. It reads one byte past the bound so
// an oversized payload is refused by name: a payload cut at the bound would fail to
// parse, or worse parse, and neither says what went wrong.
func readHookInput[T any](r io.Reader) (*T, error) {
	if r == nil {
		return nil, errors.New("no hook input")
	}
	data, err := io.ReadAll(io.LimitReader(r, maxHookInput+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxHookInput {
		return nil, errors.New("hook input too large")
	}
	var in T
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, fmt.Errorf("parse hook input: %w", err)
	}
	return &in, nil
}
