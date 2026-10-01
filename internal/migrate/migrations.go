package migrate

// migrations is every migration, in ID order; the next one is retiredThrough+1.
var migrations = []Migration{}

// retiredThrough is the last retired migration ID, never reused.
const retiredThrough = 1
