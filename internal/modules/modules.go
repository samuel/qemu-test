// Package modules is the host-side kernel-module resolver: given a modules tree
// and requested module names it returns the .ko files to pack into the initramfs,
// in dependency order.
package modules

import (
	"bytes"
	"compress/gzip"
	"debug/elf"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// Module is a kernel module to pack into the initramfs and load in the guest.
type Module struct {
	Name       string // canonical module name (underscores)
	HostPath   string // absolute path to the .ko on the host
	Base       string // file base name, e.g. "xhci-pci.ko" or "xhci-pci.ko.xz"
	Compressed bool   // needs kernel-side decompression (.gz/.xz/.zst)
}

// Resolve turns the requested module names into the ordered set of .ko files to
// load from the modules tree at modulesDir. It mirrors modprobe's "load if
// available" behavior: a module with a matching .ko is loaded along with its
// dependency closure; one absent from the tree is presumed built in and returned
// in skipped. modulesDir == "" presumes every request is built in.
func Resolve(modulesDir string, names []string) (load []Module, skipped []string, err error) {
	var requested []string
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			requested = append(requested, n)
		}
	}
	if len(requested) == 0 {
		return nil, nil, nil
	}
	if modulesDir == "" {
		slog.Debug("no modules tree available; presuming requested modules are built in", "modules", requested)
		return nil, requested, nil
	}

	index, depsOf, err := buildModuleGraph(modulesDir)
	if err != nil {
		return nil, nil, err
	}
	builtin, err := parseModulesBuiltin(modulesDir)
	if err != nil {
		return nil, nil, err
	}

	var roots []string
	for _, name := range requested {
		canon := canonicalModuleName(name)
		if _, ok := index[canon]; !ok {
			if builtin[canon] {
				slog.Debug("module is built into the kernel; nothing to load", "module", name)
			} else {
				slog.Warn("module not found in the bundle's modules tree or modules.builtin; assuming built in", "module", name)
			}
			skipped = append(skipped, name)
			continue
		}
		roots = append(roots, canon)
	}

	has := func(name string) bool { _, ok := index[name]; return ok }
	order, err := topoSortModules(roots, depsOf, has)
	if err != nil {
		return nil, nil, err
	}

	// Canonical names are derived from the .ko base name, so distinct names in
	// order always have distinct base names; packed modules cannot collide.
	for _, name := range order {
		rel := index[name]
		base := filepath.Base(rel)
		host := filepath.Join(modulesDir, rel)
		if _, err := os.Stat(host); err != nil {
			return nil, nil, fmt.Errorf("module file %q: %w", rel, err)
		}
		load = append(load, Module{
			Name:       name,
			HostPath:   host,
			Base:       base,
			Compressed: !strings.HasSuffix(base, ".ko"),
		})
	}
	return load, skipped, nil
}

// buildModuleGraph builds the lookups Resolve needs: an index from canonical
// module name to tree-relative .ko path, and depsOf, a module's canonical
// dependency names. It prefers modules.dep (depmod's output); otherwise it walks
// the tree and reads dependencies from each module's ELF .modinfo on demand.
func buildModuleGraph(modulesDir string) (map[string]string, func(string) []string, error) {
	if HasModulesDep(modulesDir) {
		depMap, idx, err := parseModulesDep(modulesDir)
		if err != nil {
			// Not fatal: fall back to the .modinfo walk below.
			slog.Warn("cannot parse modules.dep; resolving dependencies from .ko .modinfo instead", "err", err)
		} else {
			depsOf := func(name string) []string {
				var out []string
				for _, d := range depMap[idx[name]] {
					out = append(out, canonicalModuleName(d))
				}
				return out
			}
			return idx, depsOf, nil
		}
	}

	idx, err := walkModuleIndex(modulesDir)
	if err != nil {
		return nil, nil, err
	}
	slog.Debug("modules.dep not found; resolving module dependencies from .ko .modinfo", "tree", modulesDir)
	cache := make(map[string][]string)
	warned := false
	depsOf := func(name string) []string {
		if d, ok := cache[name]; ok {
			return d
		}
		deps, err := koDepends(filepath.Join(modulesDir, idx[name]))
		if err != nil {
			if !warned {
				slog.Warn("cannot read module dependencies from .modinfo; loading without auto-resolved dependencies (name them explicitly if needed)", "module", name, "err", err)
				warned = true
			}
			deps = nil
		}
		cache[name] = deps
		return deps
	}
	return idx, depsOf, nil
}

