package main

import (
	"errors"
	"fmt"
	"os/exec"

	"golang.org/x/sys/unix"
)

// runReaped runs cmd to completion while performing PID 1's reaping duty: it
// blocks in wait4(-1), reaping the test process plus any orphaned grandchildren
// reparented to init, and returns the test process's exit code. A process that
// did not exit normally (e.g. killed by a signal) is an error.
//
// It owns the wait loop rather than letting cmd.Wait do the waiting because the
// two would race to reap /tests and the loser would see ECHILD. cmd.Wait is still
// called afterwards, solely to drain the output copier goroutines, which it does
// even when its own wait fails, so the test output is flushed before the verdict.
func runReaped(cmd *exec.Cmd) (int, error) {
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid

	var status unix.WaitStatus
	for {
		wpid, err := unix.Wait4(-1, &status, 0, nil)
		switch {
		case errors.Is(err, unix.EINTR):
			continue // interrupted (e.g. the Go runtime's preemption signal); retry
		case err != nil:
			// Includes ECHILD, which would mean /tests vanished unseen.
			return 0, fmt.Errorf("wait4: %w", err)
		case wpid == pid:
			_ = cmd.Wait() //nolint:errcheck // already reaped above; this only drains the output goroutines
			return exitCode(status)
		}
		// Otherwise an orphaned grandchild was reaped; keep waiting for /tests.
	}
}

// exitCode extracts the exit code from a wait status.
func exitCode(ws unix.WaitStatus) (int, error) {
	switch {
	case ws.Exited():
		return ws.ExitStatus(), nil
	case ws.Signaled():
		return 0, fmt.Errorf("signal: %v", ws.Signal())
	default:
		return 0, fmt.Errorf("unexpected wait status %#x", uint32(ws))
	}
}
