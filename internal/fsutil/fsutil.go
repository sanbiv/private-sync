// Package fsutil provides atomic file writes and small filesystem helpers.
package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// TempPrefix is the marker used for in-flight temporary files. Every temp file
// is named "<target>.psv-tmp-<suffix>-<unique>" and lives in the target's
// directory: the suffix identifies the writer, the trailing unique part keeps
// two writes of the same target on the same machine apart.
const TempPrefix = ".psv-tmp-"

// EnsureDir creates dir with mode 0700 (parents included) when missing.
func EnsureDir(dir string) error { return os.MkdirAll(dir, 0o700) }

// WriteFileAtomic writes data to path via a same-directory temp file, fsyncs it
// and renames it over the target. suffix identifies the writer (e.g. the first 8
// chars of the machine id); the temp name additionally carries a unique tail, so
// no two writes ever share a temp file — not across machines, not across
// processes on one machine, and not across goroutines in one process.
func WriteFileAtomic(path string, data []byte, mode fs.FileMode, suffix string) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if suffix == "" {
		suffix = "w"
	}
	f, err := createTemp(dir, filepath.Base(path), suffix)
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmp, mode); err != nil {
		return err
	}
	if err = renameRetry(tmp, path); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		if d, derr := os.Open(dir); derr == nil {
			_ = d.Sync()
			_ = d.Close()
		}
	}
	return nil
}

// createTemp opens a fresh temp file for base in dir, named
// "<base>.psv-tmp-<suffix>-<unique>". os.CreateTemp picks the unique tail and
// creates the file exclusively, so two writers — two goroutines, two processes
// on one machine, or two machines — are never handed the same temp file.
func createTemp(dir, base, suffix string) (*os.File, error) {
	return os.CreateTemp(dir, base+TempPrefix+suffix+"-*")
}

func renameRetry(from, to string) error {
	var err error
	for i := 0; i < 10; i++ {
		err = os.Rename(from, to)
		if err == nil || runtime.GOOS != "windows" {
			return err
		}
		time.Sleep(time.Duration(50*(i+1)) * time.Millisecond)
	}
	return err
}

// hasTempSuffix reports whether name is a temp file whose writer suffix is
// exactly the one in marker (TempPrefix+suffix): either the bare
// "<target>.psv-tmp-<suffix>" or "<target>.psv-tmp-<suffix>-<unique>" written by
// WriteFileAtomic. A longer writer suffix that merely starts with this one
// ("…-tmp-abc1" for marker "…-tmp-abc") does not match.
func hasTempSuffix(name, marker string) bool {
	i := strings.LastIndex(name, marker)
	if i < 0 {
		return false
	}
	rest := name[i+len(marker):]
	return rest == "" || rest[0] == '-'
}

// CleanupTemp removes leftover temp files with the given suffix below root.
func CleanupTemp(root, suffix string) error {
	if suffix == "" {
		suffix = "w" // what WriteFileAtomic substitutes for an empty suffix
	}
	marker := TempPrefix + suffix
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" && p != root {
				return filepath.SkipDir
			}
			return nil
		}
		if hasTempSuffix(d.Name(), marker) {
			_ = os.Remove(p)
		}
		return nil
	})
}

// IsTemp reports whether name is one of our temp files.
func IsTemp(name string) bool { return strings.Contains(name, TempPrefix) }

// Exists reports whether path exists.
func Exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// ReadFileMax reads a file refusing anything larger than max bytes (ErrTooLarge).
func ReadFileMax(path string, max int64) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if max > 0 && st.Size() > max {
		return nil, ErrTooLarge
	}
	return os.ReadFile(path)
}

// ErrTooLarge is returned by ReadFileMax.
var ErrTooLarge = errors.New("file too large")
