//go:build !unix

package keysource

import "os"

// openFlags opens the key file read-only. O_NOFOLLOW is only defined on Unix;
// elsewhere the symlink resolved by ResolveKeyFile is opened as-is.
const openFlags = os.O_RDONLY

// isSymlinkOpenError is always false outside Unix: without O_NOFOLLOW the open
// never fails because of a symlink.
func isSymlinkOpenError(error) bool { return false }

// checkKeyFilePerms is a no-op outside Unix (spec §11: the mode/ownership checks
// are skipped on Windows; other non-Unix targets have no POSIX bits to inspect).
func checkKeyFilePerms(string, os.FileInfo) error { return nil }
