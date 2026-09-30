package daemon

import "testing"

// A relay is started again after it stepped aside for a replaced binary, and the
// service's relay whenever it stopped for any reason but its setup being gone; a relay
// that found another running is not.
func TestRestart(t *testing.T) {
	for _, c := range []struct {
		res  Result
		want bool
	}{
		{Result{}, false},
		{Result{Replaced: true}, true},
		{Result{Service: true}, true},
		{Result{Service: true, SetupGone: true}, false},
		{Result{Service: true, Replaced: true, SetupGone: true}, true},
		{Result{Service: true, AlreadyRunning: true}, false},
		{Result{SetupGone: true}, false},
	} {
		if got := c.res.Restart(); got != c.want {
			t.Errorf("%+v.Restart() = %v, want %v", c.res, got, c.want)
		}
	}
}
