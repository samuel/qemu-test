// Package cli is the command layer for qemu-test: it parses flags, dispatches
// the subcommands, and maps the guest binary's result to a process exit code.
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/samuel/qemu-test/internal/arch"
	"github.com/samuel/qemu-test/internal/runner"
)

// flagsEnv names the environment variable holding extra VM flags, prepended to
// the command line. It is how flags reach a run started through a
// go_<GOOS>_<GOARCH>_exec symlink, which go invokes with no flags of its own.
const flagsEnv = "QEMU_TEST_FLAGS"

// defaultTimeout bounds a run whose arguments carry no -test.timeout.
const defaultTimeout = 5 * time.Minute

// testTimeoutMargin is added to the binary's -test.timeout so the in-guest test
// timeout (with its goroutine dump) fires before the VM is killed. It matches
// the grace `go test` gives an -exec program.
const testTimeoutMargin = time.Minute

// Main is the program entry point, returning a process exit code. embedFiles
// carries the init source compiled into the guest build; it must be declared in
// the root package, where //go:embed can reach internal/init and go.mod/go.sum.
func Main(args []string, embedFiles embed.FS) int {
	// Subcommands are dispatched on a bare first token, which a flag never is;
	// a binary with a subcommand's name is still reachable as "./cache".
	if len(args) >= 1 {
		switch args[0] {
		case "cache":
			return cacheCommand(args[1:])
		case "bundle":
			return bundleCommand(args[1:])
		case "test":
			return testCommand(args[1:])
		case "install-exec":
			return installExecCommand(args[1:])
		}
	}
	return runCommand(args, embedFiles)
}

// vmFlags are the flags configuring the VM, shared by the default command and
// the test wrapper (which validates them and passes them through).
type vmFlags struct {
	qemuDevices MultiStringFlag
	qemuRawArgs MultiStringFlag
	env         MultiStringFlag
	modules     MultiStringFlag
	shares      MultiStringFlag
	kernel      string
	bundle      string
	refresh     bool
	verbose     bool
	smp         int
	mem         string
	cwd         string
	timeout     time.Duration
	kvm         string
	cmdline     string
	keep        bool
}

func (v *vmFlags) register(fs *flag.FlagSet) {
	fs.Var(&v.qemuDevices, "qemu.device", "Additional devices to pass to qemu (shorthand for -qemu.arg \"-device X\")")
	fs.Var(&v.qemuRawArgs, "qemu.arg", "Raw argument(s) to pass to qemu, split on the first whitespace (e.g. -qemu.arg \"-drive if=none,id=d0,file=disk.img,format=raw\"). Repeatable.")
	fs.Var(&v.env, "env", "Environment variable KEY=VALUE for the binary; repeatable")
	fs.Var(&v.modules, "module", "Kernel module to load in the guest before the binary runs, by name (repeatable); dependencies are resolved automatically and modules already built into the kernel are skipped")
	fs.Var(&v.shares, "share", "Host dir shared into the guest over virtio-9p, as hostdir[:guestpath][:ro] (repeatable); the guest path defaults to the host path")
	fs.StringVar(&v.kernel, "kernel", "", "Path or http(s) URL of a bare kernel image, overriding the arch default; URLs are downloaded and cached. Mutually exclusive with -bundle")
	fs.StringVar(&v.bundle, "bundle", "", "Path or http(s) URL of a boot bundle (tgz/tar or directory) supplying the kernel plus its matching modules (and optional dtb); URLs are downloaded, cached, and extracted. Mutually exclusive with -kernel")
	fs.BoolVar(&v.refresh, "refresh", false, "Re-validate a URL kernel or bundle against the remote (conditional GET) and re-download/re-extract if changed")
	fs.BoolVar(&v.verbose, "v", false, "Enable verbose (debug) output")
	fs.IntVar(&v.smp, "smp", 2, "Number of guest vCPUs")
	fs.StringVar(&v.mem, "mem", "1024", "Guest RAM in MB (a bare -m value, so a qemu suffix like \"2G\" also works)")
	fs.StringVar(&v.cwd, "cwd", cwdReadOnly, "How the current directory reaches the guest: ro or rw shares it at the same path and runs the binary there; none runs the binary in /")
	fs.DurationVar(&v.timeout, "timeout", 0, "Max wall-clock for the VM run; 0 = no limit (default: the binary's -test.timeout plus 1m, else 5m)")
	fs.StringVar(&v.kvm, "kvm", "auto", "KVM acceleration: auto (on when the guest arch matches the host and /dev/kvm is writable), on, or off")
	fs.StringVar(&v.cmdline, "append", "", "Extra kernel command-line tokens, merged after console=...; e.g. -append \"loglevel=7 quiet\"")
	fs.BoolVar(&v.keep, "keep", false, "Keep the temp dir (initramfs, qmp.sock) and log the full qemu command line instead of removing it on exit")
}

