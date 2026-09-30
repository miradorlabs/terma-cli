//go:build !unix && !windows

package cmd

import "os/exec"

func detach(*exec.Cmd) {}
