//go:build windows

package state

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockRange is the byte range locked on windows (the lock file is empty, but
// LockFileEx locks ranges, not files; the whole 64-bit range is used).
const lockRange = ^uint32(0)

// lockHandle takes an exclusive LockFileEx lock on f, blocking until available.
func lockHandle(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, lockRange, lockRange, ol)
}

// unlockHandle releases the LockFileEx lock on f.
func unlockHandle(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, lockRange, lockRange, ol)
}
