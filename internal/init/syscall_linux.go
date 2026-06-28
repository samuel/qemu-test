package main

import (
	"log"
	"syscall"
)

func shutdown() {
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
		log.Fatalf("Failed to power off VM: %v", err)
	}
}
