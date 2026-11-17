package main

import (
	"bytes"
	"os"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openPseudoTerminal opens a new pseudo-terminal: what a program writes to
// terminal, the test reads from controller.
func openPseudoTerminal(t *testing.T) (controller, terminal *os.File) {
	t.Helper()
	controller, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skip("no pseudo-terminal:", err)
	}
	ioctl := func(request uintptr, argument uintptr) {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, controller.Fd(), request, argument); errno != 0 {
			t.Fatal("pseudo-terminal:", errno)
		}
	}
	var name [128]byte
	ioctl(unix.TIOCPTYGRANT, 0)
	ioctl(unix.TIOCPTYUNLK, 0)
	ioctl(unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0])))
	terminal, err = os.OpenFile(string(name[:bytes.IndexByte(name[:], 0)]), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	return controller, terminal
}
