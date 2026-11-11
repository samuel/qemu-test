package cli

import (
	"debug/elf"
	"errors"
	"fmt"
)

// detectArch returns the GOARCH of the Linux executable at path, read from its
// ELF header, so the guest architecture follows the binary. It rejects files the
// guest could not run: anything that is not an ELF executable, and dynamically
// linked binaries, since the guest has no dynamic loader or libraries.
func detectArch(path string) (string, error) {
	f, err := elf.Open(path)
	if err != nil {
		if _, ok := errors.AsType[*elf.FormatError](err); ok {
			return "", fmt.Errorf("%s is not a Linux ELF executable (build with GOOS=linux)", path)
		}
		return "", err
	}
	//nolint:errcheck // read-only file; nothing to flush on close.
	defer f.Close()

	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return "", fmt.Errorf("%s is not an executable (ELF type %s)", path, f.Type)
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return "", fmt.Errorf("%s is dynamically linked; the guest has no dynamic loader (build with CGO_ENABLED=0)", path)
		}
	}
	goarch, ok := elfGOARCH(f.Machine, f.Class, f.Data)
	if !ok {
		return "", fmt.Errorf("%s: unsupported ELF machine %s (%s, %s)", path, f.Machine, f.Class, f.Data)
	}
	return goarch, nil
}

// elfGOARCH maps an ELF header's machine, word size, and byte order to a GOARCH.
func elfGOARCH(m elf.Machine, class elf.Class, data elf.Data) (string, bool) {
	is64 := class == elf.ELFCLASS64
	little := data == elf.ELFDATA2LSB
	switch {
	case m == elf.EM_X86_64 && is64:
		return "amd64", true
	case m == elf.EM_386:
		return "386", true
	case m == elf.EM_AARCH64:
		return "arm64", true
	case m == elf.EM_ARM && little:
		return "arm", true
	case m == elf.EM_RISCV && is64:
		return "riscv64", true
	case m == elf.EM_LOONGARCH && is64:
		return "loong64", true
	case m == elf.EM_PPC64 && little:
		return "ppc64le", true
	case m == elf.EM_PPC64:
		return "ppc64", true
	case m == elf.EM_S390 && is64:
		return "s390x", true
	}
	return "", false
}
