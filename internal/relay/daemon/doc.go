// Package daemon is the local relay as a running process: running, supervising, spawning
// and stopping it, its per-user service and state directory, and what it asks of the
// machine while it runs. What it needs of the CLI comes in through options, so nothing
// here imports the command tree.
package daemon
