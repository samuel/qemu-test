package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"embed"
	"encoding/json"
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
	"github.com/samuel/qemu-test/internal/bundler"
	"github.com/samuel/qemu-test/internal/cache"
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
	var modules MultiStringFlag
	flag.Var(&modules, "module", "Kernel module to load in the guest before tests, by name (repeatable); dependencies are resolved automatically and modules already built into the kernel are skipped")
	flagArch := flag.String("arch", "amd64", "Guest architecture (GOARCH name); the kernel is fetched from the arch's default URL unless -kernel or -bundle is given")
	flagKernel := flag.String("kernel", "", "Path or http(s) URL of a bare kernel image, overriding the arch default; URLs are downloaded and cached. Mutually exclusive with -bundle")
	flagBundle := flag.String("bundle", "", "Path or http(s) URL of a boot bundle (tgz/tar or directory) supplying the kernel plus its matching modules (and optional dtb); URLs are downloaded, cached, and extracted. Mutually exclusive with -kernel")
	flagRefresh := flag.Bool("refresh", false, "Re-validate a URL kernel or bundle against the remote (conditional GET) and re-download/re-extract if changed")
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

	if *flagBundle != "" && *flagKernel != "" {
		_, _ = fmt.Fprintln(os.Stderr, "Failed: -bundle and -kernel are mutually exclusive")
		os.Exit(2)
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
		// The default bundles build 9p as modules (only a bare kernel with
		// CONFIG_9P_FS=y has it built in), so the guest cannot mount the share until
		// the filesystem and the virtio transport are loaded. Request them here; the
		// module resolver pulls in their dependency closure (9pnet, netfs) and
		// harmlessly skips them where 9p is built in or the modules are absent.
		modules = append(modules, "9p", "9pnet_virtio")
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
		bundleOverride: *flagBundle,
		refresh:        *flagRefresh,
		modules:        modules,
		mem:            *flagMem,
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
			if nv, ok := errors.AsType[*noVerdictError](err); ok {
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
	bundleOverride string   // -bundle path or URL (boot bundle: kernel + modules + dtb)
	refresh        bool     // re-validate a URL kernel or bundle against the remote (-refresh)
	modules        []string // kernel modules to load in the guest before tests (-module)
	mem            string   // guest RAM (-mem), used for the module-size guardrail
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

	// Resolve the kernel (and, for a bundle, its modules + dtb) before the (slower)
	// guest build so a bad -kernel/-bundle path/URL or layout fails fast.
	env, err := resolveBootEnv(ctx, a, o, tempPath)
	if err != nil {
		return err
	}

	extraFiles := make(map[string]io.Reader)
	extraFiles["test-args.txt"] = bytes.NewReader([]byte(strings.Join(o.testArgs, "\n")))
	// test-env.txt carries KEY=VALUE lines for the guest to set on the test
	// binary's environment (see -env); empty when none were given.
	extraFiles["test-env.txt"] = bytes.NewReader([]byte(strings.Join(o.testEnv, "\n")))
	// test-modules.txt lists the in-guest .ko paths (dependency order) the guest
	// loads before tests; empty when no modules were requested or all are built in.
	modPaths, err := addModules(o, env.modulesDir, extraFiles)
	if err != nil {
		return err
	}
	extraFiles["test-modules.txt"] = bytes.NewReader([]byte(strings.Join(modPaths, "\n")))

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
		kernelPath:   env.kernelPath,
		dtbPath:      env.dtbPath,
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

// bootEnv is the resolved set of host artifacts needed to boot a guest: the kernel
// plus, when a bundle supplies them, a matching modules tree and a device tree.
type bootEnv struct {
	kernelPath string
	modulesDir string // path to the modules tree (contains modules.dep), or "" if none
	dtbPath    string // device tree to pass via -dtb, or "" if none
}

// resolveBootEnv resolves what to boot. Precedence: a -bundle override → a -kernel
// override (bare kernel) → the arch's bundleConfig (built locally) → the arch's
// bundleURL → the arch's kernelURL (bare kernel) → error. -bundle and -kernel are
// mutually exclusive (rejected earlier). URL kernels/bundles are downloaded once
// and cached; refresh re-validates them. tempPath receives a local tarball bundle's
// extracted tree.
func resolveBootEnv(ctx context.Context, a arch, o runOpts, tempPath string) (bootEnv, error) {
	switch {
	case o.bundleOverride != "":
		return resolveBundle(ctx, a, o.bundleOverride, o.refresh, tempPath)
	case o.kernelOverride != "":
		kp, err := resolveBareKernel(ctx, o.kernelOverride, o.refresh)
		return bootEnv{kernelPath: kp}, err
	case a.bundleConfig != nil:
		return resolveBuiltBundle(ctx, a, o, tempPath)
	case a.bundleURL != "":
		return resolveBundle(ctx, a, a.bundleURL, o.refresh, tempPath)
	case a.kernelURL != "":
		kp, err := resolveBareKernel(ctx, a.kernelURL, o.refresh)
		return bootEnv{kernelPath: kp}, err
	default:
		return bootEnv{}, fmt.Errorf("no kernel or bundle for arch %q; supply one with -kernel or -bundle <path-or-URL>", a.name)
	}
}

// resolveBuiltBundle builds the arch's bundleConfig into a boot bundle with the
// bundler package, then resolves it exactly like a downloaded bundle. The bundle is
// deterministic, so it is cached by a hash of its config (a config change rebuilds)
// to skip the build's heavy work — fetching the upstream sources and selecting files
// out of them — on later runs; the bundler caches the source downloads themselves.
// -refresh re-validates those sources against the remote and rebuilds.
func resolveBuiltBundle(ctx context.Context, a arch, o runOpts, tempPath string) (bootEnv, error) {
	cacheDir, err := cache.CacheDir()
	if err != nil {
		return bootEnv{}, err
	}
	cfgJSON, err := json.Marshal(a.bundleConfig)
	if err != nil {
		return bootEnv{}, fmt.Errorf("encode bundle config for arch %q: %w", a.name, err)
	}
	builtPath := filepath.Join(cacheDir, cache.CacheKey(string(cfgJSON))+".tgz")
	if _, err := os.Stat(builtPath); err != nil || o.refresh {
		if err := bundler.Bundle(ctx, a.bundleConfig, builtPath, cacheDir, o.refresh); err != nil {
			return bootEnv{}, fmt.Errorf("build bundle for arch %q: %w", a.name, err)
		}
	} else {
		slog.Debug("using cached built bundle", "arch", a.name, "path", builtPath)
	}
	// From here the built bundle is used exactly like a downloaded one: extracted
	// into the temp dir and resolved into its kernel/modules/dtb paths.
	return resolveBundle(ctx, a, builtPath, o.refresh, tempPath)
}

// resolveBareKernel resolves a single kernel source (a local path or an http(s)
// URL) to a host path, fetching + caching URL kernels.
func resolveBareKernel(ctx context.Context, src string, refresh bool) (string, error) {
	if !cache.IsHTTPURL(src) {
		path, err := filepath.Abs(src)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("kernel %q: %w", src, err)
		}
		return path, nil
	}
	cacheDir, err := cache.CacheDir()
	if err != nil {
		return "", err
	}
	return cache.FetchKernel(ctx, nil, src, cacheDir, refresh)
}

// resolveBundle resolves a boot bundle from src — an http(s) URL (fetched, cached,
// and extracted), a local tarball (extracted into tempPath), or a local directory
// (used in place) — into the kernel/modules/dtb paths it carries.
func resolveBundle(ctx context.Context, a arch, src string, refresh bool, tempPath string) (bootEnv, error) {
	var dir string
	if cache.IsHTTPURL(src) {
		cacheDir, err := cache.CacheDir()
		if err != nil {
			return bootEnv{}, err
		}
		dir, err = cache.FetchBundle(ctx, nil, src, cacheDir, refresh)
		if err != nil {
			return bootEnv{}, err
		}
	} else {
		abs, err := filepath.Abs(src)
		if err != nil {
			return bootEnv{}, err
		}
		info, err := os.Stat(abs)
		if err != nil {
			return bootEnv{}, fmt.Errorf("bundle %q: %w", src, err)
		}
		if info.IsDir() {
			dir = abs
		} else {
			dir = filepath.Join(tempPath, "bundle")
			if err := cache.ExtractArchiveAtomic(abs, dir, tempPath); err != nil {
				return bootEnv{}, fmt.Errorf("extract bundle %q: %w", src, err)
			}
		}
	}
	return bundleEnv(a, dir)
}

// bundleEnv resolves the kernel/modules/dtb paths inside an extracted bundle
// directory, defaulting to the layout convention and honoring any arch overrides.
func bundleEnv(a arch, dir string) (bootEnv, error) {
	kernelRel := a.bundleKernelPath
	if kernelRel == "" {
		kernelRel = "vmlinuz"
	}
	kernelPath := filepath.Join(dir, kernelRel)
	if _, err := os.Stat(kernelPath); err != nil {
		return bootEnv{}, fmt.Errorf("bundle kernel %q: %w", kernelRel, err)
	}
	modulesDir, err := bundleModulesDir(a, dir)
	if err != nil {
		return bootEnv{}, err
	}
	var dtbPath string
	if a.bundleDTBPath != "" {
		dtbPath = filepath.Join(dir, a.bundleDTBPath)
		if _, err := os.Stat(dtbPath); err != nil {
			return bootEnv{}, fmt.Errorf("bundle dtb %q: %w", a.bundleDTBPath, err)
		}
	}
	return bootEnv{kernelPath: kernelPath, modulesDir: modulesDir, dtbPath: dtbPath}, nil
}

// bundleModulesDir locates the modules tree inside a bundle: the arch override if
// set, else "modules/", else the sole "lib/modules/<release>/". A tree is detected
// by modules.dep or any .ko file (isModuleTree), so trees that ship only modules
// (no depmod output) still count. It returns "" (and no error) when the bundle
// carries no modules tree (a bare kernel; modules built in).
func bundleModulesDir(a arch, dir string) (string, error) {
	if a.bundleModulesPath != "" {
		md := filepath.Join(dir, a.bundleModulesPath)
		if isModuleTree(md) {
			return md, nil
		}
		return "", fmt.Errorf("bundle modules path %q is not a modules tree (no modules.dep or .ko files)", a.bundleModulesPath)
	}
	if md := filepath.Join(dir, "modules"); isModuleTree(md) {
		return md, nil
	}
	entries, err := os.ReadDir(filepath.Join(dir, "lib", "modules"))
	if err != nil {
		return "", nil // no lib/modules → no modules tree
	}
	var releases []string
	for _, e := range entries {
		if e.IsDir() && isModuleTree(filepath.Join(dir, "lib", "modules", e.Name())) {
			releases = append(releases, e.Name())
		}
	}
	switch len(releases) {
	case 0:
		return "", nil
	case 1:
		return filepath.Join(dir, "lib", "modules", releases[0]), nil
	default:
		return "", fmt.Errorf("bundle has multiple module trees under lib/modules (%s); set the arch bundleModulesPath", strings.Join(releases, ", "))
	}
}

// isModuleTree reports whether dir looks like a kernel modules tree: it has a
// modules.dep (text or .bin), or contains at least one .ko file (so a tree that
// ships only modules — e.g. an extracted netboot initrd — is recognized too).
func isModuleTree(dir string) bool {
	if hasModulesDep(dir) {
		return true
	}
	var found bool
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // a walk error just means we didn't find a .ko here
		}
		if !d.IsDir() && isKoFile(d.Name()) {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// addModules resolves the requested -module names against the bundle's modules
// tree and packs each resulting .ko (in dependency order) into extraFiles at the
// initramfs root, by basename, returning the in-guest names the guest loads (see
// test-modules.txt). Packing flat at the root matches the other initramfs files
// (init, tests, test-*.txt) and avoids the cpio archive needing directory records
// (BuildInitramfs emits only regular files, and the kernel's initramfs unpacker
// does not create parent dirs); resolveModules already guarantees unique basenames.
// Modules absent from the tree are presumed built in and skipped. .ko files are
// small (only the requested dependency closure is packed), so they are read into
// memory; a guardrail warns when their total is large relative to guest RAM,
// since initramfs content occupies guest memory.
func addModules(o runOpts, modulesDir string, extraFiles map[string]io.Reader) ([]string, error) {
	load, skipped, err := resolveModules(modulesDir, o.modules)
	if err != nil {
		return nil, err
	}
	if len(skipped) != 0 {
		slog.Debug("modules presumed built in (not loaded)", "modules", skipped)
	}
	var guestNames []string
	var total int64
	for _, m := range load {
		data, err := os.ReadFile(m.hostPath)
		if err != nil {
			return nil, fmt.Errorf("read module %q: %w", m.hostPath, err)
		}
		extraFiles[m.base] = bytes.NewReader(data)
		guestNames = append(guestNames, m.base)
		total += int64(len(data))
		slog.Debug("packing module", "name", m.name, "file", m.base, "bytes", len(data))
	}
	if total > 0 {
		warnModuleMemory(total, o.mem)
	}
	return guestNames, nil
}

// warnModuleMemory warns when the packed modules' bytes are a large fraction
// (>=25%) of guest RAM, since the initramfs is unpacked into guest memory.
func warnModuleMemory(totalBytes int64, mem string) {
	memBytes, ok := parseMemBytes(mem)
	if !ok || memBytes <= 0 {
		return
	}
	if totalBytes*4 >= memBytes {
		slog.Warn("packed kernel modules are large relative to guest RAM; the initramfs is unpacked into guest memory — consider raising -mem",
			"moduleBytes", totalBytes, "memBytes", memBytes)
	}
}

// parseMemBytes parses a qemu -m value (a bare number of MiB, or one with a
// B/K/M/G suffix) into bytes. The bool is false for an unparseable value.
func parseMemBytes(mem string) (int64, bool) {
	s := strings.TrimSpace(mem)
	if s == "" {
		return 0, false
	}
	mult := int64(1 << 20) // qemu's default unit is MiB
	switch s[len(s)-1] {
	case 'b', 'B':
		mult, s = 1, s[:len(s)-1]
	case 'k', 'K':
		mult, s = 1<<10, s[:len(s)-1]
	case 'm', 'M':
		mult, s = 1<<20, s[:len(s)-1]
	case 'g', 'G':
		mult, s = 1<<30, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
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
