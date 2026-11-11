package cli

import (
	"flag"
	"fmt"
	"strings"
)

// MultiStringFlag is a flag.Value that accumulates repeated string values.
type MultiStringFlag []string

func (f *MultiStringFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(*f, ",")
}

func (f *MultiStringFlag) Set(value string) error {
	if f != nil {
		*f = append(*f, value)
	}
	return nil
}

// usage prints the help message for the default command.
func usage(fs *flag.FlagSet) {
	out := fs.Output()
	_, _ = fmt.Fprint(out, `qemu-test runs a static Linux executable inside a freshly-booted QEMU VM and
exits with its exit code. It is built to be the -exec program of go test.

Usage:
  qemu-test [flags] <binary> [args...]
  go test -exec "qemu-test [flags]" [go test args...]

The guest architecture is read from the binary. Flags may also be supplied in
the QEMU_TEST_FLAGS environment variable.

Commands:
  test           Run go test with test binaries executed in a VM (sets GOOS/GOARCH)
  install-exec   Symlink go_linux_<arch>_exec so cross-compiled go test runs in a VM
  cache          Manage the kernel/bundle download cache (info, list, clear, dir)
  bundle         Build a boot bundle (.tgz) from a JSON config
  Run "qemu-test <command> -h" for command-specific help.

Flags:
`)
	fs.PrintDefaults()
	_, _ = fmt.Fprint(out, `
Examples:
  GOOS=linux go test -exec qemu-test ./...
  GOOS=linux GOARCH=arm64 go test -exec "qemu-test -module xhci_pci -qemu.device qemu-xhci" ./pkg
  GOOS=linux go test -exec "qemu-test -env KEY=value" -run TestFoo -v ./pkg
  GOOS=linux go test -exec qemu-test -coverprofile cover.out ./pkg
  qemu-test test -arch s390x ./...
  qemu-test ./pkg.test -test.run TestFoo -test.v
  qemu-test -kernel https://example.com/vmlinuz -refresh ./pkg.test
  qemu-test -bundle ./boot -module xhci_pci ./pkg.test
  qemu-test -append "loglevel=7 quiet" -keep -v ./pkg.test
  qemu-test -qemu.arg "-drive if=none,id=d0,file=disk.img,format=raw" ./pkg.test
  qemu-test -share ./out:/out ./pkg.test
`)
}

// parseRawQemuArgs converts each -qemu.arg value into qemu argv tokens, splitting
// on the first whitespace so "-drive if=none,..." becomes two tokens. Any later
// whitespace stays in the value.
func parseRawQemuArgs(raw []string) []string {
	var args []string
	for _, a := range raw {
		if a = strings.TrimSpace(a); a == "" {
			continue
		}
		f, v, _ := strings.Cut(a, " ")
		args = append(args, strings.TrimSpace(f))
		if v = strings.TrimSpace(v); v != "" {
			args = append(args, v)
		}
	}
	return args
}
