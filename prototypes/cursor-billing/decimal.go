package cursorbilling

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// Decimal retains the provider's decimal precision. Monetary fields are cents,
// including fractional cents. JSON output uses strings to avoid float rounding.
// A nil *Decimal is unknown. Raw records preserve absent versus explicit null.
type Decimal string

var decimalPattern = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

func (d Decimal) rat() (*big.Rat, error) {
	s := string(d)
	if len(s) > 128 || !decimalPattern.MatchString(s) {
		return nil, fmt.Errorf("invalid decimal")
	}
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		e, err := strconv.Atoi(s[i+1:])
		if err != nil || e < -30 || e > 30 {
			return nil, fmt.Errorf("decimal exponent out of range")
		}
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("invalid decimal")
	}
	return r, nil
}
func (d *Decimal) UnmarshalJSON(b []byte) error {
	s := string(b)
	if len(b) > 0 && b[0] == '"' {
		if json.Unmarshal(b, &s) != nil {
			return fmt.Errorf("invalid decimal")
		}
	}
	value := Decimal(s)
	if _, err := value.rat(); err != nil {
		return err
	}
	*d = value
	return nil
}
func (d Decimal) MarshalJSON() ([]byte, error) {
	if _, err := d.rat(); err != nil {
		return nil, err
	}
	return json.Marshal(string(d))
}

// exactDecimal represents rational sums of finite decimal inputs without rounding.
func exactDecimal(r *big.Rat) Decimal {
	denom := new(big.Int).Set(r.Denom())
	two, five := big.NewInt(2), big.NewInt(5)
	scale := 0
	for _, prime := range []*big.Int{two, five} {
		n := 0
		for new(big.Int).Mod(denom, prime).Sign() == 0 {
			denom.Quo(denom, prime)
			n++
		}
		if n > scale {
			scale = n
		}
	}
	s := r.FloatString(scale)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return Decimal(s)
}

// Millis accepts Cursor's string or numeric epoch-millisecond timestamps.
type Millis int64

func (m *Millis) UnmarshalJSON(b []byte) error {
	var s string
	if len(b) > 0 && b[0] == '"' {
		if json.Unmarshal(b, &s) != nil {
			return fmt.Errorf("invalid timestamp")
		}
	} else {
		s = string(bytes.TrimSpace(b))
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 {
		return fmt.Errorf("invalid timestamp")
	}
	*m = Millis(v)
	return nil
}
