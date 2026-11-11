package cli

import (
	"debug/elf"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samuel/qemu-test/internal/arch"
)

func TestElfGOARCH(t *testing.T) {
	cases := []struct {
		machine elf.Machine
		class   elf.Class
		data    elf.Data
		want    string
	}{
		{elf.EM_X86_64, elf.ELFCLASS64, elf.ELFDATA2LSB, "amd64"},
		{elf.EM_386, elf.ELFCLASS32, elf.ELFDATA2LSB, "386"},
		{elf.EM_AARCH64, elf.ELFCLASS64, elf.ELFDATA2LSB, "arm64"},
		{elf.EM_ARM, elf.ELFCLASS32, elf.ELFDATA2LSB, "arm"},
		{elf.EM_RISCV, elf.ELFCLASS64, elf.ELFDATA2LSB, "riscv64"},
		{elf.EM_LOONGARCH, elf.ELFCLASS64, elf.ELFDATA2LSB, "loong64"},
		{elf.EM_PPC64, elf.ELFCLASS64, elf.ELFDATA2MSB, "ppc64"},
		{elf.EM_PPC64, elf.ELFCLASS64, elf.ELFDATA2LSB, "ppc64le"},
		{elf.EM_S390, elf.ELFCLASS64, elf.ELFDATA2MSB, "s390x"},
		{elf.EM_RISCV, elf.ELFCLASS32, elf.ELFDATA2LSB, ""},
		{elf.EM_MIPS, elf.ELFCLASS64, elf.ELFDATA2MSB, ""},
	}
	var mapped []string
	for _, c := range cases {
		got, ok := elfGOARCH(c.machine, c.class, c.data)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("elfGOARCH(%s, %s, %s) = %q, %v, want %q", c.machine, c.class, c.data, got, ok, c.want)
		}
		if ok {
			mapped = append(mapped, got)
		}
	}
	// Every registered arch must be reachable from an ELF header.
	for _, name := range arch.Names() {
		a, err := arch.Lookup(name)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, m := range mapped {
			found = found || m == a.GOARCH
		}
		if !found {
			t.Errorf("arch %s (GOARCH %s) has no ELF mapping", name, a.GOARCH)
		}
	}
}

// buildHello compiles a trivial program for the given target and returns its path.
func buildHello(t *testing.T, env ...string) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "hello")
	cmd := exec.Command("go", "build", "-o", out, src)
	cmd.Env = append(os.Environ(), env...)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("go build unavailable: %v\n%s", err, b)
	}
	return out
}

func TestDetectArch(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries")
	}
	for _, goarch := range []string{"amd64", "arm64", "s390x", "ppc64le"} {
		bin := buildHello(t, "GOOS=linux", "GOARCH="+goarch, "CGO_ENABLED=0")
		got, err := detectArch(bin)
		if err != nil || got != goarch {
			t.Errorf("detectArch(linux/%s) = %q, %v", goarch, got, err)
		}
	}

	t.Run("not ELF", func(t *testing.T) {
		bin := buildHello(t, "GOOS=darwin", "GOARCH=arm64", "CGO_ENABLED=0")
		if _, err := detectArch(bin); err == nil || !strings.Contains(err.Error(), "GOOS=linux") {
			t.Errorf("detectArch(Mach-O) error = %v, want a GOOS=linux hint", err)
		}
	})

	t.Run("missing", func(t *testing.T) {
		if _, err := detectArch(filepath.Join(t.TempDir(), "nope")); err == nil {
			t.Error("detectArch(missing file) = nil error")
		}
	})
}