// validate checks the flag values that need no knowledge of the binary.
func (v *vmFlags) validate() error {
	for _, e := range v.env {
		if !strings.Contains(e, "=") {
			return fmt.Errorf("-env %q must be KEY=VALUE", e)
		}
	}
	if v.bundle != "" && v.kernel != "" {
		return errors.New("-bundle and -kernel are mutually exclusive")
	}
	switch v.cwd {
	case cwdReadOnly, cwdReadWrite, cwdNone:
	default:
		return fmt.Errorf("invalid -cwd %q (want ro, rw, or none)", v.cwd)
	}
	return nil
}

// flagSet reports whether the named flag was given on the command line.
func flagSet(fs *flag.FlagSet, name string) bool {
	var set bool
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

// fail prints an error attributed to the tool, so it stands out from the guest
// binary's own stderr, and returns the given exit code.
func fail(code int, err error) int {
	_, _ = fmt.Fprintf(os.Stderr, "qemu-test: %s\n", err)
	return code
}

// runCommand implements the default command, the form `go test -exec` invokes:
// boot a VM, run the given Linux executable in it, and exit with its exit code.
// Tool failures (as opposed to the binary's own) exit 1; usage errors exit 2.
func runCommand(args []string, embedFiles embed.FS) int {
	envArgs, err := splitQuoted(os.Getenv(flagsEnv))
	if err != nil {
		return fail(2, fmt.Errorf("%s: %w", flagsEnv, err))
	}
	args = append(envArgs, args...)

	fs := flag.NewFlagSet("qemu-test", flag.ContinueOnError)
	var vm vmFlags
	vm.register(fs)
	fs.Usage = func() { usage(fs) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if vm.verbose {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	binary := fs.Arg(0)
	if binary == "" {
		_, _ = fmt.Fprintln(fs.Output(), "qemu-test: no binary provided")
		fs.Usage()
		return 2
	}
	binArgs := fs.Args()[1:]

	if err := vm.validate(); err != nil {
		return fail(2, err)
	}

	goarch, err := detectArch(binary)
	if err != nil {
		return fail(2, err)
	}
	a, err := arch.Lookup(goarch)
	if err != nil {
		return fail(2, err)
	}

	kvm, err := runner.ResolveKVM(vm.kvm, a.GOARCH)
	if err != nil {
		return fail(2, err)
	}
	slog.Debug("kvm acceleration", "mode", vm.kvm, "enabled", kvm)

	cwd, err := os.Getwd()
	if err != nil {
		return fail(1, err)
	}
	workDir, shares, err := planShares(vm.shares, vm.cwd, cwd, binArgs)
	if err != nil {
		return fail(2, err)
	}

	timeout := vm.timeout
	if !flagSet(fs, "timeout") {
		timeout = defaultTimeout
		if d, ok := testTimeout(binArgs); ok {
			timeout = 0
			if d > 0 {
				timeout = d + testTimeoutMargin
			}
		}
	}

	var qemuArgs []string
	for _, d := range vm.qemuDevices {
		qemuArgs = append(qemuArgs, "-device", d)
	}
	qemuArgs = append(qemuArgs, parseRawQemuArgs(vm.qemuRawArgs)...)
	qemuArgs = append(qemuArgs, "-smp", strconv.Itoa(vm.smp), "-m", vm.mem)

	// BootArgs always attaches a virtio-rng device; load its driver so the guest
	// entropy pool is actually fed. Skipped where it is built in.
	moduleList := append(vm.modules, "virtio_rng")

	// go test stops an -exec program with SIGQUIT on timeout; shutting down on it
	// kills qemu and reports the console tail.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)
	defer stop()

	code, err := runner.Run(ctx, runner.Options{
		Arch:           a,
		KernelOverride: vm.kernel,
		BundleOverride: vm.bundle,
		Refresh:        vm.refresh,
		Modules:        moduleList,
		Mem:            vm.mem,
		Binary:         binary,
		Args:           binArgs,
		Env:            vm.env,
		WorkDir:        workDir,
		Shares:         shares,
		QEMUArgs:       qemuArgs,
		KVM:            kvm,
		Keep:           vm.keep,
		ExtraCmdline:   strings.Fields(vm.cmdline),
		Timeout:        timeout,
		EmbedFiles:     embedFiles,
	})
	if err != nil {
		return fail(1, err)
	}
	return code
}
