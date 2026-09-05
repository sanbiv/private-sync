//go:build windows

package keysource

import "os"

// openFlags opens the key file read-only (O_NOFOLLOW has no Windows equivalent).
const openFlags = os.O_RDONLY

// checkKeyFilePerms is a no-op on Windows (spec §11: checks skipped).
func checkKeyFilePerms(string, os.FileInfo) error { return nil }
