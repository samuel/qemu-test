package cpio

import (
	"bytes"
	"errors"
	"testing"
)

func TestWriterCloseWritesTrailer(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	for _, name := range []string{"a", "b"} {
		content := []byte("content of " + name)
		if err := w.WriteRecord(Record{
			Reader: bytes.NewReader(content),
			Info:   Info{NLink: 1, FileSize: uint32(len(content)), Name: name},
		}); err != nil {
			t.Fatalf("WriteRecord(%q): %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !bytes.Contains(buf.Bytes(), []byte(trailerName)) {
		t.Errorf("archive does not contain a %s record", trailerName)
	}
	// ReadAll stops at the trailer and yields only the real records.
	recs, err := ReadAll(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(recs) != 2 || recs[0].Name != "a" || recs[1].Name != "b" {
		t.Fatalf("read %d records, want [a b]", len(recs))
	}
}

func TestWriterWriteAfterClose(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := w.WriteRecord(Record{Info: Info{NLink: 1, Name: "late"}}); !errors.Is(err, errWriterClosed) {
		t.Fatalf("WriteRecord after Close = %v, want errWriterClosed", err)
	}
}

func TestWriterCloseIdempotent(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.WriteRecord(Record{
		Reader: bytes.NewReader([]byte("x")),
		Info:   Info{NLink: 1, FileSize: 1, Name: "f"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if n := bytes.Count(buf.Bytes(), []byte(trailerName)); n != 1 {
		t.Errorf("archive has %d %s records, want exactly 1", n, trailerName)
	}
}
