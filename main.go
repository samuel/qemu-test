package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/samuel/qemu-test/builder"
)

// errNoTests reports that the target package has no test files, so there is no
// test binary to run in the guest. Treated like `go test` (a notice, exit 0).
var errNoTests = errors.New("no test files")

func main() {
	var qemuDevices MultiStringFlag
	flag.Var(&qemuDevices, "qemu.device", "Additional devices to pass to qemu (shorthand for -qemu.arg \"-device X\")")
	var qemuRawArgs MultiStringFlag
	flag.Var(&qemuRawArgs, "qemu.arg", "Raw argument(s) to pass to qemu, split on the first whitespace (e.g. -qemu.arg \"-drive if=none,id=d0,file=disk.img,format=raw\"). Repeatable.")
	var envVars MultiStringFlag
	flag.Var(&envVars, "env", "Environment variable KEY=VALUE for the test binary; repeatable")
	flagArch := flag.String("arch", "amd64", "Guest architecture (GOARCH name); only amd64 ships a kernel, others need -kernel (a path or URL)")
	flagKernel := flag.String("kernel", "", "Path or http(s) URL of an external kernel image, overriding the embedded kernel for -arch; URLs are downloaded and cached")
	flagKernelRefresh := flag.Bool("kernel.refresh", false, "Re-validate a URL kernel against the remote (conditional GET) and re-download if changed; cached kernels are otherwise used without network access")
	flagTags := flag.String("tags", "", "Build tags to pass when compiling the test binary")
	flagTestRun := flag.String("test.run", "", "Run only tests matching regular expression")
	flagTestVerbose := flag.Bool("test.v", false, "Enable verbose test output")
	flagVerbose := flag.Bool("v", false, "Enable verbose (debug) output")
	flagSMP := flag.Int("smp", 2, "Number of guest vCPUs")
	flagMem := flag.String("mem", "1024", "Guest RAM in MB (a bare -m value, so a qemu suffix like \"2G\" also works)")
	flagOut := flag.String("out", "", "Host dir shared writable into the guest at /out (e.g. -out ./cov ... -test.coverprofile=/out/cover.out)")
	flagTimeout := flag.Duration("timeout", 5*time.Minute, "Max wall-clock for the VM run; 0 = no limit")
	flagKVM := flag.String("kvm", "auto", "KVM acceleration: auto (on when the guest arch matches the host and /dev/kvm is writable), on, or off")
	flagAppend := flag.String("append", "", "Extra kernel command-line tokens, merged after console=...; e.g. -append \"loglevel=7 quiet\"")
	flagKeep := flag.Bool("keep", false, "Keep the temp dir (initramfs, built init, qmp.sock) and log the full qemu command line instead of removing it on exit")
	flag.Usage = usage
	flag.Parse()

	if *flagVerbose {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	testPath := flag.Arg(0)
	if testPath == "" {
		_, _ = fmt.Fprintln(flag.CommandLine.Output(), "qemu-test: no package provided")
		flag.Usage()
		os.Exit(2)
	}

	for _, e := range envVars {
		if !strings.Contains(e, "=") {
			_, _ = fmt.Fprintf(os.Stderr, "Failed: -env %q must be KEY=VALUE\n", e)
			os.Exit(2)
		}
	}

	a, err := lookupArch(*flagArch)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Failed: %s\n", err)
		os.Exit(2)
	}

	kvm, err := resolveKVM(*flagKVM, a.goarch)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Failed: %s\n", err)
		os.Exit(2)
	}
	slog.Debug("kvm acceleration", "mode", *flagKVM, "enabled", kvm)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var testArgs, qemuArgs []string

	if flag.NArg() > 1 {
		testArgs = slices.Clone(flag.Args()[1:])
	}
	if *flagTestRun != "" {
		testArgs = append(testArgs, "-test.run", *flagTestRun)
	}
	if *flagTestVerbose {
		testArgs = append(testArgs, "-test.v")
	}
	for _, d := range qemuDevices {
		qemuArgs = append(qemuArgs, "-device", d)
	}
	qemuArgs = append(qemuArgs, parseRawQemuArgs(qemuRawArgs)...)
	qemuArgs = append(qemuArgs, "-smp", strconv.Itoa(*flagSMP), "-m", *flagMem)

	if *flagOut != "" {
		// Share the host dir into the guest over virtio-9p. The guest mounts the
		// "outshare" tag at /out (init/mount_linux.go); mapped-xattr stores guest
		// ownership in xattrs so the host process can still read what tests write.
		outPath, err := filepath.Abs(*flagOut)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "Failed: resolve -out path: %s\n", err)
			os.Exit(1)
		}
		if err := os.MkdirAll(outPath, 0o755); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "Failed: create -out dir: %s\n", err)
			os.Exit(1)
		}
		qemuArgs = append(qemuArgs,
			"-fsdev", "local,id=out,path="+outPath+",security_model=mapped-xattr",
			"-device", "virtio-9p-pci,fsdev=out,mount_tag=outshare")
	}

	var testBuildArgs []string
	if *flagTags != "" {
		testBuildArgs = append(testBuildArgs, "-tags", *flagTags)
	}
	// Coverage needs a coverage-instrumented test binary, but the profile is
	// written at runtime, so when a coverage runtime flag is forwarded build the
	// test binary with -cover. Pairs with -out to extract the profile, e.g.
	// -out ./cov ./pkg -test.coverprofile=/out/cover.out.
	for _, a := range testArgs {
		if strings.HasPrefix(a, "-test.coverprofile") || strings.HasPrefix(a, "-test.covermode") {
			testBuildArgs = append(testBuildArgs, "-cover")
			break
		}
	}

	if *flagTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *flagTimeout)
		defer cancel()
	}

	if err := run(ctx, runOpts{
		arch:           a,
		kernelOverride: *flagKernel,
		kernelRefresh:  *flagKernelRefresh,
		testPath:       testPath,
		testBuildArgs:  testBuildArgs,
		testArgs:       testArgs,
		testEnv:        envVars,
		qemuArgs:       qemuArgs,
		kvm:            kvm,
		keep:           *flagKeep,
		extraCmdline:   strings.Fields(*flagAppend),
	}); err != nil {
		if errors.Is(err, errNoTests) {
			// Match `go test` on a package with no tests: a notice, exit 0.
			fmt.Printf("no test files in %s\n", testPath)
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			// Keep the friendly duration, but preserve any captured console tail.
			var suffix string
			var nv *noVerdictError
			if errors.As(err, &nv) {
				suffix = tailSuffix(nv.tail)
			}
			err = fmt.Errorf("timed out after %s%s", *flagTimeout, suffix)
		}
		_, _ = fmt.Fprintf(os.Stderr, "Failed: %s\n", err)
		os.Exit(1)
	}
}

