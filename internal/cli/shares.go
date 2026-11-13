package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"samuelks.com/qemu-test/internal/runner"
)

// -cwd values.
const (
	cwdReadOnly  = "ro"
	cwdReadWrite = "rw"
	cwdNone      = "none"
)

// Test binary flags whose value is a host path the binary writes. `go test`
// passes these as absolute host paths (its build work dir, the package dir), so
// the guest needs the same paths backed by the host for the results to land.
var (
	testFileFlags = []string{"coverprofile", "cpuprofile", "memprofile", "blockprofile", "mutexprofile", "trace"}
	testDirFlags  = []string{"gocoverdir", "outputdir"}
)

// testFlags extracts the -test.* flags from a test binary's argv as name → value
// (name without the "test." prefix), accepting "-test.x=v" and "-test.x v" for
// the flags in valued. Parsing stops where the testing package's would: at "--"
// or the first non-flag argument.
func testFlags(args, valued []string) map[string]string {
	flags := make(map[string]string)
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || !strings.HasPrefix(a, "-") || a == "-" {
			break
		}
		name, ok := strings.CutPrefix(strings.TrimLeft(a, "-"), "test.")
		if !ok {
			continue
		}
		if n, v, ok := strings.Cut(name, "="); ok {
			flags[n] = v
		} else if slices.Contains(valued, name) && i+1 < len(args) {
			i++
			flags[name] = args[i]
		}
	}
	return flags
}

// testTimeout returns the binary's -test.timeout, if its argv carries a valid one.
func testTimeout(args []string) (time.Duration, bool) {
	v, ok := testFlags(args, []string{"timeout"})["timeout"]
	if !ok {
		return 0, false
	}
	d, err := time.ParseDuration(v)
	return d, err == nil
}

// mirrorShares returns writable same-path shares for the host directories the
// test binary's path flags point into, and whether a relative path flag makes
// the binary write under its working directory.
func mirrorShares(args []string) (shares []runner.Share, writesCwd bool) {
	flags := testFlags(args, slices.Concat(testFileFlags, testDirFlags))
	add := func(dir string) {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			shares = append(shares, runner.Share{HostPath: dir, GuestPath: dir})
		}
	}
	outputDir, hasOutputDir := flags["outputdir"]
	for _, name := range testDirFlags {
		v := flags[name]
		switch {
		case v == "":
		case filepath.IsAbs(v):
			add(v)
		default:
			writesCwd = true
		}
	}
	for _, name := range testFileFlags {
		v := flags[name]
		switch {
		case v == "":
		case filepath.IsAbs(v):
			add(filepath.Dir(v))
		case hasOutputDir && filepath.IsAbs(outputDir):
			// The testing package resolves a relative profile path against
			// -test.outputdir.
			add(filepath.Dir(filepath.Join(outputDir, v)))
		default:
			writesCwd = true
		}
	}
	return shares, writesCwd
}

// parseShare parses a -share value, hostdir[:guestpath][:ro]. The guest path
// defaults to the absolute host path. A writable host dir is created if missing.
func parseShare(spec string) (runner.Share, error) {
	parts := strings.Split(spec, ":")
	var s runner.Share
	switch last := parts[len(parts)-1]; last {
	case "ro", "rw":
		s.ReadOnly = last == "ro"
		parts = parts[:len(parts)-1]
	}
	if len(parts) == 0 || len(parts) > 2 || parts[0] == "" {
		return s, fmt.Errorf("invalid -share %q (want hostdir[:guestpath][:ro])", spec)
	}
	host, err := filepath.Abs(parts[0])
	if err != nil {
		return s, fmt.Errorf("resolve -share path: %w", err)
	}
	s.HostPath, s.GuestPath = host, host
	if len(parts) == 2 {
		if !strings.HasPrefix(parts[1], "/") {
			return s, fmt.Errorf("invalid -share %q: guest path must be absolute", spec)
		}
		s.GuestPath = parts[1]
	}
	if !s.ReadOnly {
		if err := os.MkdirAll(host, 0o755); err != nil {
			return s, fmt.Errorf("create -share dir: %w", err)
		}
	}
	if st, err := os.Stat(host); err != nil {
		return s, fmt.Errorf("-share: %w", err)
	} else if !st.IsDir() {
		return s, fmt.Errorf("-share %q: not a directory", host)
	}
	return s, nil
}

// planShares assembles the guest working directory and every share for a run:
// the explicit -share flags, the current directory per the -cwd mode, and the
// directories the binary's path flags write into. The cwd share is read-only
// unless -cwd rw or a relative path flag has the binary write there.
func planShares(specs []string, cwdMode, cwd string, binArgs []string) (workDir string, shares []runner.Share, err error) {
	for _, spec := range specs {
		s, err := parseShare(spec)
		if err != nil {
			return "", nil, err
		}
		shares = append(shares, s)
	}
	mirrored, writesCwd := mirrorShares(binArgs)
	shares = append(shares, mirrored...)
	if cwdMode != cwdNone {
		if strings.ContainsAny(cwd, "\t\n") {
			return "", nil, errors.New("current directory path contains a tab or newline; use -cwd none")
		}
		workDir = cwd
		shares = append(shares, runner.Share{
			HostPath:  cwd,
			GuestPath: cwd,
			ReadOnly:  cwdMode == cwdReadOnly && !writesCwd,
		})
	}
	return workDir, shares, nil
}
