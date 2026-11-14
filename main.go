// Command qemu-test runs a static Linux executable, in practice a Go test
// binary, inside a freshly-booted QEMU VM. It is built to be the -exec program
// of go test.
//
// The work lives in internal/cli, internal/runner, internal/boot, and
// internal/arch. This package is a thin shim; it also declares the embedded init
// source (embeds.go), which must live here because //go:embed paths are relative
// to the source directory and cannot reach the repo root from elsewhere.
package main

import (
	"os"

	"samuelks.com/qemu-test/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], embedFiles))
}
