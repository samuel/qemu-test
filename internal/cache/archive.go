package cache

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// ExtractArchiveAtomic extracts archivePath into a fresh temp dir under workDir,
// writes a .extracted completion marker, then replaces dest with it atomically.
func ExtractArchiveAtomic(archivePath, dest, workDir string) error {
	tmp, err := os.MkdirTemp(workDir, "extract-*")
	if err != nil {
		return err
	}
	if err := ExtractArchive(archivePath, tmp); err != nil {
		_ = os.RemoveAll(tmp) //nolint:errcheck // best-effort cleanup of the partial extraction
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, ".extracted"), nil, 0o644); err != nil {
		_ = os.RemoveAll(tmp) //nolint:errcheck // best-effort cleanup
		return err
	}
	// Rename onto an existing dir fails, so clear any prior extraction first.
	if err := os.RemoveAll(dest); err != nil {
		_ = os.RemoveAll(tmp) //nolint:errcheck // best-effort cleanup
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.RemoveAll(tmp) //nolint:errcheck // best-effort cleanup
		return err
	}
	return nil
}

// ExtractArchive extracts a tar or gzip-compressed tar (sniffed by magic) into
// destDir. Only regular files and directories are written: a modules tree needs
// nothing else, and skipping links avoids escapes.
func ExtractArchive(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read-only handle; close error is non-actionable

	br := bufio.NewReader(f)
	var src io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return fmt.Errorf("gzip: %w", err)
		}
		defer gz.Close() //nolint:errcheck // read-only decompressor; close error is non-actionable
		src = gz
	}

	tr := tar.NewReader(src)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(destDir, hdr.Name)
		// Zip-slip guard: Join cleans the path, so an entry climbing out of
		// destDir no longer has it as a prefix.
		if target != destDir && !strings.HasPrefix(target, destDir+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe archive entry %q escapes the extraction directory", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777|0o600)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, tr)
			closeErr := out.Close()
			if err := firstErr(copyErr, closeErr); err != nil {
				return fmt.Errorf("write %q: %w", hdr.Name, err)
			}
		default:
			slog.Debug("skipping non-regular archive entry", "name", hdr.Name, "type", hdr.Typeflag)
		}
	}
}
