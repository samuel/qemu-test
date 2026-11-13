// Package runner builds the guest initramfs (init + the binary to run + the
// requested kernel modules), launches the architecture's qemu binary, and parses
// the serial console for the binary's output and exit status.
package runner

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"samuelks.com/qemu-test/internal/arch"
	"samuelks.com/qemu-test/internal/boot"
	"samuelks.com/qemu-test/internal/builder"
	"samuelks.com/qemu-test/internal/cache"
	"samuelks.com/qemu-test/internal/modules"
	"samuelks.com/qemu-test/internal/qmp"
)

// Options carries the inputs for a single VM run, assembled from the flags.
type Options struct {
	Arch           arch.Arch
	KernelOverride string
	BundleOverride string        // -bundle path or URL (kernel + modules + dtb)
	Refresh        bool          // re-validate a URL kernel or bundle against the remote
	Modules        []string      // kernel modules to load in the guest before the binary runs
	Mem            string        // guest RAM (-mem), used for the module-size guardrail
	Binary         string        // host path of the static Linux executable to run in the guest
	Args           []string      // argv passed to the binary
	Env            []string      // KEY=VALUE env for the binary
	WorkDir        string        // guest working directory for the binary; "" leaves it at /
	Shares         []Share       // host directories shared into the guest
	QEMUArgs       []string      // user/resource qemu args, before the arch boot args
	KVM            bool          // enable KVM acceleration
	Keep           bool          // keep the temp dir and log the qemu command line
	ExtraCmdline   []string      // extra kernel cmdline tokens, merged after console=
	Timeout        time.Duration // max wall-clock for the VM run; 0 = no limit
	EmbedFiles     embed.FS      // embedded init source + go.mod/go.sum
}

// ResolveKVM decides whether to enable KVM acceleration. "auto" enables only
// when the guest arch matches the host (KVM cannot accelerate cross-architecture
// emulation) and /dev/kvm is writable.
func ResolveKVM(mode, guestArch string) (bool, error) {
	switch mode {
	case "off":
		return false, nil
	case "on":
		return true, nil
	case "auto":
		if guestArch != runtime.GOARCH {
			return false, nil
		}
		f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
		if err != nil {
			return false, nil
		}
		_ = f.Close() //nolint:errcheck // probe only; the open succeeding is the signal
		return true, nil
	default:
		return false, fmt.Errorf("invalid -kvm %q (want auto, on, or off)", mode)
	}
}

// firmwareDirs returns candidate directories for a qemu firmware blob. The first
// derives the install's data dir from the binary (<prefix>/bin/qemu-system-X ->
// <prefix>/share/qemu); the rest are common fixed locations.
func firmwareDirs(qemuBinaryPath string) []string {
	var dirs []string
	if qemuBinaryPath != "" {
		dirs = append(dirs, filepath.Join(filepath.Dir(qemuBinaryPath), "..", "share", "qemu"))
	}
	return append(dirs,
		"/usr/share/qemu",
		"/usr/share/edk2/loongarch64",
		"/usr/share/qemu-efi-loongarch64",
		"/opt/homebrew/share/qemu",
	)
}

// findFirmwareIn returns the first existing regular file <dir>/<name>.
func findFirmwareIn(dirs []string, name string) (string, bool) {
	for _, d := range dirs {
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, true
		}
	}
	return "", false
}

// findFirmware locates the named qemu firmware blob (e.g. an edk2 image) for the
// given qemu binary, searching next to the binary and in common system dirs.
func findFirmware(qemuBinary, name string) (string, error) {
	bin, err := exec.LookPath(qemuBinary)
	if err == nil {
		if resolved, e := filepath.EvalSymlinks(bin); e == nil {
			bin = resolved
		}
	}
	dirs := firmwareDirs(bin)
	if p, ok := findFirmwareIn(dirs, name); ok {
		return p, nil
	}
	return "", fmt.Errorf("firmware %q not found in %v; install it or pass it with -qemu.arg \"-bios /path/to/%s\"", name, dirs, name)
}

