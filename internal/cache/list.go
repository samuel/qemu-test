package cache

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Extra Kind values reported by List, beyond KindKernel/KindBundle.
const (
	KindBuilt    = "built"    // a bundle built locally from an arch bundleConfig (<key>.tgz)
	KindDownload = "download" // a downloaded body with no extraction dir (a bare kernel or a bundler source)
	KindOrphan   = "orphan"   // a sidecar or extraction with no body (a reclaimable leftover)
)

// Entry is one logical cached artifact, grouping the files that share a cache
// key: a downloaded body with its .json sidecar and optional .d extraction dir,
// or a locally built bundle (.tgz). Paths holds every file the entry owns so it
// can be removed wholesale.
type Entry struct {
	Key       string    // 64-hex sha256 of the source URL, or of a built bundle's config
	URL       string    // source URL from the sidecar; "" for built bundles and orphans
	Kind      string    // KindBundle | KindBuilt | KindDownload | KindOrphan
	FetchedAt time.Time // from the sidecar; zero when unknown
	Size      int64     // total on-disk bytes (body + sidecar + extraction dir or tgz)
	Paths     []string  // absolute paths owned by this entry, for removal
}

// acc accumulates the files seen for one cache key before they are reduced to an
// Entry.
type acc struct {
	hasBody    bool
	hasExtract bool
	hasTGZ     bool
	size       int64
	paths      []string
	metaPath   string
}

// List enumerates the cached artifacts under cacheDir, grouping the flat layout's
// files (bodies, .json sidecars, .d extraction dirs, .tgz built bundles) by their
// shared cache key. Transient temp files left by an interrupted download, write,
// or extraction are ignored. A missing cacheDir yields no entries. The result is
// sorted newest-fetched first.
func List(cacheDir string) ([]Entry, error) {
	des, err := os.ReadDir(cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	byKey := make(map[string]*acc)
	for _, de := range des {
		name := de.Name()
		if isTransient(name) {
			continue
		}
		key, role := classify(name, de.IsDir())
		if role == "" || !isHexKey(key) {
			continue // unrecognized entry: leave it untouched
		}
		full := filepath.Join(cacheDir, name)
		a := byKey[key]
		if a == nil {
			a = &acc{}
			byKey[key] = a
		}
		a.paths = append(a.paths, full)
		switch role {
		case "sidecar":
			a.metaPath = full
			a.size += fileSize(full)
		case "extract":
			a.hasExtract = true
			a.size += dirSize(full)
		case "tgz":
			a.hasTGZ = true
			a.size += fileSize(full)
		case "body":
			a.hasBody = true
			a.size += fileSize(full)
		}
	}

	entries := make([]Entry, 0, len(byKey))
	for key, a := range byKey {
		e := Entry{Key: key, Size: a.size, Paths: a.paths, Kind: kindOf(a)}
		if a.metaPath != "" {
			if m, err := LoadMeta(a.metaPath); err == nil {
				e.URL = m.URL
				if t, err := time.Parse(time.RFC3339, m.FetchedAt); err == nil {
					e.FetchedAt = t
				}
			}
		}
		entries = append(entries, e)
	}
	slices.SortFunc(entries, func(a, b Entry) int {
		if !a.FetchedAt.Equal(b.FetchedAt) {
			return b.FetchedAt.Compare(a.FetchedAt) // newest first
		}
		return strings.Compare(a.Key, b.Key)
	})
	return entries, nil
}

// Clear removes cached artifacts under cacheDir and reports what was freed. With
// no keys it empties the cache (every entry plus any transient temp leftovers),
// keeping the directory itself; CacheDir recreates it lazily. With keys it removes
// only entries whose Key matches one of them. A missing path is not an error (a
// concurrent refresh may have moved on).
func Clear(cacheDir string, keys ...string) (removed []Entry, freed int64, err error) {
	entries, err := List(cacheDir)
	if err != nil {
		return nil, 0, err
	}
	var want map[string]bool
	if len(keys) > 0 {
		want = make(map[string]bool, len(keys))
		for _, k := range keys {
			want[k] = true
		}
	}
	for _, e := range entries {
		if want != nil && !want[e.Key] {
			continue
		}
		for _, p := range e.Paths {
			if err := os.RemoveAll(p); err != nil && !os.IsNotExist(err) {
				return removed, freed, err
			}
		}
		removed = append(removed, e)
		freed += e.Size
	}
	if want == nil { // a full clear also sweeps transient leftovers
		if err := sweepTransient(cacheDir); err != nil {
			return removed, freed, err
		}
	}
	return removed, freed, nil
}

// classify reduces a cache directory entry name to its base key and role, or an
// empty role when the name does not belong to a known artifact.
func classify(name string, isDir bool) (key, role string) {
	switch {
	case strings.HasSuffix(name, ".json"):
		return strings.TrimSuffix(name, ".json"), "sidecar"
	case isDir && strings.HasSuffix(name, ".d"):
		return strings.TrimSuffix(name, ".d"), "extract"
	case strings.HasSuffix(name, BuiltBundleExt):
		return strings.TrimSuffix(name, BuiltBundleExt), "tgz"
	case isHexKey(name):
		return name, "body"
	default:
		return "", ""
	}
}

// kindOf labels an accumulated entry. A .tgz is a locally built bundle; a body
// with an extraction dir is a downloaded bundle; a body alone is a bare download
// (kernel or bundler source); anything without a body is a reclaimable leftover.
func kindOf(a *acc) string {
	switch {
	case a.hasTGZ:
		return KindBuilt
	case a.hasBody && a.hasExtract:
		return KindBundle
	case a.hasBody:
		return KindDownload
	default:
		return KindOrphan
	}
}

// isTransient reports whether name is a temp file/dir left by an in-flight
// download (dl-*), atomic write (*.tmp-*), or extraction (extract-*).
func isTransient(name string) bool {
	return strings.HasPrefix(name, "dl-") ||
		strings.HasPrefix(name, "extract-") ||
		strings.Contains(name, ".tmp-")
}

// sweepTransient removes any transient temp leftovers under cacheDir.
func sweepTransient(cacheDir string) error {
	des, err := os.ReadDir(cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, de := range des {
		if !isTransient(de.Name()) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(cacheDir, de.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// isHexKey reports whether s is a 64-character lowercase hex string, the form
// CacheKey produces.
func isHexKey(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// fileSize returns the size of the regular file at path, or 0 if it cannot be
// stat'd.
func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// dirSize sums the sizes of regular files under root.
func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error { //nolint:errcheck // best-effort sizing: an unreadable entry is skipped
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}
