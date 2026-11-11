package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// consoleMu serializes writes to the console so each record lands as one
// uninterrupted line.
var consoleMu sync.Mutex

// recordWriter frames the binary's output for the host as console records, one
// per line: "[TESTS:<key>] <text>" for text that ended in a newline and
// "[TESTS:<key>-] <text>" for a chunk that did not. Every record is a complete
// console line, so stdout, stderr, and control lines can interleave freely
// while the host still reproduces each stream byte for byte.
type recordWriter struct {
	w   io.Writer
	key string
}

func (w *recordWriter) Write(p []byte) (int, error) {
	var buf bytes.Buffer
	for rest := p; len(rest) != 0; {
		line, after, complete := bytes.Cut(rest, []byte("\n"))
		buf.WriteString("[TESTS:" + w.key)
		if !complete {
			buf.WriteByte('-')
		}
		buf.WriteString("] ")
		buf.Write(line)
		buf.WriteByte('\n')
		rest = after
	}
	consoleMu.Lock()
	defer consoleMu.Unlock()
	if _, err := w.w.Write(buf.Bytes()); err != nil {
		return 0, err
	}
	return len(p), nil
}

func main() {
	// PID 1 must always power off the VM, or the run hangs until the host timeout.
	// Nothing below may call os.Exit, which would skip this defer.
	defer func() {
		if r := recover(); r != nil {
			marker("[TESTS:FAIL] panic: %v\n", r)
		}
		shutdown()
	}()

	code, err := runBinary()
	if err != nil {
		marker("[TESTS:FAIL] %s\n", err)
	} else {
		marker("[TESTS:EXIT] %d\n", code)
	}
}

// marker prints a [TESTS:...] control line.
func marker(format string, args ...any) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	fmt.Printf(format, args...)
}

// readLines returns the lines of an optional initramfs list file.
func readLines(name string) ([]string, error) {
	raw, err := os.ReadFile(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("load %s: %w", name, err)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	return strings.Split(string(raw), "\n"), nil
}

// runBinary sets up the guest and runs /tests, returning its exit code. An error
// means the binary never produced one: setup failed, it could not start, or a
// signal killed it.
func runBinary() (int, error) {
	if err := mountfs(); err != nil {
		return 0, fmt.Errorf("mount filesystems: %w", err)
	}

	rawConsoleOutput()

	setupLoopback()

	// Needs /proc, /sys and /dev, so it follows mountfs.
	if err := loadModules(); err != nil {
		return 0, fmt.Errorf("load modules: %w", err)
	}

	// After loadModules: the default bundles build 9p as modules, so the shares
	// cannot mount until they are loaded.
	if err := mountShares(); err != nil {
		return 0, err
	}

	args, err := readLines("test-args.txt")
	if err != nil {
		return 0, err
	}
	env, err := readLines("test-env.txt")
	if err != nil {
		return 0, err
	}
	cwd, err := readLines("test-cwd.txt")
	if err != nil {
		return 0, err
	}

	fmt.Printf("Running /tests with args: %q\n", args)

	// Rescan the PCI bus in case the modules changed anything.
	if err := writeSys("/sys/bus/pci/rescan", []byte("1\n")); err != nil {
		fmt.Println(err.Error())
	}

	// Wait for devices to settle. Ideally this would wait on events instead.
	time.Sleep(time.Second)

	cmd := exec.Command("/tests", args...)
	cmd.Stdout = &recordWriter{w: os.Stdout, key: "STDOUT"}
	cmd.Stderr = &recordWriter{w: os.Stdout, key: "STDERR"}
	cmd.Env = testEnvironment(env)
	if len(cwd) != 0 {
		// Normally a mounted share; created here so the binary still starts when
		// the share is unavailable.
		if err := os.MkdirAll(cwd[0], 0o755); err != nil {
			return 0, fmt.Errorf("create working dir: %w", err)
		}
		cmd.Dir = cwd[0]
	}
	return runReaped(cmd)
}

// testEnvironment builds the environment for the /tests binary with each key at
// most once, so os/exec's last-value-wins dedup of duplicate Cmd.Env keys never
// has a duplicate to resolve and the user's -env values always win. User entries
// come first, then inherited initramfs values and defaults (PATH/HOME/TMPDIR,
// since PID 1 wakes up with a near-empty environment) for keys not yet set.
func testEnvironment(user []string) []string {
	env := dedupeVars(user)
	set := make(map[string]bool, len(env))
	for _, kv := range env {
		set[varKey(kv)] = true
	}
	for _, kv := range os.Environ() {
		if key := varKey(kv); !set[key] {
			env = append(env, kv)
			set[key] = true
		}
	}
	for _, def := range []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"TMPDIR=/tmp",
	} {
		if key := varKey(def); !set[key] {
			env = append(env, def)
			set[key] = true
		}
	}
	return env
}

// dedupeVars keeps the last value for a repeated KEY=VALUE, in first-encountered
// order, so a repeated -env flag is deterministic up front rather than left to
// whichever duplicate os/exec's dedup happens to keep.
func dedupeVars(vars []string) []string {
	out := make([]string, 0, len(vars))
	pos := make(map[string]int)
	for _, kv := range vars {
		if i, ok := pos[varKey(kv)]; ok {
			out[i] = kv
			continue
		}
		pos[varKey(kv)] = len(out)
		out = append(out, kv)
	}
	return out
}

// varKey returns the variable name portion of a KEY=VALUE string.
func varKey(kv string) string {
	if before, _, ok := strings.Cut(kv, "="); ok {
		return before
	}
	return kv
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
