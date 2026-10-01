package cli

import (
	"errors"
	"testing"
)

// A bare Enter takes the default row when there is one, and is no answer when there is not.
func TestPickAnswer(t *testing.T) {
	byName := func(name string) (int, error) {
		if name == "Acme API" {
			return 1, nil
		}
		return -1, errors.New("no project matches")
	}
	for _, c := range []struct {
		name    string
		answer  string
		def     int
		want    int
		wantErr bool
	}{
		{"Enter takes the default", "", 1, 1, false},
		{"Enter without a default selects nothing", "", -1, -1, true},
		{"a number is a row", "1", 1, 0, false},
		{"a number past the end", "3", -1, -1, true},
		{"a name resolves like the argument", "Acme API", -1, 1, false},
		{"an unknown name", "Nope", 0, -1, true},
	} {
		got, err := pickAnswer(c.answer, 2, c.def, byName)
		if got != c.want || (err != nil) != c.wantErr {
			t.Errorf("%s: pickAnswer(%q) = %d, %v; want %d (error %v)", c.name, c.answer, got, err, c.want, c.wantErr)
		}
	}
}
