package migrate

// migrations is every migration, in ID order. Append only; see the package comment for
// the rules each one keeps. IDs through retiredThrough belonged to pre-release builds
// and are retired, never reused: the next migration is retiredThrough+1.
var migrations = []Migration{}

// retiredThrough is the last retired migration ID.
const retiredThrough = 1
