//go:build unix

package keysource

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// getuid is a variable so tests can simulate a foreign owner.
var getuid = os.Getuid

// openFlags opens the (already symlink-resolved) key file read-only and refuses
// to follow a symlink that appeared at that path after resolution.
const openFlags = os.O_RDONLY | syscall.O_NOFOLLOW

// isSymlinkOpenError reports whether err from an O_NOFOLLOW open means the path
// itself is a symlink (ELOOP on Linux/BSD; macOS reports the same errno, Solaris
// and some others use EMLINK).
func isSymlinkOpenError(err error) bool {
	return errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK)
}

// checkKeyFilePerms enforces OpenSSH-style key file hygiene: no group/other bits
// and owned by the current user. The path in the descriptive part of the message
// is printed verbatim; the copy-pasteable remedy after "run:" is shell-quoted so
// it stays correct for paths containing spaces or shell metacharacters.
func checkKeyFilePerms(path string, fi os.FileInfo) error {
	perm := fi.Mode().Perm()
	if perm&0o077 != 0 {
		return &permsError{msg: fmt.Sprintf("key file %s has mode %04o; run: chmod 600 %s", path, perm, shellQuote(path))}
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if uid := getuid(); int(st.Uid) != uid {
		q := shellQuote(path)
		return &permsError{msg: fmt.Sprintf("key file %s is owned by uid %d, not the current user (uid %d); run: chown %d %s && chmod 600 %s", path, st.Uid, uid, uid, q, q)}
	}
	return nil
}
