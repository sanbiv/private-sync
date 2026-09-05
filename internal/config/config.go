// Package config loads, validates and saves the per-machine YAML configuration.
package config

import (
	"errors"
	"time"

	"github.com/sanbiv/private-sync/internal/paths"
)

// CurrentVersion is the config schema version written by this build.
const CurrentVersion = 1

// ErrNotFound is returned by Load when the config file does not exist (first run).
var ErrNotFound = errors.New("config file not found")

// Config is the per-machine configuration (see spec §3).
type Config struct {
	Version  int             `yaml:"version"`
	Machine  MachineConfig   `yaml:"machine"`
	Vault    VaultConfig     `yaml:"vault"`
	Key      KeyConfig       `yaml:"key"`
	Scan     ScanConfig      `yaml:"scan"`
	Projects []ProjectConfig `yaml:"projects"`
}

// MachineConfig holds the human label only; the machine id lives in the state dir.
type MachineConfig struct {
	Name string `yaml:"name"`
}

// VaultConfig locates the local vault copy and its remote.
type VaultConfig struct {
	Path   string       `yaml:"path"`
	Remote RemoteConfig `yaml:"remote"`
}

// RemoteType selects the transport.
type RemoteType string

const (
	RemoteNone   RemoteType = "none"
	RemoteGit    RemoteType = "git"
	RemoteRclone RemoteType = "rclone"
)

// RemoteConfig describes the remote transport.
type RemoteConfig struct {
	Type   RemoteType   `yaml:"type"`
	Git    GitRemote    `yaml:"git,omitempty"`
	Rclone RcloneRemote `yaml:"rclone,omitempty"`
}

// GitRemote configures the git transport.
type GitRemote struct {
	URL    string `yaml:"url,omitempty"`
	Branch string `yaml:"branch,omitempty"`
}

// RcloneRemote configures the rclone transport (e.g. Google Drive).
type RcloneRemote struct {
	Remote string `yaml:"remote,omitempty"`
	Path   string `yaml:"path,omitempty"`
}

// KeySource selects how the passphrase is obtained.
type KeySource string

const (
	KeyPrompt    KeySource = "prompt"
	KeyFile      KeySource = "file"
	KeyBitwarden KeySource = "bitwarden"
)

// KeyConfig configures the passphrase source.
type KeyConfig struct {
	Source    KeySource    `yaml:"source"`
	File      FileKey      `yaml:"file,omitempty"`
	Bitwarden BitwardenKey `yaml:"bitwarden,omitempty"`
}

// FileKey is the passphrase-file source.
type FileKey struct {
	Path string `yaml:"path,omitempty"`
}

// BitwardenKey is the Bitwarden CLI source.
type BitwardenKey struct {
	Item  string `yaml:"item,omitempty"`
	Field string `yaml:"field,omitempty"` // password | notes | <custom field name>
}

// ScanConfig configures the file scanner. Empty lists mean "use the defaults".
type ScanConfig struct {
	Include      []string `yaml:"include,omitempty"`
	ExcludeDirs  []string `yaml:"exclude_dirs,omitempty"`
	ExcludeFiles []string `yaml:"exclude_files,omitempty"`
	MaxFileSize  string   `yaml:"max_file_size,omitempty"` // e.g. "2MiB"
}

// ProjectConfig maps a vault project to a local directory on this machine.
type ProjectConfig struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name,omitempty"` // informational only
	Path string `yaml:"path"`
}

// DefaultMaxFileSize is used when scan.max_file_size is empty.
const DefaultMaxFileSize int64 = 2 * 1024 * 1024

// Default returns a config with sensible defaults (remote none, prompt key source).
func Default(d paths.Dirs) *Config {
	return &Config{
		Version: CurrentVersion,
		Machine: MachineConfig{Name: defaultMachineName()},
		Vault:   VaultConfig{Path: paths.ContractHome(paths.DefaultVaultDir()), Remote: RemoteConfig{Type: RemoteNone, Git: GitRemote{Branch: "main"}}},
		Key:     KeyConfig{Source: KeyPrompt, File: FileKey{Path: paths.ContractHome(d.DefaultKeyFile())}, Bitwarden: BitwardenKey{Field: "password"}},
		Scan:    ScanConfig{MaxFileSize: "2MiB"},
	}
}

// Load reads and validates the config at path. Returns ErrNotFound when missing.
func Load(path string) (*Config, error) {
	return nil, errors.New("config.Load: not implemented")
}

// Save writes the config atomically with mode 0600 (directory 0700).
func (c *Config) Save(path string) error {
	return errors.New("config.Save: not implemented")
}

// Validate checks types, paths and sizes (spec §3 validation rules).
func (c *Config) Validate() error {
	return errors.New("config.Validate: not implemented")
}

// VaultPath returns the expanded absolute vault directory.
func (c *Config) VaultPath() (string, error) { return paths.ExpandHome(c.Vault.Path) }

// KeyFilePath returns the expanded key file path (empty when unset).
func (c *Config) KeyFilePath() (string, error) {
	if c.Key.File.Path == "" {
		return "", nil
	}
	return paths.ExpandHome(c.Key.File.Path)
}

// MaxFileSize parses scan.max_file_size (DefaultMaxFileSize when empty).
func (c *Config) MaxFileSize() (int64, error) {
	if c.Scan.MaxFileSize == "" {
		return DefaultMaxFileSize, nil
	}
	return ParseSize(c.Scan.MaxFileSize)
}

// ParseSize parses "512", "2MiB", "1.5MB", "300KiB", "1GiB" (case-insensitive).
func ParseSize(s string) (int64, error) {
	return 0, errors.New("config.ParseSize: not implemented")
}

// Project finds a project by id or (case-insensitive) name.
func (c *Config) Project(idOrName string) (*ProjectConfig, bool) {
	return nil, false
}

// ProjectPath returns the expanded local path of a project.
func (c *Config) ProjectPath(id string) (string, error) {
	p, ok := c.Project(id)
	if !ok {
		return "", errors.New("unknown project " + id)
	}
	return paths.ExpandHome(p.Path)
}

// AddProject appends or replaces (by id) a project mapping.
func (c *Config) AddProject(p ProjectConfig) {}

// RemoveProject removes a mapping by id; reports whether it existed.
func (c *Config) RemoveProject(id string) bool { return false }

// Warnings returns non-fatal validation warnings (e.g. key file inside a project).
func (c *Config) Warnings() []string { return nil }

func defaultMachineName() string { return "machine-" + time.Now().Format("0102") }
