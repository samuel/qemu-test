# qemu-test

Run a Go package's tests inside a freshly-booted QEMU virtual machine instead of on the host.

`qemu-test` compiles your package's test binary, packs it together with a tiny PID-1 `init`
into an initramfs, boots `qemu-system-x86_64` with a Linux kernel, runs the tests
as init's child, streams their output back over the serial console, and exits `0`/`1` to match
the result.

## Why

Some tests need real-kernel behavior that ordinary userspace tests on the host can't
reproduce — block devices, USB, mounts, syscalls, device hotplug, cgroups. `qemu-test` gives
each run a clean, throwaway kernel and root so those tests can exercise the real thing without
touching (or requiring root on) your host.

## Requirements

- The **qemu system emulator** for your guest architecture on your `PATH` — `qemu-system-x86_64`
  for the default `amd64` guest (e.g. `brew install qemu`, `apt install qemu-system-x86`).
- **Go 1.26+** to build the tool and cross-compile the guest.
- No separate kernel needed for `amd64` — its kernel and module set come from a default Debian
  bundle, built and cached on first run (see [Boot bundles and kernel modules](#boot-bundles-and-kernel-modules)).

By default the guest is `linux/amd64`; your test package is cross-compiled with
`CGO_ENABLED=0 GOOS=linux GOARCH=<arch>`. On non-x86 hosts QEMU runs under TCG emulation
(slower); on a Linux host whose architecture matches the guest and exposes a writable
`/dev/kvm`, the default `-kvm auto` turns on KVM acceleration (`-enable-kvm -cpu host`), which is
much faster. Use `-kvm off` to force TCG or `-kvm on` to require KVM.

### Other architectures

The guest architecture is selected with `-arch <goarch>` (a Go `GOARCH` name; the tool maps it
to the right qemu binary, e.g. `arm64` → `qemu-system-aarch64`). Most arches carry a default
**bundle** — either a `bundleURL` (a prebuilt archive) or a `bundleConfig` the tool builds locally
on first run (see [Building boot bundles](#building-boot-bundles-qemu-test-bundle)) — so they boot with a
matching kernel + modules and no flags; the rest define a default `kernelURL` (a bare kernel,
downloaded and cached on first run — see [Fetching kernels from a URL](#fetching-kernels-from-a-url)).
`-kernel`/`-bundle` also override the default for any arch (`-kernel` accepts a local path or an
`http(s)://` URL):

```sh
qemu-test -arch riscv64 ./guesttests                          # uses the default Debian bundle
qemu-test -arch riscv64 -module xhci_pci -qemu.device qemu-xhci ./guesttests  # USB on riscv64
qemu-test -arch 386 ./guesttests                              # 32-bit x86 on the amd64 kernel (IA32 emulation)
qemu-test -arch arm64 -kernel ./Image ./guesttests            # override with a local image
qemu-test -arch arm64 -kernel https://example.com/arm64/vmlinuz ./guesttests
```

`amd64` and `arm64` (the default and primary arches) ship a default `bundleConfig` built from the
Debian trixie `linux-image` `.deb`, which carries both the bootable kernel and the full in-tree
module set in one version-matched package. So they boot with no flags, and any modular driver is
available via `-module` (e.g. `-module xhci_pci` for USB on the PCI bus, or the 9p modules `-out`
loads automatically — see below). `386` (32-bit x86) reuses that amd64 bundle and 64-bit kernel —
trixie ships no i386 kernel, so the 32-bit `init`/tests run via the kernel's IA32 emulation; it
boots on `qemu-system-x86_64` with no flags. `ppc64le` (little-endian POWER) is built the same way
as amd64, from its trixie `ppc64el` release `.deb` (see [Big-endian testing](#big-endian-testing)
for how it relates to big-endian `ppc64`). `riscv64` and `arm` instead build their `bundleConfig`
from the
Debian trixie debian-installer netboot tree — the d-i kernel (uncompressed for riscv64, since
qemu's riscv64 `-kernel` loader does not decompress) plus the module set out of the netboot initrd.
That initrd is the *installer's* reduced module set: it ships USB but no 9p module, so `-out`
(virtio-9p) is unavailable on `riscv64`/`arm`; the guest mounts `/out` best-effort, so runs
continue regardless.

#### Big-endian testing

`s390x` (IBM Z) and `ppc64` (big-endian PowerPC) are registered so the test binary —
cross-compiled for that `GOARCH` — runs **big-endian**, catching byte-order bugs that a
little-endian host can't.

```sh
qemu-test -arch s390x ./guesttests                       # default Debian linux-image bundle; SCLP console, CCW bus
qemu-test -arch s390x -module xhci_pci -qemu.device qemu-xhci ./guesttests  # USB on s390x via zPCI
qemu-test -arch ppc64 ./guesttests                       # default bundle built from debian-ports .debs
qemu-test -arch ppc64 -module xhci_pci -qemu.device qemu-xhci ./guesttests  # USB on a big-endian guest
qemu-test -arch ppc64le ./guesttests                     # little-endian POWER; trixie release bundle
```

`s390x` ships a default `bundleConfig` built from the Debian trixie `linux-image-s390x` `.deb`
(resolved from the Packages index like amd64/arm64, so it is not version-pinned): its
`/boot/vmlinuz-*` IPL image boots directly via `-kernel` on the `s390-ccw-virtio` machine, and the
`.deb` carries the full module set, so `-module` works. Its default virtio devices use the CCW bus,
but the machine *also* exposes PCI via IBM Z **zPCI**: a PCI device added with `-qemu.device` is
auto-wrapped in a zPCI function (don't add an explicit `zpci` device — it collides with the
auto-created one), so USB (`qemu-xhci`) and `-out` (`virtio-9p-pci`) both work, just like on a
PCI-bus arch.

`ppc64` is the big-endian arch wired up for **USB testing**. Its pseries machine has a PCI bus
(so `qemu-xhci` attaches), but the Debian kernel builds USB as loadable modules — so `ppc64` ships
a default `bundleConfig` rather than a bare kernel. Since big-endian `ppc64` is not a Debian
release arch (debian-installer ships `ppc64el`), there is no netboot kernel+initrd as for `arm`;
the bundle is instead built from **debian-ports `linux-*.deb` packages** (the bootable `vmlinux`
from `linux-binary` plus the matching module tree from `linux-modules`) — see
[Building boot bundles](#building-boot-bundles-qemu-test-bundle). On first run the tool fetches those
`.deb`s and builds the bundle (both cached), so `-arch ppc64` boots with no extra flags; name USB
modules with `-module` (e.g. `-module xhci_pci`) and attach a controller with `-qemu.device`.

`ppc64le` is the **little-endian** sibling — the mainstream production POWER arch and first-class in
Go. It uses the same `pseries` machine and `qemu-system-ppc64` binary (the kernel selects
endianness), so it behaves like `ppc64` for USB and `-out`. The difference is the kernel source:
`ppc64el` *is* a Debian release arch, so its bundle is a single trixie `main`
`linux-image-powerpc64le` `.deb` carrying both the kernel (`/boot/vmlinux-*`, an LE ELF image) and
the full module set — like amd64/arm64, rather than the split debian-ports pair big-endian `ppc64`
needs.

Note: `ppc64`, `ppc64le`, and `s390x` all attach the `-out` shared dir's `virtio-9p-pci` device —
the two POWER arches on their pseries PCI bus, `s390x` via zPCI (the device auto-wraps in a zPCI
function on `s390-ccw-virtio`). On all three, the Debian kernel builds 9p as a module, but `-out`
adds the needed 9p modules to the load list automatically (so no manual `-module` is required where
the bundle ships them).

### Fetching kernels from a URL

When `-kernel` is an `http(s)://` URL (or an arch defines a default `kernelURL`), the kernel is
downloaded once and cached on disk, so later runs reuse it. The cache lives under
`os.UserCacheDir()/qemu-test` (override with the `QEMU_TEST_CACHE` env var),
keyed by a hash of the URL, alongside a small JSON sidecar recording the HTTP validators
(ETag / Last-Modified) and a content hash. Use [`qemu-test cache`](#managing-the-cache) to
inspect or prune it.

Caching is **offline-first**: a cached kernel is used without any network access. Pass
`-refresh` to re-check the remote — it issues a conditional request and re-downloads only
when the kernel has actually changed (a `304 Not Modified` keeps the cached copy). If the remote
is unreachable during a refresh but a cached copy exists, that copy is used.

```sh
qemu-test -kernel https://example.com/vmlinuz ./guesttests              # download + cache, then reuse
qemu-test -kernel https://example.com/vmlinuz -refresh ./guesttests  # re-validate against the remote
```

### Boot bundles and kernel modules

A **boot bundle** lets the tool provide more than a bare kernel: an archive (`.tgz`/`.tar`) — or
a local directory — carrying the kernel together with its *matching* kernel modules (and,
optionally, device trees). Because the kernel and modules are packaged together, they always
match (no version/vermagic mismatch). Pass it with `-bundle <path-or-URL>` (mutually exclusive
with `-kernel`); a URL is downloaded, cached, and extracted exactly like a kernel, and
`-refresh` re-validates it against the remote.

Inside the bundle the tool looks for a conventional layout (an arch may override these paths):

```
bundle.tgz
├── vmlinuz                       # kernel
├── modules/                      # contents of /lib/modules/<release>/
│   ├── modules.dep               #   (optional; modules.dep.bin also accepted)
│   ├── modules.builtin           #   (optional; modules.builtin.bin also accepted)
│   └── kernel/.../xhci-pci.ko …
└── dtb/                          # optional device trees
```

**Loading modules.** Name the modules you want with `-module <name>` (repeatable). The tool
resolves each module's dependencies and loads them — dependencies first — in the guest before your
tests run, via `finit_module(2)`. Dependencies come from the bundle's `modules.dep` when present —
either the text file or its binary `modules.dep.bin` (and likewise `modules.builtin` /
`modules.builtin.bin`). A tree that ships only `.ko` files (no `modules.dep` at all, e.g. an
extracted netboot initrd, or a Debian `linux-modules` package) still works — the tool then reads
each module's ELF `.modinfo` to discover dependencies (as `depmod` does), transparently
decompressing `.ko.gz`/`.ko.xz`/`.ko.zst`. You name the capability you care about (say
`-module xhci_pci` for USB xHCI) without worrying whether a given arch/kernel compiles it in or
builds it as a module:

- if the module is **built into the kernel**, there is no `.ko` for it in the tree, so it is
  skipped (nothing to load);
- if it is a **loadable module**, its `.ko` (plus dependencies) is packed into the initramfs and
  loaded.

A module that is missing entirely is likewise assumed built-in and skipped; a module that *is*
present as a `.ko` but fails to load fails the run. Names use the kernel's convention, so `-` and
`_` are interchangeable (as with `modprobe`).

```sh
qemu-test -arch arm64 -bundle https://example.com/arm64-boot.tgz -module xhci_pci ./guesttests
qemu-test -bundle ./boot -module xhci_pci ./guesttests   # ./boot is an extracted bundle directory
```

Only the requested modules and their dependencies are packed (never the whole tree), so the
initramfs stays small. Since initramfs content is unpacked into guest RAM, loading a very large
set of modules may need more `-mem`; the tool warns when the packed modules are large relative to
`-mem`. Compressed modules (`.ko.gz`/`.ko.xz`/`.ko.zst`) are loaded as-is and need the guest
kernel's `CONFIG_MODULE_DECOMPRESS`; their dependencies are auto-discovered from `.modinfo`
regardless of compression, so you need not name deps by hand. Without a modules tree (e.g. a
`-kernel` bare kernel that builds its drivers in), `-module` finds nothing to load and is skipped —
runs stay green.

### Building boot bundles (`qemu-test bundle`)

The `qemu-test bundle` subcommand assembles a boot bundle from upstream artifacts — a
distribution's netboot kernel plus the matching modules from its initrd, or the kernel and modules
straight out of Debian `linux-*.deb` packages — so you don't have to hand-craft the `vmlinuz` +
`lib/modules/<release>/` layout above. It downloads each source (cached the same way as
kernels/bundles), pulls files out of archives by glob, and writes a deterministic `.tgz`.

```sh
qemu-test bundle -config arm.json -o arm-boot.tgz   # add -v to see progress, -refresh to re-fetch
```

The config is a JSON object with one key `"sources"`, an array of **sources**. Each source names a
download location — either an explicit `url` **or** a `debian` block that resolves one from a Debian
package index (see below) — and a selection mode. A *plain* source copies the downloaded file to
`dst`. An *archive* source (`"archive": true`) selects files via `files`, a map of
**glob → destination**. The example below pulls a kernel and modules out of two Debian `.deb`
packages (as `-arch ppc64` does); the glob paths match the files' installed locations inside the
package:

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

**Resolving the latest `.deb` instead of pinning a version (`debian`).** A pool `.deb` URL embeds
its exact version (`linux-image-6.12.86+deb13-amd64-unsigned_6.12.86-1_amd64.deb`) and Debian
offers no "latest" alias for it. To avoid hardcoding a version, give a source a `debian` block
instead of a `url`: the bundler fetches the suite's `binary-<arch>/Packages` index, reads the
**meta-package** named by `via` to discover the current kernel ABI (from its dependency matching
`abiDep`'s `{abi}` placeholder), substitutes that ABI into `package`, and downloads that stanza's
`.deb`. From there the source behaves exactly like a `url` one (`archive`/`files` still apply). The
suite stays pinned, but the version within it tracks the latest, picked up on `-refresh`. The
default `amd64`, `arm64`, and `ppc64` bundles use this; for example amd64:

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

- **Input archives** may be a newc **cpio** (e.g. a Linux `initrd`), a **tar**, or a Debian
  **`.deb`** (an `ar` archive — its `data.tar` member, the installed files, is read; the
  `debian-binary`/`control.tar` members are skipped). Any of these may be gzip/**xz**/**zstd**-
  compressed; the format is detected by content, not by extension.
- **Globs** use doublestar semantics: `*` matches within one path segment, `**` matches any
  depth (`usr/lib/modules/**` is the whole tree).
- **Destinations** ending in `/` are directories: the matched path with the glob's fixed
  (non-wildcard) prefix removed is appended under it, so `usr/lib/modules/**` → `lib/modules/`
  reproduces `lib/modules/<release>/...`. A destination without a trailing `/` is an exact path
  (valid for a plain file or a glob that matches exactly one entry).
- A **`.` segment is a keep-anchor** (like `rsync -R`'s `/./`): only the part matched *after* it
  is kept under a directory destination, dropping everything before — including a variable
  directory matched by a wildcard. So `usr/lib/modules/*/./**` → `modules/` drops the
  `<release>` dir matched by `*` and yields a flat `modules/` tree (`modules/modules.dep`,
  `modules/kernel/...`), whereas `usr/lib/modules/**` → `lib/modules/` keeps it as
  `lib/modules/<release>/...`. At most one `.` is allowed, and no `**` may precede it.
- A `url` that isn't an `http(s)` URL is read from the local filesystem, so you can repack local
  artifacts too.

The result drops straight into `-bundle`:

```sh
qemu-test -arch arm -bundle ./arm-boot.tgz -module xhci_pci ./guesttests
```

**As an arch default.** An arch can embed this same config (a `bundleConfig` in its registry
entry) instead of a `bundleURL`. When it has one and no `-kernel`/`-bundle` overrides it, the tool
builds the bundle on first run and then uses it exactly like a downloaded `-bundle` — so the arch
needs no flags. The built `.tgz` is deterministic and cached (keyed by a hash of its config, under
the same cache dir), so later runs skip the rebuild; a config change rebuilds, and
`-refresh` re-validates the upstream sources and rebuilds. `arm` (Debian armhf netboot
kernel + modules from its initrd) ships this way.

### Managing the cache

Downloaded kernels and bundles, and locally built bundles, accumulate in the cache dir
(`os.UserCacheDir()/qemu-test`, override with `QEMU_TEST_CACHE`). The `qemu-test cache`
subcommand inspects and prunes it — everything it removes is re-fetched or rebuilt on the next
run, so clearing is always safe.

```sh
qemu-test cache                 # item count + total size on disk (alias of "cache info")
qemu-test cache list            # one row per item: short key, kind, size, fetched-at, source URL
qemu-test cache list -v         # full keys plus each item's on-disk paths
qemu-test cache clear           # remove everything (also sweeps stray temp files)
qemu-test cache clear <key|url> # remove only the named item(s); key as shown by "list", or its URL
qemu-test cache dir             # print the cache directory path
```

`list` reports each item's *kind*: `download` (a bare kernel or a build source), `bundle` (a
downloaded bundle with its extracted tree), `built` (a bundle built from an arch's `bundleConfig`),
or `orphan` (a leftover with no body, still reclaimable). Built bundles have no source URL, so
clear them by the key shown in `list`.

## Install

```sh
# Build a local binary
go build -o qemu-test .

# …or run without installing
go run . ./your/package
```

## Quick start

```sh
# Run every test in ./guesttests inside a VM
qemu-test ./guesttests

# Run a single test, verbose test output, verbose (debug) tool logging
qemu-test -test.run TestFoo -test.v -v ./guesttests
```

The bundled `guesttests` package is a working sample: `TestEnv` passes by default and fails
only when `GUESTTEST_FAIL` is set (see [Environment variables](#environment-variables)).

## Usage

```
qemu-test [flags] <package> [test args...]
```

- **`<package>`** is a Go package path (e.g. `./guesttests`, `.`), the same as you'd give `go test`.
- **Tool flags** go *before* the package path.
- **`[test args...]`** after the package path are forwarded verbatim to the test binary inside
  the guest, so anything `go test` accepts works (e.g. `-test.count=2`, `-test.timeout=30s`).

### Flags

| Flag | Default | Description                                                                                                                                                                                                                                                   |
|------|---------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `-test.run <regexp>` | | Run only tests matching the regular expression                                                                                                                                                                                                                |
| `-test.v` | `false` | Verbose test output                                                                                                                                                                                                                                           |
| `-v` | `false` | Verbose (debug) tool logging                                                                                                                                                                                                                                  |
| `-env KEY=VALUE` | | Set an environment variable for the test binary. Repeatable                                                                                                                                                                                                   |
| `-arch <goarch>` | `amd64` | Guest architecture (Go `GOARCH` name). Most arches have a default kernel/bundle (built and cached on first run); `-kernel`/`-bundle` override it                                                                                                              |
| `-kernel <path-or-URL>` | | External kernel image, overriding the arch's default kernel. An `http(s)://` URL is downloaded and cached (see [Fetching kernels from a URL](#fetching-kernels-from-a-url))                                                                                   |
| `-bundle <path-or-URL>` | | Boot bundle (tgz/tar or directory) supplying the kernel plus its matching modules (and optional dtb). A URL is downloaded, cached, and extracted. Mutually exclusive with `-kernel` (see [Boot bundles and kernel modules](#boot-bundles-and-kernel-modules)) |
| `-refresh` | `false` | Re-validate a URL kernel or bundle against the remote and re-download/re-extract if it changed                                                                                                                                                                 |
| `-module <name>` | | Load a kernel module (by name) in the guest before tests, resolving its dependencies; modules built into the kernel are skipped. Repeatable. Needs a bundle that ships a modules tree                                                                         |
| `-tags <tags>` | | Build tags to pass when compiling the test binary                                                                                                                                                                                                             |
| `-smp <n>` | `2` | Number of guest vCPUs                                                                                                                                                                                                                                         |
| `-mem <m>` | `1024` | Guest RAM in MB (a bare qemu `-m` value, so `2G` also works)                                                                                                                                                                                                  |
| `-kvm auto\|on\|off` | `auto` | KVM acceleration. `auto` enables it when the guest arch matches the host and `/dev/kvm` is writable; `on` forces it; `off` uses TCG                                                                                                                           |
| `-append <tokens>` | | Extra kernel command-line tokens, merged after `console=...` (e.g. `-append "loglevel=7 quiet"`)                                                                                                                                                              |
| `-timeout <dur>` | `5m` | Max wall-clock for the whole run; `0` disables the limit                                                                                                                                                                                                      |
| `-keep` | `false` | Keep the temp dir (initramfs, built `init`, qmp.sock) and log the full qemu command line, instead of removing it on exit                                                                                                                                      |
| `-out <dir>` | | Share a host dir into the guest at `/out` (virtio-9p) so tests can hand files back                                                                                                                                                                            |
| `-qemu.device <dev>` | | Add a qemu `-device` (shorthand for `-qemu.arg "-device <dev>"`). Repeatable                                                                                                                                                                                  |
| `-qemu.arg <arg>` | | Pass a raw argument to qemu, split on the first whitespace. Repeatable                                                                                                                                                                                        |

`-test.run` / `-test.v` are conveniences; you can equivalently pass them as trailing test args.

### Environment variables

`-env KEY=VALUE` sets variables in the test binary's environment inside the guest. The flag is
repeatable; later values override earlier ones for the same key. The values are appended to the
guest's baseline environment, so the test still sees a normal `PATH` etc. A value with no `=`
is rejected with a usage error.

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

The default Debian bundles build 9p as loadable modules (only a bare kernel with `CONFIG_9P_FS=y`
has it built in), so the guest can't mount the share until they're loaded. `-out` therefore adds
`9p` and `9pnet_virtio` (and their dependencies) to the module load list automatically and mounts
`/out` after modules load; on a bundle/kernel where 9p is built in or the modules are absent, those
requests are harmlessly skipped. When a coverage flag (`-test.coverprofile` / `-test.covermode`) is
forwarded, the test binary is automatically built with `-cover`.

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
test is responsible for constructing valid QMP commands. The QMP channel is best-effort — if it
fails to connect during boot, only QMP-driven tests are affected.

### Debugging a run

If the guest never reports a verdict (kernel panic, init crash, a missing/incompatible
`/tests`, or bad qemu args), the tool fails with a *boot/init failure* and prints the **tail of
the console output** so the cause is visible without re-running — distinct from an ordinary test
failure.

For deeper inspection, `-keep` retains the temp dir (built `init`, initramfs, `qmp.sock`) and
logs the exact qemu command line, and `-v` shows kernel/boot noise and the resolved KVM mode:

```sh
qemu-test -keep -v ./mypkg        # logs the qemu command + keeps the temp dir
qemu-test -append "loglevel=7" ./mypkg
```

## Exit codes

| Code | Meaning |
|------|---------|
| `0` | Tests passed (or the package had no test files, matching `go test`) |
| `1` | Tests failed, the guest reported a setup error, the run timed out, or the guest produced no verdict (boot/init failure) |
| `2` | Usage error (bad flags, missing package, malformed `-env`/`-kvm`) |

## How it works

1. **Build** the guest `init` (from embedded source) and the package's test binary, both
   cross-compiled to `linux/<arch>` (the same `GOARCH` for both, so they share an architecture).
2. **Pack** them — plus `test-args.txt` (forwarded test args), `test-env.txt` (`-env` values),
   and any requested kernel modules with `test-modules.txt` (their dependency-ordered load list)
   — into a deterministic newc-cpio initramfs.
3. **Boot** the architecture's qemu binary (default `qemu-system-x86_64`) with the kernel,
   serial on stdio, display off, and a QMP control socket.
4. **Run**: `init` mounts `/proc`, `/sys`, `/dev`, etc. (and `/out` if shared), loads any
   requested kernel modules, sets the env, runs the test binary as its child, prints the result,
   and always powers the VM off.
5. **Report**: the host parses the serial console for a verdict and exits accordingly.

The guest and host communicate over a small line protocol on the serial console:

- `[TESTS:STDOUT] …` / `[TESTS:STDERR] …` — test output, forwarded to the host's stdout/stderr.
- `[TESTS:PASS]` / `[TESTS:FAIL] …` — the final verdict, which sets the exit code.
- Anything else is treated as kernel/boot noise and shown only with `-v`.

## Developing this tool

```sh
# Host-side unit tests (builder, cpio, flag parsing, log parser, cache + cache subcommand, bundler) — no qemu needed
go test ./...

# End-to-end against the sample package (requires qemu-system-x86_64)
go run . ./guesttests                          # -> PASS, exit 0
go run . -env GUESTTEST_FAIL=1 ./guesttests    # -> FAIL, exit 1
```

If you change the guest `init` program, the embedded source under `init/` is what ships —
there's no separate rebuild step, but the change must compile under the embedded `go.mod`.
