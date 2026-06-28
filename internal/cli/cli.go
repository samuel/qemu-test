// Package cli is the command layer for qemu-test: it parses flags, dispatches the
// cache/bundle subcommands, assembles a runner.Options from the flags, and maps the
// run result to a process exit code. The root main package is a thin shim over it.
package cli

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/samuel/qemu-test/internal/arch"
	"github.com/samuel/qemu-test/internal/runner"
)

// Main is the program entry point. args is os.Args[1:]; embedFiles carries the
// embedded init source compiled in the guest build (it must be declared in the
// root package, where //go:embed can reach internal/init and go.mod/go.sum). It returns a
// process exit code.
func Main(args []string, embedFiles embed.FS) int {
	// Subcommands are dispatched on a bare first token. A flag (-x) or a package
	// path (./pkg) can never equal a subcommand name, so the existing
	// "qemu-test [flags] <package>" form is unaffected; a package literally named
	// "cache"/"bundle" is still reachable as "./cache".
	if len(args) >= 1 {
		switch args[0] {
		case "cache":
			return cacheCommand(args[1:])
		case "bundle":
			return bundleCommand(args[1:])
		}
	}

	fs := flag.NewFlagSet("qemu-test", flag.ExitOnError)
	var qemuDevices MultiStringFlag
	fs.Var(&qemuDevices, "qemu.device", "Additional devices to pass to qemu (shorthand for -qemu.arg \"-device X\")")
	var qemuRawArgs MultiStringFlag
	fs.Var(&qemuRawArgs, "qemu.arg", "Raw argument(s) to pass to qemu, split on the first whitespace (e.g. -qemu.arg \"-drive if=none,id=d0,file=disk.img,format=raw\"). Repeatable.")
	var envVars MultiStringFlag
	fs.Var(&envVars, "env", "Environment variable KEY=VALUE for the test binary; repeatable")
	var moduleList MultiStringFlag
	fs.Var(&moduleList, "module", "Kernel module to load in the guest before tests, by name (repeatable); dependencies are resolved automatically and modules already built into the kernel are skipped")
	flagArch := fs.String("arch", "amd64", "Guest architecture (GOARCH name); the kernel is fetched from the arch's default URL unless -kernel or -bundle is given")
	flagKernel := fs.String("kernel", "", "Path or http(s) URL of a bare kernel image, overriding the arch default; URLs are downloaded and cached. Mutually exclusive with -bundle")
	flagBundle := fs.String("bundle", "", "Path or http(s) URL of a boot bundle (tgz/tar or directory) supplying the kernel plus its matching modules (and optional dtb); URLs are downloaded, cached, and extracted. Mutually exclusive with -kernel")
	flagRefresh := fs.Bool("refresh", false, "Re-validate a URL kernel or bundle against the remote (conditional GET) and re-download/re-extract if changed")
	flagTags := fs.String("tags", "", "Build tags to pass when compiling the test binary")
	flagTestRun := fs.String("test.run", "", "Run only tests matching regular expression")
	flagTestVerbose := fs.Bool("test.v", false, "Enable verbose test output")
	flagVerbose := fs.Bool("v", false, "Enable verbose (debug) output")
	flagSMP := fs.Int("smp", 2, "Number of guest vCPUs")
	flagMem := fs.String("mem", "1024", "Guest RAM in MB (a bare -m value, so a qemu suffix like \"2G\" also works)")
	flagOut := fs.String("out", "", "Host dir shared writable into the guest at /out (e.g. -out ./cov ... -test.coverprofile=/out/cover.out)")
	flagTimeout := fs.Duration("timeout", 5*time.Minute, "Max wall-clock for the VM run; 0 = no limit")
	flagKVM := fs.String("kvm", "auto", "KVM acceleration: auto (on when the guest arch matches the host and /dev/kvm is writable), on, or off")
	flagAppend := fs.String("append", "", "Extra kernel command-line tokens, merged after console=...; e.g. -append \"loglevel=7 quiet\"")
	flagKeep := fs.Bool("keep", false, "Keep the temp dir (initramfs, built init, qmp.sock) and log the full qemu command line instead of removing it on exit")
	fs.Usage = func() { usage(fs) }
	if err := fs.Parse(args); err != nil {
		return 2 // ExitOnError exits before returning; this keeps the type checker happy
	}

	if *flagVerbose {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	testPath := fs.Arg(0)
	if testPath == "" {
		_, _ = fmt.Fprintln(fs.Output(), "qemu-test: no package provided")
		fs.Usage()
		return 2
	}

	for _, e := range envVars {
		if !strings.Contains(e, "=") {
			_, _ = fmt.Fprintf(os.Stderr, "Failed: -env %q must be KEY=VALUE\n", e)
			return 2
		}
	}

	if *flagBundle != "" && *flagKernel != "" {
		_, _ = fmt.Fprintln(os.Stderr, "Failed: -bundle and -kernel are mutually exclusive")
		return 2
	}

	a, err := arch.Lookup(*flagArch)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Failed: %s\n", err)
		return 2
	}

	kvm, err := runner.ResolveKVM(*flagKVM, a.GOARCH)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Failed: %s\n", err)
		return 2
	}
	slog.Debug("kvm acceleration", "mode", *flagKVM, "enabled", kvm)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var testArgs, qemuArgs []string

	if fs.NArg() > 1 {
		testArgs = append(testArgs, fs.Args()[1:]...)
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
		// "outshare" tag at /out (internal/init/mount_linux.go); mapped-xattr stores guest
		// ownership in xattrs so the host process can still read what tests write.
		outPath, err := filepath.Abs(*flagOut)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "Failed: resolve -out path: %s\n", err)
			return 1
		}
		if err := os.MkdirAll(outPath, 0o755); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "Failed: create -out dir: %s\n", err)
			return 1
		}
		qemuArgs = append(qemuArgs,
			"-fsdev", "local,id=out,path="+outPath+",security_model=mapped-xattr",
			"-device", "virtio-9p-pci,fsdev=out,mount_tag=outshare")
		// The default bundles build 9p as modules (only a bare kernel with
		// CONFIG_9P_FS=y has it built in), so the guest cannot mount the share until
		// the filesystem and the virtio transport are loaded. Request them here; the
		// module resolver pulls in their dependency closure (9pnet, netfs) and
		// harmlessly skips them where 9p is built in or the modules are absent.
		moduleList = append(moduleList, "9p", "9pnet_virtio")
	}

	// BootArgs always attaches a virtio-rng device; load its driver so the guest
	// entropy pool is actually fed (crypto/rand's getrandom can block early in boot
	// before the CRNG is seeded). The Debian bundles build virtio_rng as a module;
	// the resolver skips it where it is built in or the module is absent (e.g. a
	// bare -kernel), so this is a harmless no-op there.
	moduleList = append(moduleList, "virtio_rng")

	var testBuildArgs []string
	if *flagTags != "" {
		testBuildArgs = append(testBuildArgs, "-tags", *flagTags)
	}
	// Coverage needs a coverage-instrumented test binary, but the profile is
	// written at runtime, so when a coverage runtime flag is forwarded build the
	// test binary with -cover. Pairs with -out to extract the profile, e.g.
	// -out ./cov ./pkg -test.coverprofile=/out/cover.out.
	for _, ta := range testArgs {
		if strings.HasPrefix(ta, "-test.coverprofile") || strings.HasPrefix(ta, "-test.covermode") {
			testBuildArgs = append(testBuildArgs, "-cover")
			break
		}
	}

	if err := runner.Run(ctx, runner.Options{
		Arch:           a,
		KernelOverride: *flagKernel,
		BundleOverride: *flagBundle,
		Refresh:        *flagRefresh,
		Modules:        moduleList,
		Mem:            *flagMem,
		TestPath:       testPath,
		TestBuildArgs:  testBuildArgs,
		TestArgs:       testArgs,
		TestEnv:        envVars,
		QEMUArgs:       qemuArgs,
		KVM:            kvm,
		Keep:           *flagKeep,
		ExtraCmdline:   strings.Fields(*flagAppend),
		Timeout:        *flagTimeout,
		EmbedFiles:     embedFiles,
	}); err != nil {
		if errors.Is(err, runner.ErrNoTests) {
			// Match `go test` on a package with no tests: a notice, exit 0.
			fmt.Printf("no test files in %s\n", testPath)
			return 0
		}
		_, _ = fmt.Fprintf(os.Stderr, "Failed: %s\n", err)
		return 1
	}
	return 0
}
