//go:build !unix

package cmd

import "os/exec"

func detach(*exec.Cmd) {}
