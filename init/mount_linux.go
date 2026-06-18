package main

import (
	"fmt"
	"log"
	"os"
	"syscall"
)

func mountfs() error {
	_ = os.MkdirAll("/tmp", 0777)
	for _, p := range []string{"/proc", "/sys", "/dev", "/dev/pts", "/dev/shm", "/run", "/perm", "/perm/var/run", "/sys/fs/cgroup"} {
		_ = os.MkdirAll(p, 0755)
	}

	if err := syscall.Mount("tmpfs", "/tmp", "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_RELATIME, ""); err != nil {
		return fmt.Errorf("tmpfs on /tmp: %v", err)
	}

	if err := os.Symlink("/proc/net/pnp", "/tmp/resolv.conf"); err != nil {
		return fmt.Errorf("etc: %v", err)
	}

	if err := syscall.Mount("devtmpfs", "/dev", "devtmpfs", 0, ""); err != nil {
		if sce, ok := err.(syscall.Errno); ok && sce == syscall.EBUSY {
			// /dev was already mounted (common in setups using nfsroot= or initramfs)
		} else {
			return fmt.Errorf("devtmpfs: %v", err)
		}
	}

	if err := os.MkdirAll("/dev/pts", 0755); err != nil {
		return fmt.Errorf("mkdir /dev/pts: %v", err)
	}

	if err := syscall.Mount("devpts", "/dev/pts", "devpts", 0, ""); err != nil {
		return fmt.Errorf("devpts: %v", err)
	}

	if err := os.MkdirAll("/dev/shm", 0755); err != nil {
		return fmt.Errorf("mkdir /dev/shm: %v", err)
	}

	if err := syscall.Mount("tmpfs", "/dev/shm", "tmpfs", 0, ""); err != nil {
		return fmt.Errorf("tmpfs on /dev/shm: %v", err)
	}

	if err := syscall.Mount("tmpfs", "/run", "tmpfs", 0, ""); err != nil {
		log.Printf("tmpfs on /run: %v", err)
	}

	// /proc is useful for exposing process details and for
	// interactive debugging sessions.
	if err := syscall.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		if sce, ok := err.(syscall.Errno); ok && sce == syscall.EBUSY {
			// /proc was already mounted (common in setups using nfsroot= or initramfs)
		} else {
			return fmt.Errorf("proc: %v", err)
		}
	}

	// /sys is useful for retrieving additional status from the
	// kernel, e.g. ethernet device carrier status.
	if err := syscall.Mount("sysfs", "/sys", "sysfs", 0, ""); err != nil {
		if sce, ok := err.(syscall.Errno); ok && sce == syscall.EBUSY {
			// /sys was already mounted (common in setups using nfsroot= or initramfs)
		} else {
			return fmt.Errorf("sys: %v", err)
		}
	}
	//
	// dev := rootdev.Partition(rootdev.Perm)
	// for _, fstype := range []string{"ext4", "vfat", "bcachefs"} {
	// 	if err := syscall.Mount(dev, "/perm", fstype, 0, ""); err != nil {
	// 		log.Printf("Could not mount permanent storage partition %s as %s: %v", dev, fstype, err)
	// 	} else {
	// 		break
	// 	}
	// }

	// Create /perm/var if needed so that the /var symlink resolves
	if err := os.MkdirAll("/perm/var", 0755); err != nil {
		log.Printf("mkdir /perm/var: %v", err)
	}

	if err := os.Symlink("/run", "/perm/var/run"); err != nil && !os.IsExist(err) {
		log.Printf("symlink /perm/var/run to /run: %v", err)
	}

	if err := syscall.Mount("cgroup2", "/sys/fs/cgroup", "cgroup2", 0, ""); err != nil {
		log.Printf("cgroup2 on /sys/fs/cgroup: %v", err)
	}

	// /out is an optional host-shared directory (virtio-9p) the host attaches
	// when run with -out, letting tests hand files (e.g. coverage) back to the
	// host. Best-effort: without -out the device is absent and the mount fails
	// harmlessly, so log-and-continue rather than fail the run.
	if err := os.MkdirAll("/out", 0o755); err != nil {
		log.Printf("mkdir /out: %v", err)
	} else if err := syscall.Mount("outshare", "/out", "9p", 0, "trans=virtio,version=9p2000.L"); err != nil {
		log.Printf("mount /out (9p outshare): %v", err)
	}

	return nil
}