// HasModulesDep reports whether the tree has a modules.dep, text or binary.
func HasModulesDep(modulesDir string) bool {
	for _, f := range []string{"modules.dep", "modules.dep.bin"} {
		if _, err := os.Stat(filepath.Join(modulesDir, f)); err == nil {
			return true
		}
	}
	return false
}

// parseModulesDep reads modules.dep, returning each module's dependency paths
// keyed by its tree-relative path, plus an index from canonical name to that
// path. Falls back to modules.dep.bin, whose values are the same lines.
func parseModulesDep(modulesDir string) (deps map[string][]string, index map[string]string, err error) {
	deps = make(map[string][]string)
	index = make(map[string]string)

	b, err := os.ReadFile(filepath.Join(modulesDir, "modules.dep"))
	if err == nil {
		for line := range strings.SplitSeq(string(b), "\n") {
			parseDepLine(line, deps, index)
		}
		return deps, index, nil
	} else if !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("read modules.dep: %w", err)
	}

	idx, err := readKmodIndex(filepath.Join(modulesDir, "modules.dep.bin"))
	if err != nil {
		return nil, nil, fmt.Errorf("read modules.dep.bin: %w", err)
	}
	for _, lines := range idx {
		for _, line := range lines {
			parseDepLine(line, deps, index)
		}
	}
	return deps, index, nil
}

// parseDepLine parses one "<module-path>: <dep-path>..." entry into deps and index.
func parseDepLine(line string, deps map[string][]string, index map[string]string) {
	if line = strings.TrimSpace(line); line == "" {
		return
	}
	modPath, depStr, ok := strings.Cut(line, ":")
	if !ok {
		return
	}
	modPath = strings.TrimSpace(modPath)
	deps[modPath] = strings.Fields(depStr)
	index[canonicalModuleName(modPath)] = modPath
}

// walkModuleIndex indexes every .ko under modulesDir by canonical module name,
// mapping to its tree-relative path. Used when there is no modules.dep.
func walkModuleIndex(modulesDir string) (map[string]string, error) {
	index := make(map[string]string)
	err := filepath.WalkDir(modulesDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !IsKoFile(d.Name()) {
			return nil
		}
		rel, err := filepath.Rel(modulesDir, p)
		if err != nil {
			return err
		}
		canon := canonicalModuleName(d.Name())
		if prev, ok := index[canon]; ok {
			slog.Debug("duplicate module name in tree; keeping first", "module", canon, "kept", prev, "ignored", rel)
			return nil
		}
		index[canon] = rel
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk modules tree: %w", err)
	}
	return index, nil
}

// koDepends reads a module's direct dependencies from its ELF .modinfo
// "depends=" field, as canonical names.
func koDepends(koPath string) ([]string, error) {
	f, err := os.Open(koPath)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only handle; the parsed dependencies are what matter

	ra, err := koReaderAt(f, filepath.Base(koPath))
	if err != nil {
		return nil, err
	}

	ef, err := elf.NewFile(ra)
	if err != nil {
		return nil, err
	}
	sec := ef.Section(".modinfo")
	if sec == nil {
		return nil, nil
	}
	data, err := sec.Data()
	if err != nil {
		return nil, err
	}
	return parseModinfoDepends(data), nil
}

