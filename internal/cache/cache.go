package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

const (
	KindKernel = "kernel"
	KindBundle = "bundle"
)

// IsHTTPURL reports whether s is an http(s) URL rather than a local filesystem
// path, so -kernel and arch.kernelURL can accept either form.
func IsHTTPURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// CacheDir returns the persistent on-disk directory where downloaded kernels
// are cached, creating it if needed. It honors QEMU_TEST_CACHE for an
// explicit override (also the seam used by tests); otherwise it is
// os.UserCacheDir()/qemu-test/kernels. It is only consulted when a kernel actually
// needs fetching, so embedded/local-path runs never depend on a usable cache dir.
func CacheDir() (string, error) {
	dir := os.Getenv("QEMU_TEST_CACHE")
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("locate user cache dir: %w", err)
		}
		dir = filepath.Join(base, "qemu-test")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create kernel cache dir %q: %w", dir, err)
	}
	return dir, nil
}

// CacheKey derives a stable, filesystem-safe cache filename from a URL.
func CacheKey(rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	return hex.EncodeToString(sum[:])
}

// Meta is the JSON sidecar stored next to a cached file. It records the
// HTTP validators used for a conditional refresh plus integrity
// and provenance info.
type Meta struct {
	URL          string `json:"url"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
	FetchedAt    string `json:"fetchedAt"`
}

// LoadMeta reads the JSON sidecar at path.
func LoadMeta(path string) (Meta, error) {
	var m Meta
	b, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("parse kernel meta %q: %w", path, err)
	}
	return m, nil
}

// writeFileAtomic writes b to path via a temp file in the same directory then
// renames it into place, so a crash or a concurrent run never observes a partial
// file. Rename within a directory is atomic on the local filesystems we target.
func writeFileAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	cerr := f.Close()
	if err := firstErr(werr, cerr); err != nil {
		_ = os.Remove(tmp) //nolint:errcheck // best-effort cleanup; the write/close error is what matters
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) //nolint:errcheck // best-effort cleanup
		return err
	}
	return nil
}

// firstErr returns the first non-nil error, used to surface a write error in
// preference to a close error.
func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// FetchKernel returns the on-disk path to the kernel at rawURL, downloading and
// caching it under cacheDir on first use. See FetchToCache for the caching/refresh
// semantics; a nil client uses http.DefaultClient and ctx governs cancellation.
func FetchKernel(ctx context.Context, client *http.Client, rawURL, cacheDir string, refresh bool) (string, error) {
	return FetchToCache(ctx, client, rawURL, cacheDir, refresh, KindKernel)
}

// FetchToCache returns the on-disk path to the artifact at rawURL, downloading and
// caching it under cacheDir on first use. A cached artifact is reused without any
// network access unless refresh is set, in which case a conditional GET (using the
// stored ETag/Last-Modified) re-downloads only when the remote has actually
// changed. If the network is unreachable but a cached copy exists, that copy is
// used. kind labels the artifact in log and error messages ("kernel", "bundle").
func FetchToCache(ctx context.Context, client *http.Client, rawURL, cacheDir string, refresh bool, kind string) (string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	bodyPath := filepath.Join(cacheDir, CacheKey(rawURL))
	metaPath := bodyPath + ".json"

	_, statErr := os.Stat(bodyPath)
	cached := statErr == nil

	// Offline-first: a cached artifact is used as-is unless an explicit refresh was
	// requested.
	if cached && !refresh {
		slog.Debug("using cached "+kind, "url", rawURL, "path", bodyPath)
		return bodyPath, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("%s request %q: %w", kind, rawURL, err)
	}
	// On refresh, send the stored validators so an unchanged remote answers 304 and
	// we skip the re-download.
	if cached {
		if meta, err := LoadMeta(metaPath); err == nil {
			if meta.ETag != "" {
				req.Header.Set("If-None-Match", meta.ETag)
			}
			if meta.LastModified != "" {
				req.Header.Set("If-Modified-Since", meta.LastModified)
			}
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		if cached {
			slog.Warn(kind+" refresh failed; using cached copy", "url", rawURL, "err", err)
			return bodyPath, nil
		}
		return "", fmt.Errorf("download %s %q: %w", kind, rawURL, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-path response body; close error is non-actionable

	switch resp.StatusCode {
	case http.StatusNotModified:
		slog.Debug("cached "+kind+" still current", "url", rawURL)
		return bodyPath, nil
	case http.StatusOK:
		// fall through to download
	default:
		return "", fmt.Errorf("download %s %q: unexpected status %s", kind, rawURL, resp.Status)
	}

	// Stream to a temp file, hashing as we copy, then atomically rename into place.
	tmp, err := os.CreateTemp(cacheDir, "dl-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(tmp, h), resp.Body)
	closeErr := tmp.Close()
	if err := firstErr(copyErr, closeErr); err != nil {
		_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
		return "", fmt.Errorf("download %s %q: %w", kind, rawURL, err)
	}
	if err := os.Rename(tmpName, bodyPath); err != nil {
		_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
		return "", fmt.Errorf("cache %s %q: %w", kind, rawURL, err)
	}

	meta := Meta{
		URL:          rawURL,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		SHA256:       hex.EncodeToString(h.Sum(nil)),
		Size:         n,
		FetchedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return "", fmt.Errorf("encode %s meta: %w", kind, err)
	}
	if err := writeFileAtomic(metaPath, b); err != nil {
		return "", fmt.Errorf("write %s meta: %w", kind, err)
	}
	slog.Info("downloaded "+kind, "url", rawURL, "path", bodyPath, "size", n)
	return bodyPath, nil
}

// FetchBundle returns the path to the extracted boot-bundle directory for the
// archive at rawURL. The archive is downloaded and cached like a kernel (see
// FetchToCache), then extracted once into a sibling "<cache>.d" directory; later
// runs reuse the extracted tree without re-extracting unless refresh forces a
// re-download (and thus a re-extraction). Extraction is atomic: a fresh tree is
// built in a temp dir and renamed into place, so a crash never leaves a partial
// directory that looks complete.
func FetchBundle(ctx context.Context, client *http.Client, rawURL, cacheDir string, refresh bool) (string, error) {
	archivePath, err := FetchToCache(ctx, client, rawURL, cacheDir, refresh, KindBundle)
	if err != nil {
		return "", err
	}
	extractDir := archivePath + ".d"
	marker := filepath.Join(extractDir, ".extracted")
	if !refresh {
		if _, err := os.Stat(marker); err == nil {
			slog.Debug("using extracted bundle", "url", rawURL, "path", extractDir)
			return extractDir, nil
		}
	}
	if err := ExtractArchiveAtomic(archivePath, extractDir, cacheDir); err != nil {
		return "", fmt.Errorf("extract bundle %q: %w", rawURL, err)
	}
	slog.Info("extracted bundle", "url", rawURL, "path", extractDir)
	return extractDir, nil
}
