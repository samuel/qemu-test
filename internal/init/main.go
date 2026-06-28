package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// guestStdout/guestStderr are package-level so both runTests (which routes the
// test binary's output through them) and the verdict-marker prints in main share
// them: marker() calls freshLine() on both before printing so a marker never gets
// glued onto un-terminated test output.
var (
	guestStdout = &prefixedLineWriter{w: os.Stdout, prefix: "[TESTS:STDOUT] "}
	guestStderr = &prefixedLineWriter{w: os.Stderr, prefix: "[TESTS:STDERR] "}
)

type prefixedLineWriter struct {
	w      io.Writer
	prefix string
	pre    bool
}

// freshLine terminates an in-progress (un-newline-terminated) line so a
// following write starts on its own line. No-op when not mid-line.
func (w *prefixedLineWriter) freshLine() error {
	if !w.pre {
		return nil
	}
	w.pre = false
	_, err := io.WriteString(w.w, "\n")
	return err
}

func (w *prefixedLineWriter) Write(p []byte) (n int, err error) {
	var m int
	for len(p) != 0 {
		if !w.pre {
			w.pre = true
			_, err := fmt.Fprint(w.w, w.prefix)
			if err != nil {
				return m, err
			}
		}
		ix := bytes.IndexByte(p, '\n')
		if ix < 0 {
			n, err := w.w.Write(p)
			m += n
			return m, err
		}
		n, err := w.w.Write(p[:ix+1])
		m += n
		if err != nil {
			return m, err
		}
		p = p[ix+1:]
		w.pre = false
	}
	return m, nil
}

func main() {
	// PID 1 must always power off the VM; otherwise the run hangs until the host
	// -timeout. recover() converts a panic into a verdict + poweroff, and runTests
	// returns its error (never log.Fatal, which would os.Exit past this defer) so
	// shutdown always runs.
	defer func() {
		if r := recover(); r != nil {
			marker("[TESTS:FAIL] panic: %v\n", r)
		}
		shutdown()
	}()

	if err := runTests(); err != nil {
		marker("[TESTS:FAIL] %s\n", err)
	} else {
		marker("[TESTS:PASS]\n")
	}
}

// marker prints a [TESTS:...] verdict line, first terminating any un-newline-
// terminated test output so the host parser sees the marker on its own line
// (otherwise it is glued onto STDOUT/STDERR payload and the verdict is lost,
// reported by the host as "unknown result").
func marker(format string, args ...any) {
	_ = guestStdout.freshLine() //nolint:errcheck // PID 1 cannot recover from a console write error
	_ = guestStderr.freshLine() //nolint:errcheck // ditto
	fmt.Printf(format, args...)
}

func runTests() error {
	if err := mountfs(); err != nil {
		return fmt.Errorf("mount filesystems: %w", err)
	}

	// Bring up the loopback interface so tests can use 127.0.0.1 / ::1 (httptest,
	// net.Listen on localhost, ...); lo is administratively down at boot. Best-
	// effort: it logs and continues, so a test that does no localhost networking is
	// unaffected if this somehow doesn't take.
	setupLoopback()

	// Load any requested kernel modules (and their dependencies) before the tests
	// run; needs /proc, /sys, /dev mounted, so it follows mountfs.
	if err := loadModules(); err != nil {
		return fmt.Errorf("load modules: %w", err)
	}

	// Mount the optional -out 9p share after modules load: the default bundles
	// build 9p as modules, so the share cannot mount until they are loaded.
	mountOut()

	var args []string
	argsRaw, err := os.ReadFile("test-args.txt")
	if err == nil {
		if len(argsRaw) != 0 {
			args = strings.Split(string(argsRaw), "\n")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("load test-args.txt: %w", err)
	}

	var env []string
	envRaw, err := os.ReadFile("test-env.txt")
	if err == nil {
		if len(envRaw) != 0 {
			env = strings.Split(string(envRaw), "\n")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("load test-env.txt: %w", err)
	}

	if len(args) == 0 {
		fmt.Println("Running tests")
	} else {
		fmt.Printf("Running tests with args: %s\n", argsRaw)
	}

	// Rescan PCI bus just in case the modules changed anything.
	if err := writeSys("/sys/bus/pci/rescan", []byte("1\n")); err != nil {
		fmt.Println(err.Error())
	}

	// Wait for devices to settle. Ideally this would just wait for some events.
	time.Sleep(time.Second)

	cmd := exec.Command("/tests", args...)
	cmd.Stdout = guestStdout
	cmd.Stderr = guestStderr
	cmd.Env = testEnvironment(env)
	// runReaped runs /tests to completion and, as PID 1, reaps any orphaned
	// grandchildren reparented to init. A non-nil error is a test failure; main
	// reports it as [TESTS:FAIL].
	return runReaped(cmd)
}

// testEnvironment builds the environment for the /tests binary. PID 1 inherits a
// near-empty environment (essentially just what the kernel passed on the
// cmdline), so it fills in sane defaults for PATH/HOME/TMPDIR when they are
// absent — without them tests that exec helpers (via $PATH), read $HOME, or honor
// $TMPDIR misbehave. The user's -env vars are appended last so they win (exec
// resolves a duplicate key to its last value).
func testEnvironment(user []string) []string {
	env := os.Environ()
	for _, def := range []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"TMPDIR=/tmp",
	} {
		key := def[:strings.IndexByte(def, '=')+1]
		if !slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, key) }) {
			env = append(env, def)
		}
	}
	return append(env, user...)
}

func writeSys(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("failed to open %s for write: %w", path, err)
	}
	_, err = f.Write(data)
	_ = f.Close()
	if err != nil {
		return fmt.Errorf("failed to write to %s: %w", path, err)
	}
	return nil
}
