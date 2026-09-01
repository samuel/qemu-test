package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// moduleInitCompressedFile is finit_module(2)'s MODULE_INIT_COMPRESSED_FILE: the
// kernel decompresses the module itself (needs CONFIG_MODULE_DECOMPRESS). Defined
// locally so it does not depend on the pulled x/sys/unix version.
const moduleInitCompressedFile = 0x4

// loadModules loads the modules listed in test-modules.txt, one in-initramfs .ko
// path per line in dependency order. The file is optional. A failure fails the
// run: the host only packs modules it found, so this is a genuine load error.
func loadModules() error {
	raw, err := os.ReadFile("test-modules.txt")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("load test-modules.txt: %w", err)
	}
	for path := range strings.SplitSeq(string(raw), "\n") {
		if path = strings.TrimSpace(path); path == "" {
			continue
		}
		fmt.Println("Loading module", path)
		if err := loadModule(path); err != nil {
			return err
		}
	}
	return nil
}

func loadModule(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open module %q: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only handle; the finit_module result is what matters
	var flags int
	if !strings.HasSuffix(path, ".ko") {
		flags = moduleInitCompressedFile // .ko.gz/.xz/.zst — let the kernel decompress
	}
	if err := unix.FinitModule(int(f.Fd()), "", flags); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return nil // already loaded (dependency loaded earlier, or built in)
		}
		return fmt.Errorf("load module %q: %w", path, err)
	}
	return nil
}
