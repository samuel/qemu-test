package main

import (
	"fmt"
	"log"

	"golang.org/x/sys/unix"
)

// setupLoopback brings up lo (down at boot) so tests can use 127.0.0.1 / ::1.
// Best-effort: fail only by logging.
func setupLoopback() {
	if err := bringUpLoopback(); err != nil {
		log.Printf("bring up loopback: %v", err)
	}
}

// bringUpLoopback sets IFF_UP on "lo", activating both 127.0.0.1 and ::1.
func bringUpLoopback() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	defer unix.Close(fd) //nolint:errcheck // config-only socket; nothing is written, so a close error is moot

	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return fmt.Errorf("ifreq lo: %w", err)
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return fmt.Errorf("get lo flags: %w", err)
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("set lo up: %w", err)
	}
	return nil
}
