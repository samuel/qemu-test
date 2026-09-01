// Command qemu-test runs a Go package's tests inside a freshly-booted QEMU VM.
//
// The work lives in internal/cli, internal/runner, internal/boot, and
// internal/arch. This package is a thin shim; it also declares the embedded init
// source (embeds.go), which must live here because //go:embed paths are relative
// to the source directory and cannot reach the repo root from elsewhere.
package main

import (
	"os"

	"github.com/samuel/qemu-test/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], embedFiles))
}
