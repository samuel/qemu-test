package builder

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/samuel/qemu-test/internal/cpio"
)

// Target is the cross-compilation target for the guest binaries. Init and the
// test binary share one, since they run in the same VM.
type Target struct {
	GOOS   string
	GOARCH string
}

// env returns the process environment for cross-compiling with cgo disabled.
func (t Target) env() []string {
	return append(os.Environ(), "CGO_ENABLED=0", "GOOS="+t.GOOS, "GOARCH="+t.GOARCH)
}

func GoBuild(ctx context.Context, out io.Writer, target Target, srcPath string, args ...string) error {
	var pwd string
	if strings.HasPrefix(srcPath, ".") || strings.HasPrefix(srcPath, "/") {
		var err error
		pwd, err = filepath.Abs(srcPath)
		if err != nil {
			return err
		}
	}
	args = append([]string{"build", "-o", "/dev/stdout"}, args...)
	if pwd == "" {
		args = append(args, srcPath)
	}
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Env = target.env()
	cmd.Dir = pwd
	cmd.Stdout = out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build failed: %w", err)
	}
	return nil
}

func GoBuildTest(ctx context.Context, out io.Writer, target Target, srcPath string, args ...string) error {
	var pwd string
	if strings.HasPrefix(srcPath, ".") || strings.HasPrefix(srcPath, "/") {
		var err error
		pwd, err = filepath.Abs(srcPath)
		if err != nil {
			return err
		}
	}
	args = append([]string{"test", "-o", "/dev/stdout", "-c"}, args...)
	if pwd == "" {
		args = append(args, srcPath)
	}
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Env = target.env()
	cmd.Dir = pwd
	cmd.Stdout = out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go test -c failed: %w", err)
	}
	return nil
}

// SizeFromReader returns the size of the reader's data, rewinding it if it is
// seekable. It errors when the size cannot be had without reading.
func SizeFromReader(r io.Reader) (int64, error) {
	if ln, ok := r.(interface{ Len() int }); ok {
		return int64(ln.Len()), nil
	}
	if stat, ok := r.(interface{ Stat() (fs.FileInfo, error) }); ok {
		st, err := stat.Stat()
		if err != nil {
			return 0, err
		}
		return st.Size(), nil
	}
	if stat, ok := r.(interface{ Stat() (os.FileInfo, error) }); ok {
		st, err := stat.Stat()
		if err != nil {
			return 0, err
		}
		return st.Size(), nil
	}
	if sk, ok := r.(io.Seeker); ok {
		size, err := sk.Seek(0, io.SeekEnd)
		if err != nil {
			return 0, err
		}
		if _, err := sk.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
		return size, nil
	}
	return 0, fmt.Errorf("unable to get size of reader of type %T", r)
}

func SizesFromReaders(files map[string]io.Reader) (map[string]int64, error) {
	fileSizes := make(map[string]int64)
	for name, f := range files {
		sz, err := SizeFromReader(f)
		if err != nil {
			return nil, fmt.Errorf("unable to get size of file %q: %w", name, err)
		}
		fileSizes[name] = sz
	}
	return fileSizes, nil
}

func BuildInitramfs(out io.Writer, files map[string]io.Reader, fileSizes map[string]int64) error {
	// Normalize into a fresh map rather than mutating the caller's, keying reader
	// and size alike, and rejecting a collision instead of dropping a file.
	type entry struct {
		reader io.Reader
		size   int64
	}
	norm := make(map[string]entry, len(files))
	for name, f := range files {
		n := cpio.Normalize(name)
		if _, ok := norm[n]; ok {
			return fmt.Errorf("duplicate file name after normalization: %q", n)
		}
		norm[n] = entry{reader: f, size: fileSizes[name]}
	}

	names := make([]string, 0, len(norm))
	for name := range norm {
		names = append(names, name)
	}
	slices.Sort(names)

	recWriter := cpio.NewWriter(out)
	//nolint:errcheck // On success Close's trailer write is checked by the return below; this close only covers error paths.
	defer recWriter.Close()

	for i, name := range names {
		e := norm[name]
		if err := recWriter.WriteRecord(cpio.Record{
			Reader: e.reader,
			Info: cpio.Info{
				Ino:      uint32(i),
				Mode:     0o755 | cpio.FileTypeRegular,
				UID:      0,
				GID:      0,
				NLink:    1,
				MTime:    0,
				FileSize: uint32(e.size),
				Name:     name,
			},
		}); err != nil {
			return err
		}
	}

	return recWriter.Close()
}
