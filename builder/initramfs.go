package builder

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/samuel/qemu-test/builder/internal/cpio"
)

// Target is the cross-compilation target for the guest binaries. The same Target
// is used for both the init and the test binary, since they run in one VM and
// must share an architecture.
type Target struct {
	GOOS   string
	GOARCH string
}

// env returns the process environment for cross-compiling to the target with cgo
// disabled.
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

// SizeFromReader attempts to get the size of data represented by the reader.
// The reader may be rewound to the beginning if seekable. If there's no
// way to get the size without reading then an error is returned.
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
	for name, f := range files {
		if n := cpio.Normalize(name); n != name {
			delete(files, name)
			files[n] = f
		}
	}

	// Provide a stable order for names.
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	recWriter := cpio.NewWriter(out)
	//nolint:errcheck // After success build the writer is closed and error is checked. This covers the failure paths.
	defer recWriter.Close()

	for i, name := range names {
		if err := recWriter.WriteRecord(cpio.Record{
			Reader: files[name],
			Info: cpio.Info{
				Ino:      uint32(i),
				Mode:     0o755 | cpio.FileTypeRegular,
				UID:      0,
				GID:      0,
				NLink:    1,
				MTime:    0,
				FileSize: uint32(fileSizes[name]),
				Name:     name,
			},
		}); err != nil {
			return err
		}
	}

	return recWriter.Close()
}
