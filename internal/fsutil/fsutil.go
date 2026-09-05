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
// is named "<target>.psv-tmp-<suffix>" and lives in the target's directory.
const TempPrefix = ".psv-tmp-"

// EnsureDir creates dir with mode 0700 (parents included) when missing.
func EnsureDir(dir string) error { return os.MkdirAll(dir, 0o700) }

// WriteFileAtomic writes data to path via a same-directory temp file, fsyncs it
// and renames it over the target. suffix identifies the writer (e.g. the first 8
// chars of the machine id) so concurrent writers on a shared folder never collide.
func WriteFileAtomic(path string, data []byte, mode fs.FileMode, suffix string) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if suffix == "" {
		suffix = "w"
	}
	tmp := path + TempPrefix + suffix
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
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

// CleanupTemp removes leftover temp files with the given suffix below root.
func CleanupTemp(root, suffix string) error {
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
		if strings.HasSuffix(d.Name(), marker) {
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
