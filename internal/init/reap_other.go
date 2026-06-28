//go:build !linux

package main

import "os/exec"

// runReaped has no reaping to do off Linux (the guest only ever runs on Linux);
// it just runs the command. Present so init builds on the dev host.
func runReaped(cmd *exec.Cmd) error { return cmd.Run() }
