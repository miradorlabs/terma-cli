package hookrun

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
)

// MaxInput bounds a hook's stdin payload, so a runaway writer cannot hold the agent up.
const MaxInput = 4 << 20

// ReadInput decodes one payload into T, refusing an oversized one by name rather than parsing a cut one.
func ReadInput[T any](r io.Reader) (*T, error) {
	if r == nil {
		return nil, errors.New("no hook input")
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxInput+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxInput {
		return nil, errors.New("hook input too large")
	}
	var in T
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, fmt.Errorf("parse hook input: %w", err)
	}
	return &in, nil
}

// JSONNumber reads a JSON count: the value, whether one was present, and whether it is
// finite, non-negative, exact as a float64 and whole when integer is set.
func JSONNumber(raw json.RawMessage, integer bool) (float64, bool, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false, false
	}
	var value float64
	if json.Unmarshal(raw, &value) != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 9007199254740991 || (integer && math.Trunc(value) != value) {
		return 0, true, false
	}
	return value, true, true
}
