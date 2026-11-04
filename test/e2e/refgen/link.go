package main

// newLinker is the source linker a run uses: none yet. The source lookup moves to its own
// package (sourcelink) so that refgen can link prompts, tools and flags as compatgen links
// fields; until it lands, changes are published unlinked and a later run links them.
func newLinker(bool) linker { return nil }
