package main

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

// isKernelURL reports whether s is an http(s) URL rather than a local filesystem
// path, so -kernel and arch.kernelURL can accept either form.
func isKernelURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// kernelCacheDir returns the persistent on-disk directory where downloaded kernels
// are cached, creating it if needed. It honors QEMU_TEST_KERNEL_CACHE for an
// explicit override (also the seam used by tests); otherwise it is
// os.UserCacheDir()/qemu-test/kernels. It is only consulted when a kernel actually
// needs fetching, so embedded/local-path runs never depend on a usable cache dir.
func kernelCacheDir() (string, error) {
	dir := os.Getenv("QEMU_TEST_KERNEL_CACHE")
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("locate user cache dir: %w", err)
		}
		dir = filepath.Join(base, "qemu-test", "kernels")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create kernel cache dir %q: %w", dir, err)
	}
	return dir, nil
}

// cacheKey derives a stable, filesystem-safe cache filename from a URL.
func cacheKey(rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	return hex.EncodeToString(sum[:])
}

// kernelMeta is the JSON sidecar stored next to a cached kernel. It records the
// HTTP validators used for a conditional refresh (-kernel.refresh) plus integrity
// and provenance info.
type kernelMeta struct {
	URL          string `json:"url"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
	FetchedAt    string `json:"fetchedAt"`
}

// loadKernelMeta reads the JSON sidecar at path.
func loadKernelMeta(path string) (kernelMeta, error) {
	var m kernelMeta
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

// fetchKernel returns the on-disk path to the kernel at rawURL, downloading and
// caching it under cacheDir on first use. A cached kernel is reused without any
// network access unless refresh is set, in which case a conditional GET (using the
// stored ETag/Last-Modified) re-downloads only when the remote has actually
// changed. If the network is unreachable but a cached copy exists, that copy is
// used. A nil client uses http.DefaultClient; ctx governs cancellation/timeout.
func fetchKernel(ctx context.Context, client *http.Client, rawURL, cacheDir string, refresh bool) (string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	bodyPath := filepath.Join(cacheDir, cacheKey(rawURL))
	metaPath := bodyPath + ".json"

	_, statErr := os.Stat(bodyPath)
	cached := statErr == nil

	// Offline-first: a cached kernel is used as-is unless an explicit refresh was
	// requested.
	if cached && !refresh {
		slog.Debug("using cached kernel", "url", rawURL, "path", bodyPath)
		return bodyPath, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("kernel request %q: %w", rawURL, err)
	}
	// On refresh, send the stored validators so an unchanged remote answers 304 and
	// we skip the re-download.
	if cached {
		if meta, err := loadKernelMeta(metaPath); err == nil {
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
			slog.Warn("kernel refresh failed; using cached copy", "url", rawURL, "err", err)
			return bodyPath, nil
		}
		return "", fmt.Errorf("download kernel %q: %w", rawURL, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-path response body; close error is non-actionable

	switch resp.StatusCode {
	case http.StatusNotModified:
		slog.Debug("cached kernel still current", "url", rawURL)
		return bodyPath, nil
	case http.StatusOK:
		// fall through to download
	default:
		return "", fmt.Errorf("download kernel %q: unexpected status %s", rawURL, resp.Status)
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
		return "", fmt.Errorf("download kernel %q: %w", rawURL, err)
	}
	if err := os.Rename(tmpName, bodyPath); err != nil {
		_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
		return "", fmt.Errorf("cache kernel %q: %w", rawURL, err)
	}

	meta := kernelMeta{
		URL:          rawURL,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		SHA256:       hex.EncodeToString(h.Sum(nil)),
		Size:         n,
		FetchedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return "", fmt.Errorf("encode kernel meta: %w", err)
	}
	if err := writeFileAtomic(metaPath, b); err != nil {
		return "", fmt.Errorf("write kernel meta: %w", err)
	}
	slog.Info("downloaded kernel", "url", rawURL, "path", bodyPath, "size", n)
	return bodyPath, nil
}
