package service

import (
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Systemd renders a user service with the supplied configuration environment.
func Systemd(exe string, env map[string]string) string {
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=terma local OTLP relay\n\n[Service]\n")
	b.WriteString("ExecStart=" + strconv.Quote(exe) + " relay run --idle 0 --quiet\n")
	for _, k := range slices.Sorted(maps.Keys(env)) {
		b.WriteString("Environment=" + strconv.Quote(k+"="+env[k]) + "\n")
	}
	b.WriteString("Restart=on-failure\nRestartSec=5\n\n[Install]\nWantedBy=default.target\n")
	return b.String()
}
