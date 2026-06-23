package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestIsKernelURL(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"http://example.com/vmlinuz", true},
		{"https://example.com/vmlinuz", true},
		{"HTTP://example.com/vmlinuz", true}, // scheme is lower-cased by url.Parse
		{"/abs/path/vmlinuz", false},
		{"./rel/vmlinuz", false},
		{"vmlinuz", false},
		{"", false},
		{"ftp://example.com/vmlinuz", false},
		{"file:///vmlinuz", false},
	}
	for _, tc := range cases {
		if got := isKernelURL(tc.in); got != tc.want {
			t.Errorf("isKernelURL(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestCacheKey(t *testing.T) {
	a := cacheKey("http://example.com/vmlinuz")
	if a != cacheKey("http://example.com/vmlinuz") {
		t.Fatal("cacheKey not stable for the same URL")
	}
	if a == cacheKey("http://example.com/other") {
		t.Fatal("cacheKey collided for different URLs")
	}
	if len(a) != 64 { // sha256 hex
		t.Fatalf("cacheKey length = %d, want 64", len(a))
	}
}

// fakeKernelServer serves a kernel body with an optional ETag, honors
// If-None-Match for conditional refresh, and counts the requests it actually
// handles so tests can assert the offline-first path skips the network.
type fakeKernelServer struct {
	mu   sync.Mutex
	body []byte
	etag string
	hits int
}

func (s *fakeKernelServer) set(body []byte, etag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.body, s.etag = body, etag
}

func (s *fakeKernelServer) hitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

func (s *fakeKernelServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hits++
	if s.etag != "" {
		w.Header().Set("ETag", s.etag)
		if r.Header.Get("If-None-Match") == s.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(s.body)
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return b
}

func TestFetchKernelDownloadsAndCaches(t *testing.T) {
	body := []byte("\x7fELF-fake-kernel-bytes")
	srv := httptest.NewServer(&fakeKernelServer{body: body, etag: `"v1"`})
	defer srv.Close()
	cacheDir := t.TempDir()

	path, err := fetchKernel(context.Background(), srv.Client(), srv.URL, cacheDir, false)
	if err != nil {
		t.Fatalf("fetchKernel: %v", err)
	}
	if got := readFile(t, path); !bytes.Equal(got, body) {
		t.Fatalf("cached body = %q, want %q", got, body)
	}

	// Meta sidecar records the validator and a correct content hash.
	meta, err := loadKernelMeta(path + ".json")
	if err != nil {
		t.Fatalf("loadKernelMeta: %v", err)
	}
	if meta.URL != srv.URL {
		t.Errorf("meta.URL = %q, want %q", meta.URL, srv.URL)
	}
	if meta.ETag != `"v1"` {
		t.Errorf("meta.ETag = %q, want %q", meta.ETag, `"v1"`)
	}
	sum := sha256.Sum256(body)
	if meta.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("meta.SHA256 = %q, want hash of body", meta.SHA256)
	}
	if meta.Size != int64(len(body)) {
		t.Errorf("meta.Size = %d, want %d", meta.Size, len(body))
	}
}

func TestFetchKernelOfflineFirst(t *testing.T) {
	fake := &fakeKernelServer{body: []byte("kernel"), etag: `"v1"`}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	cacheDir := t.TempDir()

	if _, err := fetchKernel(context.Background(), srv.Client(), srv.URL, cacheDir, false); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if fake.hitCount() != 1 {
		t.Fatalf("first fetch hits = %d, want 1", fake.hitCount())
	}
	// A cached kernel without refresh must not contact the network.
	if _, err := fetchKernel(context.Background(), srv.Client(), srv.URL, cacheDir, false); err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if fake.hitCount() != 1 {
		t.Fatalf("offline-first: hits = %d after cached fetch, want 1", fake.hitCount())
	}
}

func TestFetchKernelRefreshNotModified(t *testing.T) {
	fake := &fakeKernelServer{body: []byte("kernel-v1"), etag: `"v1"`}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	cacheDir := t.TempDir()

	path, err := fetchKernel(context.Background(), srv.Client(), srv.URL, cacheDir, false)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	// Refresh with an unchanged remote: server answers 304 and the cache is kept.
	path2, err := fetchKernel(context.Background(), srv.Client(), srv.URL, cacheDir, true)
	if err != nil {
		t.Fatalf("refresh fetch: %v", err)
	}
	if path2 != path {
		t.Fatalf("refresh path = %q, want %q", path2, path)
	}
	if fake.hitCount() != 2 {
		t.Fatalf("hits = %d, want 2 (initial + conditional refresh)", fake.hitCount())
	}
	if got := readFile(t, path); !bytes.Equal(got, []byte("kernel-v1")) {
		t.Fatalf("body changed after 304: %q", got)
	}
}

func TestFetchKernelRefreshChanged(t *testing.T) {
	fake := &fakeKernelServer{body: []byte("kernel-v1"), etag: `"v1"`}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	cacheDir := t.TempDir()

	path, err := fetchKernel(context.Background(), srv.Client(), srv.URL, cacheDir, false)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	// Remote changes: new body + new ETag. Refresh must re-download.
	fake.set([]byte("kernel-v2-bigger"), `"v2"`)
	if _, err := fetchKernel(context.Background(), srv.Client(), srv.URL, cacheDir, true); err != nil {
		t.Fatalf("refresh fetch: %v", err)
	}
	if got := readFile(t, path); !bytes.Equal(got, []byte("kernel-v2-bigger")) {
		t.Fatalf("body after refresh = %q, want updated content", got)
	}
	meta, err := loadKernelMeta(path + ".json")
	if err != nil {
		t.Fatalf("loadKernelMeta: %v", err)
	}
	if meta.ETag != `"v2"` {
		t.Errorf("meta.ETag = %q, want \"v2\"", meta.ETag)
	}
}

func TestFetchKernelOfflineFallback(t *testing.T) {
	fake := &fakeKernelServer{body: []byte("kernel"), etag: `"v1"`}
	srv := httptest.NewServer(fake)
	cacheDir := t.TempDir()

	path, err := fetchKernel(context.Background(), srv.Client(), srv.URL, cacheDir, false)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	url := srv.URL
	srv.Close() // network now unreachable

	// Refresh fails to reach the server, but a cached copy exists: fall back to it.
	path2, err := fetchKernel(context.Background(), http.DefaultClient, url, cacheDir, true)
	if err != nil {
		t.Fatalf("offline refresh should fall back to cache, got error: %v", err)
	}
	if path2 != path {
		t.Fatalf("fallback path = %q, want %q", path2, path)
	}
}

func TestFetchKernelNoCacheServerDown(t *testing.T) {
	srv := httptest.NewServer(&fakeKernelServer{body: []byte("kernel")})
	url := srv.URL
	srv.Close() // closed before any successful fetch; cache is empty

	if _, err := fetchKernel(context.Background(), http.DefaultClient, url, t.TempDir(), false); err == nil {
		t.Fatal("expected error when no cache exists and the server is unreachable")
	}
}

func TestResolveKernelLocalOverride(t *testing.T) {
	a, err := lookupArch("amd64")
	if err != nil {
		t.Fatal(err)
	}
	kf := filepath.Join(t.TempDir(), "vmlinuz")
	if err := os.WriteFile(kf, []byte("kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := resolveKernel(context.Background(), a, kf, false, t.TempDir())
	if err != nil {
		t.Fatalf("resolveKernel: %v", err)
	}
	if got != kf {
		t.Fatalf("resolveKernel = %q, want %q", got, kf)
	}

	if _, err := resolveKernel(context.Background(), a, filepath.Join(t.TempDir(), "missing"), false, t.TempDir()); err == nil {
		t.Fatal("expected error for a missing -kernel path")
	}
}

func TestResolveKernelArchURL(t *testing.T) {
	body := []byte("\x7fELF-arch-default-kernel")
	srv := httptest.NewServer(&fakeKernelServer{body: body, etag: `"v1"`})
	defer srv.Close()
	t.Setenv("QEMU_TEST_KERNEL_CACHE", t.TempDir())

	// An arch with no embedded kernel falls back to its configured kernelURL.
	a := arch{name: "testarch", kernelURL: srv.URL}
	got, err := resolveKernel(context.Background(), a, "", false, t.TempDir())
	if err != nil {
		t.Fatalf("resolveKernel: %v", err)
	}
	if data := readFile(t, got); !bytes.Equal(data, body) {
		t.Fatalf("fetched kernel = %q, want %q", data, body)
	}
}

func TestResolveKernelNoKernel(t *testing.T) {
	a := arch{name: "bare"} // no embedded kernel, no URL
	if _, err := resolveKernel(context.Background(), a, "", false, t.TempDir()); err == nil {
		t.Fatal("expected error when an arch has neither an embedded kernel nor a kernelURL")
	}
}
