// Package arch holds the per-architecture registry that drives a guest boot. All
// architecture-specific knowledge lives in the arches table, so adding a new
// architecture is "set a default kernel/bundle source + add one entry" rather than
// scattered code edits.
package arch

import (
	"fmt"
	"slices"
	"strings"

	"github.com/samuel/qemu-test/internal/bundler"
)

// Arch is the per-architecture configuration needed to boot a guest. All
// architecture-specific knowledge lives in the arches registry, so adding a new
// architecture is "set a default KernelURL/BundleURL + add one entry" rather than
// scattered code edits. -kernel/-bundle override the default for any arch.
type Arch struct {
	Name       string   // user-facing -arch value and kernel dir name, e.g. "amd64"
	GOARCH     string   // GOARCH for cross-compiling init + the test binary
	QEMUBinary string   // qemu system emulator, e.g. "qemu-system-x86_64"
	Machine    string   // -machine value; "" omits it (amd64 uses the implicit "pc")
	CPU        string   // -cpu value under TCG; "" omits it (amd64's default boots)
	Console    string   // kernel console= device, e.g. "ttyS0" (amd64), "ttyAMA0" (arm64)
	RNGDevice  string   // virtio-rng qemu device; "" defaults to "virtio-rng-pci" (s390x overrides with the CCW variant)
	KernelURL  string   // default bare-kernel URL, downloaded and cached when there's no embedded kernel; "" = require -kernel/-bundle
	Firmware   string   // edk2 firmware blob filename to locate and pass via -bios; "" = none (loong64 needs it for ACPI)
	Cmdline    []string // extra kernel -append tokens (e.g. "earlycon"), merged after console=
	ExtraArgs  []string // extra qemu argv, appended verbatim, if an arch ever needs them

	// Boot bundle: an archive (or directory) carrying the kernel together with its
	// matching modules (and optional device trees), so the two never mismatch. The
	// in-bundle paths default to a convention (see the boot package) and need
	// overriding only when a bundle's layout differs.
	BundleConfig      *bundler.Config // if set then used to build a bundle
	BundleURL         string          // default bundle URL (tgz/tar), fetched + cached + extracted; "" = none
	BundleKernelPath  string          // kernel path within the bundle; "" = convention ("vmlinuz")
	BundleModulesPath string          // modules-tree path within the bundle; "" = convention ("modules" else "lib/modules/<release>")
	BundleDTBPath     string          // device-tree path within the bundle passed via -dtb; "" = none
}

// arches is the registry of known architectures keyed by GOARCH-style name (the
// -arch flag value). Machine/CPU are intentionally empty for amd64 so its qemu
// invocation is byte-identical to before this abstraction existed. arm64/riscv64
// require -machine virt and a -cpu under TCG; CPU holds the TCG cpu (a future
// -enable-kvm path would substitute "host").
//
// s390x and ppc64 are the big-endian entries (Go's canonical big-endian ports),
// so the test binary cross-compiled for them runs big-endian — register one of
// these to catch byte-order bugs. ppc64 uses the pseries machine's PCI bus, so
// -out (virtio-9p-pci) works; s390x uses the CCW bus (virtio-*-ccw), so its -out
// 9p share does not attach until that device is made bus-aware (basic test runs,
// which need only the serial console + QMP, are unaffected).
//
// 386 has no arch-specific kernel: it reuses amd64BundleConfig and boots on
// qemu-system-x86_64, since a linux/386 (32-bit) guest runs on the amd64 64-bit
// kernel via the kernel's 32-bit ELF emulation (CONFIG_IA32_EMULATION, on in the
// Debian amd64 kernel). Only the cross-compiled init/test binaries differ.

