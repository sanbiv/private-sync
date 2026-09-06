package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// lockFile is the name of the advisory lock file inside a directory guarded by
// dirLock (the state dir for machine.json, a store dir for base.json and the
// trash).
const lockFile = "lock"

// dirLock is an exclusive advisory lock on <dir>/lock. It serialises
// read-modify-write cycles on the files of dir across processes: a cli and a
// tui operating on the same vault both append trash rows through the lock, so
// neither rename silently drops the other's row. The lock is released by
// Unlock and, should the process die, by the operating system.
//
// The lock is per open file description (flock on unix, LockFileEx on
// windows), so two Store handles in one process are serialised as well.
type dirLock struct {
	f *os.File
}

// lockDir acquires the exclusive lock for dir, creating dir (0700) and the
// lock file (0600) when missing. It blocks until the lock is available.
func lockDir(dir string) (*dirLock, error) {
	if dir == "" {
		return nil, errors.New("lock: empty dir")
	}
	if err := ensureDir(dir); err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	p := filepath.Join(dir, lockFile)
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, fileMode)
	if err != nil {
		return nil, fmt.Errorf("lock: open %s: %w", p, err)
	}
	if err := lockHandle(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock: acquire %s: %w", p, err)
	}
	return &dirLock{f: f}, nil
}

// Unlock releases the lock. It is safe to call on a nil lock or twice.
func (l *dirLock) Unlock() {
	if l == nil || l.f == nil {
		return
	}
	_ = unlockHandle(l.f)
	_ = l.f.Close()
	l.f = nil
}
