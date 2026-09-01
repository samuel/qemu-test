//go:build !linux

package main

import "os/exec"

// runReaped has no reaping to do off Linux; the stub only keeps init building on
// the dev host.
func runReaped(cmd *exec.Cmd) error { return cmd.Run() }
