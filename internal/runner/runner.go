// Package runner builds the guest initramfs (init + the compiled test binary +
// the requested kernel modules), launches the architecture's qemu binary, and
// parses the serial console for the test verdict.
package runner

import (
	"bytes"
	"compress/gzip"
	"context"
	"embed"
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

	"github.com/samuel/qemu-test/internal/arch"
	"github.com/samuel/qemu-test/internal/boot"
	"github.com/samuel/qemu-test/internal/builder"
	"github.com/samuel/qemu-test/internal/modules"
	"github.com/samuel/qemu-test/internal/qmp"
)

// ErrNoTests reports that the target package has no test files. Treated like
// `go test`: a notice, exit 0.
var ErrNoTests = errors.New("no test files")

// Options carries the inputs for a single VM run, assembled from the flags.
type Options struct {
	Arch           arch.Arch
	KernelOverride string
	BundleOverride string        // -bundle path or URL (kernel + modules + dtb)
	Refresh        bool          // re-validate a URL kernel or bundle against the remote
	Modules        []string      // kernel modules to load in the guest before tests
	Mem            string        // guest RAM (-mem), used for the module-size guardrail
	TestPath       string        // Go package to test
	TestBuildArgs  []string      // build flags for the test binary
	TestArgs       []string      // argv passed to the test binary in the guest
	TestEnv        []string      // KEY=VALUE env for the test binary
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

// Run builds the guest initramfs, boots qemu, and reports the test verdict: nil
// on pass, "tests failed" on a reported failure, or a noVerdictError for a
// boot/init/qemu failure or a timeout.
func Run(ctx context.Context, o Options) error {
	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}

	a := o.Arch
	// Fail fast (before the expensive build) if the arch's emulator is missing.
	if _, err := exec.LookPath(a.QEMUBinary); err != nil {
		return fmt.Errorf("qemu binary %q not found for arch %s (install it via your package manager): %w", a.QEMUBinary, a.Name, err)
	}

	// Locate the arch's firmware blob (also fail fast), unless the user supplied
	// their own -bios via -qemu.arg.
	var firmwarePath string
	if a.Firmware != "" && !slices.Contains(o.QEMUArgs, "-bios") {
		var err error
		if firmwarePath, err = findFirmware(a.QEMUBinary, a.Firmware); err != nil {
			return err
		}
	}

	tempPath, err := os.MkdirTemp("", "qemu-test-")
	if err != nil {
		return err
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
	tempRoot, err := os.OpenRoot(tempPath)
	if err != nil {
		return err
	}
	//nolint:errcheck // read-only os.Root dir handle; nothing is buffered to flush on close.
	defer tempRoot.Close()
	if err := copyEmbedFS(tempRoot, o.EmbedFiles); err != nil {
		return fmt.Errorf("failed to copy embed files: %w", err)
	}

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
		return err
	}

	extraFiles := make(map[string]io.Reader)
	extraFiles["test-args.txt"] = bytes.NewReader([]byte(strings.Join(o.TestArgs, "\n")))
	// KEY=VALUE lines the guest appends to the test binary's environment.
	extraFiles["test-env.txt"] = bytes.NewReader([]byte(strings.Join(o.TestEnv, "\n")))
	// In-guest .ko paths in dependency order, loaded before the tests run.
	modPaths, err := addModules(o, env.ModulesPath, extraFiles)
	if err != nil {
		return err
	}
	extraFiles["test-modules.txt"] = bytes.NewReader([]byte(strings.Join(modPaths, "\n")))

	ifs, err := os.Create(filepath.Join(tempPath, "initramfs.gz"))
	if err != nil {
		return err
	}
	//nolint:errcheck // buildTests' checked gzip close flushes the data. This close only releases the fd on error paths.
	defer ifs.Close()
	slog.Debug("Building initramfs")
	target := builder.Target{GOOS: "linux", GOARCH: a.GOARCH}
	if err := buildTests(ctx, ifs, target, filepath.Join(tempPath, "internal", "init"), o.TestPath, o.TestBuildArgs, extraFiles); err != nil {
		return err
	}

	qmpSock := filepath.Join(tempPath, "qmp.sock")
	qemuArgs := append(o.QEMUArgs, a.BootArgs(arch.BootConfig{
		KernelPath:   env.KernelPath,
		DTBPath:      env.DTBPath,
		FirmwarePath: firmwarePath,
		InitrdPath:   ifs.Name(),
		QMPSock:      qmpSock,
		KVM:          o.KVM,
		UserSetCPU:   slices.Contains(o.QEMUArgs, "-cpu"),
		ExtraCmdline: o.ExtraCmdline,
	})...)
	slog.Debug("Running qemu", "binary", a.QEMUBinary, "args", qemuArgs)
	if o.Keep {
		slog.Info("qemu command (-keep)", "cmdline", quoteCommand(a.QEMUBinary, qemuArgs))
	}
	qcmd := exec.CommandContext(ctx, a.QEMUBinary, qemuArgs...)
	var result int
	const (
		resultUnknown = iota
		resultPass
		resultFail
	)
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
	lp := &logParser{
		fn: func(key, line []byte) error {
			recordTail(line)
			if len(key) != 0 {
				switch k := string(key); k {
				case "STDOUT":
					if cmd, ok := bytes.CutPrefix(line, []byte("[QMP]")); ok {
						if q := qmpConn.Load(); q != nil {
							if err := q.Send(string(bytes.TrimRight(cmd, "\n"))); err != nil {
								slog.Error("forward qmp command", "err", err)
							}
						} else {
							slog.Warn("dropping [QMP] command: qmp not connected", "cmd", string(bytes.TrimRight(cmd, "\n")))
						}
						return nil
					}
					_, _ = os.Stdout.Write(line)
				case "STDERR":
					_, _ = os.Stderr.Write(line)
				case "PASS":
					result = resultPass
				case "FAIL":
					result = resultFail
				default:
					// Never return an error from this callback: it would propagate
					// out of lp.Write, stop the os/exec copier draining qemu's
					// stdout, and hang qemu until the deadline.
					slog.Warn("unknown log key", "key", k, "line", string(bytes.TrimRight(line, "\n")))
				}
			} else {
				slog.Debug(string(line))
			}
			return nil
		},
	}
	qcmd.Stdout = lp
	qcmd.Stderr = lp
	if err := qcmd.Start(); err != nil {
		return fmt.Errorf("qemu start: %w", err)
	}
	// Connect the QMP control channel while the VM boots. Best-effort: a failure
	// only disables QMP-driven tests, which then skip or time out.
	if q, err := qmp.Dial(qmpSock, 5*time.Second); err != nil {
		slog.Error("connect qmp", "err", err)
	} else {
		qmpConn.Store(q)
		//nolint:errcheck // QMP is best-effort; a close error cannot affect the test result.
		defer q.Close()
	}
	waitErr := qcmd.Wait()
	// Process any final line buffered without a trailing newline. Wait has already
	// joined the output copier goroutines, so nothing races here.
	if err := lp.flush(); err != nil {
		return err
	}
	if waitErr != nil {
		// A deadline kill surfaces as a generic Wait error (qemu was SIGKILLed);
		// report the timeout instead so the cause is clear.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return &noVerdictError{msg: fmt.Sprintf("timed out after %s", o.Timeout), tail: tail, err: ctx.Err()}
		}
		return &noVerdictError{msg: fmt.Sprintf("qemu failed: %v", waitErr), tail: tail, err: waitErr}
	}

	switch result {
	case resultPass:
		return nil
	case resultFail:
		return errors.New("tests failed")
	}
	// A clean qemu exit with no verdict means the guest powered off without
	// printing [TESTS:PASS]/[TESTS:FAIL]: a boot or init failure.
	return &noVerdictError{msg: "guest did not report a pass/fail verdict (boot or init failure)", tail: tail}
}

