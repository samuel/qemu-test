package main

import (
	"log"
	"os"

	"golang.org/x/sys/unix"
)

// rawConsoleOutput turns off the console tty's output post-processing, which
// would otherwise rewrite every "\n" the binary prints as "\r\n" and corrupt
// output the host passes on verbatim (e.g. to go test -json). Best-effort.
func rawConsoleOutput() {
	fd := int(os.Stdout.Fd())
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		log.Printf("get console termios: %v", err)
		return
	}
	t.Oflag &^= unix.OPOST
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, t); err != nil {
		log.Printf("set console termios: %v", err)
	}
}
