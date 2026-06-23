package main

import (
	"reflect"
	"slices"
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
	got := a.bootArgs(bootConfig{kernelPath: "/tmp/k", initrdPath: "/tmp/initramfs.gz", qmpSock: "/tmp/qmp.sock"})
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

func TestBootArgsKVM(t *testing.T) {
	a, err := lookupArch("amd64")
	if err != nil {
		t.Fatal(err)
	}
	// KVM enabled: -enable-kvm appears and (no user -cpu) the tool selects -cpu host.
	got := a.bootArgs(bootConfig{kernelPath: "/tmp/k", initrdPath: "/tmp/i", qmpSock: "/tmp/q", kvm: true})
	want := []string{
		"-kernel", "/tmp/k",
		"-enable-kvm",
		"-cpu", "host",
		"-serial", "stdio",
		"-display", "none",
		"-qmp", "unix:/tmp/q,server=on,wait=off",
		"-append", "console=ttyS0",
		"-initrd", "/tmp/i",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("kvm bootArgs =\n%q\nwant\n%q", got, want)
	}

	// A user-supplied -cpu suppresses the tool's -cpu host but keeps -enable-kvm.
	got = a.bootArgs(bootConfig{kernelPath: "/tmp/k", initrdPath: "/tmp/i", qmpSock: "/tmp/q", kvm: true, userSetCPU: true})
	if slices.Contains(got, "-cpu") {
		t.Fatalf("kvm+userSetCPU should not emit the tool's -cpu, got %q", got)
	}
	if !slices.Contains(got, "-enable-kvm") {
		t.Fatalf("kvm should still emit -enable-kvm, got %q", got)
	}
}

func TestBootArgsExtraCmdline(t *testing.T) {
	a, err := lookupArch("amd64")
	if err != nil {
		t.Fatal(err)
	}
	got := a.bootArgs(bootConfig{kernelPath: "/tmp/k", initrdPath: "/tmp/i", qmpSock: "/tmp/q", extraCmdline: []string{"loglevel=7", "quiet"}})
	want := "console=ttyS0 loglevel=7 quiet"
	for i, v := range got {
		if v == "-append" && i+1 < len(got) {
			if got[i+1] != want {
				t.Fatalf("-append = %q, want %q", got[i+1], want)
			}
			return
		}
	}
	t.Fatalf("no -append in %q", got)
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
	got := a.bootArgs(bootConfig{kernelPath: "/tmp/Image", initrdPath: "/tmp/initramfs.gz", qmpSock: "/tmp/qmp.sock"})
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

func TestBootArgsBigEndian(t *testing.T) {
	// The big-endian entries are registered and emit their machine/cpu/console.
	tests := []struct {
		name        string
		wantMachine string
		wantConsole string
	}{
		{"s390x", "s390-ccw-virtio", "ttysclp0"},
		{"ppc64", "pseries", "hvc0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := lookupArch(tt.name)
			if err != nil {
				t.Fatalf("lookupArch(%s): %v", tt.name, err)
			}
			got := a.bootArgs(bootConfig{kernelPath: "/tmp/k", initrdPath: "/tmp/i", qmpSock: "/tmp/q"})
			want := []string{
				"-kernel", "/tmp/k",
				"-machine", tt.wantMachine,
				"-cpu", "max",
				"-serial", "stdio",
				"-display", "none",
				"-qmp", "unix:/tmp/q,server=on,wait=off",
				"-append", "console=" + tt.wantConsole,
				"-initrd", "/tmp/i",
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s bootArgs =\n%q\nwant\n%q", tt.name, got, want)
			}
		})
	}
}