// noVerdictError reports a run that produced no pass/fail verdict. It carries the
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

// addModules resolves the requested -module names against the bundle's modules
// tree and packs each resulting .ko into extraFiles at the initramfs root, by
// basename, returning the in-guest names in dependency order. Flat at the root
// because BuildInitramfs emits only regular files and the kernel's unpacker does
// not create parent dirs; modules.Resolve guarantees unique basenames.
func addModules(o Options, modulesDir string, extraFiles map[string]io.Reader) ([]string, error) {
	load, skipped, err := modules.Resolve(modulesDir, o.Modules)
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
		warnModuleMemory(total, o.Mem)
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

func buildTests(ctx context.Context, out io.Writer, target builder.Target, initSrcPath, testPath string, testBuildArgs []string, extraFiles map[string]io.Reader) error {
	initBuf := &bytes.Buffer{}
	if err := builder.GoBuild(ctx, initBuf, target, initSrcPath); err != nil {
		return fmt.Errorf("failed to build init: %w", err)
	}
	testBuf := &bytes.Buffer{}
	if err := builder.GoBuildTest(ctx, testBuf, target, testPath, testBuildArgs...); err != nil {
		return err
	}
	// With no test files `go test -c` writes a notice instead of an ELF binary.
	// Catch that here rather than packing a /tests the guest cannot exec.
	if !bytes.HasPrefix(testBuf.Bytes(), []byte("\x7fELF")) {
		return ErrNoTests
	}

	w := gzip.NewWriter(out)
	//nolint:errcheck // After success build the writer is closed and error is checked. This covers the failure paths.
	defer w.Close()

	files := map[string]io.Reader{
		"init":  initBuf,
		"tests": testBuf,
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
