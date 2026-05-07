package main

import "os"

// writeFileOnly is a tiny wrapper used by tests to keep the helper
// signature uniform across package files.
func writeFileOnly(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}
