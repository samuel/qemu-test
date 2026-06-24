package bundler

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/samuel/qemu-test/internal/cache"
)

// entry is a resolved output file held in memory until the Bundle is written.
type entry struct {
	mode    uint32
	content []byte
}

// Run loads the config, fetches every source, resolves the output files, and
// writes the bundle to outPath.
func Run(ctx context.Context, configPath, outPath, cacheDir string, refresh bool) error {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	return Bundle(ctx, cfg, outPath, cacheDir, refresh)
}

func Bundle(ctx context.Context, cfg *Config, outPath, cacheDir string, refresh bool) error {
	if cfg == nil {
		return errors.New("bundler: no config")
	}
	// A cache dir is only needed when something is fetched over the network; an
	// all-local config never touches it.
	needCache := slices.ContainsFunc(cfg.Sources, func(s Source) bool { return cache.IsHTTPURL(s.URL) })
	switch {
	case cacheDir != "":
		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			return fmt.Errorf("create cache dir: %w", err)
		}
	case needCache:
		var err error
		if cacheDir, err = cache.CacheDir(); err != nil {
			return err
		}
	}

	client := http.DefaultClient
	out := make(map[string]entry)
	for _, src := range cfg.Sources {
		// A Debian source has no fixed URL: resolve it against the repository index
		// first, then treat it like any other downloaded source.
		if src.Debian != nil {
			url, err := resolveDebianURL(ctx, client, src.Debian, cacheDir, refresh)
			if err != nil {
				return fmt.Errorf("%s: %w", src.label(), err)
			}
			src.URL = url
		}
		local, err := fetchSource(ctx, client, src.URL, cacheDir, refresh)
		if err != nil {
			return err
		}
		if src.Archive {
			err = addArchive(src, local, out)
		} else {
			err = addFile(src, local, out)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", src.label(), err)
		}
	}
	if len(out) == 0 {
		return errors.New("no files selected for the bundle")
	}
	return writeBundle(outPath, out)
}

// fetchSource returns a local path for rawURL: an http(s) URL is downloaded and
// cached (offline-first; refresh forces a conditional GET), while any other
// value is treated as a local path used in place.
func fetchSource(ctx context.Context, client *http.Client, rawURL, cacheDir string, refresh bool) (string, error) {
	if !cache.IsHTTPURL(rawURL) {
		if _, err := os.Stat(rawURL); err != nil {
			return "", fmt.Errorf("local source: %w", err)
		}
		return rawURL, nil
	}
	return cache.FetchToCache(ctx, client, rawURL, cacheDir, refresh, "source")
}

// addFile adds a plain downloaded file to out. A Dst ending in "/" is a
// directory, so the file keeps its Source basename under it.
func addFile(src Source, local string, out map[string]entry) error {
	content, err := os.ReadFile(local)
	if err != nil {
		return err
	}
	dst := src.Dst
	if strings.HasSuffix(dst, "/") {
		dst += urlBase(src.URL)
	}
	return addEntry(out, dst, entry{mode: 0o644, content: content})
}

// addArchive selects files from the archive at local per src.Files and adds them
// to out. Globs are tried in sorted order so "first match wins" is
// deterministic; a file matching more than one glob is claimed by the first.
func addArchive(src Source, local string, out map[string]entry) error {
	type rule struct {
		pp  parsedPattern
		dst string
	}
	rules := make([]rule, 0, len(src.Files))
	for g, d := range src.Files {
		pp, err := parsePattern(normPattern(g))
		if err != nil {
			return err
		}
		rules = append(rules, rule{pp: pp, dst: d})
	}
	slices.SortFunc(rules, func(a, b rule) int { return strings.Compare(a.pp.glob, b.pp.glob) })

	// Counter is keyed by rule index: two patterns can normalize to the same glob.
	matches := make([]int, len(rules))
	err := readArchive(local, func(e archiveEntry) error {
		for i := range rules {
			r := rules[i]
			ok, err := matchGlob(r.pp.glob, e.name)
			if err != nil {
				return fmt.Errorf("bad glob %q: %w", r.pp.glob, err)
			}
			if !ok {
				continue
			}
			// A non-directory dst names a single file, so a second match is a
			// configuration error rather than a silent overwrite.
			if !strings.HasSuffix(r.dst, "/") && matches[i] > 0 {
				return fmt.Errorf("glob %q maps to file %q but matches multiple entries (e.g. %q)", r.pp.glob, r.dst, e.name)
			}
			if err := addEntry(out, mapDest(r.pp, r.dst, e.name), entry{mode: e.mode, content: e.content}); err != nil {
				return err
			}
			matches[i]++
			break
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := range rules {
		if matches[i] == 0 {
			slog.Warn("glob matched no files in archive", "archive", src.URL, "glob", rules[i].pp.glob)
		}
	}
	return nil
}

// addEntry records e under a normalized output path, rejecting a path that
// escapes the Bundle root or collides with one already added.
func addEntry(out map[string]entry, name string, e entry) error {
	name = strings.TrimLeft(path.Clean("/"+name), "/")
	if name == "" || name == "." {
		return errors.New("empty destination path")
	}
	if _, ok := out[name]; ok {
		return fmt.Errorf("duplicate output path %q", name)
	}
	out[name] = e
	return nil
}

// writeBundle writes out as a deterministic gzip-compressed tar: regular-file
// records only (extraction recreates parent dirs), sorted by name, with a zeroed
// mtime. It is staged in a temp file and renamed into place atomically.
func writeBundle(outPath string, out map[string]entry) error {
	names := make([]string, 0, len(out))
	for n := range out {
		names = append(names, n)
	}
	slices.Sort(names)

	tmp, err := os.CreateTemp(filepath.Dir(outPath), filepath.Base(outPath)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)
	werr := writeEntries(tw, names, out)
	// Close from innermost to outermost; a flush or trailer write can fail on any.
	if err := firstErr(werr, tw.Close(), gz.Close(), tmp.Close()); err != nil {
		_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup of the partial bundle
		return err
	}
	if err := os.Rename(tmpName, outPath); err != nil {
		_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
		return err
	}
	slog.Info("wrote bundle", "path", outPath, "files", len(names))
	return nil
}

func writeEntries(tw *tar.Writer, names []string, out map[string]entry) error {
	for _, name := range names {
		e := out[name]
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     int64(mode),
			Size:     int64(len(e.content)),
			Typeflag: tar.TypeReg,
			ModTime:  time.Unix(0, 0).UTC(),
		}); err != nil {
			return err
		}
		if _, err := tw.Write(e.content); err != nil {
			return err
		}
	}
	return nil
}

// normPattern trims a leading "/" from a glob so it matches the normalized
// (root-relative) entry names from readArchive. A leading "." is left intact so
// it can act as a root keep-anchor (see parsePattern).
func normPattern(p string) string {
	return strings.TrimLeft(p, "/")
}

// urlBase returns the final path element of a URL (or plain path), ignoring any
// query string, for filling in a directory destination.
func urlBase(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Path != "" {
		return path.Base(u.Path)
	}
	return path.Base(rawURL)
}

// firstErr returns the first non-nil error, used to surface a write error in
// preference to the close errors that follow it.
func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
