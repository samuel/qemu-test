package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"syscall"
)

func mountfs() error {
	_ = os.MkdirAll("/tmp", 0777)
	for _, p := range []string{"/root", "/proc", "/sys", "/dev", "/dev/pts", "/dev/shm", "/run", "/perm", "/perm/var/run", "/sys/fs/cgroup"} {
		_ = os.MkdirAll(p, 0755)
	}

	if err := syscall.Mount("tmpfs", "/tmp", "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_RELATIME, ""); err != nil {
		if sce, ok := err.(syscall.Errno); ok && sce == syscall.EBUSY {
			// /tmp was already mounted
		} else {
			return fmt.Errorf("tmpfs on /tmp: %w", err)
		}
	}

	if err := syscall.Mount("devtmpfs", "/dev", "devtmpfs", 0, ""); err != nil {
		if sce, ok := err.(syscall.Errno); ok && sce == syscall.EBUSY {
			// /dev was already mounted (common in setups using nfsroot= or initramfs)
		} else {
			return fmt.Errorf("devtmpfs: %w", err)
		}
	}

	if err := os.MkdirAll("/dev/pts", 0755); err != nil {
		return fmt.Errorf("mkdir /dev/pts: %w", err)
	}

	if err := syscall.Mount("devpts", "/dev/pts", "devpts", 0, ""); err != nil {
		if sce, ok := err.(syscall.Errno); ok && sce == syscall.EBUSY {
			// /dev/pts was already mounted
		} else {
			return fmt.Errorf("devpts: %w", err)
		}
	}

	if err := os.MkdirAll("/dev/shm", 0755); err != nil {
		return fmt.Errorf("mkdir /dev/shm: %w", err)
	}

	if err := syscall.Mount("tmpfs", "/dev/shm", "tmpfs", 0, ""); err != nil {
		if sce, ok := err.(syscall.Errno); ok && sce == syscall.EBUSY {
			// /dev/shm was already mounted
		} else {
			return fmt.Errorf("tmpfs on /dev/shm: %w", err)
		}
	}

	if err := syscall.Mount("tmpfs", "/run", "tmpfs", 0, ""); err != nil {
		log.Printf("tmpfs on /run: %v", err)
	}

	if err := syscall.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		if sce, ok := err.(syscall.Errno); ok && sce == syscall.EBUSY {
			// /proc was already mounted (common in setups using nfsroot= or initramfs)
		} else {
			return fmt.Errorf("proc: %w", err)
		}
	}

	if err := syscall.Mount("sysfs", "/sys", "sysfs", 0, ""); err != nil {
		if sce, ok := err.(syscall.Errno); ok && sce == syscall.EBUSY {
			// /sys was already mounted (common in setups using nfsroot= or initramfs)
		} else {
			return fmt.Errorf("sys: %w", err)
		}
	}

	// So the /var symlink resolves.
	if err := os.MkdirAll("/perm/var", 0755); err != nil {
		log.Printf("mkdir /perm/var: %v", err)
	}

	if err := os.Symlink("/run", "/perm/var/run"); err != nil && !os.IsExist(err) {
		log.Printf("symlink /perm/var/run to /run: %v", err)
	}

	if err := syscall.Mount("cgroup2", "/sys/fs/cgroup", "cgroup2", 0, ""); err != nil {
		log.Printf("cgroup2 on /sys/fs/cgroup: %v", err)
	}

	return nil
}

// warn reports a non-fatal setup problem to the host, which logs it as a warning.
func warn(format string, args ...any) {
	marker("[TESTS:WARN] "+format+"\n", args...)
}

// mountShares mounts the virtio-9p shares listed in test-shares.txt, one
// "tag<TAB>guest path<TAB>ro|rw" per line. It must run after loadModules, which
// registers the 9p driver and its transport. A share that cannot mount (e.g. a
// kernel without 9p) leaves an empty directory at its path, so the binary still
// runs with paths that resolve; the host is warned that the contents are missing.
func mountShares() error {
	lines, err := readLines("test-shares.txt")
	if err != nil {
		return err
	}
	for _, line := range lines {
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			return fmt.Errorf("malformed test-shares.txt line %q", line)
		}
		tag, path, mode := f[0], f[1], f[2]
		if err := os.MkdirAll(path, 0o755); err != nil {
			warn("share %s: mkdir: %v", path, err)
			continue
		}
		var flags uintptr
		if mode == "ro" {
			flags = syscall.MS_RDONLY
		}
		if err := syscall.Mount(tag, path, "9p", flags, "trans=virtio,version=9p2000.L"); err != nil {
			warn("share %s not mounted (%v); using an empty directory", path, err)
		}
	}
	return nil
}
