package builder

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/qemu-test/internal/cpio"
)

func TestBuildInitramfs(t *testing.T) {
	// "init" carries NUL/binary bytes to exercise FileSize-based copying.
	contents := map[string]string{
		"init":          "INIT-BINARY\x00\x01\x02more",
		"tests":         "the compiled test binary bytes",
		"test-args.txt": "-test.v\n-test.run\nTestX",
	}
	build := func() []byte {
		files := make(map[string]io.Reader, len(contents))
		for name, c := range contents {
			files[name] = bytes.NewBufferString(c)
		}
		sizes, err := SizesFromReaders(files)
		if err != nil {
			t.Fatalf("SizesFromReaders: %v", err)
		}
		var buf bytes.Buffer
		if err := BuildInitramfs(&buf, files, sizes); err != nil {
			t.Fatalf("BuildInitramfs: %v", err)
		}
		return buf.Bytes()
	}

	archive := build()
	recs, err := cpio.ReadAll(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("cpio.ReadAll: %v", err)
	}

	// One record per input file; ReadAll stops at the trailer Close writes.
	if len(recs) != len(contents) {
		t.Fatalf("record count = %d, want %d", len(recs), len(contents))
	}

	// Records are written in sorted name order, so "init" is present and first
	// (it sorts before "test-args.txt" and "tests").
	wantOrder := []string{"init", "test-args.txt", "tests"}
	for i, want := range wantOrder {
		if recs[i].Name != want {
			t.Errorf("record[%d].Name = %q, want %q", i, recs[i].Name, want)
		}
	}
	if recs[0].Name != "init" {
		t.Errorf("first record = %q, want %q", recs[0].Name, "init")
	}

	for i, r := range recs {
		if r.NLink != 1 {
			t.Errorf("%s: NLink = %d, want 1", r.Name, r.NLink)
		}
		if r.MTime != 0 {
			t.Errorf("%s: MTime = %d, want 0 (deterministic)", r.Name, r.MTime)
		}
		if r.Mode != 0o100755 {
			t.Errorf("%s: Mode = %#o, want %#o", r.Name, r.Mode, 0o100755)
		}
		if r.UID != 0 || r.GID != 0 {
			t.Errorf("%s: UID/GID = %d/%d, want 0/0", r.Name, r.UID, r.GID)
		}
		if int(r.Ino) != i {
			t.Errorf("%s: Ino = %d, want %d", r.Name, r.Ino, i)
		}
		want := contents[r.Name]
		if r.FileSize != uint32(len(want)) {
			t.Errorf("%s: FileSize = %d, want %d", r.Name, r.FileSize, len(want))
		}
		got, err := io.ReadAll(r.Reader)
		if err != nil {
			t.Fatalf("%s: read content: %v", r.Name, err)
		}
		if string(got) != want {
			t.Errorf("%s: content = %q, want %q", r.Name, got, want)
		}
	}

	// Sorted names + zero MTime mean the same inputs yield byte-identical output.
	if archive2 := build(); !bytes.Equal(archive, archive2) {
		t.Error("BuildInitramfs output is not deterministic")
	}
}

// Two inputs that normalize to the same name must be rejected rather than
// silently collapsed.
func TestBuildInitramfsRejectsCollision(t *testing.T) {
	files := map[string]io.Reader{
		"init":  bytes.NewBufferString("a"),
		"/init": bytes.NewBufferString("b"),
	}
	sizes, err := SizesFromReaders(files)
	if err != nil {
		t.Fatalf("SizesFromReaders: %v", err)
	}
	if err := BuildInitramfs(io.Discard, files, sizes); err == nil {
		t.Fatal("expected error for names colliding after normalization, got nil")
	}
}

// BuildInitramfs must not mutate the caller's files map while normalizing names.
func TestBuildInitramfsDoesNotMutateInput(t *testing.T) {
	files := map[string]io.Reader{
		"/init": bytes.NewBufferString("x"),
	}
	sizes, err := SizesFromReaders(files)
	if err != nil {
		t.Fatalf("SizesFromReaders: %v", err)
	}
	if err := BuildInitramfs(io.Discard, files, sizes); err != nil {
		t.Fatalf("BuildInitramfs: %v", err)
	}
	if _, ok := files["/init"]; !ok || len(files) != 1 {
		t.Errorf("BuildInitramfs mutated the caller's files map: %v", files)
	}
}

// seekOnlyReader exposes only Read and Seek, so SizeFromReader must fall back to
// seeking.
type seekOnlyReader struct{ r *bytes.Reader }

func (s seekOnlyReader) Read(p []byte) (int, error)                { return s.r.Read(p) }
func (s seekOnlyReader) Seek(off int64, whence int) (int64, error) { return s.r.Seek(off, whence) }

// readOnly exposes only Read, so its size cannot be determined.
type readOnly struct{}

func (readOnly) Read([]byte) (int, error) { return 0, io.EOF }

func TestSizeFromReader(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(tmp, []byte("on-disk!"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(tmp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })

	tests := []struct {
		name string
		r    io.Reader
		want int64
	}{
		{"bytes.Buffer", bytes.NewBufferString("hello"), 5},
		{"bytes.Reader", bytes.NewReader([]byte("abcd")), 4},
		{"strings.Reader", strings.NewReader("xyz"), 3},
		{"os.File", f, 8},
		{"seekOnly", seekOnlyReader{bytes.NewReader([]byte("seek-me"))}, 7},
		{"empty", bytes.NewBuffer(nil), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SizeFromReader(tc.r)
			if err != nil {
				t.Fatalf("SizeFromReader: %v", err)
			}
			if got != tc.want {
				t.Errorf("size = %d, want %d", got, tc.want)
			}
		})
	}

	t.Run("seek rewinds", func(t *testing.T) {
		sr := seekOnlyReader{bytes.NewReader([]byte("rewind-content"))}
		if _, err := SizeFromReader(sr); err != nil {
			t.Fatalf("SizeFromReader: %v", err)
		}
		got, err := io.ReadAll(sr)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "rewind-content" {
			t.Errorf("reader not rewound after sizing: read %q", got)
		}
	})

	t.Run("unsupported", func(t *testing.T) {
		if _, err := SizeFromReader(readOnly{}); err == nil {
			t.Error("expected error for reader without a determinable size, got nil")
		}
	})
}

func TestSizesFromReaders(t *testing.T) {
	sizes, err := SizesFromReaders(map[string]io.Reader{
		"a": bytes.NewBufferString("12345"),
		"b": strings.NewReader("xy"),
	})
	if err != nil {
		t.Fatalf("SizesFromReaders: %v", err)
	}
	if sizes["a"] != 5 || sizes["b"] != 2 {
		t.Errorf("sizes = %v, want a=5 b=2", sizes)
	}

	// A single unsized reader fails the whole call.
	if _, err := SizesFromReaders(map[string]io.Reader{"bad": readOnly{}}); err == nil {
		t.Error("expected error for unsized reader, got nil")
	}
}
