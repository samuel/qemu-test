package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/qemu-test/internal/cache"
)

// runCache invokes cacheCommand with output captured, returning the exit code
// and combined output.
func runCache(t *testing.T, args ...string) (int, string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = w, w
	code := cacheCommand(args)
	_ = w.Close() //nolint:errcheck // pipe write end; nothing more is written
	os.Stdout, os.Stderr = oldOut, oldErr
	out, _ := io.ReadAll(r) //nolint:errcheck // reading the captured pipe; a read error fails the assertions below
	return code, string(out)
}

func TestCacheCommandExitCodes(t *testing.T) {
	t.Setenv("QEMU_TEST_CACHE", t.TempDir())
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"info empty", nil, 0},
		{"list empty", []string{"list"}, 0},
		{"dir", []string{"dir"}, 0},
		{"clear empty", []string{"clear"}, 0},
		{"help", []string{"-h"}, 0},
		{"unknown verb", []string{"frobnicate"}, 2},
		{"bad flag", []string{"list", "-nope"}, 2},
		{"clear missing target", []string{"clear", "https://example.com/nope"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := runCache(t, tc.args...); got != tc.want {
				t.Errorf("cacheCommand(%v) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

func TestCacheDirPrintsPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QEMU_TEST_CACHE", dir)
	code, out := runCache(t, "dir")
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	if strings.TrimSpace(out) != dir {
		t.Errorf("dir output = %q, want %q", strings.TrimSpace(out), dir)
	}
}

func TestCacheListAndClear(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QEMU_TEST_CACHE", dir)
	key := cache.CacheKey("https://example.com/vmlinuz")
	if err := os.WriteFile(filepath.Join(dir, key), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, key+".json"), []byte(`{"url":"https://example.com/vmlinuz","fetchedAt":"2026-06-24T10:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out := runCache(t, "list")
	if code != 0 {
		t.Fatalf("list code = %d", code)
	}
	if !strings.Contains(out, "https://example.com/vmlinuz") || !strings.Contains(out, "1 item(s)") {
		t.Errorf("list output missing url/summary:\n%s", out)
	}

	// Clear by URL (the command maps it to the cache key).
	code, out = runCache(t, "clear", "https://example.com/vmlinuz")
	if code != 0 {
		t.Fatalf("clear code = %d out = %s", code, out)
	}
	if !strings.Contains(out, "removed 1 item") {
		t.Errorf("clear output = %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, key)); !os.IsNotExist(err) {
		t.Error("body still present after clear")
	}

	code, out = runCache(t, "list")
	if code != 0 || !strings.Contains(out, "0 item(s)") {
		t.Errorf("post-clear list = %d %q", code, out)
	}
}

func TestCacheClearByKeyPrefix(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QEMU_TEST_CACHE", dir)
	// Built bundles have no source URL, so the key is the only way to name one.
	keyA := "aaaa" + strings.Repeat("0", 60)
	keyB := "aabb" + strings.Repeat("0", 60)
	keyC := "cccc" + strings.Repeat("0", 60)
	for _, k := range []string{keyA, keyB, keyC} {
		if err := os.WriteFile(filepath.Join(dir, k+cache.BuiltBundleExt), make([]byte, 16), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(k string) bool {
		_, err := os.Stat(filepath.Join(dir, k+cache.BuiltBundleExt))
		return err == nil
	}

	// An ambiguous or unknown target removes nothing, even beside a valid one.
	for _, target := range []string{"aa", "ffff"} {
		if code, out := runCache(t, "clear", keyC[:shortKeyLen], target); code != 1 {
			t.Errorf("clear %q code = %d, want 1; out = %s", target, code, out)
		}
		if !exists(keyA) || !exists(keyB) || !exists(keyC) {
			t.Fatalf("clear %q removed an item", target)
		}
	}

	// The short key printed by "list" names its item.
	if code, out := runCache(t, "clear", keyA[:shortKeyLen]); code != 0 || !strings.Contains(out, "removed 1 item") {
		t.Fatalf("clear by short key = %d %q", code, out)
	}
	if exists(keyA) || !exists(keyB) || !exists(keyC) {
		t.Error("clear by short key removed the wrong items")
	}

	// A full key still works.
	if code, out := runCache(t, "clear", keyB); code != 0 {
		t.Fatalf("clear by full key = %d %q", code, out)
	}
	if exists(keyB) || !exists(keyC) {
		t.Error("clear by full key removed the wrong items")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1048576, "1.0 MiB"},
		{1073741824, "1.0 GiB"},
	}
	for _, tc := range cases {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
