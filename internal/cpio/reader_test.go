package cpio

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestReaderRoundTrip(t *testing.T) {
	want := []struct {
		name    string
		content []byte
	}{
		{"init", []byte("INIT-BINARY\x00\x01\x02more")},
		{"empty", nil}, // FileSize 0: no content and no trailing pad
		{"a-rather-long-name-to-force-name-padding", []byte("x")},
		{"mid", []byte("0123456789")},
	}

	var buf bytes.Buffer
	w := NewWriter(&buf)
	for i, f := range want {
		if err := w.WriteRecord(Record{
			Reader: bytes.NewReader(f.content),
			Info: Info{
				Ino:      uint32(i),
				Mode:     0o100644,
				UID:      1,
				GID:      2,
				NLink:    1,
				MTime:    42,
				FileSize: uint32(len(f.content)),
				Name:     f.name,
			},
		}); err != nil {
			t.Fatalf("WriteRecord(%q): %v", f.name, err)
		}
	}

	recs, err := ReadAll(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(recs) != len(want) {
		t.Fatalf("read %d records, want %d", len(recs), len(want))
	}
	for i, f := range want {
		r := recs[i]
		if r.Name != f.name {
			t.Errorf("record[%d].Name = %q, want %q", i, r.Name, f.name)
		}
		wantInfo := Info{Ino: uint32(i), Mode: 0o100644, UID: 1, GID: 2, NLink: 1, MTime: 42, FileSize: uint32(len(f.content)), Name: f.name}
		if r.Info != wantInfo {
			t.Errorf("%q: Info = %+v, want %+v", f.name, r.Info, wantInfo)
		}
		got, err := io.ReadAll(r.Reader)
		if err != nil {
			t.Fatalf("%q: read content: %v", f.name, err)
		}
		if !bytes.Equal(got, f.content) {
			t.Errorf("%q: content = %q, want %q", f.name, got, f.content)
		}
	}
}

func TestReaderBadMagic(t *testing.T) {
	// "070702" is the CRC newc variant, which this package neither writes nor reads.
	if _, err := ReadAll(bytes.NewReader([]byte("070702garbage"))); err == nil {
		t.Fatal("expected a bad-magic error, got nil")
	}
}

func TestReaderTruncated(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.WriteRecord(Record{
		Reader: bytes.NewReader([]byte("some content here")),
		Info:   Info{NLink: 1, FileSize: 17, Name: "file"},
	}); err != nil {
		t.Fatal(err)
	}
	// Drop the tail so the read runs off the end mid-record.
	truncated := buf.Bytes()[:buf.Len()-5]
	if _, err := ReadAll(bytes.NewReader(truncated)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestReaderCleanEOF(t *testing.T) {
	// An empty archive yields no records and no error.
	recs, err := ReadAll(bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("ReadAll(empty): %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("got %d records from empty archive, want 0", len(recs))
	}

	// A single record: ReadRecord yields it, then reports io.EOF.
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.WriteRecord(Record{
		Reader: bytes.NewReader([]byte("hi")),
		Info:   Info{NLink: 1, FileSize: 2, Name: "f"},
	}); err != nil {
		t.Fatal(err)
	}
	r := NewReader(bytes.NewReader(buf.Bytes()))
	if _, err := r.ReadRecord(); err != nil {
		t.Fatalf("first ReadRecord: %v", err)
	}
	if _, err := r.ReadRecord(); !errors.Is(err, io.EOF) {
		t.Fatalf("second ReadRecord err = %v, want io.EOF", err)
	}
}

func TestReaderStopsAtTrailer(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.WriteRecord(Record{
		Reader: bytes.NewReader([]byte("data")),
		Info:   Info{NLink: 1, FileSize: 4, Name: "real"},
	}); err != nil {
		t.Fatal(err)
	}
	// A standard TRAILER!!! record (no content) ends the archive.
	if err := w.WriteRecord(Record{Info: Info{NLink: 1, Name: trailerName}}); err != nil {
		t.Fatal(err)
	}
	recs, err := ReadAll(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(recs) != 1 || recs[0].Name != "real" {
		t.Fatalf("read %d records, want exactly [real]", len(recs))
	}
}