// koReaderAt returns an io.ReaderAt over a module's uncompressed ELF bytes,
// decompressing a .ko.gz/.ko.xz/.ko.zst into memory (modules are small).
func koReaderAt(f *os.File, base string) (io.ReaderAt, error) {
	switch {
	case strings.HasSuffix(base, ".ko"):
		return f, nil
	case strings.HasSuffix(base, ".ko.gz"):
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		return readAllReaderAt(gz, gz)
	case strings.HasSuffix(base, ".ko.xz"):
		xr, err := xz.NewReader(f)
		if err != nil {
			return nil, err
		}
		return readAllReaderAt(xr, nil) // ulikunitz/xz readers hold no resources to close
	case strings.HasSuffix(base, ".ko.zst"):
		zr, err := zstd.NewReader(f)
		if err != nil {
			return nil, err
		}
		rc := zr.IOReadCloser()
		return readAllReaderAt(rc, rc)
	default:
		return nil, fmt.Errorf("unrecognized module file %q", base)
	}
}

// readAllReaderAt reads all of r into memory and returns an io.ReaderAt over it,
// closing closer afterward. A read error takes precedence over a close error.
func readAllReaderAt(r io.Reader, closer io.Closer) (io.ReaderAt, error) {
	data, rerr := io.ReadAll(r)
	var cerr error
	if closer != nil {
		cerr = closer.Close()
	}
	if rerr != nil {
		return nil, rerr
	}
	if cerr != nil {
		return nil, cerr
	}
	return bytes.NewReader(data), nil
}

// parseModinfoDepends extracts canonical dependency names from a module's
// .modinfo section, a sequence of NUL-separated "key=value" entries.
func parseModinfoDepends(modinfo []byte) []string {
	var deps []string
	for field := range bytes.SplitSeq(modinfo, []byte{0}) {
		v, ok := bytes.CutPrefix(field, []byte("depends="))
		if !ok {
			continue
		}
		for d := range strings.SplitSeq(string(v), ",") {
			if d = strings.TrimSpace(d); d != "" {
				deps = append(deps, canonicalModuleName(d))
			}
		}
	}
	return deps
}

// parseModulesBuiltin reads modules.builtin (or modules.builtin.bin) into a set
// of canonical module names. Both files are optional and an unparseable .bin is
// non-fatal: built-in detection only affects whether a skip is warned about.
func parseModulesBuiltin(modulesDir string) (map[string]bool, error) {
	set := make(map[string]bool)

	b, err := os.ReadFile(filepath.Join(modulesDir, "modules.builtin"))
	if err == nil {
		for line := range strings.SplitSeq(string(b), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				set[canonicalModuleName(line)] = true
			}
		}
		return set, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read modules.builtin: %w", err)
	}

	idx, err := readKmodIndex(filepath.Join(modulesDir, "modules.builtin.bin"))
	if err != nil {
		if os.IsNotExist(err) {
			return set, nil // neither form present
		}
		slog.Warn("cannot parse modules.builtin.bin; built-in module detection disabled", "err", err)
		return set, nil
	}
	for key := range idx {
		set[canonicalModuleName(key)] = true
	}
	return set, nil
}

