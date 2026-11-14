# qemu-test

Run Go tests (or any static Linux executable) inside a freshly booted QEMU virtual machine
instead of on the host.

`qemu-test` is an **exec wrapper** for `go test`: `go test` builds each package's test binary
and hands it to `qemu-test`, which packs it together with a tiny PID-1 `init` into an initramfs,
boots the matching qemu system emulator with a Linux kernel, runs the binary as init's child,
streams its output back over the serial console, and exits with its exit code. Everything
`go test` does (`./...`, `-run`, `-v`, `-json`, `-count`, `-tags`, `-cover`) keeps working,
because `go test` is still the one doing it.

Each run gets a clean, throwaway kernel and root filesystem. That suits tests that need
real-kernel behavior (block devices, USB, mounts, syscalls, device hotplug, cgroups) that
userspace tests on the host can't reproduce, and it neither touches the host nor needs root on
it.

## Requirements

- The **qemu system emulator** for the guest architecture on your `PATH`, e.g.
  `qemu-system-x86_64` for an `amd64` guest (`brew install qemu`, `apt install qemu-system-x86`).
- **Go 1.26+**: `go test` builds the test binaries, and the tool compiles the guest `init`.
- Network access on the first run for each architecture, to download its kernel and modules
  from Debian. They are cached afterwards (see
  [Kernels, bundles & modules](#kernels-bundles--modules)). No separate kernel is needed.

## Install

```sh
go install .                 # from a checkout; or: go build -o qemu-test .
```

The examples below assume `qemu-test` is on your `PATH`.

## Quick start

```sh
# Run every package's tests, each test binary in its own VM
qemu-test test ./...

# go test flags work as usual; -arch picks the guest architecture
qemu-test test -arch s390x ./internal/guesttests -run TestEnv -v
```

`qemu-test test` is shorthand for the explicit form, which you can use directly:

```sh
GOOS=linux GOARCH=arm64 go test -exec qemu-test ./...
```

The bundled `guesttests` package is a working sample: `TestEnv` passes by default and fails
only when `GUESTTEST_FAIL` is set (see [Environment variables](#environment-variables)).

## Usage

There are three ways in, all running the same thing.

**1. As the `-exec` program of `go test`** (or `go run`). VM flags go inside the `-exec` string:

```sh
GOOS=linux go test -exec qemu-test ./...
GOOS=linux GOARCH=arm64 go test -exec "qemu-test -module xhci_pci -qemu.device qemu-xhci" ./usb
```

`go test` runs the `-exec` program from each package's directory, so name it by absolute path
(`-exec "$PWD/qemu-test"`) unless it is on `PATH`.

**2. Through the `test` wrapper**, which sets `GOOS=linux`, `GOARCH` (from `-arch`, default
`$GOARCH` or the host's), and `CGO_ENABLED=0`, then runs `go test -exec`:

```
qemu-test test [vm flags] [-arch A] [--] [go test args...]
```

VM flags end at the first argument that is not a flag, or at `--`. Use `--` when go test flags
come first, since `-v` and `-timeout` exist on both sides:

```sh
qemu-test test ./pkg -run TestFoo -v              # go test's -run and -v
qemu-test test -smp 4 -- -v -run TestFoo ./pkg    # VM flag, then go test flags
```

**3. Automatically, with no `-exec` at all.** When cross-compiling, the go command runs binaries
through a program named `go_$GOOS_$GOARCH_exec` if one is on `PATH`. `qemu-test install-exec`
creates those symlinks for every supported arch (in the given directory, or beside the
`qemu-test` executable when none is given):

```sh
qemu-test install-exec ~/bin                 # ~/bin/go_linux_<arch>_exec -> qemu-test
GOOS=linux GOARCH=riscv64 go test ./...      # runs in a VM
```

Runs started this way take their VM flags from the `QEMU_TEST_FLAGS` environment variable
(also honored by the other forms, prepended to the command line):

```sh
QEMU_TEST_FLAGS='-mem 2G -module xhci_pci' GOOS=linux GOARCH=arm64 go test ./...
```

Underneath all three is the direct form, which runs any static Linux executable:

```
qemu-test [flags] <binary> [args...]
```

```sh
GOOS=linux go test -c -o pkg.test ./pkg && qemu-test pkg.test -test.run TestFoo -test.v
```

- The **guest architecture is read from the binary's ELF header**; the direct form has no arch
  flag.
- **Flags** go before the binary; everything after it is the binary's argv, verbatim.
- The binary must be **statically linked**: the guest has no dynamic loader or libraries. Build
  with `GOOS=linux CGO_ENABLED=0`. Cross-compiling disables cgo by default and the `test`
  wrapper sets both, so `CGO_ENABLED=0` only needs spelling out when building on a Linux host
  for its own architecture.
- One VM boots per test binary, i.e. per package. `go test` runs packages in parallel (`-p`),
  so several VMs may run at once.
- `go test` does not cache test results when an `-exec` program is used, so every run boots.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-v` | `false` | Verbose (debug) tool logging, including kernel/boot console output |
| `-env KEY=VALUE` | | Set an env var for the binary; repeatable (see [Environment variables](#environment-variables)) |
| `-kernel <path-or-URL>` | | Bare kernel image overriding the arch default; a URL is downloaded and cached (see [Kernels and bundles from a URL](#kernels-and-bundles-from-a-url)). Mutually exclusive with `-bundle` |
| `-bundle <path-or-URL>` | | Boot bundle (tgz/tar or directory) supplying the kernel plus matching modules, overriding the arch default; a URL is downloaded and cached. Mutually exclusive with `-kernel` |
| `-refresh` | `false` | Re-validate a URL kernel/bundle (or the default bundle's upstream sources) against the remote; re-download only if changed |
| `-module <name>` | | Load a kernel module in the guest before the binary runs (dependencies auto-resolved); repeatable. Skipped when the module is built into the kernel |
| `-smp <n>` | `2` | Guest vCPUs |
| `-mem <m>` | `1024` | Guest RAM in MB (a bare qemu `-m` value, so `2G` works) |
| `-kvm auto\|on\|off` | `auto` | KVM acceleration: `auto` enables it when the guest arch matches the host and `/dev/kvm` is writable, else falls back to TCG emulation; `on` requires KVM; `off` forces TCG |
| `-append <tokens>` | | Extra kernel command-line tokens, merged after `console=...` (e.g. `-append "loglevel=7 quiet"`) |
| `-timeout <dur>` | derived | Max wall clock for the whole run (boot included); `0` disables the limit. Unset, it is the binary's `-test.timeout` plus 1m (`go test` always passes one), else `5m` |
| `-keep` | `false` | Keep the temp dir and log the qemu command line instead of removing the dir on exit |
| `-cwd ro\|rw\|none` | `ro` | Share the current directory into the guest at the same path and run the binary there (see [Working directory and shares](#working-directory-and-shares)) |
| `-share <spec>` | | Share a host dir into the guest, as `hostdir[:guestpath][:ro]`; repeatable |
| `-qemu.device <dev>` | | Add a qemu `-device`; repeatable |
| `-qemu.arg <arg>` | | Pass a raw argument to qemu; repeatable |

The `test` wrapper accepts the same flags plus `-arch <goarch>`.

## Kernels, bundles & modules

By default the tool boots a **boot bundle** rather than a bare kernel: a kernel packed together
with its kernel modules, so the two always match. Every supported architecture has a default
bundle, built from Debian packages and cached on first run (see
[Architectures](#architectures)), so no flags are needed. To boot something else:

- `-bundle <path-or-URL>` points at another bundle: a `.tgz`/`.tar` or a local directory.
- `-kernel <path-or-URL>` supplies a bare kernel with no modules.

The two are mutually exclusive.

Inside a bundle the tool looks for this layout:

```
bundle.tgz
├── vmlinuz                       # kernel
└── modules/                      # contents of /lib/modules/<release>/
    ├── modules.dep               #   optional (modules.dep.bin also accepted)
    ├── modules.builtin           #   optional (modules.builtin.bin also accepted)
    └── kernel/.../xhci-pci.ko …
```

The modules tree may instead sit at `lib/modules/<release>/`, provided there is only one
release. A bundle without a modules tree is treated as a bare kernel.

### Loading modules

`-module <name>` (repeatable) loads a module in the guest before the binary runs, with its
dependencies loaded first. Dependencies come from the bundle's `modules.dep` when present, or
otherwise from each module's ELF `.modinfo` (as `depmod` reads them), so a tree that ships only
`.ko` files still works. You name the capability you care about (say `-module xhci_pci` for USB
xHCI) without worrying whether a given kernel compiles it in or builds it as a module:

- a **loadable** module's `.ko` (plus dependencies) is packed into the initramfs and loaded;
- a module **built into** the kernel, or missing from the tree entirely, has no `.ko` and is
  skipped;
- a `.ko` that is present but fails to load fails the run.

Names follow the kernel's convention, so `-` and `_` are interchangeable (as with `modprobe`).
Module aliases are not resolved; use the module's real name.

```sh
qemu-test test -module xhci_pci -qemu.device qemu-xhci ./usb          # default bundle
qemu-test test -arch arm64 -bundle https://example.com/arm64-boot.tgz -module xhci_pci ./usb
qemu-test test -bundle ./boot -module xhci_pci ./usb                  # ./boot is an extracted bundle directory
```

Only the requested modules and their dependencies are packed, never the whole tree, so the
initramfs stays small. Initramfs content is unpacked into guest RAM, so a large module set may
need more `-mem`; the tool warns when the packed modules are large relative to it. Compressed
modules (`.ko.gz`/`.ko.xz`/`.ko.zst`) need the guest kernel's `CONFIG_MODULE_DECOMPRESS`. With
no modules tree (a bare `-kernel`), every `-module` is skipped and the run proceeds.

### Kernels and bundles from a URL

A `-kernel` or `-bundle` given as an `http(s)://` URL is downloaded once and cached under
`os.UserCacheDir()/qemu-test` (override with the `QEMU_TEST_CACHE` env var), keyed by a hash of
the URL. Caching is **offline-first**: a cached copy is reused with no network access.
`-refresh` re-validates against the remote with a conditional request and re-downloads only
when it changed; an unreachable remote keeps the cached copy.

```sh
qemu-test test -kernel https://example.com/vmlinuz ./internal/guesttests            # download + cache, then reuse
qemu-test test -kernel https://example.com/vmlinuz -refresh ./internal/guesttests   # re-validate
```

### Building bundles (`qemu-test bundle`)

`qemu-test bundle` assembles a bundle from upstream artifacts, such as a distribution's netboot
kernel plus the modules in its initrd, or the kernel and modules out of Debian `linux-*.deb`
packages. It downloads each source (cached like kernels and bundles), pulls files out of
archives by glob, and writes a deterministic `.tgz`:

```sh
qemu-test bundle -config arm.json -o arm-boot.tgz
```

| Flag | Description |
|------|-------------|
| `-config <file>` | The JSON bundle config (required) |
| `-o <file>` | Output bundle path (required) |
| `-cache <dir>` | Download cache dir (default: the shared qemu-test cache dir) |
| `-refresh` | Re-validate cached downloads against the remote; re-download if changed |
| `-v` | Verbose (debug) output |

The config is a JSON object with one key, `"sources"`, an array of sources. Each source names a
download location and how to select files from it. This one takes the Debian netboot kernel
as-is and the module tree out of the installer's initrd:

```json
{
  "sources": [
    {
      "url": "https://deb.debian.org/debian/dists/trixie/main/installer-armhf/current/images/netboot/vmlinuz",
      "dst": "vmlinuz"
    },
    {
      "url": "https://deb.debian.org/debian/dists/trixie/main/installer-armhf/current/images/netboot/initrd.gz",
      "archive": true,
      "files": {
        "usr/lib/modules/*/./**": "modules/"
      }
    }
  ]
}
```

The result drops straight into `-bundle`:

```sh
qemu-test test -arch arm -bundle ./arm-boot.tgz -module xhci_pci ./internal/guesttests
```

See the [Bundle config reference](#bundle-config-reference) for the full schema, the glob
semantics, and resolving the current version of a Debian package instead of pinning a URL.

The default bundles are built the same way: each arch embeds one of these configs, and the tool
builds the bundle on first run and caches it, keyed by a hash of the config. `-refresh`
re-validates the upstream sources and rebuilds.

### Managing the cache

Downloaded kernels and bundles, locally built bundles, and compiled guest inits accumulate in
the cache dir (`os.UserCacheDir()/qemu-test`, override with `QEMU_TEST_CACHE`). `qemu-test
cache` inspects and prunes it. Everything it removes is re-fetched or rebuilt on the next run,
so clearing is always safe.

```sh
qemu-test cache                 # item count + total size on disk (alias of "cache info")
qemu-test cache list            # one row per item: short key, kind, size, fetched-at, source URL
qemu-test cache list -v         # full keys plus each item's on-disk paths
qemu-test cache clear           # remove everything (also sweeps stray temp files)
qemu-test cache clear <key|url> # remove only the named item(s): key (or unique prefix) from "list", or the URL
qemu-test cache dir             # print the cache directory path
```

`list` reports each item's *kind*: `download` (a bare kernel or a bundle build source),
`bundle` (a downloaded bundle with its extracted tree), `built` (a bundle built from an arch's
default config), `init` (a compiled guest init), or `orphan` (a leftover with no body). Built
bundles and inits have no source URL, so clear them by key.

## Guest environment

### Environment variables

`-env KEY=VALUE` sets a variable in the binary's environment inside the guest. The flag is
repeatable; the last value for a key wins. A value with no `=` is a usage error.

The host's environment is not forwarded. PID 1 starts with a near-empty environment, so the
guest fills in defaults for `PATH`, `HOME` (`/root`), and `TMPDIR` (`/tmp`) when they are not
already set. `-env` values override everything else.

```sh
qemu-test test -env DB_URL=memory:// -env DEBUG=1 ./mypkg
```

### Working directory and shares

`go test` runs a test binary in its package directory and passes it host paths (the coverage
directory, profile outputs). The guest mirrors those so tests behave as they do on the host:

- The **current directory** is shared into the guest over virtio-9p **at the same absolute
  path**, read-only, and the binary runs there, so `testdata/` and other relative paths work.
  `-cwd rw` makes it writable; `-cwd none` shares nothing and runs the binary in `/`.
- The directories named by `-test.gocoverdir`, `-test.outputdir`, `-test.coverprofile`, and the
  profile flags (`-test.cpuprofile`, `-test.memprofile`, `-test.blockprofile`,
  `-test.mutexprofile`, `-test.trace`) are shared writable at the same path, so coverage and
  profiles land on the host with no extra flags. A relative path among these makes the current
  directory share writable.

```sh
qemu-test test -- -coverprofile cover.out ./mypkg && go tool cover -html=cover.out
```

`-share hostdir[:guestpath][:ro]` (repeatable) adds your own shares. The guest path defaults to
the host path, and a writable host dir is created if missing:

```sh
qemu-test -share ./out:/out pkg.test      # the binary writes /out/..., the host sees ./out/...
```

Shares need 9p in the guest kernel. The default bundles build it as loadable modules, so any
share adds `9p` and `9pnet_virtio` (and their dependencies) to the module load list
automatically; where 9p is built in, those requests are skipped. The default `arm` and
`riscv64` bundles have **no 9p at all**. There, each share path still exists in the guest as an
empty directory, so the binary runs, but `testdata/` is missing and written files are lost. The
tool logs a warning per share.

### QEMU devices and raw arguments

```sh
qemu-test test -qemu.device qemu-xhci,id=xhci ./mypkg
qemu-test test -qemu.arg "-drive if=none,id=d0,file=disk.img,format=raw" ./mypkg
```

`-qemu.arg` is split on its first whitespace, so a single quoted value like
`"-drive if=none,..."` reaches qemu as the two argv tokens it expects.

### Driving QEMU from inside a test (QMP)

A test running in the guest can control QEMU on the host (e.g. device hotplug) by printing a
line of the form `[QMP]<json>` to stdout. The host forwards that JSON to QEMU's QMP socket and
leaves the line out of the output. The test is responsible for constructing valid QMP commands.
The QMP channel is best-effort: if it fails to connect during boot, only QMP-driven tests are
affected.

## Debugging a run

If the guest never reports an exit status (kernel panic, init crash, bad qemu args, timeout),
the tool reports a boot/init failure, distinct from an ordinary test failure, and prints the
**tail of the console output** so the cause is visible without re-running.

- `-v` shows kernel/boot console output and the resolved KVM mode.
- `-keep` retains the temp dir (initramfs, `qmp.sock`) and logs the exact qemu command line.
- `-append` adds kernel command-line tokens, e.g. `loglevel=7`.

The tool logs to stderr, and `go test` hides a passing test binary's stderr unless given its
own `-v`, so pair them:

```sh
qemu-test test -keep -v -- -v ./mypkg     # logs the qemu command + keeps the temp dir
qemu-test test -append "loglevel=7" -v -- -v ./mypkg
```

## Architectures

The guest architecture is the binary's: build for a `GOARCH` (via `GOARCH=` or the `test`
wrapper's `-arch`) and the tool picks the matching qemu binary. Every arch has a default bundle
(kernel plus modules) built from Debian packages on first run, so it boots with no flags.
`-kernel`/`-bundle` override the default for any arch.

| Arch | QEMU binary | Notes |
|------|-------------|-------|
| `amd64` | `qemu-system-x86_64` | Debian trixie `linux-image` package: kernel + full module set |
| `386` | `qemu-system-x86_64` | Reuses the amd64 bundle and its 64-bit kernel; the 32-bit binary runs via the kernel's IA32 emulation |
| `arm64` | `qemu-system-aarch64` | Debian trixie `linux-image` package, as for amd64 |
| `arm` | `qemu-system-arm` | Debian trixie netboot kernel + the modules in its initrd (the installer's reduced set: USB, but no 9p, so no shares) |
| `riscv64` | `qemu-system-riscv64` | Debian trixie netboot kernel + initrd modules, as for arm (USB, but no 9p, so no shares) |
| `ppc64` | `qemu-system-ppc64` | Big-endian POWER. debian-ports `linux-binary` + `linux-modules` packages: kernel + full module set |
| `ppc64le` | `qemu-system-ppc64` | Little-endian POWER. Debian trixie `linux-image` package, as for amd64 |
| `s390x` | `qemu-system-s390x` | Big-endian IBM Z. Debian trixie `linux-image` package. PCI devices attach via zPCI: qemu wraps a device added with `-qemu.device` automatically, so don't add an explicit `zpci` device |
| `loong64` | `qemu-system-loongarch64` | LoongArch. debian-ports `linux-image` package. Boots through edk2 firmware (`edk2-loongarch64-code.fd`, located in qemu's data dir and passed via `-bios`) to get ACPI tables |

`s390x` and `ppc64` are **big-endian**: the test binary is cross-compiled for that `GOARCH`, so
the guest code runs big-endian, catching byte-order bugs a little-endian host can't.

USB works on every arch with `-module xhci_pci -qemu.device qemu-xhci`. Shares work on every
arch except `arm` and `riscv64`.

```sh
qemu-test test -arch riscv64 ./internal/guesttests
qemu-test test -arch riscv64 -module xhci_pci -qemu.device qemu-xhci ./internal/guesttests  # USB
qemu-test test -arch 386 ./internal/guesttests                              # 32-bit x86 on the amd64 kernel
qemu-test test -arch s390x ./internal/guesttests                            # big-endian IBM Z
qemu-test test -arch arm64 -kernel ./Image ./internal/guesttests            # override with a local kernel
qemu-test test -arch arm64 -kernel https://example.com/arm64/vmlinuz ./internal/guesttests
```

## How it works

1. **Detect** the guest architecture from the binary's ELF header, and **build** the guest
   `init` for it from embedded source (compiled once per arch, then cached).
2. **Pack** init and the binary into a deterministic newc-cpio initramfs, along with
   `test-args.txt` (the binary's argv), `test-env.txt` (`-env` values), `test-cwd.txt` (its
   working directory), `test-shares.txt` (the 9p shares to mount), and any requested kernel
   modules with `test-modules.txt` (their dependency-ordered load list).
3. **Boot** the architecture's qemu binary with the kernel, serial on stdio, display off, a QMP
   control socket, a virtio-9p device per share, a virtio-rng device (so the guest entropy pool
   is seeded and `crypto/rand` doesn't stall at boot), and `-no-reboot` + `panic=-1` (so a
   kernel panic exits qemu immediately with the console tail rather than hanging until the
   timeout).
4. **Run**: `init` mounts `/proc`, `/sys`, `/dev`, etc., brings up the loopback interface (so
   tests can use `127.0.0.1`/`::1`), loads any requested kernel modules, mounts the shares, and
   builds the environment (defaults plus `-env` values). It then runs the binary as its child,
   reaps it and any orphaned grandchildren as PID 1, reports its exit code, and always powers
   the VM off.
5. **Report**: the host writes the binary's stdout and stderr to its own, byte for byte, and
   exits with the binary's exit code.

The guest and host communicate over a small line protocol on the serial console:

- `[TESTS:STDOUT] …` / `[TESTS:STDERR] …`: a line of the binary's output. With a `-` after the
  key (`[TESTS:STDOUT-] …`), a chunk of output that did not end in a newline.
- `[TESTS:EXIT] <n>`: the binary's exit code, which becomes the tool's.
- `[TESTS:FAIL] …`: the binary produced no exit code (guest setup failed, it could not start, or
  a signal killed it).
- `[TESTS:WARN] …`: a non-fatal guest setup problem, logged as a warning.
- Anything else is treated as kernel/boot noise and shown only with `-v`.

## Exit codes

| Code | Meaning |
|------|---------|
| *n* | The binary's own exit code (`0` when tests pass, `1` when they fail) |
| `1` | Tool failure: the guest reported a setup error, the run timed out or was interrupted, or the guest produced no exit status (boot/init failure). The reason is printed to stderr prefixed `qemu-test:` |
| `2` | Usage error (bad flags, missing or unusable binary, malformed `-env`/`-kvm`/`-share`) |

`qemu-test test` exits with `go test`'s exit code.

## Developing this tool

```sh
# Host-side unit tests; no qemu needed
go test ./...

# End-to-end against the sample package (requires the guest arch's qemu binary)
go build -o qemu-test .
./qemu-test test ./internal/guesttests                          # -> ok
./qemu-test test -env GUESTTEST_FAIL=1 ./internal/guesttests    # -> FAIL, exit 1
./qemu-test test ./...                                          # this repo's own tests, in VMs
```

The guest `init` is compiled from the source under `internal/init/`, which is embedded in the
`qemu-test` binary. After changing it, rebuilding `qemu-test` is the only step: the init cache
is keyed by a hash of that source. The change must compile under the embedded `go.mod`.

## Bundle config reference

Each entry in a `qemu-test bundle` config's `"sources"` array combines a download location with
a selection mode.

**Download location**, exactly one of:

- `"url"`: an `http(s)` URL, downloaded and cached. Any other value is read as a local file
  path, so you can repack local artifacts too.
- `"debian"`: a Debian package resolved from a repository's package index (see below).

**Selection mode**, one of:

- *Plain* (the default): the downloaded file is copied to `"dst"`.
- *Archive* (`"archive": true`): files are selected from the archive via `"files"`, a map of
  **glob → destination**. Globs match the files' paths inside the archive.

### Debian packages

A Debian pool `.deb` URL embeds an exact kernel version, so it goes stale. A `debian` block
resolves the current version instead: the bundler fetches the suite's
`binary-<arch>/Packages` index, reads the **meta-package** named by `via`, finds the current
kernel ABI from the dependency matching `abiDep`'s `{abi}` placeholder, substitutes that ABI
into `package`, and downloads that package's `.deb`. The suite stays pinned while the version
within it tracks the latest, picked up on `-refresh`. All seven fields are required. The
default `amd64` bundle is:

```json
{
  "sources": [
    {
      "debian": {
        "mirror": "https://deb.debian.org/debian",
        "suite": "trixie",
        "component": "main",
        "arch": "amd64",
        "via": "linux-image-amd64",
        "abiDep": "linux-image-{abi}-amd64",
        "package": "linux-image-{abi}-amd64-unsigned"
      },
      "archive": true,
      "files": {
        "boot/vmlinuz-*": "vmlinuz",
        "usr/lib/modules/*/./**": "modules/"
      }
    }
  ]
}
```

### Selection semantics

- **Input archives** may be a newc **cpio** (e.g. a Linux `initrd`), a **tar**, or a Debian
  **`.deb`** (its `data.tar` member, the installed files, is read). Any may be compressed with
  gzip, xz, or zstd. The format is detected by content, not extension.
- **Globs** use doublestar semantics: `*` matches within one path segment, `**` matches any
  depth (`usr/lib/modules/**` is the whole tree).
- A **destination ending in `/`** is a directory: the matched path, minus the glob's fixed
  (non-wildcard) prefix, is appended under it. So `usr/lib/modules/**` → `lib/modules/`
  reproduces `lib/modules/<release>/...`.
- A **destination without a trailing `/`** is an exact path, so its glob must match exactly one
  file.
- A **`.` segment is a keep-anchor** (like `rsync -R`'s `/./`): only the part matched *after* it
  is kept under a directory destination. Everything before it is dropped, including a variable
  directory matched by a wildcard. So `usr/lib/modules/*/./**` → `modules/` drops the `<release>` dir and yields a flat
  tree (`modules/modules.dep`, `modules/kernel/...`). At most one `.` is allowed, and no `**`
  may precede it.
- Two files mapping to the same output path is an error.