// Run packs the binary into a guest initramfs, boots qemu, and returns the
// binary's exit code. An error means the binary produced none: a host-side
// failure, a guest setup failure, or a noVerdictError for a boot/qemu failure,
// timeout, or interrupt.
func Run(ctx context.Context, o Options) (int, error) {
	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}

	a := o.Arch
	// Fail fast (before the expensive build) if the arch's emulator is missing.
	if _, err := exec.LookPath(a.QEMUBinary); err != nil {
		return 0, fmt.Errorf("qemu binary %q not found for arch %s (install it via your package manager): %w", a.QEMUBinary, a.Name, err)
	}

	// Locate the arch's firmware blob (also fail fast), unless the user supplied
	// their own -bios via -qemu.arg.
	var firmwarePath string
	if a.Firmware != "" && !slices.Contains(o.QEMUArgs, "-bios") {
		var err error
		if firmwarePath, err = findFirmware(a.QEMUBinary, a.Firmware); err != nil {
			return 0, err
		}
	}

	bin, err := os.Open(o.Binary)
	if err != nil {
		return 0, err
	}
	//nolint:errcheck // read-only file; nothing to flush on close.
	defer bin.Close()

	tempPath, err := os.MkdirTemp("", "qemu-test-")
	if err != nil {
		return 0, err
	}
	defer func() {
		if o.Keep {
			slog.Info("keeping temp dir (-keep)", "path", tempPath)
			return
		}
		if err := os.RemoveAll(tempPath); err != nil {
			slog.Error("Failed to remove temp dir", "path", tempPath, "err", err)
		}
	}()

	// Resolve the kernel before the slower guest build so a bad -kernel/-bundle
	// path, URL, or layout fails fast.
	env, err := boot.Resolve(ctx, boot.Options{
		Arch:           a,
		KernelOverride: o.KernelOverride,
		BundleOverride: o.BundleOverride,
		Refresh:        o.Refresh,
		WorkDir:        tempPath,
	})
	if err != nil {
		return 0, err
	}

	shares := normalizeShares(o.Shares)
	moduleNames := o.Modules
	if len(shares) != 0 {
		// The default bundles build 9p as modules, so a share cannot mount until
		// the filesystem and its virtio transport are loaded. The resolver pulls in
		// their dependencies and skips them where 9p is built in.
		moduleNames = append(slices.Clone(moduleNames), "9p", "9pnet_virtio")
	}

	extraFiles := make(map[string]io.Reader)
	extraFiles["test-args.txt"] = strings.NewReader(strings.Join(o.Args, "\n"))
	// KEY=VALUE lines the guest adds to the binary's environment.
	extraFiles["test-env.txt"] = strings.NewReader(strings.Join(o.Env, "\n"))
	extraFiles["test-cwd.txt"] = strings.NewReader(o.WorkDir)
	extraFiles["test-shares.txt"] = strings.NewReader(shareList(shares))
	// In-guest .ko paths in dependency order, loaded before the binary runs.
	modPaths, err := addModules(moduleNames, o.Mem, env.ModulesPath, extraFiles)
	if err != nil {
		return 0, err
	}
	extraFiles["test-modules.txt"] = strings.NewReader(strings.Join(modPaths, "\n"))

	initBin, err := buildInit(ctx, a.GOARCH, o.EmbedFiles, tempPath)
	if err != nil {
		return 0, err
	}

	ifs, err := os.Create(filepath.Join(tempPath, "initramfs.gz"))
	if err != nil {
		return 0, err
	}
	//nolint:errcheck // writeInitramfs' checked gzip close flushes the data. This close only releases the fd on error paths.
	defer ifs.Close()
	slog.Debug("Building initramfs")
	if err := writeInitramfs(ifs, initBin, bin, extraFiles); err != nil {
		return 0, err
	}

	qmpSock := filepath.Join(tempPath, "qmp.sock")
	qemuArgs := slices.Concat(o.QEMUArgs, shareQEMUArgs(shares), a.BootArgs(arch.BootConfig{
		KernelPath:   env.KernelPath,
		DTBPath:      env.DTBPath,
		FirmwarePath: firmwarePath,
		InitrdPath:   ifs.Name(),
		QMPSock:      qmpSock,
		KVM:          o.KVM,
		UserSetCPU:   slices.Contains(o.QEMUArgs, "-cpu"),
		ExtraCmdline: o.ExtraCmdline,
	}))
	slog.Debug("Running qemu", "binary", a.QEMUBinary, "args", qemuArgs)
	if o.Keep {
		slog.Info("qemu command (-keep)", "cmdline", quoteCommand(a.QEMUBinary, qemuArgs))
	}
	qcmd := exec.CommandContext(ctx, a.QEMUBinary, qemuArgs...)
	// Set once the QMP handshake completes. The guest forwards QMP commands by
	// printing "[QMP]<json>", which tests only do well after boot.
	var qmpConn atomic.Pointer[qmp.Conn]
	// The last console lines, so a run that never reports a verdict (kernel panic,
	// init crash, bad qemu args) can show what the guest last printed.
	const tailMax = 25
	var tail []string
	recordTail := func(line []byte) {
		if s := string(bytes.TrimRight(line, "\r\n")); s != "" {
			tail = append(tail, s)
			if len(tail) > tailMax {
				tail = tail[len(tail)-tailMax:]
			}
		}
	}
	stdout := &streamWriter{w: os.Stdout, onQMP: func(cmd string) {
		q := qmpConn.Load()
		if q == nil {
			slog.Warn("dropping [QMP] command: qmp not connected", "cmd", cmd)
		} else if err := q.Send(cmd); err != nil {
			slog.Error("forward qmp command", "err", err)
		}
	}}
	stderr := &streamWriter{w: os.Stderr}
	var v verdict
	lp := &logParser{
		fn: func(key, line []byte) error {
			recordTail(line)
			if len(key) == 0 {
				slog.Debug(string(line))
				return nil
			}
			// Never return an error from this callback: it would propagate out
			// of lp.Write, stop the os/exec copier draining qemu's stdout, and
			// hang qemu until the deadline.
			switch k := string(key); k {
			case "STDOUT", "STDOUT-":
				stdout.record(line, k == "STDOUT-")
			case "STDERR", "STDERR-":
				stderr.record(line, k == "STDERR-")
			case "WARN":
				slog.Warn("guest: " + string(bytes.TrimRight(line, "\r\n")))
			default:
				if !v.record(k, line) {
					slog.Warn("unknown log key", "key", k, "line", string(bytes.TrimRight(line, "\n")))
				}
			}
			return nil
		},
	}
	qcmd.Stdout = lp
	qcmd.Stderr = lp
	if err := qcmd.Start(); err != nil {
		return 0, fmt.Errorf("qemu start: %w", err)
	}
	// Connect the QMP control channel while the VM boots. Best-effort: a failure
	// only disables QMP-driven tests, which then skip or time out.
	if q, err := qmp.Dial(qmpSock, 5*time.Second); err != nil {
		slog.Error("connect qmp", "err", err)
	} else {
		qmpConn.Store(q)
		//nolint:errcheck // QMP is best-effort; a close error cannot affect the result.
		defer q.Close()
	}
	waitErr := qcmd.Wait()
	// Process any final line buffered without a trailing newline. Wait has already
	// joined the output copier goroutines, so nothing races here.
	if err := lp.flush(); err != nil {
		return 0, err
	}
	stdout.flush()
	if waitErr != nil {
		// A context kill surfaces as a generic Wait error (qemu was SIGKILLed);
		// report the cause instead.
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return 0, &noVerdictError{msg: fmt.Sprintf("timed out after %s", o.Timeout), tail: tail, err: ctx.Err()}
		case ctx.Err() != nil:
			return 0, &noVerdictError{msg: "interrupted", tail: tail, err: ctx.Err()}
		}
		return 0, &noVerdictError{msg: fmt.Sprintf("qemu failed: %v", waitErr), tail: tail, err: waitErr}
	}

	switch {
	case v.failure != "":
		return 0, errors.New("guest: " + v.failure)
	case v.exited:
		return v.code, nil
	}
	// A clean qemu exit with no verdict means the guest powered off without
	// printing [TESTS:EXIT]/[TESTS:FAIL]: a boot or init failure.
	return 0, &noVerdictError{msg: "guest did not report an exit status (boot or init failure)", tail: tail}
}

