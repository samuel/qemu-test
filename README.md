# qemu-test

Run a Go package's tests inside a freshly-booted QEMU virtual machine instead of on the host.

`qemu-test` compiles your package's test binary, packs it together with a tiny PID-1 `init`
into an initramfs, boots `qemu-system-x86_64` with an embedded Linux kernel, runs the tests
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
- No separate kernel needed for `amd64` — a Linux `vmlinuz` is embedded in the binary.

By default the guest is `linux/amd64`; your test package is cross-compiled with
`CGO_ENABLED=0 GOOS=linux GOARCH=<arch>`. On non-x86 hosts QEMU runs under TCG emulation
(slower); on a Linux host exposing `/dev/kvm`, KVM acceleration is much faster.

### Other architectures

The guest architecture is selected with `-arch <goarch>` (a Go `GOARCH` name; the tool maps it
to the right qemu binary, e.g. `arm64` → `qemu-system-aarch64`). Only `amd64` ships an embedded
kernel today, so other architectures need a kernel supplied with `-kernel <path>`:

```sh
qemu-test -arch arm64 -kernel ./Image ./guesttests
```

Support is intentionally data-driven: see [`kernel/README.md`](kernel/README.md) for how to add
an architecture (drop a kernel at `kernel/<goarch>/vmlinuz` and add an entry to `arches` in
`arch.go`).

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

| Flag | Default | Description |
|------|---------|-------------|
| `-test.run <regexp>` | | Run only tests matching the regular expression |
| `-test.v` | `false` | Verbose test output |
| `-v` | `false` | Verbose (debug) tool logging |
| `-env KEY=VALUE` | | Set an environment variable for the test binary. Repeatable |
| `-arch <goarch>` | `amd64` | Guest architecture (Go `GOARCH` name). Only `amd64` ships a kernel; others need `-kernel` |
| `-kernel <path>` | | External kernel image, overriding the embedded kernel for `-arch` |
| `-tags <tags>` | | Build tags to pass when compiling the test binary |
| `-smp <n>` | `2` | Number of guest vCPUs |
| `-mem <m>` | `1024` | Guest RAM in MB (a bare qemu `-m` value, so `2G` also works) |
| `-timeout <dur>` | `5m` | Max wall-clock for the whole run; `0` disables the limit |
| `-out <dir>` | | Share a host dir into the guest at `/out` (virtio-9p) so tests can hand files back |
| `-qemu.device <dev>` | | Add a qemu `-device` (shorthand for `-qemu.arg "-device <dev>"`). Repeatable |
| `-qemu.arg <arg>` | | Pass a raw argument to qemu, split on the first whitespace. Repeatable |

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

When a coverage flag (`-test.coverprofile` / `-test.covermode`) is forwarded, the test binary
is automatically built with `-cover`.

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

## Exit codes

| Code | Meaning |
|------|---------|
| `0` | Tests passed (or the package had no test files, matching `go test`) |
| `1` | Tests failed, the guest reported a setup error, or the run timed out |
| `2` | Usage error (bad flags, missing package, malformed `-env`) |

## How it works

1. **Build** the guest `init` (from embedded source) and the package's test binary, both
   cross-compiled to `linux/<arch>` (the same `GOARCH` for both, so they share an architecture).
2. **Pack** them — plus `test-args.txt` (forwarded test args) and `test-env.txt` (`-env`
   values) — into a deterministic newc-cpio initramfs.
3. **Boot** the architecture's qemu binary (default `qemu-system-x86_64`) with the kernel,
   serial on stdio, display off, and a QMP control socket.
4. **Run**: `init` mounts `/proc`, `/sys`, `/dev`, etc. (and `/out` if shared), sets the env,
   runs the test binary as its child, prints the result, and always powers the VM off.
5. **Report**: the host parses the serial console for a verdict and exits accordingly.

The guest and host communicate over a small line protocol on the serial console:

- `[TESTS:STDOUT] …` / `[TESTS:STDERR] …` — test output, forwarded to the host's stdout/stderr.
- `[TESTS:PASS]` / `[TESTS:FAIL] …` — the final verdict, which sets the exit code.
- Anything else is treated as kernel/boot noise and shown only with `-v`.

## Developing this tool

```sh
# Host-side unit tests (builder, cpio, flag parsing, log parser) — no qemu needed
go test ./...

# End-to-end against the sample package (requires qemu-system-x86_64)
go run . ./guesttests                          # -> PASS, exit 0
go run . -env GUESTTEST_FAIL=1 ./guesttests    # -> FAIL, exit 1
```

If you change the guest `init` program, the embedded source under `init/` is what ships —
there's no separate rebuild step, but the change must compile under the embedded `go.mod`.
