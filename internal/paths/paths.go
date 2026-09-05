// Package paths resolves the per-user directories used by private-sync.
// It is the only package allowed to read HOME / XDG_* environment variables.
package paths

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// AppName is the directory name used under the XDG base directories.
const AppName = "private-sync"

// Dirs holds the absolute, expanded per-user directories.
type Dirs struct {
	Config string // $XDG_CONFIG_HOME/private-sync
	State  string // $XDG_STATE_HOME/private-sync
}

// Default resolves the directories: XDG_CONFIG_HOME / XDG_STATE_HOME when set
// (any OS), otherwise ~/.config and ~/.local/state (plain XDG on every OS).
func Default() (Dirs, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Dirs{}, err
	}
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(home, ".config")
	}
	st := os.Getenv("XDG_STATE_HOME")
	if st == "" {
		st = filepath.Join(home, ".local", "state")
	}
	return Dirs{Config: filepath.Join(cfg, AppName), State: filepath.Join(st, AppName)}, nil
}

// ConfigFile returns the default config file path.
func (d Dirs) ConfigFile() string { return filepath.Join(d.Config, "config.yaml") }

// DefaultKeyFile returns the default passphrase file path.
func (d Dirs) DefaultKeyFile() string { return filepath.Join(d.Config, "key") }

// DefaultVaultDir returns the default local vault directory (~/.local/share/private-sync/vault).
func DefaultVaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", AppName, "vault")
	}
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		data = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(data, AppName, "vault")
}

// ExpandHome expands a leading "~" or "~/" using os.UserHomeDir and returns an
// absolute, cleaned path. Other "~user" forms are returned unchanged.
func ExpandHome(p string) (string, error) {
	if p == "" {
		return "", errors.New("empty path")
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, p[1:])
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// ContractHome replaces the home directory prefix with "~" for display / config writing.
func ContractHome(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~" + filepath.ToSlash(p[len(home):])
	}
	return p
}

// EnsureDir creates dir (and parents) with mode 0700 when missing.
func EnsureDir(dir string) error {
	return os.MkdirAll(dir, 0o700)
}
