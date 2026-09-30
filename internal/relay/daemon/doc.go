// Package daemon is the local relay as a running process: `terma relay run` (Run), the
// Windows supervisor that restarts it (Supervise), starting and stopping it from a hook
// (Spawn, Stop), the per-user service that keeps it running (Service), its state
// directory (Dir and the files in it), and what it asks of the machine while it runs:
// the policy for a claimed session (Resolver), global mode's catch-all (CatchAll), a
// key for a project connected in Terma (KeyMinter), the heartbeat's facts, and the
// collection policies it keeps fresh (PolicyRefresher).
//
// The engine it runs, routing and capture, is package relay. What the daemon needs of
// the CLI (its configuration, sign-in, the registered agents) comes in through each
// part's options, so nothing here imports the command tree.
package daemon
