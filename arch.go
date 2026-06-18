package main

import (
	"fmt"
	"slices"
	"strings"
)

// arch is the per-architecture configuration needed to boot a guest. All
// architecture-specific knowledge lives in the arches registry, so adding a new
// architecture is "drop in a kernel + add one entry" rather than scattered code
// edits. Today only amd64 ships a kernel; other entries are usable via -kernel.
type arch struct {
	name       string   // user-facing -arch value and kernel dir name, e.g. "amd64"
	goarch     string   // GOARCH for cross-compiling init + the test binary
	qemuBinary string   // qemu system emulator, e.g. "qemu-system-x86_64"
	machine    string   // -machine value; "" omits it (amd64 uses the implicit "pc")
	cpu        string   // -cpu value under TCG; "" omits it (amd64's default boots)
	console    string   // kernel console= device, e.g. "ttyS0" (amd64), "ttyAMA0" (arm64)
	kernelPath string   // path within embedFiles; "" means no embedded kernel (-kernel required)
	cmdline    []string // extra kernel -append tokens (e.g. "earlycon"), merged after console=
	extraArgs  []string // extra qemu argv, appended verbatim, if an arch ever needs them
}

// arches is the registry of known architectures keyed by GOARCH-style name (the
// -arch flag value). machine/cpu are intentionally empty for amd64 so its qemu
// invocation is byte-identical to before this abstraction existed. arm64/riscv64
// require -machine virt and a -cpu under TCG; cpu holds the TCG cpu (a future
// -enable-kvm path would substitute "host").
//
// Only amd64 ships an embedded kernel; arm64/riscv64 are registered so they can
// be run today with -kernel, and so shipping them later is just dropping a kernel
// at the entry's kernelPath (the //go:embed of kernel/ picks it up automatically).
var arches = map[string]arch{
	"amd64": {
		name:       "amd64",
		goarch:     "amd64",
		qemuBinary: "qemu-system-x86_64",
		console:    "ttyS0",
		kernelPath: "kernel/amd64/vmlinuz",
	},
	"arm64": {
		name:       "arm64",
		goarch:     "arm64",
		qemuBinary: "qemu-system-aarch64",
		machine:    "virt",
		cpu:        "max",
		console:    "ttyAMA0",
		kernelPath: "kernel/arm64/vmlinuz", // no kernel committed yet; use -kernel
	},
	"riscv64": {
		name:       "riscv64",
		goarch:     "riscv64",
		qemuBinary: "qemu-system-riscv64",
		machine:    "virt",
		cpu:        "max",
		console:    "ttyS0",                  // verify against the kernel; riscv64 virt may use hvc0
		kernelPath: "kernel/riscv64/vmlinuz", // no kernel committed yet; use -kernel
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

// bootArgs returns the architecture-derived qemu arguments for a run. The qemu
// binary itself is a.qemuBinary (the exec target), not part of this slice. These
// are appended after the user's qemu args so the tool's -append wins, matching
// qemu's last-wins rule for the kernel command line.
func (a arch) bootArgs(kernelPath, initrdPath, qmpSock string) []string {
	args := []string{"-kernel", kernelPath}
	if a.machine != "" {
		args = append(args, "-machine", a.machine)
	}
	if a.cpu != "" {
		args = append(args, "-cpu", a.cpu)
	}
	args = append(args,
		"-serial", "stdio",
		"-display", "none",
		"-qmp", "unix:"+qmpSock+",server=on,wait=off")
	args = append(args, a.extraArgs...)
	cmdline := append([]string{"console=" + a.console}, a.cmdline...)
	args = append(args, "-append", strings.Join(cmdline, " "), "-initrd", initrdPath)
	return args
}
