package main

import (
	"errors"
	"fmt"
	"os/exec"

	"golang.org/x/sys/unix"
)

// runReaped starts cmd and runs the test process to completion while also
// performing PID 1's reaping duty: any process whose parent exits first is
// reparented to init, and only init can reap it. It blocks in wait4(-1),
// reaping the test process plus any such orphaned grandchildren, and returns the
// test process's result the way exec.Cmd.Run does (nil on exit 0, else an
// "exit status N" / "signal: X" error).
//
// We own the wait loop instead of calling cmd.Wait() to do the waiting because a
// generic reaper (wait4(-1)) and cmd.Wait() race to reap /tests, and whichever
// lost would see ECHILD and misreport the result. Reaping ourselves keeps
// orphan-reaping semantics working for tests that depend on them, without that
// race. We still call cmd.Wait() afterwards — solely to drain the stdout/stderr
// copier goroutines (exec.Cmd.Wait does this even when its own process wait fails
// with ECHILD), so the full test output is flushed before main prints the
// verdict; its returned error is the expected ECHILD and is ignored.
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
			// ECHILD here would mean the test process vanished without us seeing it,
			// which should not happen; surface it rather than spin.
			return fmt.Errorf("wait4: %w", err)
		case wpid == pid:
			_ = cmd.Wait() //nolint:errcheck // already reaped above; this only drains the output goroutines
			return waitStatusErr(status)
		}
		// Else: reaped an orphaned grandchild — keep waiting for /tests.
	}
}

// waitStatusErr renders a child's wait status the way exec.Cmd.Run reports it:
// nil for a clean exit 0, an "exit status N" error for a non-zero exit, and a
// "signal: X" error for termination by a signal.
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
