package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRecordWriter(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"line\n", "[TESTS:OUT] line\n"},
		{"partial", "[TESTS:OUT-] partial\n"},
		{"a\nb\n", "[TESTS:OUT] a\n[TESTS:OUT] b\n"},
		{"a\nrest", "[TESTS:OUT] a\n[TESTS:OUT-] rest\n"},
		{"\n", "[TESTS:OUT] \n"},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		w := &recordWriter{w: &buf, key: "OUT"}
		n, err := w.Write([]byte(c.in))
		if err != nil || n != len(c.in) {
			t.Errorf("Write(%q) = %d, %v", c.in, n, err)
		}
		if got := buf.String(); got != c.want {
			t.Errorf("Write(%q) wrote %q, want %q", c.in, got, c.want)
		}
	}
}

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
