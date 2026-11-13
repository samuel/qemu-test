package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"samuelks.com/qemu-test/internal/arch"
)

// installExecCommand implements "qemu-test install-exec": it symlinks
// go_linux_<arch>_exec to this executable for every supported arch. The go
// command runs a program of that name found on PATH for binaries cross-compiled
// to that target, so `GOOS=linux GOARCH=<arch> go test` then runs in a VM with
// no -exec flag.
func installExecCommand(args []string) int {
	fs := flag.NewFlagSet("qemu-test install-exec", flag.ContinueOnError)
	fs.Usage = func() {
		_, _ = fmt.Fprint(fs.Output(), `qemu-test install-exec symlinks go_linux_<arch>_exec to qemu-test for every
supported architecture. With the directory on PATH, the go command runs
cross-compiled linux binaries (go test, go run) through qemu-test automatically:

  GOOS=linux GOARCH=arm64 go test ./...

VM flags for such runs come from the QEMU_TEST_FLAGS environment variable.

Usage:
  qemu-test install-exec [dir]

dir defaults to the directory containing the qemu-test executable.
`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}

	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		return fail(1, fmt.Errorf("locate qemu-test executable: %w", err))
	}
	dir := fs.Arg(0)
	if dir == "" {
		dir = filepath.Dir(self)
	}

	for _, name := range arch.Names() {
		link := filepath.Join(dir, "go_linux_"+name+"_exec")
		if st, err := os.Lstat(link); err == nil {
			if st.Mode()&os.ModeSymlink == 0 {
				return fail(1, fmt.Errorf("%s exists and is not a symlink", link))
			}
			if err := os.Remove(link); err != nil {
				return fail(1, err)
			}
		}
		if err := os.Symlink(self, link); err != nil {
			return fail(1, err)
		}
		fmt.Printf("%s -> %s\n", link, self)
	}
	return 0
}
