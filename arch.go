package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/samuel/qemu-test/internal/bundler"
)

// arch is the per-architecture configuration needed to boot a guest. All
// architecture-specific knowledge lives in the arches registry, so adding a new
// architecture is "set a default kernelURL/bundleURL + add one entry" rather than
// scattered code edits. -kernel/-bundle override the default for any arch.
type arch struct {
	name       string   // user-facing -arch value and kernel dir name, e.g. "amd64"
	goarch     string   // GOARCH for cross-compiling init + the test binary
	qemuBinary string   // qemu system emulator, e.g. "qemu-system-x86_64"
	machine    string   // -machine value; "" omits it (amd64 uses the implicit "pc")
	cpu        string   // -cpu value under TCG; "" omits it (amd64's default boots)
	console    string   // kernel console= device, e.g. "ttyS0" (amd64), "ttyAMA0" (arm64)
	kernelURL  string   // default bare-kernel URL, downloaded and cached when there's no embedded kernel; "" = require -kernel/-bundle
	cmdline    []string // extra kernel -append tokens (e.g. "earlycon"), merged after console=
	extraArgs  []string // extra qemu argv, appended verbatim, if an arch ever needs them

	// Boot bundle: an archive (or directory) carrying the kernel together with its
	// matching modules (and optional device trees), so the two never mismatch. The
	// in-bundle paths default to a convention (see resolveBundle) and need overriding
	// only when a bundle's layout differs.
	bundleConfig      *bundler.Config // if set then used to build a bundle
	bundleURL         string          // default bundle URL (tgz/tar), fetched + cached + extracted; "" = none
	bundleKernelPath  string          // kernel path within the bundle; "" = convention ("vmlinuz")
	bundleModulesPath string          // modules-tree path within the bundle; "" = convention ("modules" else "lib/modules/<release>")
	bundleDTBPath     string          // device-tree path within the bundle passed via -dtb; "" = none
}

