package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// writeBody writes a fake cached download (body + .json sidecar) for rawURL with
// the given body size and fetch time, returning its cache key.
func writeBody(t *testing.T, dir, rawURL string, size int, fetchedAt time.Time) string {
	t.Helper()
	key := CacheKey(rawURL)
	if err := os.WriteFile(filepath.Join(dir, key), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := Meta{URL: rawURL, SHA256: "deadbeef", Size: int64(size), FetchedAt: fetchedAt.UTC().Format(time.RFC3339)}
	b, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, key+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return key
}

// writeExtract creates a fake .d extraction dir for key holding one file.
func writeExtract(t *testing.T, dir, key string, fileSize int) {
	t.Helper()
	d := filepath.Join(dir, key+".d")
	if err := os.MkdirAll(filepath.Join(d, "modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, ".extracted"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "modules", "vmlinuz"), make([]byte, fileSize), 0o644); err != nil {
		t.Fatal(err)
	}
}

func findEntry(entries []Entry, key string) (Entry, bool) {
	for _, e := range entries {
		if e.Key == key {
			return e, true
		}
	}
	return Entry{}, false
}

func TestListGroupsAndIgnoresTransient(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)

	// A bare download (kernel/source): body + sidecar, no extraction.
	kernelKey := writeBody(t, dir, "https://example.com/vmlinuz", 100, now.Add(-2*time.Hour))

	// A downloaded bundle: body + sidecar + extraction dir.
	bundleKey := writeBody(t, dir, "https://example.com/boot.tgz", 200, now.Add(-1*time.Hour))
	writeExtract(t, dir, bundleKey, 500)

	// A locally built bundle: <key>.tgz, no sidecar.
	builtKey := CacheKey(`{"sources":[]}`)
	if err := os.WriteFile(filepath.Join(dir, builtKey+BuiltBundleExt), make([]byte, 300), 0o644); err != nil {
		t.Fatal(err)
	}

	// An orphan sidecar (its body was removed).
	orphanKey := CacheKey("https://example.com/orphan")
	if err := os.WriteFile(filepath.Join(dir, orphanKey+".json"), []byte(`{"url":"https://example.com/orphan","fetchedAt":""}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Transient leftovers that must be ignored by List.
	for _, name := range []string{"dl-12345", "extract-67890", kernelKey + ".json.tmp-99"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("junk"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("got %d entries, want 4: %+v", len(entries), entries)
	}

	// Sorted newest-fetched first: bundle (-1h) before kernel (-2h); the built
	// bundle and orphan have no fetch time and sort last.
	if entries[0].Key != bundleKey || entries[1].Key != kernelKey {
		t.Errorf("unexpected sort order: %s, %s", entries[0].Kind, entries[1].Kind)
	}

	kernel, ok := findEntry(entries, kernelKey)
	if !ok || kernel.Kind != KindDownload {
		t.Errorf("kernel entry kind = %q, want %q", kernel.Kind, KindDownload)
	}
	if kernel.URL != "https://example.com/vmlinuz" {
		t.Errorf("kernel URL = %q", kernel.URL)
	}
	if kernel.Size <= 100 { // 100 body + sidecar JSON, so strictly greater than the body
		t.Errorf("kernel size = %d, want > 100 (body + sidecar)", kernel.Size)
	}

	bundle, _ := findEntry(entries, bundleKey)
	if bundle.Kind != KindBundle {
		t.Errorf("bundle kind = %q, want %q", bundle.Kind, KindBundle)
	}
	if bundle.Size < 700 { // 200 body + 500 extracted file + sidecar JSON
		t.Errorf("bundle size = %d, want >= 700", bundle.Size)
	}

	built, _ := findEntry(entries, builtKey)
	if built.Kind != KindBuilt {
		t.Errorf("built kind = %q, want %q", built.Kind, KindBuilt)
	}
	if built.URL != "" {
		t.Errorf("built URL = %q, want empty", built.URL)
	}

	orphan, _ := findEntry(entries, orphanKey)
	if orphan.Kind != KindOrphan {
		t.Errorf("orphan kind = %q, want %q", orphan.Kind, KindOrphan)
	}
}

func TestListMissingDir(t *testing.T) {
	entries, err := List(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("List of missing dir errored: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("got %d entries, want 0", len(entries))
	}
}

func TestClearTargeted(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	k1 := writeBody(t, dir, "https://example.com/a", 100, now)
	k2 := writeBody(t, dir, "https://example.com/b", 50, now)

	// Clear only k1, addressed by its URL (the command maps URL -> key).
	removed, freed, err := Clear(dir, CacheKey("https://example.com/a"))
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0].Key != k1 {
		t.Fatalf("removed = %+v, want just k1", removed)
	}
	if freed != removed[0].Size || freed <= 100 {
		t.Errorf("freed = %d, want removed[0].Size (%d), > 100 (body + sidecar)", freed, removed[0].Size)
	}
	if _, err := os.Stat(filepath.Join(dir, k1)); !os.IsNotExist(err) {
		t.Error("k1 body still present after targeted clear")
	}
	if _, err := os.Stat(filepath.Join(dir, k2)); err != nil {
		t.Error("k2 body wrongly removed by targeted clear")
	}
}

func TestClearAllSweepsTransient(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	writeBody(t, dir, "https://example.com/a", 100, now)
	bundleKey := writeBody(t, dir, "https://example.com/boot", 200, now)
	writeExtract(t, dir, bundleKey, 500)
	for _, name := range []string{"dl-1", "extract-2", "x.tmp-3"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("junk"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	removed, freed, err := Clear(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed %d entries, want 2", len(removed))
	}
	if freed < 800 {
		t.Errorf("freed = %d, want >= 800", freed)
	}

	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		names := make([]string, len(left))
		for i, de := range left {
			names[i] = de.Name()
		}
		slices.Sort(names)
		t.Errorf("cache dir not empty after full clear: %v", names)
	}
}
