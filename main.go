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
	flagArch := flag.String("arch", "amd64", "Guest architecture (GOARCH name); only amd64 ships a kernel, others need -kernel")
	flagKernel := flag.String("kernel", "", "Path to an external kernel image, overriding the embedded kernel for -arch")
	flagTags := flag.String("tags", "", "Build tags to pass when compiling the test binary")
	flagTestRun := flag.String("test.run", "", "Run only tests matching regular expression")
	flagTestVerbose := flag.Bool("test.v", false, "Enable verbose test output")
	flagVerbose := flag.Bool("v", false, "Enable verbose (debug) output")
	flagSMP := flag.Int("smp", 2, "Number of guest vCPUs")
	flagMem := flag.String("mem", "1024", "Guest RAM in MB (a bare -m value, so a qemu suffix like \"2G\" also works)")
	flagOut := flag.String("out", "", "Host dir shared writable into the guest at /out (e.g. -out ./cov ... -test.coverprofile=/out/cover.out)")
	flagTimeout := flag.Duration("timeout", 5*time.Minute, "Max wall-clock for the VM run; 0 = no limit")
	flag.Usage = usage
	flag.Parse()

	logLevel := new(slog.LevelVar) // Info by default
	if *flagVerbose {
		logLevel.Set(slog.LevelDebug)
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// Drop time since the output is already logged with time.
			if a.Key == "time" {
				return slog.Attr{}
			}
			return a
		},
	})
	slog.SetDefault(slog.New(h))

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

	if err := run(ctx, a, *flagKernel, testPath, testBuildArgs, testArgs, envVars, qemuArgs); err != nil {
		if errors.Is(err, errNoTests) {
			// Match `go test` on a package with no tests: a notice, exit 0.
			fmt.Printf("no test files in %s\n", testPath)
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("timed out after %s", *flagTimeout)
		}
		_, _ = fmt.Fprintf(os.Stderr, "Failed: %s\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, a arch, kernelOverride, testPath string, testBuildArgs, testArgs, testEnv, qemuArgs []string) error {
	// Fail fast (before the expensive build) if the arch's emulator is missing.
	if _, err := exec.LookPath(a.qemuBinary); err != nil {
		return fmt.Errorf("qemu binary %q not found for arch %s (install it via your package manager): %w", a.qemuBinary, a.name, err)
	}

	tempPath, err := os.MkdirTemp("", "qemu-test-")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(tempPath); err != nil {
			slog.Error("Failed to remove temp dir", "path", tempPath, "err", err)
		}
	}()
	tepRoot, err := os.OpenRoot(tempPath)
	if err != nil {
		return err
	}
	//nolint:errcheck
	defer tepRoot.Close()
	if err := copyEmbedFS(tepRoot, embedFiles); err != nil {
		return fmt.Errorf("failed to copy embed files: %w", err)
	}

	// Resolve the kernel before the (slower) guest build so a bad -kernel path or
	// a missing embedded kernel fails fast.
	kernelPath, err := resolveKernel(a, kernelOverride, tempPath)
	if err != nil {
		return err
	}

	extraFiles := make(map[string]io.Reader)
	extraFiles["test-args.txt"] = bytes.NewReader([]byte(strings.Join(testArgs, "\n")))
	// test-env.txt carries KEY=VALUE lines for the guest to set on the test
	// binary's environment (see -env); empty when none were given.
	extraFiles["test-env.txt"] = bytes.NewReader([]byte(strings.Join(testEnv, "\n")))

	ifs, err := os.Create(filepath.Join(tempPath, "initramfs.gz"))
	if err != nil {
		return err
	}
	//nolint:errcheck
	defer ifs.Close()
	slog.Debug("Building initramfs")
	target := builder.Target{GOOS: "linux", GOARCH: a.goarch}
	if err := buildTests(ctx, ifs, target, filepath.Join(tempPath, "init"), testPath, testBuildArgs, extraFiles); err != nil {
		return err
	}

	slog.Debug("Running qemu")
	qmpSock := filepath.Join(tempPath, "qmp.sock")
	qemuArgs = append(qemuArgs, a.bootArgs(kernelPath, ifs.Name(), qmpSock)...)
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
	lp := &logParser{
		fn: func(key, line []byte) error {
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
		//nolint:errcheck
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
		// report the timeout instead so the cause is clear.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("timed out: %w", ctx.Err())
		}
		return fmt.Errorf("qemu failed: %w", waitErr)
	}

	switch result {
	case resultPass:
		return nil
	case resultFail:
		return errors.New("tests failed")
	}
	return fmt.Errorf("unknown result %d", result)
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
			return out.Mkdir(path, 0755)
		}
		fi, err := efs.Open(path)
		if err != nil {
			return err
		}
		fo, err := out.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0644)
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

// resolveKernel returns the host path to the kernel image to boot. An explicit
// -kernel override wins (and works for any arch, including those with no embedded
// kernel); otherwise the arch's embedded kernel — already copied into tempPath —
// is used, after confirming it exists.
func resolveKernel(a arch, override, tempPath string) (string, error) {
	if override != "" {
		path, err := filepath.Abs(override)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("kernel %q: %w", override, err)
		}
		return path, nil
	}
	if a.kernelPath == "" {
		return "", fmt.Errorf("no embedded kernel for arch %q; supply one with -kernel <path>", a.name)
	}
	if _, err := fs.Stat(embedFiles, a.kernelPath); err != nil {
		return "", fmt.Errorf("no embedded kernel for arch %q; supply one with -kernel <path>", a.name)
	}
	return filepath.Join(tempPath, a.kernelPath), nil
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
		return fmt.Errorf("failed to close gzip file: %s", err)
	}
	return nil
}
