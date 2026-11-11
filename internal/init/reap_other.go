//go:build !linux

package main

import (
	"errors"
	"os/exec"
)

// runReaped has no reaping to do off Linux; the stub only keeps init building on
// the dev host.
func runReaped(cmd *exec.Cmd) (int, error) {
	err := cmd.Run()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok && ee.Exited() {
		return ee.ExitCode(), nil
	}
	return 0, err
}
