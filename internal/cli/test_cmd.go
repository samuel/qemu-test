package cli

import (
	"cmp"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/samuel/qemu-test/internal/arch"
)

// testCommand implements "qemu-test test": a thin wrapper that runs
// `go test -exec "qemu-test <vm flags>" <go test args>` cross-compiling for the
// guest (GOOS=linux, GOARCH from -arch, CGO_ENABLED=0). Returns go test's exit
// code.
func testCommand(args []string) int {
	fs := flag.NewFlagSet("qemu-test test", flag.ContinueOnError)
	var vm vmFlags
	vm.register(fs)
	flagArch := fs.String("arch", cmp.Or(os.Getenv("GOARCH"), runtime.GOARCH), "Guest architecture (GOARCH name)")
	fs.Usage = func() { testUsage(fs) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if err := vm.validate(); err != nil {
		return fail(2, err)
	}
	if _, err := arch.Lookup(*flagArch); err != nil {
		return fail(2, err)
	}

	goArgs := fs.Args()
	vmArgs := args[:len(args)-len(goArgs)]
	if n := len(vmArgs); n > 0 && vmArgs[n-1] == "--" {
		vmArgs = vmArgs[:n-1]
	}
	vmArgs = dropFlag(fs, vmArgs, "arch")

	self, err := os.Executable()
	if err != nil {
		return fail(1, fmt.Errorf("locate qemu-test executable: %w", err))
	}
	execArg, err := joinQuoted(append([]string{self}, vmArgs...))
	if err != nil {
		return fail(2, err)
	}

	cmd := exec.Command("go", append([]string{"test", "-exec", execArg}, goArgs...)...)
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+*flagArch, "CGO_ENABLED=0")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return ee.ExitCode()
		}
		return fail(1, fmt.Errorf("run go test: %w", err))
	}
	return 0
}

// dropFlag removes every occurrence of the named flag, with its value, from
// args, a list of flag tokens already parsed successfully by fs.
func dropFlag(fs *flag.FlagSet, args []string, name string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		tok := args[i]
		n, _, hasValue := strings.Cut(strings.TrimLeft(tok, "-"), "=")
		// A flag without an inline value takes the next token, unless boolean.
		var next []string
		if f := fs.Lookup(n); !hasValue && f != nil && !isBoolFlag(f) && i+1 < len(args) {
			i++
			next = args[i : i+1]
		}
		if n == name {
			continue
		}
		out = append(append(out, tok), next...)
	}
	return out
}

func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

func testUsage(fs *flag.FlagSet) {
	out := fs.Output()
	_, _ = fmt.Fprint(out, `qemu-test test runs go test with each package's test binary executed in a VM.

It is shorthand for:
  GOOS=linux GOARCH=<arch> CGO_ENABLED=0 go test -exec "qemu-test <vm flags>" <go test args>

Usage:
  qemu-test test [vm flags] [--] [go test args...]

VM flags end at the first argument that is not a flag, or at "--", which is
needed when go test flags come first (-v and -timeout exist on both sides).

Flags:
`)
	fs.PrintDefaults()
	_, _ = fmt.Fprint(out, `
Examples:
  qemu-test test ./...
  qemu-test test -arch s390x ./pkg -run TestFoo -v
  qemu-test test -module xhci_pci -qemu.device qemu-xhci -- -v -run TestUSB ./pkg
  qemu-test test -- -coverprofile cover.out ./...
`)
}
