package arch

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestLookupArch(t *testing.T) {
	a, err := Lookup("amd64")
	if err != nil {
		t.Fatalf("Lookup(amd64) error: %v", err)
	}
	if a.GOARCH != "amd64" || a.QEMUBinary != "qemu-system-x86_64" {
		t.Fatalf("Lookup(amd64) = %+v, unexpected GOARCH/QEMUBinary", a)
	}

	_, err = Lookup("bogus")
	if err == nil {
		t.Fatal("Lookup(bogus) = nil error, want error")
	}
	if !strings.Contains(err.Error(), "amd64") {
		t.Fatalf("Lookup(bogus) error %q should list available arches", err)
	}
}

func TestBootArgsAmd64ByteIdentity(t *testing.T) {
	// Guards the shipping arch's qemu command: amd64 emits no -machine/-cpu and a
	// console=ttyS0 cmdline, plus the tool's standard -no-reboot, virtio-rng device,
	// and panic=-1 (so a panic exits qemu rather than hanging).
	a, err := Lookup("amd64")
	if err != nil {
		t.Fatal(err)
	}
	got := a.BootArgs(BootConfig{KernelPath: "/tmp/k", InitrdPath: "/tmp/initramfs.gz", QMPSock: "/tmp/qmp.sock"})
	want := []string{
		"-kernel", "/tmp/k",
		"-serial", "stdio",
		"-display", "none",
		"-no-reboot",
		"-device", "virtio-rng-pci",
		"-qmp", "unix:/tmp/qmp.sock,server=on,wait=off",
		"-append", "console=ttyS0 panic=-1",
		"-initrd", "/tmp/initramfs.gz",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("amd64 BootArgs =\n%q\nwant\n%q", got, want)
	}
}

func TestBootArgsKVM(t *testing.T) {
	a, err := Lookup("amd64")
	if err != nil {
		t.Fatal(err)
	}
	// KVM enabled: -enable-kvm appears and (no user -cpu) the tool selects -cpu host.
	got := a.BootArgs(BootConfig{KernelPath: "/tmp/k", InitrdPath: "/tmp/i", QMPSock: "/tmp/q", KVM: true})
	want := []string{
		"-kernel", "/tmp/k",
		"-enable-kvm",
		"-cpu", "host",
		"-serial", "stdio",
		"-display", "none",
		"-no-reboot",
		"-device", "virtio-rng-pci",
		"-qmp", "unix:/tmp/q,server=on,wait=off",
		"-append", "console=ttyS0 panic=-1",
		"-initrd", "/tmp/i",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("kvm BootArgs =\n%q\nwant\n%q", got, want)
	}

	// A user-supplied -cpu suppresses the tool's -cpu host but keeps -enable-kvm.
	got = a.BootArgs(BootConfig{KernelPath: "/tmp/k", InitrdPath: "/tmp/i", QMPSock: "/tmp/q", KVM: true, UserSetCPU: true})
	if slices.Contains(got, "-cpu") {
		t.Fatalf("kvm+UserSetCPU should not emit the tool's -cpu, got %q", got)
	}
	if !slices.Contains(got, "-enable-kvm") {
		t.Fatalf("kvm should still emit -enable-kvm, got %q", got)
	}
}

func TestBootArgsExtraCmdline(t *testing.T) {
	a, err := Lookup("amd64")
	if err != nil {
		t.Fatal(err)
	}
	got := a.BootArgs(BootConfig{KernelPath: "/tmp/k", InitrdPath: "/tmp/i", QMPSock: "/tmp/q", ExtraCmdline: []string{"loglevel=7", "quiet"}})
	want := "console=ttyS0 panic=-1 loglevel=7 quiet"
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
	a := Arch{
		Name:       "arm64",
		GOARCH:     "arm64",
		QEMUBinary: "qemu-system-aarch64",
		Machine:    "virt",
		CPU:        "max",
		Console:    "ttyAMA0",
		Cmdline:    []string{"earlycon"},
	}
	got := a.BootArgs(BootConfig{KernelPath: "/tmp/Image", InitrdPath: "/tmp/initramfs.gz", QMPSock: "/tmp/qmp.sock"})
	want := []string{
		"-kernel", "/tmp/Image",
		"-machine", "virt",
		"-cpu", "max",
		"-serial", "stdio",
		"-display", "none",
		"-no-reboot",
		"-device", "virtio-rng-pci",
		"-qmp", "unix:/tmp/qmp.sock,server=on,wait=off",
		"-append", "console=ttyAMA0 panic=-1 earlycon",
		"-initrd", "/tmp/initramfs.gz",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("arm64 BootArgs =\n%q\nwant\n%q", got, want)
	}
}

func TestBootArgsDTB(t *testing.T) {
	a, err := Lookup("amd64")
	if err != nil {
		t.Fatal(err)
	}
	// With a DTBPath set, -dtb is emitted right after -kernel.
	got := a.BootArgs(BootConfig{KernelPath: "/tmp/k", DTBPath: "/tmp/board.dtb", InitrdPath: "/tmp/i", QMPSock: "/tmp/q"})
	want := []string{
		"-kernel", "/tmp/k",
		"-dtb", "/tmp/board.dtb",
		"-serial", "stdio",
		"-display", "none",
		"-no-reboot",
		"-device", "virtio-rng-pci",
		"-qmp", "unix:/tmp/q,server=on,wait=off",
		"-append", "console=ttyS0 panic=-1",
		"-initrd", "/tmp/i",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dtb BootArgs =\n%q\nwant\n%q", got, want)
	}
	// With no DTBPath, no -dtb appears (amd64 byte-identity is covered separately).
	if slices.Contains(a.BootArgs(BootConfig{KernelPath: "/tmp/k", InitrdPath: "/tmp/i", QMPSock: "/tmp/q"}), "-dtb") {
		t.Fatal("no DTBPath should emit no -dtb")
	}
}

func TestBootArgsBigEndian(t *testing.T) {
	// The big-endian entries are registered and emit their machine/cpu/console.
	tests := []struct {
		name        string
		wantMachine string
		wantConsole string
		wantRng     string // s390x uses the CCW rng variant; the PCI arches the default
	}{
		{"s390x", "s390-ccw-virtio", "ttysclp0", "virtio-rng-ccw"},
		{"ppc64", "pseries", "hvc0", "virtio-rng-pci"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := Lookup(tt.name)
			if err != nil {
				t.Fatalf("Lookup(%s): %v", tt.name, err)
			}
			got := a.BootArgs(BootConfig{KernelPath: "/tmp/k", InitrdPath: "/tmp/i", QMPSock: "/tmp/q"})
			want := []string{
				"-kernel", "/tmp/k",
				"-machine", tt.wantMachine,
				"-cpu", "max",
				"-serial", "stdio",
				"-display", "none",
				"-no-reboot",
				"-device", tt.wantRng,
				"-qmp", "unix:/tmp/q,server=on,wait=off",
				"-append", "console=" + tt.wantConsole + " panic=-1",
				"-initrd", "/tmp/i",
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s BootArgs =\n%q\nwant\n%q", tt.name, got, want)
			}
		})
	}
}
