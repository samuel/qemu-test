package bundler

import (
	"bytes"
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samuel/qemu-test/internal/cache"
)

// bundleServer serves the named blobs over HTTP for the end-to-end tests.
func bundleServer(t *testing.T, blobs map[string][]byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for name, body := range blobs {
		mux.HandleFunc("/"+name, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(body)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// writeConfig writes cfg to a temp config file and returns its path.
func writeConfig(t *testing.T, cfg string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

// extractBundle extracts the tgz at path and returns its files as relpath->content.
func extractBundle(t *testing.T, path string) map[string]string {
	t.Helper()
	dir := t.TempDir()
	if err := cache.ExtractArchive(path, dir); err != nil {
		t.Fatalf("extract bundle: %v", err)
	}
	got := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		got[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walk bundle: %v", err)
	}
	return got
}

func TestRunEndToEnd(t *testing.T) {
	initrd := cpioBytes(t, true,
		dirRec("usr/lib/modules/6.12-test/kernel"),
		regRec("usr/lib/modules/6.12-test/modules.dep", "deps"),
		regRec("usr/lib/modules/6.12-test/kernel/drivers/net/e1000.ko", "ko-e1000"),
		regRec("usr/lib/modules/6.12-test/kernel/usb/xhci.ko", "ko-xhci"),
		regRec("usr/bin/ignored", "nope"),
	)
	srv := bundleServer(t, map[string][]byte{
		"vmlinuz":   []byte("KERNEL-IMAGE"),
		"initrd.gz": initrd,
	})
	cfg := `{"sources": [
		{"url": "` + srv.URL + `/vmlinuz", "dst": "vmlinuz"},
		{"url": "` + srv.URL + `/initrd.gz", "archive": true,
		 "files": {"usr/lib/modules/**": "lib/modules/"}}
	]}`

	out := filepath.Join(t.TempDir(), "boot.tgz")
	if err := Run(context.Background(), writeConfig(t, cfg), out, t.TempDir(), false); err != nil {
		t.Fatalf("run: %v", err)
	}

	want := map[string]string{
		"vmlinuz":                           "KERNEL-IMAGE",
		"lib/modules/6.12-test/modules.dep": "deps",
		"lib/modules/6.12-test/kernel/drivers/net/e1000.ko": "ko-e1000",
		"lib/modules/6.12-test/kernel/usb/xhci.ko":          "ko-xhci",
	}
	got := extractBundle(t, out)
	if len(got) != len(want) {
		t.Fatalf("bundle has %d files, want %d: %v", len(got), len(want), keys(got))
	}
	for name, body := range want {
		if got[name] != body {
			t.Errorf("bundle[%q] = %q, want %q", name, got[name], body)
		}
	}
	if _, ok := got["usr/bin/ignored"]; ok {
		t.Error("unmatched file usr/bin/ignored leaked into the bundle")
	}
}

func TestRunAnchoredModules(t *testing.T) {
	// A "." keep-anchor drops the variable version dir, producing a clean modules/
	// tree (modules/modules.dep, modules/kernel/...).
	initrd := cpioBytes(t, true,
		regRec("usr/lib/modules/6.12.86+deb13-armmp/modules.dep", "deps"),
		regRec("usr/lib/modules/6.12.86+deb13-armmp/kernel/net/e1000.ko", "ko"),
	)
	srv := bundleServer(t, map[string][]byte{"initrd.gz": initrd})
	cfg := `{"sources": [{"url": "` + srv.URL + `/initrd.gz", "archive": true,
		"files": {"usr/lib/modules/*/./**": "modules/"}}]}`

	out := filepath.Join(t.TempDir(), "boot.tgz")
	if err := Run(context.Background(), writeConfig(t, cfg), out, t.TempDir(), false); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := map[string]string{
		"modules/modules.dep":         "deps",
		"modules/kernel/net/e1000.ko": "ko",
	}
	got := extractBundle(t, out)
	if len(got) != len(want) {
		t.Fatalf("bundle = %v, want %v", keys(got), keys(want))
	}
	for name, body := range want {
		if got[name] != body {
			t.Errorf("bundle[%q] = %q, want %q", name, got[name], body)
		}
	}
}

func TestRunDeterministic(t *testing.T) {
	srv := bundleServer(t, map[string][]byte{
		"vmlinuz": []byte("KERNEL"),
		"initrd":  cpioBytes(t, false, regRec("usr/lib/modules/6.12/x.ko", "ko")),
	})
	cfg := `{"sources": [
		{"url": "` + srv.URL + `/vmlinuz", "dst": "vmlinuz"},
		{"url": "` + srv.URL + `/initrd", "archive": true, "files": {"usr/lib/modules/**": "lib/modules/"}}
	]}`
	cfgPath := writeConfig(t, cfg)
	cacheDir := t.TempDir()

	read := func() []byte {
		out := filepath.Join(t.TempDir(), "boot.tgz")
		if err := Run(context.Background(), cfgPath, out, cacheDir, false); err != nil {
			t.Fatalf("run: %v", err)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("read bundle: %v", err)
		}
		return b
	}
	if a, b := read(), read(); !bytes.Equal(a, b) {
		t.Errorf("bundle is not byte-for-byte reproducible (%d vs %d bytes)", len(a), len(b))
	}
}

func TestRunCollision(t *testing.T) {
	srv := bundleServer(t, map[string][]byte{"a": []byte("1"), "b": []byte("2")})
	cfg := `{"sources": [
		{"url": "` + srv.URL + `/a", "dst": "vmlinuz"},
		{"url": "` + srv.URL + `/b", "dst": "vmlinuz"}
	]}`
	err := Run(context.Background(), writeConfig(t, cfg), filepath.Join(t.TempDir(), "o.tgz"), t.TempDir(), false)
	if err == nil || !strings.Contains(err.Error(), "duplicate output path") {
		t.Fatalf("err = %v, want a duplicate-output-path error", err)
	}
}

func TestAddArchiveMultiMatchNonDirDst(t *testing.T) {
	data := cpioBytes(t, false,
		regRec("mods/a.ko", "a"),
		regRec("mods/b.ko", "b"),
	)
	src := Source{URL: "x", Archive: true, Files: map[string]string{"mods/*.ko": "only.ko"}}
	err := addArchive(src, writeTemp(t, data), map[string]entry{})
	if err == nil || !strings.Contains(err.Error(), "multiple entries") {
		t.Fatalf("err = %v, want a multiple-entries error", err)
	}
}

func TestLoadConfigValidation(t *testing.T) {
	cases := map[string]string{
		"missing url":      `{"sources": [{"dst": "vmlinuz"}]}`,
		"archive no files": `{"sources": [{"url": "u", "archive": true}]}`,
		"plain no dst":     `{"sources": [{"url": "u"}]}`,
		"plain with files": `{"sources": [{"url": "u", "dst": "v", "files": {"a": "b"}}]}`,
		"empty":            `{"sources": []}`,
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadConfig(writeConfig(t, cfg)); err == nil {
				t.Errorf("loadConfig accepted invalid config %q", cfg)
			}
		})
	}
}

func keys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
