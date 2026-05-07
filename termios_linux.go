// Minimal termios helpers — turns echo off long enough to read a
// password from stdin, then restores the previous tty state. We roll
// our own instead of pulling in golang.org/x/term to avoid that
// package's transitive dependency on newer go/x/sys versions.
//
// Linux-only because the test harness itself is Linux-only (the
// production-port emulator + --network=host docker run combo doesn't
// work on macOS/Windows in v1; see docs/test-harness.md).

//go:build linux

package main

import (
	"bufio"
	"errors"
	"io"
	"os"
	"syscall"
	"unsafe"
)

const (
	ioctlGetTermios = syscall.TCGETS
	ioctlSetTermios = syscall.TCSETS
)

// isTerminal returns true when the given file is a tty. Falls back to
// "not a terminal" on any ioctl error so behavior degrades gracefully
// when stdin is a pipe / fixture.
func isTerminal(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL,
		f.Fd(), ioctlGetTermios, uintptr(unsafe.Pointer(&t)), 0, 0, 0)
	return errno == 0
}

// readPasswordNoEcho reads a single line from f with terminal echo
// disabled, then restores the original termios. The returned slice does
// NOT include the trailing newline.
func readPasswordNoEcho(f *os.File) ([]byte, error) {
	fd := f.Fd()
	var orig syscall.Termios
	if _, _, err := syscall.Syscall6(syscall.SYS_IOCTL,
		fd, ioctlGetTermios, uintptr(unsafe.Pointer(&orig)), 0, 0, 0); err != 0 {
		return nil, errors.New("get termios: " + err.Error())
	}
	disabled := orig
	disabled.Lflag &^= syscall.ECHO
	disabled.Lflag |= syscall.ICANON | syscall.ISIG
	if _, _, err := syscall.Syscall6(syscall.SYS_IOCTL,
		fd, ioctlSetTermios, uintptr(unsafe.Pointer(&disabled)), 0, 0, 0); err != 0 {
		return nil, errors.New("disable echo: " + err.Error())
	}
	defer func() {
		_, _, _ = syscall.Syscall6(syscall.SYS_IOCTL,
			fd, ioctlSetTermios, uintptr(unsafe.Pointer(&orig)), 0, 0, 0)
	}()

	r := bufio.NewReader(f)
	line, err := r.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	// trim trailing CR/LF
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
		line = line[:len(line)-1]
	}
	return line, nil
}
