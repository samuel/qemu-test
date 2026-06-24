package bundler

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/samuel/qemu-test/internal/cpio"
)

// modeTypeMask isolates the file-type bits (S_IFMT) of a cpio record's mode so
// regular files can be told apart from directories, symlinks, and device nodes.
const modeTypeMask = 0o170000

// archiveEntry is one regular file read out of an input archive.
type archiveEntry struct {
	name    string // normalized name (no leading "./" or "/")
	mode    uint32 // permission bits only
	content []byte
}

// readArchive reads every regular file from the archive at path, calling fn for
// each. The archive may be a newc cpio (e.g. a Linux initramfs) or a tar,
// optionally gzip-compressed; the format is detected by leading magic bytes,
// not by file extension. Directories, symlinks, and other non-regular entries
// are skipped, mirroring cache.ExtractArchive.
func readArchive(path string, fn func(archiveEntry) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read-only handle; close error is non-actionable

	br := bufio.NewReader(f)
	src := br
	switch m, _ := br.Peek(6); {
	case len(m) >= 2 && m[0] == 0x1f && m[1] == 0x8b:
		gz, err := gzip.NewReader(br)
		if err != nil {
			return fmt.Errorf("gzip: %w", err)
		}
		defer gz.Close() //nolint:errcheck // read-only decompressor; close error is non-actionable
		src = bufio.NewReader(gz)
	case len(m) >= 5 && m[0] == 0xfd && string(m[1:5]) == "7zXZ":
		return errors.New("xz-compressed archives are not supported; recompress the source as gzip")
	case len(m) >= 4 && m[0] == 0x28 && m[1] == 0xb5 && m[2] == 0x2f && m[3] == 0xfd:
		return errors.New("zstd-compressed archives are not supported; recompress the source as gzip")
	}

	// newc cpio carries its magic at offset 0; anything else is treated as tar
	// (whose "ustar" magic sits at offset 257), and a non-archive surfaces as a
	// read error from the tar reader.
	if m, _ := src.Peek(6); string(m) == "070701" {
		return readCPIO(src, fn)
	}
	return readTar(src, fn)
}

func readCPIO(r io.Reader, fn func(archiveEntry) error) error {
	cr := cpio.NewReader(r)
	for {
		rec, err := cr.ReadRecord()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("cpio: %w", err)
		}
		if rec.Mode&modeTypeMask != cpio.FileTypeRegular {
			continue
		}
		content, err := io.ReadAll(rec.Reader)
		if err != nil {
			return fmt.Errorf("cpio read %q: %w", rec.Name, err)
		}
		if err := fn(archiveEntry{name: cpio.Normalize(rec.Name), mode: rec.Mode & 0o7777, content: content}); err != nil {
			return err
		}
	}
}

func readTar(r io.Reader, fn func(archiveEntry) error) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			return fmt.Errorf("tar read %q: %w", hdr.Name, err)
		}
		if err := fn(archiveEntry{name: cpio.Normalize(hdr.Name), mode: uint32(hdr.Mode) & 0o7777, content: content}); err != nil {
			return err
		}
	}
}