// amd64BundleConfig is the default boot bundle for amd64 — shared with 386, which
// runs its 32-bit userspace on this same 64-bit kernel (see the arches comment).
// The Debian trixie linux-image .deb carries both the bootable kernel
// (/boot/vmlinuz-*, a self-decompressing bzImage qemu's x86 -kernel loader boots
// directly) and the full in-tree module set (/usr/lib/modules/<release>/) in one
// package — so it stays version-matched by construction and exposes every modular
// driver (USB, 9p for -out, ...) via -module, rather than the fixed gokrazy kernel
// that baked drivers in. The "unsigned" image is the plain (non-Secure-Boot) build;
// its bzImage boots in qemu unchanged. The modules are .ko.xz with no modules.dep
// (depmod runs at install time), so dependency resolution reads each module's
// .modinfo (modules.Resolve) and the guest decompresses them in-kernel
// (CONFIG_MODULE_DECOMPRESS). Rather than pin a point release, the .deb is resolved
// from the suite's Packages index: the linux-image-amd64 meta-package names the
// current ABI, which selects the matching unsigned image (see bundler.DebianRef /
// resolveDebianURL). The suite (trixie) stays pinned; the version within it tracks
// the latest, refreshed via -refresh.
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

var arches = map[string]Arch{
	"amd64": {
		Name:         "amd64",
		GOARCH:       "amd64",
		QEMUBinary:   "qemu-system-x86_64",
		Console:      "ttyS0",
		BundleConfig: amd64BundleConfig,
	},
	// 386 (32-bit x86) is first-class in Go. Trixie ships no i386 kernel (i386 is a
	// userland-only port now), so it reuses the amd64 bundle and 64-bit kernel: the
	// 32-bit init/tests run under the kernel's IA32 emulation. Machine/CPU are empty
	// like amd64 (the qemu-system-x86_64 pc machine boots it).
	"386": {
		Name:         "386",
		GOARCH:       "386",
		QEMUBinary:   "qemu-system-x86_64", // 64-bit guest CPU; the 32-bit userspace runs via IA32 emulation
		Console:      "ttyS0",
		BundleConfig: amd64BundleConfig,
	},
	// arm64 mirrors amd64: a default bundle built from the Debian trixie linux-image
	// .deb carrying both the kernel (/boot/vmlinuz-*, a gzip-compressed Image qemu's
	// aarch64 -kernel loader boots on the virt machine) and the full module tree, so
	// USB and other modular drivers (9p for -out, ...) are available via -module on
	// the virt machine's PCI bus. Same .ko.xz/.modinfo notes as amd64, and the .deb
	// is likewise resolved from the suite's Packages index (linux-image-arm64
	// meta-package -> current ABI) rather than version-pinned.
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
		Machine:    "virt,highmem=off", // restrict PCI to 32-bit address space
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
	// riscv64 mirrors arm: it ships a default bundle built from the Debian trixie
	// debian-installer netboot tree (the kernel plus the matching module set out of
	// the netboot initrd), so USB modules are available via -module (e.g. xhci_pci on
	// the virt machine's PCI bus). The kernel is the uncompressed d-i Image, which
	// boots directly via -kernel on qemu's virt machine; a compressed vmlinuz/Image.gz
	// would not — qemu's riscv64 -kernel loader places a raw image at 0x80200000 and
	// does not decompress (riscv64 has no self-decompressing top-level vmlinuz like
	// 32-bit armhf, so the d-i tree nests the raw image under debian-installer/riscv64/).
	// The initrd's modules are .ko.xz with a binary modules.dep.bin (no text
	// modules.dep), resolved by readKmodIndex, and decompressed in-guest
	// (CONFIG_MODULE_DECOMPRESS).
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
	// loong64 (LoongArch 64-bit, little-endian) is a newer ISA that Go and Debian both
	// support. Debian builds the loong64 kernel as a PE32+ EFI-stub image
	// (/boot/vmlinuz-*), but qemu-system-loongarch64's virt-machine -kernel loader boots
	// it directly — it sets up a minimal EFI environment itself, so no edk2/-bios
	// firmware is needed (unlike a full UEFI boot). loong64 is not a Debian release arch,
	// so the bundle comes from debian-ports: one linux-image .deb carrying both the kernel
	// and the full module set (.ko.xz, no modules.dep -> resolved via .modinfo and
	// decompressed in-guest), resolved from the Packages index (linux-image-loong64
	// meta-package -> current ABI; no signed/unsigned split) rather than version-pinned.
	"loong64": {
		Name:       "loong64",
		GOARCH:     "loong64",
		QEMUBinary: "qemu-system-loongarch64",
		Machine:    "virt",
		CPU:        "max",
		Console:    "ttyS0",                    // 16550 UART on the virt machine
		Firmware:   "edk2-loongarch64-code.fd", // edk2 supplies ACPI tables; without it the i8042 driver faults at boot
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
	// s390x (IBM Z) is big-endian: cross-compiling the test binary for it runs the
	// code big-endian, so it catches byte-order bugs. Its default virtio devices sit
	// on the CCW bus, but the s390-ccw-virtio machine also supports PCI via IBM Z's
	// zPCI: a PCI device added with -qemu.device is auto-wrapped in a zPCI function
	// (so plain `-qemu.device qemu-xhci,id=xhci` attaches it — do NOT also add an
	// explicit `zpci` device, which collides with the auto-created one). So USB
	// (qemu-xhci) and -out (virtio-9p-pci) both work here once their modules are
	// present. They come from the default bundle, which mirrors amd64/arm64: the
	// Debian trixie linux-image .deb carries the bootable kernel (/boot/vmlinuz-*,
	// the s390 IPL image, which qemu's s390-ccw-virtio -kernel loader boots directly)
	// plus the full in-tree module set, version-matched by construction and resolved
	// from the suite's Packages index rather than version-pinned (linux-image-s390x
	// meta-package -> current ABI; no signed/unsigned split on s390x, so the target
	// package name has no -unsigned suffix). Same .ko.xz/.modinfo notes as amd64.
	"s390x": {
		Name:       "s390x",
		GOARCH:     "s390x",
		QEMUBinary: "qemu-system-s390x",
		Machine:    "s390-ccw-virtio",
		CPU:        "max",            // all TCG-emulable facilities; kvm branch substitutes "host"
		Console:    "ttysclp0",       // SCLP line console; qemu maps it to -serial stdio.
		RNGDevice:  "virtio-rng-ccw", // CCW bus; avoids the zPCI auto-wrap a -pci device would trigger
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
	// ppc64 (big-endian PowerPC; ppc64le is little-endian) is the other big-endian
	// entry, and the one wired up to exercise code on a big-endian guest: the pseries
	// machine has a PCI bus (so qemu-xhci attaches), but the Debian kernel builds USB
	// as modules, so a bundle is required to get it. ppc64 big-endian is not a Debian
	// release arch (debian-installer ships ppc64el), so there is no netboot
	// kernel+initrd as for arm; the bundle is instead built from debian-ports linux
	// .deb packages — the bootable vmlinux from linux-binary plus the matching module
	// tree from linux-modules. Those modules are .ko.xz with no modules.dep, so
	// dependency resolution reads each module's .modinfo (modules.Resolve)
	// and the guest decompresses them in-kernel (CONFIG_MODULE_DECOMPRESS, set in the
	// Debian kernel). -out (virtio-9p-pci) attaches, but 9p is modular here too, so a
	// test needing the share must load it (e.g. -module 9pnet_virtio). Both .debs are
	// resolved from the debian-ports unstable Packages index rather than version-
	// pinned: the linux-image-powerpc64 meta-package names the current ABI, which is
	// substituted into both the linux-binary and linux-modules package names (see
	// bundler.DebianRef); the two share that ABI so kernel and modules stay matched.
	"ppc64": {
		Name:       "ppc64",
		GOARCH:     "ppc64",
		QEMUBinary: "qemu-system-ppc64",
		Machine:    "pseries",
		CPU:        "max",  // pseries defaults to a recent POWER cpu
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
	// ppc64le (little-endian PowerPC, POWER8+) is the mainstream production POWER arch
	// and first-class in Go. It boots on the same pseries machine as big-endian ppc64
	// (qemu-system-ppc64 runs LE guests; endianness is decided by the kernel, and the
	// recent POWER cpu from -cpu max is LE-capable), so this entry is essentially ppc64
	// with goarch flipped — but with a better kernel story: ppc64el is a Debian *release*
	// arch (unlike big-endian ppc64), so the bundle comes from one trixie main
	// linux-image .deb carrying both the bootable kernel (/boot/vmlinux-*, an LE ELF V2
	// image qemu's pseries -kernel loader boots directly) and the full in-tree module set
	// — version-matched by construction, like amd64/arm64, and resolved from the Packages
	// index (linux-image-powerpc64le meta -> current ABI; no signed/unsigned split, so the
	// target package has no -unsigned suffix) rather than version-pinned. Modules are
	// .ko.xz with no modules.dep, resolved via .modinfo and decompressed in-guest
	// (CONFIG_MODULE_DECOMPRESS). USB (qemu-xhci) and -out (virtio-9p-pci) work on the
	// pseries PCI bus; both are modular, but -out adds its 9p modules automatically.
	"ppc64le": {
		Name:       "ppc64le",
		GOARCH:     "ppc64le",
		QEMUBinary: "qemu-system-ppc64", // same binary as ppc64; the LE kernel selects endianness
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
		return Arch{}, fmt.Errorf("unknown architecture %q (available: %s)", name, strings.Join(names, ", "))
	}
	return a, nil
}

// BootConfig carries the per-run inputs BootArgs needs beyond the static arch
// data: the resolved paths plus the run-time accel and kernel-cmdline choices.
type BootConfig struct {
	KernelPath   string
	DTBPath      string // device tree to pass via -dtb; "" omits it (most machines auto-generate one)
	FirmwarePath string // firmware blob to pass via -bios; "" omits it (only loong64 needs one)
	InitrdPath   string
	QMPSock      string
	KVM          bool     // append -enable-kvm and use -cpu host (native virtualization)
	UserSetCPU   bool     // user already passed -cpu (via -qemu.arg); suppress the tool's -cpu
	ExtraCmdline []string // extra kernel -append tokens (-append flag), merged after console=
}

// BootArgs returns the architecture-derived qemu arguments for a run. The qemu
// binary itself is a.QEMUBinary (the exec target), not part of this slice. These
// are appended after the user's qemu args so the tool's -append wins, matching
// qemu's last-wins rule for the kernel command line.
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
	// -cpu: under KVM use "host" for native speed and feature pass-through;
	// otherwise the arch's TCG cpu. A user-supplied -cpu (via -qemu.arg, which
	// precedes these args) takes precedence, so suppress ours when one was given.
	switch {
	case c.UserSetCPU:
		// keep the user's -cpu
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
		// Exit (rather than reset) on a guest-initiated reboot, so the panic=-1
		// below turns a kernel panic into an immediate qemu exit — the host then
		// reports the captured console tail instead of waiting out the -timeout.
		// The normal shutdown path powers off, which already exits qemu regardless.
		"-no-reboot",
		// Feed the guest entropy pool (the guest loads virtio_rng): crypto/rand uses
		// blocking getrandom, which can stall early in boot before the CRNG is seeded.
		"-device", rngDevice,
		"-qmp", "unix:"+c.QMPSock+",server=on,wait=off")
	args = append(args, a.ExtraArgs...)
	// panic=-1 reboots immediately on a kernel panic; paired with -no-reboot that
	// makes qemu exit at once (a fast, diagnosable failure) instead of hanging until
	// the host -timeout. Placed before the user's -append tokens so -append wins.
	cmdline := append([]string{"console=" + a.Console, "panic=-1"}, a.Cmdline...)
	cmdline = append(cmdline, c.ExtraCmdline...)
	args = append(args, "-append", strings.Join(cmdline, " "), "-initrd", c.InitrdPath)
	return args
}
