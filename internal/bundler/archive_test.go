package bundler

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/samuel/qemu-test/internal/cpio"
)

// regRec builds a regular-file cpio record.
func regRec(name, body string) cpio.Record {
	return cpio.Record{
		Reader: strings.NewReader(body),
		Info: cpio.Info{
			Mode:     0o644 | cpio.FileTypeRegular,
			NLink:    1,
			FileSize: uint32(len(body)),
			Name:     name,
		},
	}
}

// dirRec builds a directory cpio record (no content).
func dirRec(name string) cpio.Record {
	return cpio.Record{Info: cpio.Info{Mode: 0o755 | cpio.FileTypeDirectory, NLink: 2, Name: name}}
}

// cpioBytes encodes recs as a newc cpio archive, gzip-compressed when compress.
func cpioBytes(t *testing.T, compress bool, recs ...cpio.Record) []byte {
	t.Helper()
	var raw bytes.Buffer
	cw := cpio.NewWriter(&raw)
	for _, r := range recs {
		if err := cw.WriteRecord(r); err != nil {
			t.Fatalf("write cpio record %q: %v", r.Name, err)
		}
	}
	if err := cw.Close(); err != nil {
		t.Fatalf("close cpio: %v", err)
	}
	return maybeGzip(t, compress, raw.Bytes())
}

// tarBytes encodes files as a tar archive, gzip-compressed when compress. A
// name ending in "/" is written as a directory entry.
func tarBytes(t *testing.T, compress bool, files map[string]string) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		body := files[name]
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if strings.HasSuffix(name, "/") {
			hdr.Typeflag, hdr.Mode, hdr.Size = tar.TypeDir, 0o755, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header %q: %v", name, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatalf("write tar body %q: %v", name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return maybeGzip(t, compress, raw.Bytes())
}

func maybeGzip(t *testing.T, compress bool, b []byte) []byte {
	t.Helper()
	if !compress {
		return b
	}
	var out bytes.Buffer
	gw := gzip.NewWriter(&out)
	if _, err := gw.Write(b); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return out.Bytes()
}

// writeTemp writes data to a fresh temp file and returns its path.
func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "archive")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	return p
}

// readAll collects the regular-file entries readArchive yields, keyed by name.
func readAll(t *testing.T, data []byte) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := readArchive(writeTemp(t, data), func(e archiveEntry) error {
		got[e.name] = string(e.content)
		return nil
	})
	if err != nil {
		t.Fatalf("readArchive: %v", err)
	}
	return got
}

func TestReadArchiveFormats(t *testing.T) {
	want := map[string]string{
		"usr/lib/modules/6.12/modules.dep":     "deps",
		"usr/lib/modules/6.12/kernel/e1000.ko": "ko",
	}
	// Fresh records per call: WriteRecord consumes each record's reader, so a
	// shared slice cannot be encoded twice.
	cpioRecs := func() []cpio.Record {
		return []cpio.Record{
			regRec("usr/lib/modules/6.12/modules.dep", "deps"),
			regRec("usr/lib/modules/6.12/kernel/e1000.ko", "ko"),
		}
	}
	cases := []struct {
		name string
		data []byte
	}{
		{"cpio", cpioBytes(t, false, cpioRecs()...)},
		{"cpio.gz", cpioBytes(t, true, cpioRecs()...)},
		{"tar", tarBytes(t, false, want)},
		{"tar.gz", tarBytes(t, true, want)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := readAll(t, tc.data); !maps.Equal(got, want) {
				t.Errorf("entries = %v, want %v", got, want)
			}
		})
	}
}

func TestReadArchiveSkipsNonRegular(t *testing.T) {
	cpioData := cpioBytes(t, false,
		dirRec("usr/lib/modules"),
		dirRec("usr/lib/modules/6.12"),
		regRec("usr/lib/modules/6.12/x.ko", "ko"),
	)
	if got := readAll(t, cpioData); !maps.Equal(got, map[string]string{"usr/lib/modules/6.12/x.ko": "ko"}) {
		t.Errorf("cpio: directory entries not skipped: %v", got)
	}

	tarData := tarBytes(t, false, map[string]string{
		"usr/lib/modules/":     "",
		"usr/lib/modules/x.ko": "ko",
	})
	if got := readAll(t, tarData); !maps.Equal(got, map[string]string{"usr/lib/modules/x.ko": "ko"}) {
		t.Errorf("tar: directory entries not skipped: %v", got)
	}
}

func TestReadArchiveUnsupportedCompression(t *testing.T) {
	cases := map[string][]byte{
		"xz":   {0xfd, '7', 'z', 'X', 'Z', 0x00},
		"zstd": {0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x00},
	}
	for name, magic := range cases {
		t.Run(name, func(t *testing.T) {
			err := readArchive(writeTemp(t, magic), func(archiveEntry) error { return nil })
			if err == nil || !strings.Contains(err.Error(), "not supported") {
				t.Errorf("err = %v, want an unsupported-compression error", err)
			}
		})
	}
}
