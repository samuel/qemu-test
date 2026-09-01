package modules

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// writeModuleTree builds a synthetic modules tree: a modules.dep (each entry
// "relpath: dep...") plus an empty .ko for every path mentioned, and an optional
// modules.builtin.
func writeModuleTree(t *testing.T, dep map[string]string, builtin []string) string {
	t.Helper()
	dir := t.TempDir()

	var depLines strings.Builder
	kos := map[string]bool{}
	for mod, deps := range dep {
		depLines.WriteString(mod + ":")
		if deps != "" {
			depLines.WriteString(" " + deps)
		}
		depLines.WriteString("\n")
		kos[mod] = true
		for d := range strings.FieldsSeq(deps) {
			kos[d] = true
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "modules.dep"), []byte(depLines.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	for ko := range kos {
		full := filepath.Join(dir, ko)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("ko"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if builtin != nil {
		if err := os.WriteFile(filepath.Join(dir, "modules.builtin"), []byte(strings.Join(builtin, "\n")), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func bases(mods []Module) []string {
	out := make([]string, len(mods))
	for i, m := range mods {
		out[i] = m.Base
	}
	return out
}

func TestResolveModulesDependencyOrder(t *testing.T) {
	// xhci-pci depends (transitively) on xhci-hcd and usbcore; usbcore is the leaf.
	dir := writeModuleTree(t, map[string]string{
		"kernel/drivers/usb/host/xhci-pci.ko": "kernel/drivers/usb/host/xhci-hcd.ko kernel/drivers/usb/core/usbcore.ko",
		"kernel/drivers/usb/host/xhci-hcd.ko": "kernel/drivers/usb/core/usbcore.ko",
		"kernel/drivers/usb/core/usbcore.ko":  "",
	}, nil)

	load, skipped, err := Resolve(dir, []string{"xhci_pci"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v, want none", skipped)
	}
	got := bases(load)
	want := []string{"usbcore.ko", "xhci-hcd.ko", "xhci-pci.ko"}
	if !slices.Equal(got, want) {
		t.Fatalf("load order = %v, want %v (dependencies first)", got, want)
	}
}

func TestResolveModulesNameNormalization(t *testing.T) {
	dir := writeModuleTree(t, map[string]string{
		"kernel/drivers/usb/host/xhci-hcd.ko": "",
	}, nil)
	// The on-disk file uses a hyphen; the request uses an underscore.
	load, _, err := Resolve(dir, []string{"xhci_hcd"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := bases(load); !slices.Equal(got, []string{"xhci-hcd.ko"}) {
		t.Fatalf("load = %v, want [xhci-hcd.ko]", got)
	}
}

func TestResolveModulesBuiltinSkipped(t *testing.T) {
	dir := writeModuleTree(t, map[string]string{
		"kernel/drivers/usb/core/usbcore.ko": "",
	}, []string{"kernel/drivers/usb/host/xhci-hcd.ko"})

	// xhci_hcd is listed in modules.builtin but has no .ko: presumed built in.
	load, skipped, err := Resolve(dir, []string{"xhci_hcd"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(load) != 0 {
		t.Fatalf("load = %v, want none (built in)", bases(load))
	}
	if !slices.Equal(skipped, []string{"xhci_hcd"}) {
		t.Fatalf("skipped = %v, want [xhci_hcd]", skipped)
	}
}

func TestResolveModulesUnknownSkipped(t *testing.T) {
	dir := writeModuleTree(t, map[string]string{
		"kernel/drivers/usb/core/usbcore.ko": "",
	}, nil)
	// An unknown module is not an error under the hybrid policy: it's skipped.
	load, skipped, err := Resolve(dir, []string{"does_not_exist"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(load) != 0 {
		t.Fatalf("load = %v, want none", bases(load))
	}
	if !slices.Equal(skipped, []string{"does_not_exist"}) {
		t.Fatalf("skipped = %v, want [does_not_exist]", skipped)
	}
}

func TestResolveModulesNoTree(t *testing.T) {
	// No modules tree (bare kernel): every request is presumed built in.
	load, skipped, err := Resolve("", []string{"xhci_pci", "foo"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(load) != 0 {
		t.Fatalf("load = %v, want none", bases(load))
	}
	if !slices.Equal(skipped, []string{"xhci_pci", "foo"}) {
		t.Fatalf("skipped = %v, want [xhci_pci foo]", skipped)
	}
}

func TestResolveModulesCompressedFlag(t *testing.T) {
	dir := writeModuleTree(t, map[string]string{
		"kernel/drivers/usb/core/usbcore.ko.xz": "",
	}, nil)
	load, _, err := Resolve(dir, []string{"usbcore"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(load) != 1 || !load[0].Compressed {
		t.Fatalf("expected one compressed module, got %+v", load)
	}
}

func TestParseModinfoDepends(t *testing.T) {
	cases := []struct {
		name    string
		modinfo string
		want    []string
	}{
		{"two deps, hyphen folded", "license=GPL\x00depends=xhci-hcd,usbcore\x00", []string{"xhci_hcd", "usbcore"}},
		{"empty depends", "license=GPL\x00depends=\x00", nil},
		{"no depends key", "license=GPL\x00vermagic=6.1\x00", nil},
		{"depends among other keys", "depends=usbcore\x00alias=usb:v\x00", []string{"usbcore"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseModinfoDepends([]byte(tc.modinfo))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("parseModinfoDepends(%q) = %v, want %v", tc.modinfo, got, tc.want)
			}
		})
	}
}

// writeFakeKo writes a minimal ELF64 object whose .modinfo declares the given
// dependencies, compressed when the path has a .gz/.xz/.zst suffix.
func writeFakeKo(t *testing.T, path string, depends []string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data := fakeKoBytes(t, depends)
	switch {
	case strings.HasSuffix(path, ".gz"):
		data = gzipData(t, data)
	case strings.HasSuffix(path, ".xz"):
		data = xzData(t, data)
	case strings.HasSuffix(path, ".zst"):
		data = zstdData(t, data)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeKoBytes builds a minimal but valid ELF64 object whose .modinfo section
// declares the given dependencies.
func fakeKoBytes(t *testing.T, depends []string) []byte {
	t.Helper()
	modinfo := []byte("license=GPL\x00depends=" + strings.Join(depends, ",") + "\x00")
	shstrtab := []byte("\x00.modinfo\x00.shstrtab\x00")
	const ( // offsets of the names within shstrtab
		nameModinfo  = 1
		nameShstrtab = 10
	)
	const (
		ehSize    = 64
		shEntSize = 64
	)
	modinfoOff := ehSize
	shstrtabOff := modinfoOff + len(modinfo)
	shoff := shstrtabOff + len(shstrtab)

	var buf bytes.Buffer
	put := func(vs ...any) {
		for _, v := range vs {
			if err := binary.Write(&buf, binary.LittleEndian, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	// e_ident: magic, ELFCLASS64, ELFDATA2LSB, EV_CURRENT, then padding.
	buf.Write([]byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	put(uint16(1), uint16(62), uint32(1))                                   // e_type=ET_REL, e_machine=EM_X86_64, e_version
	put(uint64(0), uint64(0), uint64(shoff))                                // e_entry, e_phoff, e_shoff
	put(uint32(0), uint16(ehSize), uint16(0), uint16(0), uint16(shEntSize)) // e_flags, e_ehsize, e_phentsize, e_phnum, e_shentsize
	put(uint16(3), uint16(2))                                               // e_shnum, e_shstrndx (.shstrtab is section 2)
	buf.Write(modinfo)
	buf.Write(shstrtab)
	writeSH := func(name, typ uint32, off, size uint64) {
		put(name, typ, uint64(0), uint64(0), off, size, uint32(0), uint32(0), uint64(0), uint64(0))
	}
	writeSH(0, 0, 0, 0)                                                  // SHT_NULL
	writeSH(nameModinfo, 1, uint64(modinfoOff), uint64(len(modinfo)))    // SHT_PROGBITS
	writeSH(nameShstrtab, 3, uint64(shstrtabOff), uint64(len(shstrtab))) // SHT_STRTAB
	return buf.Bytes()
}

// gzipData, xzData, and zstdData return b compressed with the named codec.
func gzipData(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	gw := gzip.NewWriter(&out)
	if _, err := gw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func xzData(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	xw, err := xz.NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := xw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := xw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func zstdData(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	zw, err := zstd.NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestResolveModulesNoModulesDepFromModinfo(t *testing.T) {
	// A tree with .ko files but no modules.dep: dependencies come from .modinfo.
	dir := t.TempDir()
	writeFakeKo(t, filepath.Join(dir, "kernel/drivers/usb/host/xhci-pci.ko"), []string{"xhci_hcd"})
	writeFakeKo(t, filepath.Join(dir, "kernel/drivers/usb/host/xhci-hcd.ko"), []string{"usbcore"})
	writeFakeKo(t, filepath.Join(dir, "kernel/drivers/usb/core/usbcore.ko"), nil)

	load, skipped, err := Resolve(dir, []string{"xhci_pci"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v, want none", skipped)
	}
	got := bases(load)
	want := []string{"usbcore.ko", "xhci-hcd.ko", "xhci-pci.ko"}
	if !slices.Equal(got, want) {
		t.Fatalf("load order = %v, want %v (deps from .modinfo, dependencies first)", got, want)
	}
}

func TestResolveModulesNoModulesDepDegrade(t *testing.T) {
	// A .ko that is not parseable as ELF is still found and loaded, just without
	// auto-resolved dependencies.
	dir := t.TempDir()
	ko := filepath.Join(dir, "kernel/usbcore.ko")
	if err := os.MkdirAll(filepath.Dir(ko), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ko, []byte("not an elf file"), 0o644); err != nil {
		t.Fatal(err)
	}
	load, _, err := Resolve(dir, []string{"usbcore"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := bases(load); !slices.Equal(got, []string{"usbcore.ko"}) {
		t.Fatalf("load = %v, want [usbcore.ko]", got)
	}
}

func TestKoDependsCompressed(t *testing.T) {
	// koDepends reads .modinfo through gzip/xz/zstd as well as uncompressed.
	want := []string{"xhci_hcd", "usbcore"}
	dir := t.TempDir()
	for _, base := range []string{"m.ko", "m.ko.gz", "m.ko.xz", "m.ko.zst"} {
		t.Run(base, func(t *testing.T) {
			p := filepath.Join(dir, base)
			writeFakeKo(t, p, []string{"xhci-hcd", "usbcore"})
			got, err := koDepends(p)
			if err != nil {
				t.Fatalf("koDepends: %v", err)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("koDepends(%s) = %v, want %v", base, got, want)
			}
		})
	}
}

func TestResolveModulesNoModulesDepFromModinfoXz(t *testing.T) {
	// The ppc64 bundle's shape: .ko.xz with no modules.dep, so each module must be
	// decompressed to read its .modinfo depends=.
	dir := t.TempDir()
	writeFakeKo(t, filepath.Join(dir, "kernel/drivers/usb/host/xhci-pci.ko.xz"), []string{"xhci_hcd"})
	writeFakeKo(t, filepath.Join(dir, "kernel/drivers/usb/host/xhci-hcd.ko.xz"), []string{"usbcore"})
	writeFakeKo(t, filepath.Join(dir, "kernel/drivers/usb/core/usbcore.ko.xz"), nil)

	load, skipped, err := Resolve(dir, []string{"xhci_pci"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v, want none", skipped)
	}
	want := []string{"usbcore.ko.xz", "xhci-hcd.ko.xz", "xhci-pci.ko.xz"}
	if got := bases(load); !slices.Equal(got, want) {
		t.Fatalf("load order = %v, want %v (deps from .modinfo, dependencies first)", got, want)
	}
	if !load[0].Compressed {
		t.Fatalf("expected compressed modules, got %+v", load)
	}
}

// writeKmodIndex writes a kmod binary index mapping each key to a single value.
// The trie shares no prefixes, which is still valid.
func writeKmodIndex(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	type node struct {
		children map[byte]*node
		value    string
		hasValue bool
	}
	root := &node{children: map[byte]*node{}}
	for k, v := range entries {
		n := root
		for i := range len(k) {
			c := k[i]
			child := n.children[c]
			if child == nil {
				child = &node{children: map[byte]*node{}}
				n.children[c] = child
			}
			n = child
		}
		n.value, n.hasValue = v, true
	}

	var nodes bytes.Buffer
	put := func(v any) {
		if err := binary.Write(&nodes, binary.BigEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	var ser func(n *node) uint32
	ser = func(n *node) uint32 {
		childOff := map[byte]uint32{}
		var first, last byte
		var have bool
		for c := range 256 {
			if ch, ok := n.children[byte(c)]; ok {
				childOff[byte(c)] = ser(ch)
				if !have {
					first, have = byte(c), true
				}
				last = byte(c)
			}
		}
		off := uint32(12 + nodes.Len())
		var flags uint32
		if have {
			flags |= kmodNodeChilds
		}
		if n.hasValue {
			flags |= kmodNodeValues
		}
		if have {
			nodes.WriteByte(first)
			nodes.WriteByte(last)
			for c := int(first); c <= int(last); c++ {
				put(childOff[byte(c)]) // 0 for gaps
			}
		}
		if n.hasValue {
			put(uint32(1)) // value count
			put(uint32(0)) // priority
			nodes.WriteString(n.value)
			nodes.WriteByte(0)
		}
		return off | flags
	}
	rootOff := ser(root)

	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.BigEndian, []uint32{uint32(kmodIndexMagic), 0x00020001, rootOff}); err != nil {
		t.Fatal(err)
	}
	buf.Write(nodes.Bytes())
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadKmodIndexRoundTrip(t *testing.T) {
	entries := map[string]string{
		"a":        "kernel/a.ko:", // single-char key exercises trie edges
		"usbcore":  "kernel/drivers/usb/core/usbcore.ko:",
		"xhci_hcd": "kernel/drivers/usb/host/xhci-hcd.ko: kernel/drivers/usb/core/usbcore.ko",
		"xhci_pci": "kernel/drivers/usb/host/xhci-pci.ko: kernel/drivers/usb/host/xhci-hcd.ko kernel/drivers/usb/core/usbcore.ko",
	}
	path := filepath.Join(t.TempDir(), "modules.dep.bin")
	writeKmodIndex(t, path, entries)
	got, err := readKmodIndex(path)
	if err != nil {
		t.Fatalf("readKmodIndex: %v", err)
	}
	if len(got) != len(entries) {
		t.Fatalf("got %d entries, want %d (%v)", len(got), len(entries), got)
	}
	for k, v := range entries {
		if g := got[k]; len(g) != 1 || g[0] != v {
			t.Fatalf("got[%q] = %v, want [%q]", k, g, v)
		}
	}
}

func TestReadKmodIndexBadMagic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.bin")
	if err := os.WriteFile(path, []byte("not a kmod index at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readKmodIndex(path); err == nil {
		t.Fatal("expected an error for a non-kmod-index file")
	}
}

func mkKo(t *testing.T, dir, rel string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("ko"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveModulesFromModulesDepBin(t *testing.T) {
	// Only the binary modules.dep.bin (no text modules.dep) drives resolution.
	dir := t.TempDir()
	mkKo(t, dir, "kernel/drivers/usb/core/usbcore.ko")
	mkKo(t, dir, "kernel/drivers/usb/host/xhci-hcd.ko")
	mkKo(t, dir, "kernel/drivers/usb/host/xhci-pci.ko")
	writeKmodIndex(t, filepath.Join(dir, "modules.dep.bin"), map[string]string{
		"usbcore":  "kernel/drivers/usb/core/usbcore.ko:",
		"xhci_hcd": "kernel/drivers/usb/host/xhci-hcd.ko: kernel/drivers/usb/core/usbcore.ko",
		"xhci_pci": "kernel/drivers/usb/host/xhci-pci.ko: kernel/drivers/usb/host/xhci-hcd.ko kernel/drivers/usb/core/usbcore.ko",
	})
	load, skipped, err := Resolve(dir, []string{"xhci_pci"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v, want none", skipped)
	}
	if got := bases(load); !slices.Equal(got, []string{"usbcore.ko", "xhci-hcd.ko", "xhci-pci.ko"}) {
		t.Fatalf("load = %v, want deps-first order from modules.dep.bin", got)
	}
}

func TestResolveModulesBuiltinBinSkipped(t *testing.T) {
	// modules.builtin.bin (no text form) marks a requested module built in → skipped.
	dir := t.TempDir()
	mkKo(t, dir, "kernel/usbcore.ko") // a .ko so the tree is recognized
	writeKmodIndex(t, filepath.Join(dir, "modules.builtin.bin"), map[string]string{"xhci_pci": ""})
	load, skipped, err := Resolve(dir, []string{"xhci_pci"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(load) != 0 {
		t.Fatalf("load = %v, want none (built in)", bases(load))
	}
	if !slices.Equal(skipped, []string{"xhci_pci"}) {
		t.Fatalf("skipped = %v, want [xhci_pci]", skipped)
	}
}
