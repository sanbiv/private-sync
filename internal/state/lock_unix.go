//go:build !windows

package state

import (
	"os"
	"syscall"
)

// lockHandle takes an exclusive flock(2) lock on f, blocking until available.
// flock locks belong to the open file description, so every os.OpenFile call
// (and therefore every dirLock) is an independent lock holder.
func lockHandle(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			return err
		}
	}
}

// unlockHandle releases the flock(2) lock on f.
func unlockHandle(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
