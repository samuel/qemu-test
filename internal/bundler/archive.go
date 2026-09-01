package bundler

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/samuel/qemu-test/internal/cpio"
	"github.com/ulikunitz/xz"
)

// modeTypeMask isolates the file-type bits (S_IFMT) of a cpio record's mode.
const modeTypeMask = 0o170000

// arMagic is the global header of an ar archive, which a Debian .deb is.
const arMagic = "!<arch>\n"

// archiveEntry is one regular file read out of an input archive.
type archiveEntry struct {
	name    string // normalized name (no leading "./" or "/")
	mode    uint32 // permission bits only
	content []byte
}

// readArchive reads every regular file from the archive at path, calling fn for
// each. The archive may be a newc cpio, a tar, or a Debian .deb, optionally
// gzip/xz/zstd-compressed; the format is detected by magic, not extension.
func readArchive(path string, fn func(archiveEntry) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read-only handle; close error is non-actionable

	return readArchiveStream(bufio.NewReader(f), fn)
}

// readArchiveStream reads an archive from r: a .deb is unwrapped to its data.tar
// member (recursively), any compression wrapper stripped, then cpio or tar read.
func readArchiveStream(r *bufio.Reader, fn func(archiveEntry) error) error {
	if m, _ := r.Peek(len(arMagic)); string(m) == arMagic {
		return readDeb(r, fn)
	}

	src, closer, err := decompress(r)
	if err != nil {
		return err
	}
	if closer != nil {
		defer closer.Close() //nolint:errcheck // read-only decompressor; close error is non-actionable
	}
	sb, ok := src.(*bufio.Reader)
	if !ok {
		sb = bufio.NewReader(src)
	}

	// newc cpio carries its magic at offset 0; anything else is read as tar, and
	// a non-archive surfaces as a read error from the tar reader.
	if m, _ := sb.Peek(6); string(m) == "070701" {
		return readCPIO(sb, fn)
	}
	return readTar(sb, fn)
}

// decompress wraps r in a gzip, xz, or zstd decompressor chosen by magic,
// returning it plus an optional Closer releasing its resources. An uncompressed
// stream is returned unchanged.
func decompress(r *bufio.Reader) (io.Reader, io.Closer, error) {
	switch m, _ := r.Peek(6); {
	case len(m) >= 2 && m[0] == 0x1f && m[1] == 0x8b:
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, nil, fmt.Errorf("gzip: %w", err)
		}
		return gz, gz, nil
	case len(m) >= 6 && m[0] == 0xfd && string(m[1:6]) == "7zXZ\x00":
		xr, err := xz.NewReader(r)
		if err != nil {
			return nil, nil, fmt.Errorf("xz: %w", err)
		}
		return xr, nil, nil // ulikunitz/xz readers hold no resources to close
	case len(m) >= 4 && m[0] == 0x28 && m[1] == 0xb5 && m[2] == 0x2f && m[3] == 0xfd:
		zr, err := zstd.NewReader(r)
		if err != nil {
			return nil, nil, fmt.Errorf("zstd: %w", err)
		}
		rc := zr.IOReadCloser() // Close releases the decoder's goroutines
		return rc, rc, nil
	default:
		return r, nil, nil
	}
}

// readDeb reads the data.tar member of a .deb (an ar archive), which holds the
// installed files; the metadata members are skipped.
func readDeb(r *bufio.Reader, fn func(archiveEntry) error) error {
	if _, err := r.Discard(len(arMagic)); err != nil {
		return fmt.Errorf("ar: %w", err)
	}
	for {
		name, size, err := readArHeader(r)
		if errors.Is(err, io.EOF) {
			return errors.New("deb: no data.tar member found")
		}
		if err != nil {
			return err
		}
		if strings.HasPrefix(name, "data.tar") {
			return readArchiveStream(bufio.NewReader(io.LimitReader(r, size)), fn)
		}
		// ar members are padded to an even byte boundary.
		skip := size + size%2
		if _, err := io.CopyN(io.Discard, r, skip); err != nil {
			return fmt.Errorf("ar: skip member %q: %w", name, err)
		}
	}
}

// readArHeader reads one 60-byte ar member header, returning the member's name
// and content size, or io.EOF when the archive has no more members.
func readArHeader(r io.Reader) (name string, size int64, err error) {
	var hdr [60]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return "", 0, err // io.EOF here means the last member was consumed
	}
	if hdr[58] != 0x60 || hdr[59] != 0x0a {
		return "", 0, errors.New("ar: bad member header")
	}
	name = strings.TrimSuffix(strings.TrimRight(string(hdr[0:16]), " "), "/")
	sizeStr := strings.TrimSpace(string(hdr[48:58]))
	size, err = strconv.ParseInt(sizeStr, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("ar: bad member size %q: %w", sizeStr, err)
	}
	return name, size, nil
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
