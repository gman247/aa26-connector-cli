// Stub termios helpers for non-Linux builds. Lets `go build` succeed on
// macOS/Windows even though `aa26-connector test` is Linux-only — the
// other subcommands (new, validate, package) work everywhere.

//go:build !linux

package main

import "os"

func isTerminal(_ *os.File) bool { return false }

func readPasswordNoEcho(f *os.File) ([]byte, error) {
	// On non-Linux platforms the `test` subcommand isn't supported, but
	// we still want the binary to build for new/validate/package use.
	// Fall back to a plain echoed read; callers only reach this when
	// `test` is invoked on an unsupported OS.
	buf := make([]byte, 0, 256)
	one := make([]byte, 1)
	for {
		n, err := f.Read(one)
		if n == 0 || err != nil || one[0] == '\n' {
			break
		}
		buf = append(buf, one[0])
	}
	return buf, nil
}
