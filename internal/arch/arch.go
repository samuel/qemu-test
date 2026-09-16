// Package arch holds the per-architecture registry that drives a guest boot.
package arch

import (
	"fmt"
	"slices"
	"strings"

	"github.com/samuel/qemu-test/internal/bundler"
)

// Arch is the per-architecture configuration needed to boot a guest.
// -kernel/-bundle override the defaults for any arch.
type Arch struct {
	Name       string   // user-facing -arch value
	GOARCH     string   // GOARCH for cross-compiling init + the test binary
	QEMUBinary string   // qemu system emulator, e.g. "qemu-system-x86_64"
	Machine    string   // -machine value; "" omits it
	CPU        string   // -cpu value under TCG; "" omits it
	Console    string   // kernel console= device, e.g. "ttyS0"
	RNGDevice  string   // virtio-rng qemu device; "" defaults to "virtio-rng-pci"
	KernelURL  string   // default bare-kernel URL, downloaded and cached
	Firmware   string   // firmware blob filename to locate and pass via -bios
	Cmdline    []string // extra kernel -append tokens, merged after console=
	ExtraArgs  []string // extra qemu argv, appended verbatim

	// Boot bundle: an archive or directory carrying the kernel together with its
	// matching modules (and optional device tree). The in-bundle paths default to
	// the layout convention (see the boot package).
	BundleConfig      *bundler.Config // built locally into a bundle when set
	BundleURL         string          // default bundle URL (tgz/tar), fetched + cached + extracted
	BundleKernelPath  string          // kernel path within the bundle; "" = "vmlinuz"
	BundleModulesPath string          // modules-tree path; "" = "modules" else "lib/modules/<release>"
	BundleDTBPath     string          // device-tree path passed via -dtb; "" = none
}

// amd64BundleConfig is the default boot bundle for amd64, shared with 386, which
// runs its 32-bit userspace on this same 64-bit kernel.
var amd64BundleConfig = &bundler.Config{
	Sources: []bundler.Source{
		{
			Debian: &bundler.DebianRef{
				Mirror:    "https://deb.debian.org/debian",
				Suite:     "trixie",
				Component: "main",
				Arch:      "amd64",
				Via:       "linux-image-amd64",
				AbiDep:    "linux-image-{abi}-amd64",
				Package:   "linux-image-{abi}-amd64-unsigned",
			},
			Archive: true,
			Files: map[string]string{
				"boot/vmlinuz-*":         "vmlinuz",
				"usr/lib/modules/*/./**": "modules/",
			},
		},
	},
}