// verdict is the outcome the guest init reports on the console: the binary's
// exit code ([TESTS:EXIT] n), or why it produced none ([TESTS:FAIL] reason).
type verdict struct {
	exited  bool
	code    int
	failure string
}

// record applies a verdict line, reporting false when key is not a verdict key.
func (v *verdict) record(key string, line []byte) bool {
	text := strings.TrimSpace(string(line))
	switch key {
	case "EXIT":
		code, err := strconv.Atoi(text)
		if err != nil {
			v.failure = fmt.Sprintf("malformed exit status %q", text)
			return true
		}
		v.exited, v.code = true, code
	case "FAIL":
		if text == "" {
			text = "failed"
		}
		v.failure = text
	default:
		return false
	}
	return true
}

// noVerdictError reports a run in which the guest reported no verdict. It carries the
// captured console tail and unwraps to the underlying cause, so a timeout still
// matches context.DeadlineExceeded.
type noVerdictError struct {
	msg  string
	tail []string
	err  error
}

func (e *noVerdictError) Error() string { return e.msg + tailSuffix(e.tail) }
func (e *noVerdictError) Unwrap() error { return e.err }

// tailSuffix formats captured console lines for an error message, or "" if none.
func tailSuffix(tail []string) string {
	if len(tail) == 0 {
		return ""
	}
	return "; last console output:\n" + strings.Join(tail, "\n")
}

