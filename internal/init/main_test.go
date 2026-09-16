package main

import (
	"bytes"
	"strings"
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

// envOf returns the value of the first KEY= in env, matching the Go runtime's
// first-occurrence-wins resolution.
func envOf(env []string, key string) (string, bool) {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v, true
		}
	}
	return "", false
}

// A -env value must win over inherited (host) and default values even though it
// is first in the list: the runtime keeps the first occurrence of a key.
func TestTestEnvironmentUserOverrides(t *testing.T) {
	env := testEnvironment([]string{"FOO=bar", "PATH=/custom"})
	if got, ok := envOf(env, "FOO"); !ok || got != "bar" {
		t.Fatalf("FOO = %q, %v; want \"bar\", true", got, ok)
	}
	// PATH exists in the host environment and as a default; the user's value wins.
	if got, ok := envOf(env, "PATH"); !ok || got != "/custom" {
		t.Fatalf("PATH = %q, %v; want \"/custom\", true", got, ok)
	}
}

// A repeated -env key resolves to the last value, whichever order the entries
// take after the user list is deduplicated.
func TestTestEnvironmentRepeatedKey(t *testing.T) {
	env := testEnvironment([]string{"FOO=first", "FOO=last"})
	if got, ok := envOf(env, "FOO"); !ok || got != "last" {
		t.Fatalf("FOO = %q, %v; want \"last\", true", got, ok)
	}
}

// Defaults fill in only keys neither the user nor the inherited environment set,
// and a user-supplied key must remain the only surviving value.
func TestTestEnvironmentDefaultsNotUsed(t *testing.T) {
	env := testEnvironment([]string{"HOME=/custom"})
	if got, ok := envOf(env, "HOME"); !ok || got != "/custom" {
		t.Fatalf("HOME = %q, %v; want \"/custom\", true", got, ok)
	}
}