// arches is the registry of known architectures keyed by GOARCH-style name (the
// -arch flag value). s390x and ppc64 are the big-endian entries.
var arches = map[string]Arch{
	"amd64": {
		Name:         "amd64",
		GOARCH:       "amd64",
		QEMUBinary:   "qemu-system-x86_64",
		Console:      "ttyS0",
		BundleConfig: amd64BundleConfig,
	},
	// Trixie ships no i386 kernel, so 386 reuses the amd64 bundle and 64-bit
	// kernel: the 32-bit init/tests run under the kernel's IA32 emulation.
	"386": {
		Name:         "386",
		GOARCH:       "386",
		QEMUBinary:   "qemu-system-x86_64",
		Console:      "ttyS0",
		BundleConfig: amd64BundleConfig,
	},
	"arm64": {
		Name:       "arm64",
		GOARCH:     "arm64",
		QEMUBinary: "qemu-system-aarch64",
		Machine:    "virt",
		CPU:        "max",
		Console:    "ttyAMA0",
		BundleConfig: &bundler.Config{
			Sources: []bundler.Source{
				{
					Debian: &bundler.DebianRef{
						Mirror:    "https://deb.debian.org/debian",
						Suite:     "trixie",
						Component: "main",
						Arch:      "arm64",
						Via:       "linux-image-arm64",
						AbiDep:    "linux-image-{abi}-arm64",
						Package:   "linux-image-{abi}-arm64-unsigned",
					},
					Archive: true,
					Files: map[string]string{
						"boot/vmlinuz-*":         "vmlinuz",
						"usr/lib/modules/*/./**": "modules/",
					},
				},
			},
		},
	},
	"arm": {
		Name:       "arm",
		GOARCH:     "arm",
		QEMUBinary: "qemu-system-arm",
		Machine:    "virt,highmem=off", // restrict PCI to the 32-bit address space
		CPU:        "max",
		Console:    "ttyAMA0",
		BundleConfig: &bundler.Config{
			Sources: []bundler.Source{
				{
					URL: "https://deb.debian.org/debian/dists/trixie/main/installer-armhf/current/images/netboot/vmlinuz",
					Dst: "vmlinuz",
				},
				{
					URL:     "https://deb.debian.org/debian/dists/trixie/main/installer-armhf/current/images/netboot/initrd.gz",
					Archive: true,
					Files: map[string]string{
						"usr/lib/modules/*/./**": "modules/",
					},
				},
			},
		},
	},
	"riscv64": {
		Name:       "riscv64",
		GOARCH:     "riscv64",
		QEMUBinary: "qemu-system-riscv64",
		Machine:    "virt",
		CPU:        "max",
		Console:    "ttyS0", // ns16550a UART on the virt machine
		BundleConfig: &bundler.Config{
			Sources: []bundler.Source{
				{
					URL: "https://deb.debian.org/debian/dists/trixie/main/installer-riscv64/current/images/netboot/debian-installer/riscv64/linux",
					Dst: "vmlinuz",
				},
				{
					URL:     "https://deb.debian.org/debian/dists/trixie/main/installer-riscv64/current/images/netboot/debian-installer/riscv64/initrd.gz",
					Archive: true,
					Files: map[string]string{
						"usr/lib/modules/*/./**": "modules/",
					},
				},
			},
		},
	},
	// Not a Debian release arch, so the bundle comes from debian-ports: one
	// linux-image .deb with the kernel and the full module set.
	"loong64": {
		Name:       "loong64",
		GOARCH:     "loong64",
		QEMUBinary: "qemu-system-loongarch64",
		Machine:    "virt",
		CPU:        "max",
		Console:    "ttyS0",                    // 16550 UART on the virt machine
		Firmware:   "edk2-loongarch64-code.fd", // supplies ACPI tables; without them the i8042 driver faults at boot
		BundleConfig: &bundler.Config{
			Sources: []bundler.Source{
				{
					Debian: &bundler.DebianRef{
						Mirror:    "https://deb.debian.org/debian-ports",
						Suite:     "unstable",
						Component: "main",
						Arch:      "loong64",
						Via:       "linux-image-loong64",
						AbiDep:    "linux-image-{abi}-loong64",
						Package:   "linux-image-{abi}-loong64",
					},
					Archive: true,
					Files: map[string]string{
						"boot/vmlinuz-*":         "vmlinuz",
						"usr/lib/modules/*/./**": "modules/",
					},
				},
			},
		},
	},
	// s390x (IBM Z) is big-endian. Its default virtio devices sit on the CCW bus,
	// but the s390-ccw-virtio machine also supports PCI via zPCI: qemu auto-wraps a
	// PCI device added with -qemu.device in a zPCI function, so USB (qemu-xhci) and
	// -out (virtio-9p-pci) both work. Do not add an explicit zpci device; it
	// collides with the auto-created one.
	"s390x": {
		Name:       "s390x",
		GOARCH:     "s390x",
		QEMUBinary: "qemu-system-s390x",
		Machine:    "s390-ccw-virtio",
		CPU:        "max",            // all TCG-emulable facilities
		Console:    "ttysclp0",       // SCLP line console; qemu maps it to -serial stdio
		RNGDevice:  "virtio-rng-ccw", // CCW bus; avoids the zPCI auto-wrap a -pci device triggers
		BundleConfig: &bundler.Config{
			Sources: []bundler.Source{
				{
					Debian: &bundler.DebianRef{
						Mirror:    "https://deb.debian.org/debian",
						Suite:     "trixie",
						Component: "main",
						Arch:      "s390x",
						Via:       "linux-image-s390x",
						AbiDep:    "linux-image-{abi}-s390x",
						Package:   "linux-image-{abi}-s390x",
					},
					Archive: true,
					Files: map[string]string{
						"boot/vmlinuz-*":         "vmlinuz",
						"usr/lib/modules/*/./**": "modules/",
					},
				},
			},
		},
	},
	// Big-endian PowerPC is not a Debian release arch, so the bundle is built from
	// debian-ports .debs: the vmlinux from linux-binary plus the matching module
	// tree from linux-modules (one shared via/abiDep keeps them version-matched).
	"ppc64": {
		Name:       "ppc64",
		GOARCH:     "ppc64",
		QEMUBinary: "qemu-system-ppc64",
		Machine:    "pseries",
		CPU:        "max",  // a recent POWER cpu
		Console:    "hvc0", // spapr-vty hypervisor console; qemu maps it to -serial stdio
		BundleConfig: &bundler.Config{
			Sources: []bundler.Source{
				{
					Debian: &bundler.DebianRef{
						Mirror:    "https://deb.debian.org/debian-ports",
						Suite:     "unstable",
						Component: "main",
						Arch:      "ppc64",
						Via:       "linux-image-powerpc64",
						AbiDep:    "linux-image-{abi}-powerpc64",
						Package:   "linux-binary-{abi}-powerpc64",
					},
					Archive: true,
					Files:   map[string]string{"boot/vmlinux-*": "vmlinuz"},
				},
				{
					Debian: &bundler.DebianRef{
						Mirror:    "https://deb.debian.org/debian-ports",
						Suite:     "unstable",
						Component: "main",
						Arch:      "ppc64",
						Via:       "linux-image-powerpc64",
						AbiDep:    "linux-image-{abi}-powerpc64",
						Package:   "linux-modules-{abi}-powerpc64",
					},
					Archive: true,
					Files:   map[string]string{"usr/lib/modules/*/./**": "modules/"},
				},
			},
		},
	},
	// Little-endian POWER8+ on the same pseries machine as ppc64 (endianness is
	// decided by the kernel). Unlike big-endian ppc64 it is a Debian release arch,
	// so one trixie linux-image .deb supplies both kernel and modules.
	"ppc64le": {
		Name:       "ppc64le",
		GOARCH:     "ppc64le",
		QEMUBinary: "qemu-system-ppc64", // same binary as ppc64; the kernel selects endianness
		Machine:    "pseries",
		CPU:        "max",  // a recent POWER cpu, which is little-endian-capable
		Console:    "hvc0", // spapr-vty hypervisor console; qemu maps it to -serial stdio
		BundleConfig: &bundler.Config{
			Sources: []bundler.Source{
				{
					Debian: &bundler.DebianRef{
						Mirror:    "https://deb.debian.org/debian",
						Suite:     "trixie",
						Component: "main",
						Arch:      "ppc64el",
						Via:       "linux-image-powerpc64le",
						AbiDep:    "linux-image-{abi}-powerpc64le",
						Package:   "linux-image-{abi}-powerpc64le",
					},
					Archive: true,
					Files: map[string]string{
						"boot/vmlinux-*":         "vmlinuz",
						"usr/lib/modules/*/./**": "modules/",
					},
				},
			},
		},
	},
}