// quoteCommand renders a binary + args as a roughly copy-pasteable shell command.
func quoteCommand(bin string, args []string) string {
	var b strings.Builder
	b.WriteString(bin)
	for _, a := range args {
		b.WriteByte(' ')
		if strings.ContainsAny(a, " \t") {
			b.WriteByte('\'')
			b.WriteString(a)
			b.WriteByte('\'')
		} else {
			b.WriteString(a)
		}
	}
	return b.String()
}

func copyEmbedFS(out *os.Root, efs embed.FS) error {
	return fs.WalkDir(efs, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == "." {
				return nil
			}
			return out.Mkdir(path, 0o755)
		}
		fi, err := efs.Open(path)
		if err != nil {
			return err
		}
		fo, err := out.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			_ = fi.Close()
			return err
		}
		_, err = io.Copy(fo, fi)
		_ = fi.Close()
		fiErr := fo.Close()
		if err == nil {
			err = fiErr
		}
		return err
	})
}

// addModules resolves the requested module names against the bundle's modules
// tree and packs each resulting .ko into extraFiles at the initramfs root, by
// basename, returning the in-guest names in dependency order. Flat at the root
// because BuildInitramfs emits only regular files and the kernel's unpacker does
// not create parent dirs; modules.Resolve guarantees unique basenames.
func addModules(names []string, mem, modulesDir string, extraFiles map[string]io.Reader) ([]string, error) {
	load, skipped, err := modules.Resolve(modulesDir, names)
	if err != nil {
		return nil, err
	}
	if len(skipped) != 0 {
		slog.Debug("modules presumed built in (not loaded)", "modules", skipped)
	}
	var guestNames []string
	var total int64
	for _, m := range load {
		data, err := os.ReadFile(m.HostPath)
		if err != nil {
			return nil, fmt.Errorf("read module %q: %w", m.HostPath, err)
		}
		extraFiles[m.Base] = bytes.NewReader(data)
		guestNames = append(guestNames, m.Base)
		total += int64(len(data))
		slog.Debug("packing module", "name", m.Name, "file", m.Base, "bytes", len(data))
	}
	if total > 0 {
		warnModuleMemory(total, mem)
	}
	return guestNames, nil
}

// warnModuleMemory warns when the packed modules are >=25% of guest RAM, since
// the initramfs is unpacked into guest memory.
func warnModuleMemory(totalBytes int64, mem string) {
	memBytes, ok := parseMemBytes(mem)
	if !ok || memBytes <= 0 {
		return
	}
	if totalBytes*4 >= memBytes {
		slog.Warn("packed kernel modules are large relative to guest RAM; the initramfs is unpacked into guest memory. Consider raising -mem",
			"moduleBytes", totalBytes, "memBytes", memBytes)
	}
}