// topoSortModules returns the de-duplicated dependency closure of roots in load
// order. has reports whether a name has a .ko in the tree, so a dependency that
// is built in (or otherwise absent) is skipped rather than chased.
func topoSortModules(roots []string, depsOf func(string) []string, has func(string) bool) ([]string, error) {
	const (
		visiting = 1
		done     = 2
	)
	state := make(map[string]int)
	var order []string
	var visit func(name string) error
	visit = func(name string) error {
		switch state[name] {
		case done:
			return nil
		case visiting:
			return fmt.Errorf("module dependency cycle at %q", name)
		}
		state[name] = visiting
		for _, d := range depsOf(name) {
			if !has(d) {
				slog.Debug("module dependency not in tree; assuming built in", "module", name, "dependency", d)
				continue
			}
			if err := visit(d); err != nil {
				return err
			}
		}
		state[name] = done
		order = append(order, name) // post-order: dependencies come first
		return nil
	}
	for _, r := range roots {
		if err := visit(r); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// IsKoFile reports whether name is a kernel module file (optionally compressed).
func IsKoFile(name string) bool {
	return strings.HasSuffix(name, ".ko") ||
		strings.HasSuffix(name, ".ko.gz") ||
		strings.HasSuffix(name, ".ko.xz") ||
		strings.HasSuffix(name, ".ko.zst")
}

// canonicalModuleName reduces a module name or .ko path to its base name with
// any ".ko"(+compression) suffix removed and hyphens folded to underscores, as
// modprobe treats the two interchangeably.
func canonicalModuleName(s string) string {
	s = filepath.Base(s)
	if i := strings.Index(s, ".ko"); i >= 0 {
		s = s[:i]
	}
	return strings.ReplaceAll(s, "-", "_")
}

// kmod binary-index (.bin) format constants, from kmod's libkmod-index.c. A .bin
// file is a 12-byte header (magic, version, root-node offset) followed by a radix
// trie; a node's offset word flags whether it carries a prefix, children, values.
const (
	kmodIndexMagic = 0xB007F457
	kmodNodePrefix = 0x80000000
	kmodNodeValues = 0x40000000
	kmodNodeChilds = 0x20000000
	kmodNodeMask   = 0x0FFFFFFF
)

var errCorruptKmodIndex = errors.New("corrupt kmod index")

// readKmodIndex parses a kmod binary index (modules.dep.bin, modules.builtin.bin)
// into a map of key → values. For modules.dep.bin each value is a modules.dep
// line; for modules.builtin.bin the keys are module names and values are empty.
func readKmodIndex(path string) (map[string][]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 12 {
		return nil, fmt.Errorf("kmod index %q: too short", path)
	}
	if binary.BigEndian.Uint32(data) != kmodIndexMagic {
		return nil, fmt.Errorf("kmod index %q: bad magic", path)
	}
	root := binary.BigEndian.Uint32(data[8:12])
	out := make(map[string][]string)
	if err := readKmodNode(data, root, "", out, make(map[uint32]bool)); err != nil {
		return nil, fmt.Errorf("kmod index %q: %w", path, err)
	}
	return out, nil
}

// readKmodNode walks the node at word (offset + flags), accumulating key prefixes
// down the trie. seen guards against cycles in a corrupt file.
func readKmodNode(data []byte, word uint32, prefix string, out map[string][]string, seen map[uint32]bool) error {
	nodeOff := word & kmodNodeMask
	if nodeOff == 0 {
		return nil // empty subtree
	}
	if seen[nodeOff] {
		return errCorruptKmodIndex
	}
	seen[nodeOff] = true
	p := int(nodeOff)
	if p >= len(data) {
		return errCorruptKmodIndex
	}

	key := prefix
	if word&kmodNodePrefix != 0 {
		s, np, err := readKmodCString(data, p)
		if err != nil {
			return err
		}
		key += s
		p = np
	}

	var first int
	var childOffs []uint32
	if word&kmodNodeChilds != 0 {
		if p+2 > len(data) {
			return errCorruptKmodIndex
		}
		first = int(data[p])
		last := int(data[p+1])
		p += 2
		n := last - first + 1
		if n <= 0 || n > 256 {
			return errCorruptKmodIndex
		}
		childOffs = make([]uint32, n)
		for i := range childOffs {
			if p+4 > len(data) {
				return errCorruptKmodIndex
			}
			childOffs[i] = binary.BigEndian.Uint32(data[p:])
			p += 4
		}
	}

	if word&kmodNodeValues != 0 {
		if p+4 > len(data) {
			return errCorruptKmodIndex
		}
		count := binary.BigEndian.Uint32(data[p:])
		p += 4
		var vals []string
		for range count {
			if p+4 > len(data) {
				return errCorruptKmodIndex
			}
			p += 4 // value priority (unused here)
			s, np, err := readKmodCString(data, p)
			if err != nil {
				return err
			}
			vals = append(vals, s)
			p = np
		}
		out[key] = vals
	}

	for i, off := range childOffs {
		if off&kmodNodeMask == 0 {
			continue // gap in the child table
		}
		if err := readKmodNode(data, off, key+string([]byte{byte(first + i)}), out, seen); err != nil {
			return err
		}
	}
	return nil
}

func readKmodCString(data []byte, p int) (string, int, error) {
	if p > len(data) {
		return "", 0, errCorruptKmodIndex
	}
	i := bytes.IndexByte(data[p:], 0)
	if i < 0 {
		return "", 0, errCorruptKmodIndex
	}
	return string(data[p : p+i]), p + i + 1, nil
}
