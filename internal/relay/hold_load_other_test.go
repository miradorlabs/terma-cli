//go:build !unix

package relay

import "time"

// cpuTime is unmeasured off Unix; the load's other numbers still hold.
func cpuTime() time.Duration { return 0 }