// arches is the registry of known architectures keyed by GOARCH-style name (the
// -arch flag value). machine/cpu are intentionally empty for amd64 so its qemu
// invocation is byte-identical to before this abstraction existed. arm64/riscv64
// require -machine virt and a -cpu under TCG; cpu holds the TCG cpu (a future
// -enable-kvm path would substitute "host").
//
// s390x and ppc64 are the big-endian entries (Go's canonical big-endian ports),
// so the test binary cross-compiled for them runs big-endian — register one of
// these to catch byte-order bugs. ppc64 uses the pseries machine's PCI bus, so
// -out (virtio-9p-pci) works; s390x uses the CCW bus (virtio-*-ccw), so its -out
// 9p share does not attach until that device is made bus-aware (basic test runs,
// which need only the serial console + QMP, are unaffected).
var arches = map[string]arch{
	"amd64": {
		name:       "amd64",
		goarch:     "amd64",
		qemuBinary: "qemu-system-x86_64",
		console:    "ttyS0",
		kernelURL:  "https://raw.githubusercontent.com/gokrazy/kernel.amd64/refs/heads/main/vmlinuz", // fetched + cached on first run
	},
	"arm64": {
		name:       "arm64",
		goarch:     "arm64",
		qemuBinary: "qemu-system-aarch64",
		machine:    "virt",
		cpu:        "max",
		console:    "ttyAMA0",
		kernelURL:  "https://raw.githubusercontent.com/gokrazy/kernel.arm64/refs/heads/main/vmlinuz", // fetched + cached on first run
	},
	"arm": {
		name:       "arm",
		goarch:     "arm",
		qemuBinary: "qemu-system-arm",
		machine:    "virt,highmem=off", // restrict PCI to 32-bit address space
		cpu:        "max",
		console:    "ttyAMA0",
		bundleConfig: &bundler.Config{
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
		name:       "riscv64",
		goarch:     "riscv64",
		qemuBinary: "qemu-system-riscv64",
		machine:    "virt",
		cpu:        "max",
		console:    "ttyS0", // ns16550a UART on the virt machine
		// Debian trixie riscv64 debian-installer kernel: an uncompressed Image that boots
		// directly via -kernel on qemu's virt machine. A compressed vmlinuz/Image.gz would
		// not — qemu's riscv64 -kernel loader places a raw image at 0x80200000 and does not
		// decompress. The d-i tree nests it under debian-installer/riscv64/ (riscv64 has no
		// self-decompressing top-level vmlinuz like 32-bit armhf). Fetched + cached on first run.
		kernelURL: "https://deb.debian.org/debian/dists/trixie/main/installer-riscv64/current/images/netboot/debian-installer/riscv64/linux",
	},
	// s390x (IBM Z) is big-endian: cross-compiling the test binary for it runs the
	// code big-endian, so it catches byte-order bugs. Uses the CCW bus, not PCI.
	"s390x": {
		name:       "s390x",
		goarch:     "s390x",
		qemuBinary: "qemu-system-s390x",
		machine:    "s390-ccw-virtio",
		cpu:        "max",      // all TCG-emulable facilities; kvm branch substitutes "host"
		console:    "ttysclp0", // SCLP line console; qemu maps it to -serial stdio.
		// Debian trixie s390x debian-installer kernel: the s390 IPL "generic" image, which
		// self-decompresses and boots directly via -kernel on qemu's s390-ccw-virtio machine
		// (with virtio-ccw). Unlike riscv64, qemu's s390x loader handles the compressed image.
		// Fetched + cached on first run.
		kernelURL: "https://deb.debian.org/debian/dists/trixie/main/installer-s390x/current/images/generic/kernel.debian",
	},
	// ppc64 (big-endian PowerPC; ppc64le is little-endian) is the other big-endian
	// entry, and the one wired up to exercise USB on a big-endian guest: the pseries
	// machine has a PCI bus (so qemu-xhci attaches), but the Debian kernel builds USB
	// as modules, so a bundle is required to get it. ppc64 big-endian is not a Debian
	// release arch (debian-installer ships ppc64el), so there is no netboot
	// kernel+initrd as for arm; the bundle is instead built from debian-ports linux
	// .deb packages — the bootable vmlinux from linux-binary plus the matching module
	// tree from linux-modules. Those modules are .ko.xz with no modules.dep, so
	// dependency resolution reads each module's .modinfo (resolveModules/koDepends)
	// and the guest decompresses them in-kernel (CONFIG_MODULE_DECOMPRESS, set in the
	// Debian kernel). -out (virtio-9p-pci) attaches, but 9p is modular here too, so a
	// test needing the share must load it (e.g. -module 9pnet_virtio).
	"ppc64": {
		name:       "ppc64",
		goarch:     "ppc64",
		qemuBinary: "qemu-system-ppc64",
		machine:    "pseries",
		cpu:        "max",  // pseries defaults to a recent POWER cpu
		console:    "hvc0", // spapr-vty hypervisor console; qemu maps it to -serial stdio
		bundleConfig: &bundler.Config{
			Sources: []bundler.Source{
				{
					URL:     "https://deb.debian.org/debian-ports/pool-ppc64/main/l/linux/linux-binary-7.0.12+deb14-powerpc64_7.0.12-1_ppc64.deb",
					Archive: true,
					Files:   map[string]string{"boot/vmlinux-*": "vmlinuz"},
				},
				{
					URL:     "https://deb.debian.org/debian-ports/pool-ppc64/main/l/linux/linux-modules-7.0.12+deb14-powerpc64_7.0.12-1_ppc64.deb",
					Archive: true,
					Files:   map[string]string{"usr/lib/modules/*/./**": "modules/"},
				},
			},
		},
	},
}

// lookupArch returns the configuration for the named architecture, or an error
// listing the available names.
func lookupArch(name string) (arch, error) {
	a, ok := arches[name]
	if !ok {
		names := make([]string, 0, len(arches))
		for n := range arches {
			names = append(names, n)
		}
		slices.Sort(names)
		return arch{}, fmt.Errorf("unknown architecture %q (available: %s)", name, strings.Join(names, ", "))
	}
	return a, nil
}

// bootConfig carries the per-run inputs bootArgs needs beyond the static arch
// data: the resolved paths plus the run-time accel and kernel-cmdline choices.
type bootConfig struct {
	kernelPath   string
	dtbPath      string // device tree to pass via -dtb; "" omits it (most machines auto-generate one)
	initrdPath   string
	qmpSock      string
	kvm          bool     // append -enable-kvm and use -cpu host (native virtualization)
	userSetCPU   bool     // user already passed -cpu (via -qemu.arg); suppress the tool's -cpu
	extraCmdline []string // extra kernel -append tokens (-append flag), merged after console=
}

// bootArgs returns the architecture-derived qemu arguments for a run. The qemu
// binary itself is a.qemuBinary (the exec target), not part of this slice. These
// are appended after the user's qemu args so the tool's -append wins, matching
// qemu's last-wins rule for the kernel command line.
func (a arch) bootArgs(c bootConfig) []string {
	args := []string{"-kernel", c.kernelPath}
	if c.dtbPath != "" {
		args = append(args, "-dtb", c.dtbPath)
	}
	if a.machine != "" {
		args = append(args, "-machine", a.machine)
	}
	if c.kvm {
		args = append(args, "-enable-kvm")
	}
	// -cpu: under KVM use "host" for native speed and feature pass-through;
	// otherwise the arch's TCG cpu. A user-supplied -cpu (via -qemu.arg, which
	// precedes these args) takes precedence, so suppress ours when one was given.
	switch {
	case c.userSetCPU:
		// keep the user's -cpu
	case c.kvm:
		args = append(args, "-cpu", "host")
	case a.cpu != "":
		args = append(args, "-cpu", a.cpu)
	}
	args = append(args,
		"-serial", "stdio",
		"-display", "none",
		"-qmp", "unix:"+c.qmpSock+",server=on,wait=off")
	args = append(args, a.extraArgs...)
	cmdline := append([]string{"console=" + a.console}, a.cmdline...)
	cmdline = append(cmdline, c.extraCmdline...)
	args = append(args, "-append", strings.Join(cmdline, " "), "-initrd", c.initrdPath)
	return args
}
