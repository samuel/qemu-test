package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

type prefixedLineWriter struct {
	w      io.Writer
	prefix string
	pre    bool
}

func (w *prefixedLineWriter) Write(p []byte) (n int, err error) {
	var m int
	for len(p) != 0 {
		if !w.pre {
			w.pre = true
			_, err := fmt.Fprint(w.w, w.prefix)
			if err != nil {
				return 0, err
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
			fmt.Printf("[TESTS:FAIL] panic: %v\n", r)
		}
		shutdown()
	}()

	if err := runTests(); err != nil {
		fmt.Printf("[TESTS:FAIL] %s\n", err)
	} else {
		fmt.Println("[TESTS:PASS]")
	}
}

func runTests() error {
	if err := mountfs(); err != nil {
		return fmt.Errorf("mount filesystems: %w", err)
	}

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

	cmd := exec.Command("/tests", args...)
	cmd.Stdout = &prefixedLineWriter{w: os.Stdout, prefix: "[TESTS:STDOUT] "}
	cmd.Stderr = &prefixedLineWriter{w: os.Stderr, prefix: "[TESTS:STDERR] "}
	if len(env) != 0 {
		// Append the user vars to the inherited environment so the test keeps a
		// baseline env; later entries win, so user vars override duplicates.
		cmd.Env = append(os.Environ(), env...)
	}
	// A non-nil error here is a test failure; main reports it as [TESTS:FAIL].
	return cmd.Run()
}
