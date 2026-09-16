# qemu-test

Run a Go package's tests inside a freshly-booted QEMU virtual machine instead of on the host.

`qemu-test` compiles your package's test binary, packs it together with a tiny PID-1 `init`
into an initramfs, boots `qemu-system-x86_64` with a Linux kernel, runs the tests
as init's child, streams their output back over the serial console, and exits `0`/`1` to match
the result.

Each run gets a clean, throwaway kernel and root for tests that need real-kernel behavior
(block devices, USB, mounts, syscalls, device hotplug, cgroups) that ordinary userspace tests on
the host can't reproduce, without touching (or requiring root on) your host.

## Quick start

```sh
# Run every test in ./internal/guesttests inside a VM
qemu-test ./internal/guesttests

# Run a single test, verbose test output, verbose (debug) tool logging
qemu-test -test.run TestFoo -test.v -v ./internal/guesttests
```

The bundled `guesttests` package is a working sample: `TestEnv` passes by default and fails
only when `GUESTTEST_FAIL` is set (see [Environment variables](#environment-variables)).

## Install

```sh
# Build a local binary
go build -o qemu-test .

# …or run without installing
go run . ./your/package
```

## Requirements

- The **qemu system emulator** for your guest architecture on your `PATH`: `qemu-system-x86_64`
  for the default `amd64` guest (e.g. `brew install qemu`, `apt install qemu-system-x86`).
- **Go 1.26+** to build the tool and cross-compile the guest.
- No separate kernel needed. The default `amd64` kernel and module set come from a Debian
  bundle, built and cached on first run (see [Kernels, bundles & modules](#kernels-bundles--modules)).

The guest is `linux/amd64` by default; your test package is cross-compiled with
`CGO_ENABLED=0 GOOS=linux GOARCH=<arch>`. On a Linux host whose architecture matches the guest
and exposes a writable `/dev/kvm`, KVM acceleration is on by default (`-kvm auto`); otherwise
QEMU runs under slower TCG emulation. Use `-kvm on` to require KVM or `-kvm off` to force TCG.

## Usage

```
qemu-test [flags] <package> [test args...]
```

- **`<package>`** is a Go package path (e.g. `./internal/guesttests`, `.`), the same as you'd give `go test`.
- **Tool flags** go *before* the package path.
- **`[test args...]`** after the package path are forwarded verbatim to the test binary inside
  the guest, so anything `go test` accepts works (e.g. `-test.count=2`, `-test.timeout=30s`).
  Note `-timeout` is the wall clock for the *whole* run (boot + tests): a `-test.timeout` longer
  than it never gets a chance, so raise `-timeout` to match.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-test.run <regexp>` | | Only run tests matching the regular expression |
| `-test.v` | `false` | Verbose test output (also passable as a trailing test arg) |
| `-v` | `false` | Verbose (debug) tool logging |
| `-env KEY=VALUE` | | Set an env var for the test binary; repeatable (see [Environment variables](#environment-variables)) |
| `-arch <goarch>` | `amd64` | Guest architecture (a Go `GOARCH` name). Most arches have a default kernel/bundle (see [Architectures](#architectures)) |
| `-kernel <path-or-URL>` | | Kernel image overriding the arch default; a URL is downloaded and cached (see [Kernels from a URL](#kernels-from-a-url)) |
| `-bundle <path-or-URL>` | | Boot bundle (tgz/tar or directory) supplying the kernel plus matching modules; a URL is downloaded and cached. Mutually exclusive with `-kernel` |
| `-refresh` | `false` | Re-validate a URL kernel/bundle against the remote; re-download only if changed |
| `-module <name>` | | Load a kernel module in the guest before tests (dependencies auto-resolved); repeatable. Needs a bundle with a modules tree |
| `-tags <tags>` | | Build tags to pass when compiling the test binary |
| `-smp <n>` | `2` | Guest vCPUs |
| `-mem <m>` | `1024` | Guest RAM (a bare qemu `-m` value, so `2G` works) |
| `-kvm auto\|on\|off` | `auto` | KVM acceleration: `auto` enables it when guest arch matches host and `/dev/kvm` is writable, else TCG |
| `-append <tokens>` | | Extra kernel command-line tokens, merged after `console=...` (e.g. `-append "loglevel=7 quiet"`) |
| `-timeout <dur>` | `5m` | Max wall clock for the whole run (boot + tests), not just the test binary; `0` disables the limit. A `-test.timeout` longer than this is cut short; raise `-timeout` to match |
| `-keep` | `false` | Keep the temp dir and log the qemu command line instead of removing them on exit |
| `-out <dir>` | | Share a host dir into the guest at `/out` (virtio-9p) |
| `-qemu.device <dev>` | | Add a qemu `-device`; repeatable |
| `-qemu.arg <arg>` | | Pass a raw argument to qemu; repeatable |

## Kernels, bundles & modules

By default the tool boots a **boot bundle**, (a kernel packed together with its *matching*
kernel modules (and optional device trees)) rather than a bare kernel, so kernel and modules
always match. `-bundle <path-or-URL>` points at one (a `.tgz`/`.tar` or a local directory;
mutually exclusive with `-kernel`); `-kernel <path-or-URL>` supplies a bare kernel instead.
Overrides only include what your run needs, the arch's default covers everything else (see
[Architectures](#architectures)).

Inside a bundle the tool looks for a conventional layout (an arch may override these paths):

```
bundle.tgz
├── vmlinuz                       # kernel
├── modules/                      # contents of /lib/modules/<release>/
│   ├── modules.dep               #   optional (modules.dep.bin also accepted)
│   ├── modules.builtin           #   optional (modules.builtin.bin also accepted)
│   └── kernel/.../xhci-pci.ko …
└── dtb/                          # optional device trees
```

### Loading modules

Select the modules you want with `-module <name>` (repeatable); dependencies are resolved and
loaded first, in the guest, before tests run. Dependencies come from the bundle's `modules.dep`
when present, or from each module's ELF `.modinfo` otherwise (as `depmod` does); so a tree that
ships only `.ko` files still works. You name the capability you care about (say `-module
xhci_pci` for USB xHCI) without worrying whether a given kernel compiles it in or builds it as a
module:

- a module **built into** the kernel has no `.ko` in the tree, so it is skipped (nothing to load);
- a **loadable** module's `.ko` (plus dependencies) is packed into the initramfs and loaded;
- a module missing entirely is skipped (assumed built-in); one present as a `.ko` but that fails
  to load fails the run.

Names use the kernel's convention, so `-` and `_` are interchangeable (as with `modprobe`).

```sh
qemu-test -arch arm64 -bundle https://example.com/arm64-boot.tgz -module xhci_pci ./internal/guesttests
qemu-test -bundle ./boot -module xhci_pci ./internal/guesttests   # ./boot is an extracted bundle directory
```

Only the requested modules and their dependencies are packed (never the whole tree), so the
initramfs stays small. Because initramfs content is unpacked into guest RAM, a large module set
may need more `-mem`; the tool warns when the packed modules are large relative to it. Compressed
modules (`.ko.gz`/`.ko.xz`/`.ko.zst`) need the guest kernel's `CONFIG_MODULE_DECOMPRESS`. Without
a modules tree (e.g. a bare `-kernel` with drivers built in), `-module` finds nothing to load and
is skipped: runs stay green.

### Kernels from a URL

A `-kernel` (or `-bundle`) given as an `http(s)://` URL (or an arch's default `kernelURL`) is
downloaded once and cached on disk under `os.UserCacheDir()/qemu-test` (override with the
`QEMU_TEST_CACHE` env var), keyed by a hash of the URL with a small JSON sidecar of HTTP
validators. Caching is **offline-first**: a cached copy is reused with no network access.
`-refresh` re-validates against the remote with a conditional request and re-downloads only when
it changed (`304` keeps the cache; an unreachable remote keeps the cached copy).

```sh
qemu-test -kernel https://example.com/vmlinuz ./internal/guesttests              # download + cache, then reuse
qemu-test -kernel https://example.com/vmlinuz -refresh ./internal/guesttests
```

### Building bundles (`qemu-test bundle`)

For kernels and modules you can't get as a prebuilt bundle, `qemu-test bundle` assembles one
from upstream artifacts (a distribution's netboot kernel plus the modules from its initrd), or
the kernel and modules straight out of Debian `linux-*.deb` packages. It downloads each source
(cached like kernels/bundles), pulls files out of archives by glob, and writes a deterministic
`.tgz`:

```sh
qemu-test bundle -config arm.json -o arm-boot.tgz   # add -v for progress, -refresh to re-fetch
```

The config is a JSON object with one key `"sources"`, an array of sources. Each source names a
download location (an explicit `url` or a `debian` block resolved from a Debian package index)
and how to select files from it. A pool `.deb` URL embeds its exact version, so a `debian` block
resolves the current one from the suite's `Packages` index instead: the suite stays pinned, but
the version within it tracks the latest (picked up on `-refresh`). See the
[Bundle config reference](#bundle-config-reference) for the full schema and glob semantics. A
plain example (pulling a kernel and modules out of two Debian `.deb` packages) is:

```json
{
  "sources": [
    {
      "url": "https://deb.debian.org/debian-ports/.../linux-binary-...-powerpc64_..._ppc64.deb",
      "archive": true,
      "files": {
        "boot/vmlinux-*": "vmlinuz"
      }
    },
    {
      "url": "https://deb.debian.org/debian-ports/.../linux-modules-...-powerpc64_..._ppc64.deb",
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
qemu-test -arch arm -bundle ./arm-boot.tgz -module xhci_pci ./internal/guesttests
```

An arch can also embed one of these configs (a `bundleConfig` in its registry entry), so the
arch boots with no flags: the tool builds the bundle on first run and caches it (keyed by a hash
of the config). A config change rebuilds, and `-refresh` re-validates the upstream sources and
rebuilds.

### Managing the cache

Downloaded kernels/bundles and locally built bundles accumulate in the cache dir
(`os.UserCacheDir()/qemu-test`, override with `QEMU_TEST_CACHE`). `qemu-test cache` inspects and
prunes it; everything it removes is re-fetched or rebuilt on the next run, so clearing is always
safe.

```sh
qemu-test cache                 # item count + total size on disk (alias of "cache info")
qemu-test cache list            # one row per item: short key, kind, size, fetched-at, source URL
qemu-test cache list -v         # full keys plus each item's on-disk paths
qemu-test cache clear           # remove everything (also sweeps stray temp files)
qemu-test cache clear <key|url> # remove only the named item(s); key as shown by "list", or its URL
qemu-test cache dir             # print the cache directory path
```

`list` reports each item's *kind*: `download` (a bare kernel or build source), `bundle` (a
downloaded bundle with its extracted tree), `built` (a bundle built from an arch's
`bundleConfig`), or `orphan` (a leftover with no body). Built bundles have no source URL, so
clear them by the key shown in `list`.

## Environment & sharing

### Environment variables

`-env KEY=VALUE` sets variables in the test binary's environment inside the guest. The flag is
repeatable; the last value for a key wins. PID 1 inherits a near-empty environment, so the guest
fills in baseline defaults (`PATH`, `HOME=/root`, `TMPDIR=/tmp`) for any not already set; your
`-env` values always override both defaults and inherited values. A value with no `=` is rejected
with a usage error.

```sh
qemu-test -env DB_URL=memory:// -env DEBUG=1 ./mypkg
```

### Getting files out of the guest (`-out`)

`-out <dir>` shares a host directory into the guest at `/out` over virtio-9p, letting tests
write files the host can read afterward. A common use is coverage:

```sh
qemu-test -out ./cov ./mypkg -test.coverprofile=/out/cover.out   # -> ./cov/cover.out
go tool cover -html=./cov/cover.out
```

The default bundles build 9p as loadable modules, so `-out` adds `9p` and `9pnet_virtio` (and
their dependencies) to the module load list automatically and mounts `/out` after modules load;
where 9p is built in or the modules are absent, those requests are harmlessly skipped. When a
coverage flag (`-test.coverprofile` / `-test.covermode`) is forwarded, the test binary is
automatically built with `-cover`.

### Attaching QEMU devices / raw args

```sh
qemu-test -qemu.device qemu-xhci,id=xhci ./mypkg
qemu-test -qemu.arg "-drive if=none,id=d0,file=disk.img,format=raw" ./mypkg
```

`-qemu.arg` is split on its first whitespace, so a single quoted value like
`"-drive if=none,..."` reaches qemu as the two argv tokens it expects.

### Driving QEMU from inside a test (QMP)

A test running in the guest can control QEMU from the host (e.g. device hotplug) by printing a
line of the form `[QMP]<json>` to stdout. The host forwards that JSON to QEMU's QMP socket; the
test is responsible for constructing valid QMP commands. The QMP channel is best-effort; if it
fails to connect during boot, only QMP-driven tests are affected.

### Debugging a run

If the guest never reports a verdict (kernel panic, init crash, a missing/incompatible
`/tests`, or bad qemu args), the tool reports a *boot/init failure*, distinct from an ordinary
test failure, and prints the **tail of the console output** so the cause is visible without
re-running. `-keep` retains the temp dir (built `init`, initramfs, `qmp.sock`) and logs the exact
qemu command line; `-v` shows kernel/boot noise and the resolved KVM mode:

```sh
qemu-test -keep -v ./mypkg        # logs the qemu command + keeps the temp dir
qemu-test -append "loglevel=7" ./mypkg
```

## Architectures

`-arch <goarch>` selects the guest architecture (a Go `GOARCH` name, default `amd64`), mapped to
the right qemu binary (e.g. `arm64` → `qemu-system-aarch64`). Most arches carry a default
**bundle**, a `bundleURL` (a prebuilt archive) or a `bundleConfig` the tool builds locally on
first run, so they boot with a matching kernel + modules and no flags; the rest define a default
`kernelURL` (a bare kernel, downloaded and cached on first run). `-kernel`/`-bundle` override
the default for any arch.

| Arch | Guest | Notes |
|------|-------|-------|
| `amd64` (default) | `qemu-system-x86_64` | Debian trixie `linux-image` bundle (kernel + full module set, version-matched). Any modular driver via `-module` |
| `386` | x86 (32-bit) | Reuses the amd64 bundle and 64-bit kernel; 32-bit init/tests run via the kernel's IA32 emulation. No extra flags |
| `arm64` | `qemu-system-aarch64` | Same trixie `linux-image` bundle as amd64 |
| `arm` | `qemu-system-arm` | Bundle from the Debian netboot kernel + modules out of its initrd (the installer's reduced set: USB, no 9p) |
| `riscv64` | `qemu-system-riscv64` | Bundle from the netboot kernel (uncompressed) + initrd modules (no 9p). USB via `-module xhci_pci -qemu.device qemu-xhci` |
| `ppc64` | `qemu-system-ppc64` | Big-endian PowerPC. Bundle built from debian-ports `linux-*.deb` packages. USB works on the pseries PCI bus |
| `ppc64le` | `qemu-system-ppc64` | Little-endian POWER (the production arch). Single trixie `main` bundle, like amd64. USB/`-out` as for ppc64 |
| `s390x` | `qemu-system-s390x` | Big-endian IBM Z. Single trixie bundle; CCW virtio bus plus PCI via zPCI (`-qemu.device` auto-wraps, so don't add an explicit `zpci` device) |
| `loong64` | `qemu-system-loongarch64` | LoongArch (LE). debian-ports bundle; boots through edk2 firmware (auto-located, passed via `-bios`) to supply ACPI tables |

`s390x` and `ppc64` are the **big-endian** entries: the test binary is cross-compiled for that
`GOARCH`, so the guest code runs big-endian, catching byte-order bugs a little-endian host can't.
On `ppc64`, `ppc64le`, and `s390x` alike, `-out` attaches its `virtio-9p-pci` device and 9p is
modular, but `-out` adds the needed modules automatically (see [Loading modules](#loading-modules)).

```sh
qemu-test -arch riscv64 ./internal/guesttests                          # uses the default Debian bundle
qemu-test -arch riscv64 -module xhci_pci -qemu.device qemu-xhci ./internal/guesttests  # USB on riscv64
qemu-test -arch 386 ./internal/guesttests                              # 32-bit x86 on the amd64 kernel (IA32 emulation)
qemu-test -arch loong64 ./internal/guesttests                          # LoongArch; edk2 firmware auto-located for -bios
qemu-test -arch s390x ./internal/guesttests                            # big-endian IBM Z
qemu-test -arch s390x -module xhci_pci -qemu.device qemu-xhci ./internal/guesttests  # USB on s390x via zPCI
qemu-test -arch arm64 -kernel ./Image ./internal/guesttests            # override with a local image
qemu-test -arch arm64 -kernel https://example.com/arm64/vmlinuz ./internal/guesttests
```

## How it works

1. **Build** the guest `init` (from embedded source) and the package's test binary, both
   cross-compiled to `linux/<arch>`, the same `GOARCH` for both, so they share an architecture.
2. **Pack** them into a deterministic newc-cpio initramfs, along with `test-args.txt`
   (forwarded test args), `test-env.txt` (`-env` values), and any requested kernel modules with
   `test-modules.txt` (their dependency-ordered load list).
3. **Boot** the architecture's qemu binary (default `qemu-system-x86_64`) with the kernel,
   serial on stdio, display off, a QMP control socket, a virtio-rng device (so the guest entropy
   pool is seeded and `crypto/rand` doesn't stall at boot), and `-no-reboot` + `panic=-1` (so a
   kernel panic exits qemu immediately with the console tail rather than hanging until `-timeout`).
4. **Run**: `init` mounts `/proc`, `/sys`, `/dev`, etc., brings up the loopback interface (so
   tests can use `127.0.0.1`/`::1`), loads any requested kernel modules, mounts `/out` if shared,
   fills in default env vars (`PATH`/`HOME`/`TMPDIR`) plus any `-env` values, then runs the test
   binary as its child, reaping it and any orphaned grandchildren as PID 1, prints the result,
   and always powers the VM off.
5. **Report**: the host parses the serial console for a verdict and exits accordingly.

The guest and host communicate over a small line protocol on the serial console:

- `[TESTS:STDOUT] …` / `[TESTS:STDERR] …`: test output, forwarded to the host's stdout/stderr.
- `[TESTS:PASS]` / `[TESTS:FAIL] …`: the final verdict, which sets the exit code.
- Anything else is treated as kernel/boot noise and shown only with `-v`.

## Exit codes

| Code | Meaning |
|------|---------|
| `0` | Tests passed (or the package had no test files, matching `go test`) |
| `1` | Tests failed, the guest reported a setup error, the run timed out, or the guest produced no verdict (boot/init failure) |
| `2` | Usage error (bad flags, missing package, malformed `-env`/`-kvm`) |

## Developing this tool

```sh
# Host-side unit tests (builder, cpio, flag parsing, log parser, cache + cache subcommand, bundler); no qemu needed
go test ./...

# End-to-end against the sample package (requires qemu-system-x86_64)
go run . ./internal/guesttests                          # -> PASS, exit 0
go run . -env GUESTTEST_FAIL=1 ./internal/guesttests    # -> FAIL, exit 1
```

If you change the guest `init` program, the embedded source under `internal/init/` is what ships.
There's no separate rebuild step, but the change must compile under the embedded `go.mod`.

## Bundle config reference

A `qemu-test bundle` config's `"sources"` array mixes a download location with a selection mode.
A *plain* source copies the downloaded file to `dst`. An *archive* source (`"archive": true`)
selects files via `files`, a map of **glob → destination**. The glob paths match the files'
installed locations inside the archive/package.

To avoid pinning a version in a pool `.deb` URL, replace the `url` with a `debian` block: the
bundler fetches the suite's `binary-<arch>/Packages` index, reads the **meta-package** named by
`via` to discover the current kernel ABI (from its dependency matching `abiDep`'s `{abi}`
placeholder), substitutes that ABI into `package`, and downloads that stanza's `.deb`. The
default `amd64`/`arm64` bundles use this:

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

Selection semantics:

- **Input archives** may be a newc **cpio** (e.g. a Linux `initrd`), a **tar**, or a Debian
  **`.deb`**: an `ar` archive whose `data.tar` member (the installed files) is read, skipping
  `debian-binary`/`control.tar`. Any may be gzip/**xz**/**zstd**-compressed; the format is
  detected by content, not extension. Compressed `.ko` modules are handled the same way (see
  [Loading modules](#loading-modules)).
- **Globs** use doublestar semantics: `*` matches within one path segment, `**` matches any
  depth (`usr/lib/modules/**` is the whole tree).
- A **destination ending in `/`** is a directory: the matched path with the glob's fixed
  (non-wildcard) prefix removed is appended under it, so `usr/lib/modules/**` → `lib/modules/`
  reproduces `lib/modules/<release>/...`. A destination without a trailing `/` is an exact path
  (valid for a plain file or a glob that matches exactly one entry).
- A **`.` segment is a keep-anchor** (like `rsync -R`'s `/./`): only the part matched *after* it
  is kept under a directory destination, dropping everything before, including a variable dir
  matched by a wildcard. So `usr/lib/modules/*/./**` → `modules/` drops the `<release>` dir and
  yields a flat tree (`modules/modules.dep`, `modules/kernel/...`), whereas
  `usr/lib/modules/**` → `lib/modules/` keeps `lib/modules/<release>/...`. At most one `.` is
  allowed, and no `**` may precede it.
- A `url` that isn't an `http(s)` URL is read from the local filesystem, so you can repack local
  artifacts too.