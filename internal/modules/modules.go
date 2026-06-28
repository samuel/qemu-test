// Package modules is the host-side kernel-module resolver. Given a bundle's
// modules tree and a list of requested module names, it computes each module's
// dependency closure in load order and returns the .ko files to pack into the
// initramfs, mirroring modprobe's "load if available" behavior.
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

// Module is a kernel module to pack into the initramfs and load in the guest, in
// dependency order (dependencies before dependents).
type Module struct {
	Name       string // canonical module name (underscores), for logging
	HostPath   string // absolute path to the .ko on the host
	Base       string // file base name, e.g. "xhci-pci.ko" or "xhci-pci.ko.xz"
	Compressed bool   // Base is a compressed .ko (.gz/.xz/.zst) needing kernel-side decompression
}

// Resolve turns the requested module names into the ordered set of .ko files to
// load, sourced from the bundle's modules tree at modulesDir. It mirrors
// modprobe's "load if available" behavior: a requested module that has a matching
// .ko is loaded together with its dependency closure (dependencies first); one
// that is absent from the tree is presumed built into the kernel (or simply
// unavailable) and returned in skipped rather than failing. modulesDir == ""
// (a bare kernel with no modules tree) presumes every request is built in.
//
// Dependencies come from modules.dep when present, else from each module's ELF
// .modinfo (see buildModuleGraph), so a tree that ships only .ko files still works.
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

	// order holds distinct canonical names, and a canonical name is derived from
	// the .ko base name, so distinct names always have distinct base names — no
	// two packed modules can collide on their root basename.
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

// buildModuleGraph builds the lookups Resolve needs from a modules tree:
// an index from canonical module name to its tree-relative .ko path, and depsOf,
// a per-module list of canonical dependency names. It prefers modules.dep (the
// authoritative depmod output) when present; otherwise it walks the tree to index
// the .ko files and reads dependencies from each module's ELF .modinfo on demand
// (cached), transparently decompressing .ko.gz/.ko.xz/.ko.zst. A module whose
// .modinfo cannot be read (not a parseable ELF, or an unknown compression) yields
// no dependencies, with a single warning.
func buildModuleGraph(modulesDir string) (map[string]string, func(string) []string, error) {
	if HasModulesDep(modulesDir) {
		depMap, idx, err := parseModulesDep(modulesDir)
		if err != nil {
			// A modules.dep(.bin) we cannot parse is not fatal: fall back to the
			// .modinfo walk below, which works for uncompressed/.gz modules.
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

// HasModulesDep reports whether the tree has a modules.dep in either form (text
// or the binary modules.dep.bin).
func HasModulesDep(modulesDir string) bool {
	for _, f := range []string{"modules.dep", "modules.dep.bin"} {
		if _, err := os.Stat(filepath.Join(modulesDir, f)); err == nil {
			return true
		}
	}
	return false
}

// parseModulesDep reads modules.dep, returning each module's dependency paths
// (keyed by the module's tree-relative path) and an index from canonical module
// name to that path. Each entry is a "<module-path>: <dep-path>..." line, with
// paths relative to the modules-tree root. The text modules.dep is preferred;
// when it is absent the binary modules.dep.bin (whose values are the same lines)
// is read instead.
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

// walkModuleIndex indexes every .ko file under modulesDir by canonical module
// name, mapping to its tree-relative path. On a duplicate name (rare) the first
// wins. Used when there is no modules.dep to provide the index.
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

// koDepends reads a module's direct dependencies from its ELF .modinfo "depends="
// field, as canonical names. It handles uncompressed .ko and gzip/xz/zstd-compressed
// modules (.ko.gz/.ko.xz/.ko.zst), decompressing into memory to parse the ELF. A
// module with no .modinfo or no depends= yields no dependencies.
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

// koReaderAt returns an io.ReaderAt over a module's uncompressed ELF bytes: a
// plain .ko is read straight from the file, while a .ko.gz/.ko.xz/.ko.zst is
// decompressed into memory (modules are small enough to hold).
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
// closing closer (when non-nil) afterward so a decompressor's resources are freed.
// A read error takes precedence over a close error.
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

// parseModinfoDepends extracts the canonical dependency names from a module's
// .modinfo section, a sequence of NUL-separated "key=value" entries. It reads the
// "depends=" entry (a comma-separated module-name list, possibly empty).
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

// parseModulesBuiltin reads modules.builtin into a set of canonical module names.
// The text form is preferred; when it is absent the binary modules.builtin.bin
// (whose keys are the built-in module names) is read instead. Both files are
// optional — a tree with neither yields an empty set — and a .bin we cannot parse
// is non-fatal (built-in detection is advisory; it only affects whether a missing
// requested module is skipped quietly or with a warning).
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

// topoSortModules returns the dependency closure of roots (canonical module names)
// in load order — dependencies before the modules that need them — de-duplicated.
// depsOf gives a module's direct dependencies; has reports whether a name has a
// .ko in the tree, so a dependency that is built into the kernel (or otherwise
// absent) is skipped rather than chased.
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
		order = append(order, name) // post-order: dependencies appended before name
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

// canonicalModuleName reduces a module name or .ko path to the canonical form
// used for matching: the base name with any ".ko"(+compression) suffix removed
// and hyphens folded to underscores (modprobe treats the two interchangeably).
func canonicalModuleName(s string) string {
	s = filepath.Base(s)
	if i := strings.Index(s, ".ko"); i >= 0 {
		s = s[:i]
	}
	return strings.ReplaceAll(s, "-", "_")
}

// kmod binary-index (.bin) format constants, from kmod's libkmod-index.c. A .bin
// file is a 12-byte header (big-endian magic, version, root-node offset) followed
// by a radix trie of nodes; each node may carry a prefix, a child table, and/or a
// list of values, flagged in the high bits of its offset word.
const (
	kmodIndexMagic = 0xB007F457
	kmodNodePrefix = 0x80000000
	kmodNodeValues = 0x40000000
	kmodNodeChilds = 0x20000000
	kmodNodeMask   = 0x0FFFFFFF
)

var errCorruptKmodIndex = errors.New("corrupt kmod index")

// readKmodIndex parses a kmod binary index file (modules.dep.bin,
// modules.builtin.bin) into a map of key → values, used to read the .bin forms
// when the text modules.dep/modules.builtin are absent (some trees ship only the
// binary indices). For modules.dep.bin each value is a modules.dep line; for
// modules.builtin.bin the keys are the built-in module names (values empty).
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
// down the trie and recording each node's values under its full key. seen guards
// against cycles in a corrupt file.
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