// Lookup returns the configuration for the named architecture, or an error
// listing the available names.
func Lookup(name string) (Arch, error) {
	a, ok := arches[name]
	if !ok {
		names := make([]string, 0, len(arches))
		for n := range arches {
			names = append(names, n)
		}
		slices.Sort(names)
		return Arch{}, fmt.Errorf("unknown architecture %q (available: %s)",
			name, strings.Join(names, ", "))
	}
	return a, nil
}

// BootConfig carries the per-run inputs BootArgs needs beyond the static arch
// data: resolved paths plus the accel and kernel-cmdline choices.
type BootConfig struct {
	KernelPath   string
	DTBPath      string // device tree to pass via -dtb; "" omits it
	FirmwarePath string // firmware blob to pass via -bios; "" omits it
	InitrdPath   string
	QMPSock      string
	KVM          bool     // append -enable-kvm and use -cpu host
	UserSetCPU   bool     // user passed -cpu via -qemu.arg; suppress the tool's -cpu
	ExtraCmdline []string // extra kernel -append tokens, merged after console=
}

// BootArgs returns the architecture-derived qemu arguments for a run, excluding
// the binary itself. They are appended after the user's qemu args so the tool's
// -append wins, matching qemu's last-wins rule for the kernel command line.
func (a Arch) BootArgs(c BootConfig) []string {
	args := []string{"-kernel", c.KernelPath}
	if c.DTBPath != "" {
		args = append(args, "-dtb", c.DTBPath)
	}
	if c.FirmwarePath != "" {
		args = append(args, "-bios", c.FirmwarePath)
	}
	if a.Machine != "" {
		args = append(args, "-machine", a.Machine)
	}
	if c.KVM {
		args = append(args, "-enable-kvm")
	}
	switch {
	case c.UserSetCPU:
		// A user-supplied -cpu precedes these args and takes precedence.
	case c.KVM:
		args = append(args, "-cpu", "host")
	case a.CPU != "":
		args = append(args, "-cpu", a.CPU)
	}
	rngDevice := a.RNGDevice
	if rngDevice == "" {
		rngDevice = "virtio-rng-pci"
	}
	args = append(args,
		"-serial", "stdio",
		"-display", "none",
		// Exit rather than reset on a guest-initiated reboot, so panic=-1 below
		// turns a kernel panic into an immediate qemu exit with the console tail.
		"-no-reboot",
		// Seed the guest CRNG: crypto/rand's blocking getrandom would otherwise
		// stall early in boot.
		"-device", rngDevice)
	if c.QMPSock != "" {
		args = append(args, "-qmp", "unix:"+c.QMPSock+",server=on,wait=off")
	}
	args = append(args, a.ExtraArgs...)
	// panic=-1 goes before the user's -append tokens so -append can override it.
	cmdline := append([]string{"console=" + a.Console, "panic=-1"}, a.Cmdline...)
	cmdline = append(cmdline, c.ExtraCmdline...)
	args = append(args, "-append", strings.Join(cmdline, " "), "-initrd", c.InitrdPath)
	return args
}
