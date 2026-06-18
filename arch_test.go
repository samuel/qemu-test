package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestLookupArch(t *testing.T) {
	a, err := lookupArch("amd64")
	if err != nil {
		t.Fatalf("lookupArch(amd64) error: %v", err)
	}
	if a.goarch != "amd64" || a.qemuBinary != "qemu-system-x86_64" {
		t.Fatalf("lookupArch(amd64) = %+v, unexpected goarch/qemuBinary", a)
	}

	_, err = lookupArch("bogus")
	if err == nil {
		t.Fatal("lookupArch(bogus) = nil error, want error")
	}
	if !strings.Contains(err.Error(), "amd64") {
		t.Fatalf("lookupArch(bogus) error %q should list available arches", err)
	}
}

func TestBootArgsAmd64ByteIdentity(t *testing.T) {
	// Guards that the arch abstraction did not change the shipping arch's qemu
	// command: amd64 must emit no -machine/-cpu and console=ttyS0, exactly as the
	// tool did before the registry existed.
	a, err := lookupArch("amd64")
	if err != nil {
		t.Fatal(err)
	}
	got := a.bootArgs("/tmp/k", "/tmp/initramfs.gz", "/tmp/qmp.sock")
	want := []string{
		"-kernel", "/tmp/k",
		"-serial", "stdio",
		"-display", "none",
		"-qmp", "unix:/tmp/qmp.sock,server=on,wait=off",
		"-append", "console=ttyS0",
		"-initrd", "/tmp/initramfs.gz",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("amd64 bootArgs =\n%q\nwant\n%q", got, want)
	}
}

func TestBootArgsDataDriven(t *testing.T) {
	// A non-amd64 entry is pure data: machine/cpu/console and extra cmdline tokens
	// must surface in the right places with no code changes.
	a := arch{
		name:       "arm64",
		goarch:     "arm64",
		qemuBinary: "qemu-system-aarch64",
		machine:    "virt",
		cpu:        "max",
		console:    "ttyAMA0",
		cmdline:    []string{"earlycon"},
	}
	got := a.bootArgs("/tmp/Image", "/tmp/initramfs.gz", "/tmp/qmp.sock")
	want := []string{
		"-kernel", "/tmp/Image",
		"-machine", "virt",
		"-cpu", "max",
		"-serial", "stdio",
		"-display", "none",
		"-qmp", "unix:/tmp/qmp.sock,server=on,wait=off",
		"-append", "console=ttyAMA0 earlycon",
		"-initrd", "/tmp/initramfs.gz",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("arm64 bootArgs =\n%q\nwant\n%q", got, want)
	}
}
