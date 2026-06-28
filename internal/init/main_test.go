package main

import (
	"bytes"
	"testing"
)

func TestPrefixedLineWriterFreshLine(t *testing.T) {
	var buf bytes.Buffer
	w := &prefixedLineWriter{w: &buf, prefix: "P "}

	// A write without a trailing newline leaves the writer mid-line.
	if _, err := w.Write([]byte("abc")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !w.pre {
		t.Fatal("expected mid-line (pre=true) after un-terminated write")
	}

	// freshLine terminates the pending line and clears the mid-line state.
	if err := w.freshLine(); err != nil {
		t.Fatalf("freshLine: %v", err)
	}
	if w.pre {
		t.Fatal("freshLine should clear pre")
	}
	if got, want := buf.String(), "P abc\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// freshLine when not mid-line is a no-op.
	buf.Reset()
	if err := w.freshLine(); err != nil {
		t.Fatalf("freshLine (no-op): %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("freshLine should be a no-op when not mid-line, wrote %q", buf.String())
	}

	// A newline-terminated write does not leave the writer mid-line.
	buf.Reset()
	if _, err := w.Write([]byte("def\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if w.pre {
		t.Fatal("expected not mid-line (pre=false) after newline-terminated write")
	}
	if err := w.freshLine(); err != nil {
		t.Fatalf("freshLine: %v", err)
	}
	if got, want := buf.String(), "P def\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
