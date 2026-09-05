//go:build !windows

package keysource

import (
	"fmt"
	"os"
	"syscall"
)

// getuid is a variable so tests can simulate a foreign owner.
var getuid = os.Getuid

// openFlags opens the (already symlink-resolved) key file read-only and refuses
// to follow a symlink that appeared at that path after resolution.
const openFlags = os.O_RDONLY | syscall.O_NOFOLLOW

// checkKeyFilePerms enforces OpenSSH-style key file hygiene: no group/other bits
// and owned by the current user.
func checkKeyFilePerms(path string, fi os.FileInfo) error {
	perm := fi.Mode().Perm()
	if perm&0o077 != 0 {
		return &permsError{msg: fmt.Sprintf("key file %s has mode %04o; run: chmod 600 %s", path, perm, path)}
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if uid := getuid(); int(st.Uid) != uid {
		return &permsError{msg: fmt.Sprintf("key file %s is owned by uid %d, not the current user (uid %d); run: chown %d %s && chmod 600 %s", path, st.Uid, uid, uid, path, path)}
	}
	return nil
}
