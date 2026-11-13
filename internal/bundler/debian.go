package bundler

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"samuelks.com/qemu-test/internal/cache"
)

// resolveDebianURL resolves a DebianRef to a concrete .deb URL: it follows the
// meta-package (Via) to discover the current kernel ABI, substitutes that into
// the target Package name, and returns mirror+Filename for that stanza. The index
// download is cached like any other source.
func resolveDebianURL(ctx context.Context, client *http.Client, ref *DebianRef, cacheDir string, refresh bool) (string, error) {
	raw, err := fetchPackagesIndex(ctx, client, ref, cacheDir, refresh)
	if err != nil {
		return "", err
	}
	pkgs, err := parsePackages(raw)
	if err != nil {
		return "", err
	}
	meta, ok := pkgs[ref.Via]
	if !ok {
		return "", fmt.Errorf("meta-package %q not found in %s/%s %s index", ref.Via, ref.Suite, ref.Component, ref.Arch)
	}
	abi, err := extractABI(meta["Depends"], ref.AbiDep)
	if err != nil {
		return "", fmt.Errorf("meta-package %q: %w", ref.Via, err)
	}
	target := strings.ReplaceAll(ref.Package, abiPlaceholder, abi)
	stanza, ok := pkgs[target]
	if !ok {
		return "", fmt.Errorf("package %q (abi %q) not found in %s index", target, abi, ref.Arch)
	}
	filename := stanza["Filename"]
	if filename == "" {
		return "", fmt.Errorf("package %q has no Filename in %s index", target, ref.Arch)
	}
	resolved := strings.TrimRight(ref.Mirror, "/") + "/" + strings.TrimLeft(filename, "/")
	slog.Debug("resolved debian package", "via", ref.Via, "abi", abi, "package", target, "url", resolved)
	return resolved, nil
}

// fetchPackagesIndex downloads and decompresses the binary-<arch> "Packages"
// index for the ref's suite/component, trying .xz then falling back to .gz.
func fetchPackagesIndex(ctx context.Context, client *http.Client, ref *DebianRef, cacheDir string, refresh bool) ([]byte, error) {
	base := fmt.Sprintf("%s/dists/%s/%s/binary-%s/Packages",
		strings.TrimRight(ref.Mirror, "/"), ref.Suite, ref.Component, ref.Arch)
	var lastErr error
	for _, ext := range []string{".xz", ".gz"} {
		path, err := cache.FetchToCache(ctx, client, base+ext, cacheDir, refresh, "package index")
		if err != nil {
			lastErr = err
			continue
		}
		raw, err := readMaybeCompressed(path)
		if err != nil {
			return nil, fmt.Errorf("read package index %s: %w", base+ext, err)
		}
		return raw, nil
	}
	return nil, fmt.Errorf("fetch package index %s: %w", base+".xz", lastErr)
}

// readMaybeCompressed reads the file at path, decompressing a gzip/xz/zstd
// stream and passing a plain one through unchanged.
func readMaybeCompressed(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only handle; close error is non-actionable
	src, closer, err := decompress(bufio.NewReader(f))
	if err != nil {
		return nil, err
	}
	if closer != nil {
		defer closer.Close() //nolint:errcheck // read-only decompressor; close error is non-actionable
	}
	return io.ReadAll(src)
}

// parsePackages parses a Debian "Packages" index into package name → fields.
// Stanzas are blank-line separated; a line starting with whitespace continues the
// previous field.
func parsePackages(raw []byte) (map[string]map[string]string, error) {
	pkgs := make(map[string]map[string]string)
	cur := map[string]string{}
	var lastField string
	flush := func() {
		if name := cur["Package"]; name != "" {
			pkgs[name] = cur
		}
		cur = map[string]string{}
		lastField = ""
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // long Depends lines exceed the default 64K cap
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case (line[0] == ' ' || line[0] == '\t') && lastField != "":
			cur[lastField] += "\n" + strings.TrimSpace(line)
		default:
			key, val, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			key = strings.TrimSpace(key)
			cur[key] = strings.TrimSpace(val)
			lastField = key
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan package index: %w", err)
	}
	if len(pkgs) == 0 {
		return nil, errors.New("package index is empty")
	}
	return pkgs, nil
}

// extractABI finds the dependency matching abiDep among a meta-package's Depends
// and returns the substring captured by {abi}. Version constraints are ignored,
// and only the first of a "pkg | alt" alternative is considered.
func extractABI(depends, abiDep string) (string, error) {
	prefix, suffix, ok := strings.Cut(abiDep, abiPlaceholder)
	if !ok {
		return "", fmt.Errorf("abiDep %q has no %s placeholder", abiDep, abiPlaceholder)
	}
	for dep := range strings.SplitSeq(depends, ",") {
		name := dep
		if i := strings.IndexByte(name, '|'); i >= 0 {
			name = name[:i]
		}
		if i := strings.IndexByte(name, '('); i >= 0 {
			name = name[:i]
		}
		name = strings.TrimSpace(name)
		if len(name) < len(prefix)+len(suffix) || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
			continue
		}
		if abi := name[len(prefix) : len(name)-len(suffix)]; abi != "" {
			return abi, nil
		}
	}
	return "", fmt.Errorf("no dependency matching %q in %q", abiDep, depends)
}