// unitMult returns the byte multiplier for a qemu -m unit letter.
func unitMult(b byte) (int64, bool) {
	switch b {
	case 'b', 'B':
		return 1, true
	case 'k', 'K':
		return 1 << 10, true
	case 'm', 'M':
		return 1 << 20, true
	case 'g', 'G':
		return 1 << 30, true
	case 't', 'T':
		return 1 << 40, true
	}
	return 0, false
}

// parseMemBytes parses a qemu -m value (bare MiB, or a B/K/M/G/T suffix with an
// optional binary "i" marker such as MiB or Gi) into bytes, reporting false when
// it cannot be parsed.
func parseMemBytes(mem string) (int64, bool) {
	s := strings.TrimSpace(mem)
	if s == "" {
		return 0, false
	}
	mult := int64(1 << 20) // qemu's default unit is MiB
	if m, ok := unitMult(s[len(s)-1]); ok {
		mult, s = m, s[:len(s)-1]
	}
	// An optional binary "i" marker (Mi, GiB, Ki) follows the unit letter and
	// consumes it next.
	if n := len(s); n > 1 && (s[n-1] == 'i' || s[n-1] == 'I') {
		s = s[:n-1]
		m, ok := unitMult(s[len(s)-1])
		if !ok {
			return 0, false
		}
		mult, s = m, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 || n > math.MaxInt64/mult {
		return 0, false
	}
	return n * mult, true
}

// buildInit returns the guest init compiled for goarch. The embedded source is
// copied into workDir and built there, so the build works from any directory;
// the result is cached by arch and source hash, since each `go test -exec`
// package run is a separate invocation.
func buildInit(ctx context.Context, goarch string, efs embed.FS, workDir string) ([]byte, error) {
	var cachePath string
	if dir, err := cache.CacheDir(); err != nil {
		slog.Debug("init cache unavailable", "err", err)
	} else if key, err := initKey(goarch, efs); err != nil {
		return nil, err
	} else {
		cachePath = filepath.Join(dir, key+cache.InitExt)
		if b, err := os.ReadFile(cachePath); err == nil {
			return b, nil
		}
	}

	root, err := os.OpenRoot(workDir)
	if err != nil {
		return nil, err
	}
	//nolint:errcheck // os.Root dir handle; nothing is buffered to flush on close.
	defer root.Close()
	if err := copyEmbedFS(root, efs); err != nil {
		return nil, fmt.Errorf("failed to copy embed files: %w", err)
	}
	slog.Debug("Building init", "goarch", goarch)
	var buf bytes.Buffer
	target := builder.Target{GOOS: "linux", GOARCH: goarch}
	if err := builder.GoBuild(ctx, &buf, target, filepath.Join(workDir, "internal", "init")); err != nil {
		return nil, fmt.Errorf("failed to build init: %w", err)
	}
	if cachePath != "" {
		if err := cache.WriteFileAtomic(cachePath, buf.Bytes()); err != nil {
			slog.Warn("cache init", "err", err)
		}
	}
	return buf.Bytes(), nil
}

// initKey hashes the target arch and the embedded init source into a cache key.
func initKey(goarch string, efs embed.FS) (string, error) {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\n", goarch)
	err := fs.WalkDir(efs, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := efs.ReadFile(path)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(h, "%s %d\n", path, len(b))
		_, _ = h.Write(b)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeInitramfs writes the gzipped initramfs: init, the binary as /tests, and
// the extra files, all at the root.
func writeInitramfs(out io.Writer, initBin []byte, binary io.Reader, extraFiles map[string]io.Reader) error {
	w := gzip.NewWriter(out)
	//nolint:errcheck // After a successful build the writer is closed and the error checked. This covers the failure paths.
	defer w.Close()

	files := map[string]io.Reader{
		"init":  bytes.NewReader(initBin),
		"tests": binary,
	}
	maps.Copy(files, extraFiles)
	sizes, err := builder.SizesFromReaders(files)
	if err != nil {
		return err
	}
	if err := builder.BuildInitramfs(w, files, sizes); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("failed to close gzip file: %w", err)
	}
	return nil
}
