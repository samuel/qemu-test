package bundler

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/samuel/qemu-test/internal/cpio"
	"github.com/ulikunitz/xz"
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

// xzBytes returns b compressed as an xz stream.
func xzBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	xw, err := xz.NewWriter(&out)
	if err != nil {
		t.Fatalf("xz writer: %v", err)
	}
	if _, err := xw.Write(b); err != nil {
		t.Fatalf("xz write: %v", err)
	}
	if err := xw.Close(); err != nil {
		t.Fatalf("xz close: %v", err)
	}
	return out.Bytes()
}

// zstdBytes returns b compressed as a zstd stream.
func zstdBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	zw, err := zstd.NewWriter(&out)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	if _, err := zw.Write(b); err != nil {
		t.Fatalf("zstd write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	return out.Bytes()
}

// debBytes builds a minimal Debian .deb (an ar archive) whose data member is
// named dataName with content dataTar. A debian-binary and a stub control member
// precede it, as in a real .deb, so the reader must skip past them.
func debBytes(t *testing.T, dataName string, dataTar []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	out.WriteString("!<arch>\n")
	member := func(name string, content []byte) {
		// 60-byte header: name(16) mtime(12) uid(6) gid(6) mode(8) size(10) magic(2).
		hdr := fmt.Sprintf("%-16s%-12d%-6d%-6d%-8s%-10d`\n", name, 0, 0, 0, "100644", len(content))
		if len(hdr) != 60 {
			t.Fatalf("ar header for %q is %d bytes, want 60", name, len(hdr))
		}
		out.WriteString(hdr)
		out.Write(content)
		if len(content)%2 == 1 {
			out.WriteByte('\n') // members are padded to an even byte boundary
		}
	}
	member("debian-binary", []byte("2.0\n"))
	member("control.tar", []byte("stub")) // metadata; the reader skips it
	member(dataName, dataTar)
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
	// Fresh records per call: WriteRecord consumes each record's reader.
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
		{"tar.xz", xzBytes(t, tarBytes(t, false, want))},
		{"tar.zst", zstdBytes(t, tarBytes(t, false, want))},
		{"cpio.xz", xzBytes(t, cpioBytes(t, false, cpioRecs()...))},
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

func TestReadArchiveDeb(t *testing.T) {
	// A .deb is unwrapped to its data.tar member, which may itself be compressed.
	want := map[string]string{
		"usr/lib/modules/6.12/kernel/e1000.ko.xz": "ko",
		"boot/vmlinux-6.12":                       "kernel",
	}
	rawTar := tarBytes(t, false, map[string]string{
		"./usr/lib/modules/6.12/kernel/e1000.ko.xz": "ko",
		"./boot/vmlinux-6.12":                       "kernel",
	})
	cases := []struct {
		name     string
		dataName string
		data     []byte
	}{
		{"data.tar", "data.tar", rawTar},
		{"data.tar.gz", "data.tar.gz", maybeGzip(t, true, rawTar)},
		{"data.tar.xz", "data.tar.xz", xzBytes(t, rawTar)},
		{"data.tar.zst", "data.tar.zst", zstdBytes(t, rawTar)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := readAll(t, debBytes(t, tc.dataName, tc.data)); !maps.Equal(got, want) {
				t.Errorf("entries = %v, want %v", got, want)
			}
		})
	}
}

func TestReadArchiveDebNoDataMember(t *testing.T) {
	// An ar archive with no data.tar member is a malformed .deb.
	deb := debBytes(t, "not-data.tar", tarBytes(t, false, map[string]string{"x": "y"}))
	err := readArchive(writeTemp(t, deb), func(archiveEntry) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "no data.tar member") {
		t.Errorf("err = %v, want a missing-data.tar error", err)
	}
}
