// Command qemu-test runs a Go package's tests inside a freshly-booted QEMU VM.
//
// The real work lives in internal packages: internal/cli parses flags and
// dispatches the cache/bundle subcommands; internal/runner builds the guest
// initramfs, launches qemu, and parses the verdict; internal/boot resolves the
// kernel + modules; internal/arch is the per-architecture registry. This file is a
// thin shim — its one job beyond calling cli.Main is to declare the embedded init
// source (see embeds.go), which must live here because //go:embed paths are
// relative to the source directory and cannot reach the repo root from elsewhere.
package main

import (
	"os"

	"github.com/samuel/qemu-test/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], embedFiles))
}
