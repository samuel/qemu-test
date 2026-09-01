package boot

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
	"sync/atomic"
	"testing"

	"github.com/samuel/qemu-test/internal/arch"
	"github.com/samuel/qemu-test/internal/bundler"
	"github.com/samuel/qemu-test/internal/cache"
)

// fakeKernelServer serves a kernel body with an optional ETag, honors
// If-None-Match, and counts handled requests so tests can assert no network use.
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

	path, err := cache.FetchKernel(context.Background(), srv.Client(), srv.URL, cacheDir, false)
	if err != nil {
		t.Fatalf("fetchKernel: %v", err)
	}
	if got := readFile(t, path); !bytes.Equal(got, body) {
		t.Fatalf("cached body = %q, want %q", got, body)
	}

	// Meta sidecar records the validator and a correct content hash.
	meta, err := cache.LoadMeta(path + ".json")
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

	if _, err := cache.FetchKernel(context.Background(), srv.Client(), srv.URL, cacheDir, false); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if fake.hitCount() != 1 {
		t.Fatalf("first fetch hits = %d, want 1", fake.hitCount())
	}
	// A cached kernel without refresh must not contact the network.
	if _, err := cache.FetchKernel(context.Background(), srv.Client(), srv.URL, cacheDir, false); err != nil {
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

	path, err := cache.FetchKernel(context.Background(), srv.Client(), srv.URL, cacheDir, false)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	// Refresh with an unchanged remote: server answers 304 and the cache is kept.
	path2, err := cache.FetchKernel(context.Background(), srv.Client(), srv.URL, cacheDir, true)
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

	path, err := cache.FetchKernel(context.Background(), srv.Client(), srv.URL, cacheDir, false)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	// Remote changes: new body + new ETag. Refresh must re-download.
	fake.set([]byte("kernel-v2-bigger"), `"v2"`)
	if _, err := cache.FetchKernel(context.Background(), srv.Client(), srv.URL, cacheDir, true); err != nil {
		t.Fatalf("refresh fetch: %v", err)
	}
	if got := readFile(t, path); !bytes.Equal(got, []byte("kernel-v2-bigger")) {
		t.Fatalf("body after refresh = %q, want updated content", got)
	}
	meta, err := cache.LoadMeta(path + ".json")
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

	path, err := cache.FetchKernel(context.Background(), srv.Client(), srv.URL, cacheDir, false)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	url := srv.URL
	srv.Close() // network now unreachable

	// Refresh fails to reach the server, but a cached copy exists: fall back to it.
	path2, err := cache.FetchKernel(context.Background(), http.DefaultClient, url, cacheDir, true)
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

	if _, err := cache.FetchKernel(context.Background(), http.DefaultClient, url, t.TempDir(), false); err == nil {
		t.Fatal("expected error when no cache exists and the server is unreachable")
	}
}

func TestResolveBootEnvLocalKernel(t *testing.T) {
	a, err := arch.Lookup("amd64")
	if err != nil {
		t.Fatal(err)
	}
	kf := filepath.Join(t.TempDir(), "vmlinuz")
	if err := os.WriteFile(kf, []byte("kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	env, err := Resolve(context.Background(), Options{Arch: a, KernelOverride: kf, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if env.KernelPath != kf {
		t.Fatalf("KernelPath = %q, want %q", env.KernelPath, kf)
	}
	if env.ModulesPath != "" || env.DTBPath != "" {
		t.Fatalf("bare -kernel should carry no modules/dtb, got %+v", env)
	}

	if _, err := Resolve(context.Background(), Options{Arch: a, KernelOverride: filepath.Join(t.TempDir(), "missing"), WorkDir: t.TempDir()}); err == nil {
		t.Fatal("expected error for a missing -kernel path")
	}
}

func TestResolveBootEnvArchKernelURL(t *testing.T) {
	body := []byte("\x7fELF-arch-default-kernel")
	srv := httptest.NewServer(&fakeKernelServer{body: body, etag: `"v1"`})
	defer srv.Close()
	t.Setenv("QEMU_TEST_CACHE", t.TempDir())

	// An arch with no bundle falls back to its configured KernelURL.
	a := arch.Arch{Name: "testarch", KernelURL: srv.URL}
	env, err := Resolve(context.Background(), Options{Arch: a, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if data := readFile(t, env.KernelPath); !bytes.Equal(data, body) {
		t.Fatalf("fetched kernel = %q, want %q", data, body)
	}
}

func TestResolveBootEnvNoKernel(t *testing.T) {
	a := arch.Arch{Name: "bare"} // no kernel, no bundle, no URL
	if _, err := Resolve(context.Background(), Options{Arch: a, WorkDir: t.TempDir()}); err == nil {
		t.Fatal("expected error when an arch has neither a kernel/bundle nor a URL")
	}
}

func TestResolveBootEnvBuiltBundle(t *testing.T) {
	// A BundleConfig is built from upstream sources and then used like a
	// downloaded bundle.
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/vmlinuz", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("KERNEL-IMAGE"))
	})
	mux.HandleFunc("/modules.dep", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("kernel/foo.ko:\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("QEMU_TEST_CACHE", t.TempDir())

	a := arch.Arch{Name: "built", BundleConfig: &bundler.Config{Sources: []bundler.Source{
		{URL: srv.URL + "/vmlinuz", Dst: "vmlinuz"},
		{URL: srv.URL + "/modules.dep", Dst: "modules/modules.dep"},
	}}}

	env, err := Resolve(context.Background(), Options{Arch: a, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if data := readFile(t, env.KernelPath); !bytes.Equal(data, []byte("KERNEL-IMAGE")) {
		t.Fatalf("built bundle kernel = %q, want %q", data, "KERNEL-IMAGE")
	}
	if filepath.Base(env.ModulesPath) != "modules" {
		t.Fatalf("built bundle ModulesDir = %q, want a 'modules' tree", env.ModulesPath)
	}

	// A second run reuses the cached build and sources, hitting no network.
	srcHits := hits.Load()
	if _, err := Resolve(context.Background(), Options{Arch: a, WorkDir: t.TempDir()}); err != nil {
		t.Fatalf("Resolve (reuse): %v", err)
	}
	if got := hits.Load(); got != srcHits {
		t.Fatalf("offline-first: hits = %d after reuse, want %d", got, srcHits)
	}
}
