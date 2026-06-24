package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"maps"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/samuel/qemu-test/internal/cache"
)

// makeTarGz builds a gzip-compressed tar from name->content, in sorted name order.
func makeTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		content := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestFetchBundleDownloadsExtractsAndReuses(t *testing.T) {
	tgz := makeTarGz(t, map[string]string{
		"vmlinuz":               "kernelbytes",
		"modules/modules.dep":   "kernel/foo.ko:\n",
		"modules/kernel/foo.ko": "ko",
	})
	fake := &fakeKernelServer{body: tgz, etag: `"v1"`}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	cacheDir := t.TempDir()

	dir, err := cache.FetchBundle(context.Background(), srv.Client(), srv.URL, cacheDir, false)
	if err != nil {
		t.Fatalf("fetchBundle: %v", err)
	}
	if got := readFile(t, filepath.Join(dir, "vmlinuz")); string(got) != "kernelbytes" {
		t.Fatalf("extracted vmlinuz = %q, want %q", got, "kernelbytes")
	}

	// Second call: cached + already extracted, so no extra network request.
	dir2, err := cache.FetchBundle(context.Background(), srv.Client(), srv.URL, cacheDir, false)
	if err != nil {
		t.Fatalf("fetchBundle (reuse): %v", err)
	}
	if dir2 != dir {
		t.Fatalf("reuse dir = %q, want %q", dir2, dir)
	}
	if fake.hitCount() != 1 {
		t.Fatalf("offline-first: hits = %d after reuse, want 1", fake.hitCount())
	}
}

func TestExtractArchiveRejectsTraversal(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o644, Size: 3, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("bad")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	arc := filepath.Join(t.TempDir(), "evil.tar")
	if err := os.WriteFile(arc, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cache.ExtractArchive(arc, t.TempDir()); err == nil {
		t.Fatal("expected extractArchive to reject a path-traversal entry")
	}
}

func TestResolveBundleLocalDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vmlinuz"), []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}
	mod := filepath.Join(dir, "modules")
	if err := os.MkdirAll(mod, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mod, "modules.dep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	env, err := resolveBundle(context.Background(), arch{name: "x"}, dir, false, t.TempDir())
	if err != nil {
		t.Fatalf("resolveBundle: %v", err)
	}
	if env.kernelPath != filepath.Join(dir, "vmlinuz") {
		t.Fatalf("kernelPath = %q", env.kernelPath)
	}
	if env.modulesDir != mod {
		t.Fatalf("modulesDir = %q, want %q", env.modulesDir, mod)
	}
}

func TestBundleEnvModulesWithoutModulesDep(t *testing.T) {
	// A bundle whose lib/modules/<release> tree ships only .ko files (no
	// modules.dep, like an extracted netboot initrd) is still recognized.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vmlinuz"), []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(dir, "lib", "modules", "6.12.0", "kernel", "drivers", "usb")
	if err := os.MkdirAll(rel, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rel, "usbcore.ko"), []byte("ko"), 0o644); err != nil {
		t.Fatal(err)
	}
	env, err := bundleEnv(arch{name: "x"}, dir)
	if err != nil {
		t.Fatalf("bundleEnv: %v", err)
	}
	want := filepath.Join(dir, "lib", "modules", "6.12.0")
	if env.modulesDir != want {
		t.Fatalf("modulesDir = %q, want %q (a .ko-only tree should be recognized)", env.modulesDir, want)
	}
}

func TestBundleEnvLibModulesAndDTBOverrides(t *testing.T) {
	dir := t.TempDir()
	// Non-default kernel name + a lib/modules/<release> tree + a dtb, all selected
	// via arch overrides.
	if err := os.WriteFile(filepath.Join(dir, "Image"), []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(dir, "lib", "modules", "6.12.0")
	if err := os.MkdirAll(rel, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rel, "modules.dep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "dtb"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dtb", "board.dtb"), []byte("dtb"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := arch{name: "x", bundleKernelPath: "Image", bundleDTBPath: "dtb/board.dtb"}
	env, err := bundleEnv(a, dir)
	if err != nil {
		t.Fatalf("bundleEnv: %v", err)
	}
	if env.kernelPath != filepath.Join(dir, "Image") {
		t.Fatalf("kernelPath = %q", env.kernelPath)
	}
	if env.modulesDir != rel {
		t.Fatalf("modulesDir = %q, want %q", env.modulesDir, rel)
	}
	if env.dtbPath != filepath.Join(dir, "dtb", "board.dtb") {
		t.Fatalf("dtbPath = %q", env.dtbPath)
	}
}
