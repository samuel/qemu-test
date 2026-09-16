package main

import (
	"errors"
	"fmt"
	"os/exec"

	"golang.org/x/sys/unix"
)

// runReaped runs cmd to completion while performing PID 1's reaping duty: it
// blocks in wait4(-1), reaping the test process plus any orphaned grandchildren
// reparented to init, and returns the test process's result the way cmd.Run does.
//
// It owns the wait loop rather than letting cmd.Wait do the waiting because the
// two would race to reap /tests and the loser would see ECHILD. cmd.Wait is still
// called afterwards, solely to drain the output copier goroutines, which it does
// even when its own wait fails, so the test output is flushed before the verdict.
func runReaped(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
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
			return fmt.Errorf("wait4: %w", err)
		case wpid == pid:
			_ = cmd.Wait() //nolint:errcheck // already reaped above; this only drains the output goroutines
			return waitStatusErr(status)
		}
		// Otherwise an orphaned grandchild was reaped; keep waiting for /tests.
	}
}

// waitStatusErr renders a wait status the way exec.Cmd.Run reports it.
func waitStatusErr(ws unix.WaitStatus) error {
	switch {
	case ws.Exited():
		if code := ws.ExitStatus(); code != 0 {
			return fmt.Errorf("exit status %d", code)
		}
		return nil
	case ws.Signaled():
		return fmt.Errorf("signal: %v", ws.Signal())
	default:
		return fmt.Errorf("unexpected wait status %#x", uint32(ws))
	}
}
