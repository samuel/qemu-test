package main

import (
	"flag"
	"fmt"
	"strings"
)

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

// usage prints the help/usage message. It is wired in as flag.Usage, so it is
// also shown for -h/-help and on flag parse errors.
func usage() {
	out := flag.CommandLine.Output()
	_, _ = fmt.Fprint(out, `qemu-test runs a Go package's tests inside a freshly-booted QEMU VM.

Usage:
  qemu-test [flags] <package> [test args...]

Flags:
`)
	flag.PrintDefaults()
	_, _ = fmt.Fprint(out, `
Examples:
  qemu-test ./tests
  qemu-test -test.run TestFoo -test.v -v ./tests
  qemu-test -env KEY=value -env OTHER=1 ./tests
  qemu-test -kvm on ./tests
  qemu-test -arch arm64 -kernel https://example.com/arm64/vmlinuz ./tests
  qemu-test -kernel https://example.com/vmlinuz -refresh ./tests
  qemu-test -arch arm64 -bundle https://example.com/arm64-boot.tgz -module xhci_pci ./tests
  qemu-test -bundle ./boot -module xhci_pci ./tests
  qemu-test -append "loglevel=7 quiet" ./tests
  qemu-test -keep -v ./tests
  qemu-test -qemu.device qemu-xhci,id=xhci ./tests
  qemu-test -qemu.arg "-drive if=none,id=d0,file=disk.img,format=raw" ./tests
  qemu-test -out ./cov ./tests -test.coverprofile=/out/cover.out
`)
}

// parseRawQemuArgs converts each -qemu.arg value into the argv tokens qemu
// expects. Each value is split on its first whitespace so a single quoted flag
// like "-drive if=none,id=d0,..." reaches qemu as the two tokens
// ["-drive", "if=none,id=d0,..."]; the value keeps any later whitespace. Values
// with no whitespace pass through as a single token, and blank entries are
// skipped.
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
