package agents

// Launched is an agent whose command takes its exporter from the environment alone, so
// the shell function setup writes hands its start to `terma launch <name>`.
type Launched interface {
	Agent
	// LaunchCommand is the command the developer types, found on PATH.
	LaunchCommand() string
	// LaunchEnv is what the relay exporter sets in the command's environment and takes out
	// of it.
	LaunchEnv(cfg RelayConfig) (set map[string]string, unset []string)
}