// resolveKVM decides whether to enable KVM acceleration. "off" never enables,
// "on" always does, and "auto" enables only when the guest arch matches the host
// (KVM cannot accelerate cross-architecture emulation) and /dev/kvm is writable.
func resolveKVM(mode, guestArch string) (bool, error) {
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

// runOpts carries the inputs for a single VM run, assembled from the flags.
type runOpts struct {
	arch           arch
	kernelOverride string
	kernelRefresh  bool // re-validate URL kernels against the remote (-kernel.refresh)
	testPath       string
	testBuildArgs  []string // build flags for the test binary (e.g. -tags, -cover)
	testArgs       []string // argv passed to the test binary in the guest
	testEnv        []string // KEY=VALUE env for the test binary
	qemuArgs       []string // user/resource qemu args, before the arch boot args
	kvm            bool     // enable KVM acceleration (-enable-kvm, -cpu host)
	keep           bool     // keep the temp dir and log the qemu command line
	extraCmdline   []string // extra kernel cmdline tokens (-append), merged after console=
}

func run(ctx context.Context, o runOpts) error {
	a := o.arch
	// Fail fast (before the expensive build) if the arch's emulator is missing.
	if _, err := exec.LookPath(a.qemuBinary); err != nil {
		return fmt.Errorf("qemu binary %q not found for arch %s (install it via your package manager): %w", a.qemuBinary, a.name, err)
	}

	tempPath, err := os.MkdirTemp("", "qemu-test-")
	if err != nil {
		return err
	}
	defer func() {
		if o.keep {
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
	if err := copyEmbedFS(tempRoot, embedFiles); err != nil {
		return fmt.Errorf("failed to copy embed files: %w", err)
	}

	// Resolve the kernel before the (slower) guest build so a bad -kernel path/URL
	// or a missing embedded kernel fails fast.
	kernelPath, err := resolveKernel(ctx, a, o.kernelOverride, o.kernelRefresh, tempPath)
	if err != nil {
		return err
	}

	extraFiles := make(map[string]io.Reader)
	extraFiles["test-args.txt"] = bytes.NewReader([]byte(strings.Join(o.testArgs, "\n")))
	// test-env.txt carries KEY=VALUE lines for the guest to set on the test
	// binary's environment (see -env); empty when none were given.
	extraFiles["test-env.txt"] = bytes.NewReader([]byte(strings.Join(o.testEnv, "\n")))

	ifs, err := os.Create(filepath.Join(tempPath, "initramfs.gz"))
	if err != nil {
		return err
	}
	//nolint:errcheck // data is flushed via the checked gzip.Writer.Close in buildTests; this deferred close only releases the fd on early-return paths.
	defer ifs.Close()
	slog.Debug("Building initramfs")
	target := builder.Target{GOOS: "linux", GOARCH: a.goarch}
	if err := buildTests(ctx, ifs, target, filepath.Join(tempPath, "init"), o.testPath, o.testBuildArgs, extraFiles); err != nil {
		return err
	}

	qmpSock := filepath.Join(tempPath, "qmp.sock")
	qemuArgs := append(o.qemuArgs, a.bootArgs(bootConfig{
		kernelPath:   kernelPath,
		initrdPath:   ifs.Name(),
		qmpSock:      qmpSock,
		kvm:          o.kvm,
		userSetCPU:   slices.Contains(o.qemuArgs, "-cpu"),
		extraCmdline: o.extraCmdline,
	})...)
	slog.Debug("Running qemu", "binary", a.qemuBinary, "args", qemuArgs)
	if o.keep {
		slog.Info("qemu command (-keep)", "cmdline", quoteCommand(a.qemuBinary, qemuArgs))
	}
	qcmd := exec.CommandContext(ctx, a.qemuBinary, qemuArgs...)
	var result int
	const (
		resultUnknown = iota
		resultPass
		resultFail
	)
	// qmp is set once the QMP handshake completes (after the VM starts). The
	// guest forwards QMP commands by printing "[QMP]<json>"; tests only do so
	// well after boot, so by then the connection is established.
	var qmp atomic.Pointer[qmpConn]
	// tail keeps the last few console lines so a run that never reports a verdict
	// (kernel panic, init crash, bad qemu args) can show what the guest last
	// printed instead of a bare "unknown result".
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
						if q := qmp.Load(); q != nil {
							if err := q.send(string(bytes.TrimRight(cmd, "\n"))); err != nil {
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
					// Never return an error from the parser callback: it would
					// propagate out of lp.Write, stop the os/exec copier from
					// draining qemu's stdout, and hang qemu until the deadline.
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
	// only disables QMP-driven tests (e.g. hotplug), which then skip/time out.
	if q, err := dialQMP(qmpSock, 5*time.Second); err != nil {
		slog.Error("connect qmp", "err", err)
	} else {
		qmp.Store(q)
		//nolint:errcheck // QMP is best-effort; a close error cannot affect the test result.
		defer q.Close()
	}
	waitErr := qcmd.Wait()
	// Process any final line the parser buffered without a trailing newline.
	// The qemu output copier goroutines have finished by the time Wait returns,
	// so there is no concurrent writer to race with here.
	if err := lp.flush(); err != nil {
		return err
	}
	if waitErr != nil {
		// A deadline kill surfaces as a generic Wait error (qemu was SIGKILLed);
		// report the timeout instead so the cause is clear. Either way carry the
		// console tail so a boot failure is diagnosable.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return &noVerdictError{msg: "timed out", tail: tail, err: ctx.Err()}
		}
		return &noVerdictError{msg: fmt.Sprintf("qemu failed: %v", waitErr), tail: tail, err: waitErr}
	}

	switch result {
	case resultPass:
		return nil
	case resultFail:
		return errors.New("tests failed")
	}
	// No verdict despite a clean qemu exit: the guest powered off without printing
	// [TESTS:PASS]/[TESTS:FAIL] — a boot/init failure (kernel panic, init crash,
	// missing /tests), not a test failure.
	return &noVerdictError{msg: "guest did not report a pass/fail verdict (boot or init failure)", tail: tail}
}

// noVerdictError reports a run that produced no pass/fail verdict (boot/init
// failure, qemu failure, or timeout). It carries the captured console tail so the
// cause is visible, and unwraps to the underlying cause so errors.Is still works
// (e.g. a timeout still matches context.DeadlineExceeded).
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

// quoteCommand renders a binary + args as a single, roughly copy-pasteable shell
// command, single-quoting any argument that contains whitespace.
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

// resolveKernel returns the host path to the kernel image to boot. Precedence:
// an explicit -kernel override (a local path or an http(s) URL) wins for any arch;
// otherwise the arch's embedded kernel — already copied into tempPath — is used;
// otherwise the arch's default kernelURL is fetched. URL kernels are downloaded
// once and cached (see fetchKernel); refresh re-validates them against the remote.
func resolveKernel(ctx context.Context, a arch, override string, refresh bool, tempPath string) (string, error) {
	// A local -kernel path (not a URL) resolves immediately.
	if override != "" && !isKernelURL(override) {
		path, err := filepath.Abs(override)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("kernel %q: %w", override, err)
		}
		return path, nil
	}

	var fetchURL string
	switch {
	case override != "": // a URL (local paths handled above)
		fetchURL = override
	case a.kernelURL != "":
		fetchURL = a.kernelURL
	default:
		return "", fmt.Errorf("no embedded kernel for arch %q; supply one with -kernel <path-or-URL>", a.name)
	}

	cacheDir, err := kernelCacheDir()
	if err != nil {
		return "", err
	}
	return fetchKernel(ctx, nil, fetchURL, cacheDir, refresh)
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
	// go test -c writes the compiled ELF test binary to stdout when the package
	// has test files; with none it writes a "?  <pkg> [no test files]" notice and
	// no binary. Detect the missing ELF here, before building the initramfs, so we
	// report it clearly instead of packing a non-executable /tests that fails in
	// the guest with "exec format error".
	if !bytes.HasPrefix(testBuf.Bytes(), []byte("\x7fELF")) {
		return errNoTests
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
