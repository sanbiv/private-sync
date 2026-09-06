// Package config loads, validates and saves the per-machine YAML configuration.
//
// Paths are stored exactly as typed in the file ("~/Work/myapp", not
// "/Users/me/Work/myapp") so that Save writes back what the user wrote and the
// same config.yaml can be copied between machines. Consumers must therefore
// read paths through the accessor methods (VaultPath, KeyFilePath,
// ProjectPath), which expand "~" and return absolute, cleaned paths; the raw
// struct fields (Vault.Path, Key.File.Path, Projects[i].Path) are for display
// and editing only.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sanbiv/private-sync/internal/fsutil"
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

// DefaultBranch is the git branch used when vault.remote.git.branch is empty.
const DefaultBranch = "main"

// DefaultBitwardenField is the Bitwarden field used when key.bitwarden.field is empty.
const DefaultBitwardenField = "password"

// FileMode is the permission the config file is written with.
const FileMode fs.FileMode = 0o600

// DirMode is the permission the config directory is created with.
const DirMode fs.FileMode = 0o700

// tempSeq numbers the temp files created by this process so that concurrent
// Save calls (goroutines) never share a temp file either.
var tempSeq atomic.Uint64

// tempSuffix returns the suffix used for the atomic temp file of one Save
// call. It embeds the process id and a per-process counter so that two
// private-sync processes (or two goroutines) saving the config at the same
// instant never open the same temp file: fsutil.WriteFileAtomic truncates an
// existing temp file instead of failing, so a shared name could rename an
// interleaved or half-written file over config.yaml.
func tempSuffix() string {
	return "cfg-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatUint(tempSeq.Add(1), 10)
}

// Default returns a config with sensible defaults (remote none, prompt key source).
//
// d is normally the result of paths.Default(). A zero Dirs is tolerated: the
// default key file path is then left empty (the prompt source does not need
// one) instead of the relative "key" that Validate would reject. The vault
// path always comes from paths.DefaultVaultDir, which needs a resolvable home
// directory; without one it falls back to a relative path that Validate
// reports, so Default(...).Save fails loudly rather than persisting a config
// whose vault location depends on the working directory.
func Default(d paths.Dirs) *Config {
	keyFile := ""
	if d.Config != "" {
		keyFile = paths.ContractHome(d.DefaultKeyFile())
	}
	return &Config{
		Version: CurrentVersion,
		Machine: MachineConfig{Name: defaultMachineName()},
		Vault:   VaultConfig{Path: paths.ContractHome(paths.DefaultVaultDir()), Remote: RemoteConfig{Type: RemoteNone, Git: GitRemote{Branch: DefaultBranch}}},
		Key:     KeyConfig{Source: KeyPrompt, File: FileKey{Path: keyFile}, Bitwarden: BitwardenKey{Field: DefaultBitwardenField}},
		Scan:    ScanConfig{MaxFileSize: "2MiB"},
	}
}

// Load reads and validates the config at path. Returns ErrNotFound when missing.
// Values are kept as typed ("~" is not expanded); use the accessor methods
// (VaultPath, KeyFilePath, ProjectPath) for expanded absolute paths.
func Load(path string) (*Config, error) {
	if path == "" {
		return nil, errors.New("config: empty config path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	c, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return c, nil
}

// Parse decodes YAML bytes into a validated Config. Unknown keys are rejected
// so that typos (e.g. "max_filesize") do not silently fall back to defaults.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("config file is empty")
		}
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	// A second document ("---") would be silently dropped otherwise, leaving
	// the user with a misleading "vault.path: required" style error.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("config file must contain a single YAML document (found a second document after ---)")
	}
	if c.Version == 0 {
		c.Version = 1
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Save validates the config and writes it atomically with mode 0600
// (directory 0700). Values are written as stored ("~" is not expanded).
// An invalid config is refused before anything touches the disk, so Save
// can never persist a file that Load would immediately reject; Validate also
// fills type-dependent defaults (git branch, Bitwarden field) before writing.
func (c *Config) Save(path string) error {
	if c == nil {
		return errors.New("config: Save on nil config")
	}
	if path == "" {
		return errors.New("config: empty config path")
	}
	if err := c.Validate(); err != nil {
		return fmt.Errorf("config: refusing to save: %w", err)
	}
	data, err := c.Marshal()
	if err != nil {
		return err
	}
	// DirMode is applied here directly (not via fsutil.EnsureDir) so that the
	// exported constant can never drift from the mode actually used.
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return fmt.Errorf("config: create directory: %w", err)
	}
	// MkdirAll only sets the mode on directories it creates. A config
	// directory created by hand (or by an older build) with looser
	// permissions would otherwise keep exposing config.yaml's parent, so the
	// spec's "config dir 0700" is re-applied on every save. Windows has no
	// POSIX permission bits, and a failure here is not fatal: the file itself
	// is still written 0600.
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(dir); err == nil && st.IsDir() && st.Mode().Perm() != DirMode {
			_ = os.Chmod(dir, DirMode)
		}
	}
	if err := fsutil.WriteFileAtomic(path, data, FileMode, tempSuffix()); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	return nil
}

// Marshal renders the config as YAML (2-space indent, header comment).
func (c *Config) Marshal() ([]byte, error) {
	if c == nil {
		return nil, errors.New("config: Marshal on nil config")
	}
	out := *c
	if out.Version == 0 {
		out.Version = CurrentVersion
	}
	var buf bytes.Buffer
	buf.WriteString("# private-sync configuration (edit with `private-sync config edit`).\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&out); err != nil {
		return nil, fmt.Errorf("config: encode yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("config: encode yaml: %w", err)
	}
	return buf.Bytes(), nil
}

// Validate checks types, paths and sizes (spec §3 validation rules). It also
// fills defaults that depend on the chosen type (git branch "main", Bitwarden
// field "password", empty remote type = none, empty key source = prompt).
// Warnings (non-fatal issues) are reported separately by Warnings.
func (c *Config) Validate() error {
	if c == nil {
		return errors.New("config: nil config")
	}
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	switch {
	case c.Version < 0:
		add("version: must be a positive integer (got %d)", c.Version)
	case c.Version > CurrentVersion:
		add("version %d is newer than this build supports (%d); upgrade private-sync", c.Version, CurrentVersion)
	}

	// Vault.
	vaultStable := false
	if strings.TrimSpace(c.Vault.Path) == "" {
		add("vault.path: required")
	} else if !isStablePath(c.Vault.Path) {
		add("vault.path: %q must be absolute or start with ~/ (a relative path would depend on the working directory)", c.Vault.Path)
	} else {
		vaultStable = true
	}
	if c.Vault.Remote.Type == "" {
		c.Vault.Remote.Type = RemoteNone
	}
	switch c.Vault.Remote.Type {
	case RemoteNone:
	case RemoteGit:
		if strings.TrimSpace(c.Vault.Remote.Git.URL) == "" {
			add("vault.remote.git.url: required when vault.remote.type is git")
		}
		if strings.TrimSpace(c.Vault.Remote.Git.Branch) == "" {
			c.Vault.Remote.Git.Branch = DefaultBranch
		}
	case RemoteRclone:
		if strings.TrimSpace(c.Vault.Remote.Rclone.Remote) == "" {
			add("vault.remote.rclone.remote: required when vault.remote.type is rclone")
		}
		if strings.TrimSpace(c.Vault.Remote.Rclone.Path) == "" {
			add("vault.remote.rclone.path: required when vault.remote.type is rclone")
		}
	default:
		add("vault.remote.type: unknown type %q (want none, git or rclone)", string(c.Vault.Remote.Type))
	}

	// Key source.
	if c.Key.Source == "" {
		c.Key.Source = KeyPrompt
	}
	switch c.Key.Source {
	case KeyPrompt:
	case KeyFile:
		if strings.TrimSpace(c.Key.File.Path) == "" {
			add("key.file.path: required when key.source is file")
		}
	case KeyBitwarden:
		if strings.TrimSpace(c.Key.Bitwarden.Item) == "" {
			add("key.bitwarden.item: required when key.source is bitwarden")
		}
		if strings.TrimSpace(c.Key.Bitwarden.Field) == "" {
			c.Key.Bitwarden.Field = DefaultBitwardenField
		}
	default:
		add("key.source: unknown source %q (want prompt, file or bitwarden)", string(c.Key.Source))
	}

	// The key file path is checked whenever it is set, not only when the file
	// source is selected: a stale but dangerous value should be reported
	// before the user switches sources.
	keyStable := false
	if c.Key.File.Path != "" && !isStablePath(c.Key.File.Path) {
		add("key.file.path: %q must be absolute or start with ~/ (a relative path would depend on the working directory)", c.Key.File.Path)
	} else if c.Key.File.Path != "" {
		keyStable = true
	}

	// Key file must never live inside the vault (it would be synced in clear).
	// The check only runs when both paths are stable: a relative path has
	// already been reported above, and expanding it against the working
	// directory could add a second, cwd-dependent and misleading error (e.g.
	// when the command happens to run from inside the vault).
	if keyStable && vaultStable {
		keyPath, kerr := paths.ExpandHome(c.Key.File.Path)
		vaultPath, verr := paths.ExpandHome(c.Vault.Path)
		switch {
		case kerr != nil:
			add("key.file.path: %v", kerr)
		case verr != nil:
			add("vault.path: %v", verr)
		case isWithin(vaultPath, keyPath):
			add("key.file.path %q is inside vault.path %q: the passphrase file must not live in the vault", c.Key.File.Path, c.Vault.Path)
		}
	}

	// Scan.
	if c.Scan.MaxFileSize != "" {
		n, err := ParseSize(c.Scan.MaxFileSize)
		switch {
		case err != nil:
			add("scan.max_file_size: %v", err)
		case n <= 0:
			add("scan.max_file_size: must be greater than zero (got %q)", c.Scan.MaxFileSize)
		}
	}

	// Projects.
	// Ids are compared exactly everywhere else (Project, AddProject,
	// RemoveProject), so an id with surrounding whitespace from a hand-edited
	// file is rejected outright and duplicates are detected on the trimmed
	// form so that "a" and " a" can never coexist as distinct projects.
	seen := make(map[string]int, len(c.Projects))
	for i, p := range c.Projects {
		id := strings.TrimSpace(p.ID)
		switch {
		case id == "":
			add("projects[%d]: id is required", i)
		case id != p.ID:
			add("projects[%d]: id %q must not contain leading or trailing whitespace", i, p.ID)
		}
		if id != "" {
			if j, dup := seen[id]; dup {
				add("projects[%d]: duplicate project id %q (also projects[%d])", i, p.ID, j)
			} else {
				seen[id] = i
			}
		}
		switch {
		case strings.TrimSpace(p.Path) == "":
			add("projects[%d] (%s): path is required", i, projectLabel(p))
		case !isStablePath(p.Path):
			add("projects[%d] (%s): path %q must be absolute or start with ~/ (a relative path would depend on the working directory)", i, projectLabel(p), p.Path)
		default:
			if _, err := paths.ExpandHome(p.Path); err != nil {
				add("projects[%d] (%s): path: %v", i, projectLabel(p), err)
			}
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("invalid config: %w", errors.Join(errs...))
}

// Warnings returns non-fatal validation warnings (e.g. key file inside a project).
// Paths that are empty or not stable (relative, see isStablePath) are skipped:
// Validate already reports them as hard errors, and comparing them after
// expansion against the working directory would yield cwd-dependent warnings.
func (c *Config) Warnings() []string {
	if c == nil {
		return nil
	}
	var out []string
	keyPath := ""
	if isStablePath(c.Key.File.Path) {
		if kp, err := paths.ExpandHome(c.Key.File.Path); err == nil {
			keyPath = kp
		}
	}
	vaultPath := ""
	if isStablePath(c.Vault.Path) {
		if vp, err := paths.ExpandHome(c.Vault.Path); err == nil {
			vaultPath = vp
		}
	}
	// Duplicate names are legal (Validate allows them) but make name-based
	// lookups ambiguous (Project / ProjectPath then refuse to guess), so the
	// user is told to use ids. Names are compared the way Project does,
	// case-insensitively.
	byName := make(map[string]string, len(c.Projects))
	byPath := make(map[string]string, len(c.Projects))
	for _, p := range c.Projects {
		if name := strings.TrimSpace(p.Name); name != "" {
			key := strings.ToLower(name)
			if other, dup := byName[key]; dup {
				out = append(out, fmt.Sprintf("projects %s and %s share the name %q: name lookups are ambiguous, refer to them by id", other, projectLabel(p), p.Name))
			} else {
				byName[key] = projectLabel(p)
			}
		}
		if !isStablePath(p.Path) {
			continue
		}
		pp, err := paths.ExpandHome(p.Path)
		if err != nil {
			continue
		}
		if keyPath != "" && isWithin(pp, keyPath) {
			out = append(out, fmt.Sprintf("key file %s is inside project %s (%s): it is excluded from scans, but keep it out of the project directory", c.Key.File.Path, projectLabel(p), p.Path))
		}
		if vaultPath != "" && isWithin(vaultPath, pp) {
			out = append(out, fmt.Sprintf("project %s path %s is inside the vault directory %s", projectLabel(p), p.Path, c.Vault.Path))
		}
		// Same folding as isWithin: on a case-insensitive filesystem
		// ~/Work/myapp and ~/work/MyApp are one directory.
		key := pp
		if caseInsensitivePaths {
			key = strings.ToLower(pp)
		}
		if other, dup := byPath[key]; dup {
			out = append(out, fmt.Sprintf("projects %s and %s map to the same path %s", other, projectLabel(p), p.Path))
		} else {
			byPath[key] = projectLabel(p)
		}
	}
	return out
}

// errNilConfig is returned by accessors called on a nil *Config (e.g. the nil
// Config that app.Load hands back alongside ErrNoConfig).
var errNilConfig = errors.New("config: nil config")

// VaultPath returns the expanded absolute vault directory.
func (c *Config) VaultPath() (string, error) {
	if c == nil {
		return "", errNilConfig
	}
	return paths.ExpandHome(c.Vault.Path)
}

// KeyFilePath returns the expanded key file path (empty when unset).
func (c *Config) KeyFilePath() (string, error) {
	if c == nil {
		return "", errNilConfig
	}
	if c.Key.File.Path == "" {
		return "", nil
	}
	return paths.ExpandHome(c.Key.File.Path)
}

// MaxFileSize parses scan.max_file_size (DefaultMaxFileSize when empty).
func (c *Config) MaxFileSize() (int64, error) {
	if c == nil {
		return 0, errNilConfig
	}
	if c.Scan.MaxFileSize == "" {
		return DefaultMaxFileSize, nil
	}
	return ParseSize(c.Scan.MaxFileSize)
}

var sizeRe = regexp.MustCompile(`^([0-9]*\.?[0-9]+)\s*([A-Za-z]*)$`)

// sizeUnits maps a lower-cased unit suffix to its multiplier. Decimal units
// (kb, mb, gb, tb) are powers of 1000; binary units (kib, mib, gib, tib) are
// powers of 1024. Bare letters ("2M", "2k") are deliberately NOT accepted:
// tools disagree on whether they mean 1000 or 1024, so a hand-edited config
// would get a silently different limit. ParseSize reports them as an unknown
// unit and lists the accepted spellings.
var sizeUnits = map[string]int64{
	"":    1,
	"b":   1,
	"kb":  1000,
	"kib": 1 << 10,
	"mb":  1000 * 1000,
	"mib": 1 << 20,
	"gb":  1000 * 1000 * 1000,
	"gib": 1 << 30,
	"tb":  1000 * 1000 * 1000 * 1000,
	"tib": 1 << 40,
}

// ParseSize parses "512", "2MiB", "1.5MB", "300KiB", "1GiB" (case-insensitive).
// Decimal units (KB, MB, GB, TB) are powers of 1000, binary units (KiB, MiB,
// GiB, TiB) powers of 1024; whitespace between number and unit is allowed.
// Ambiguous bare-letter units ("2M") are rejected, see sizeUnits.
func ParseSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, errors.New("empty size")
	}
	m := sizeRe.FindStringSubmatch(t)
	if m == nil {
		return 0, fmt.Errorf("invalid size %q (want e.g. 512, 300KiB, 2MiB, 1.5MB)", s)
	}
	numStr, unit := m[1], strings.ToLower(m[2])
	mult, ok := sizeUnits[unit]
	if !ok {
		return 0, fmt.Errorf("invalid size %q: unknown unit %q (want B, KB, KiB, MB, MiB, GB, GiB, TB or TiB)", s, m[2])
	}
	// The arithmetic is exact (big.Rat), never floating point: a float64 has
	// only 53 bits of mantissa, so past a few GB it cannot tell "16.001GB"
	// (exactly 16001000000 bytes) from a genuine fraction, and past 512TiB
	// it cannot see a fraction at all. sizeRe has already restricted numStr
	// to plain decimal digits with at most one dot, so SetString cannot be
	// handed exponents, hex or "a/b" forms here.
	r, ok := new(big.Rat).SetString(numStr)
	if !ok {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	r.Mul(r, new(big.Rat).SetInt64(mult))
	// A fractional value must land on a whole number of bytes whatever the
	// unit: "1.5" (1.5 bytes) and "1.0001KB" (1000.1 bytes) are both rejected
	// rather than silently rounded.
	if !r.IsInt() {
		return 0, fmt.Errorf("invalid size %q: fractional bytes", s)
	}
	if !r.Num().IsInt64() {
		return 0, fmt.Errorf("invalid size %q: too large", s)
	}
	return r.Num().Int64(), nil
}

// FormatSize renders n in the largest binary unit that divides it exactly
// (e.g. 2097152 -> "2MiB", 1536 -> "1536"), the inverse of ParseSize.
func FormatSize(n int64) string {
	if n < 0 {
		return strconv.FormatInt(n, 10)
	}
	for _, u := range []struct {
		name string
		mult int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if n != 0 && n%u.mult == 0 {
			return strconv.FormatInt(n/u.mult, 10) + u.name
		}
	}
	return strconv.FormatInt(n, 10)
}

// ErrUnknownProject is wrapped by ProjectPath when no project has the given
// id or name.
var ErrUnknownProject = errors.New("unknown project")

// ErrAmbiguousProject is wrapped by ProjectPath when a name matches more than
// one project (Validate allows duplicate names; ids are the only identity).
var ErrAmbiguousProject = errors.New("ambiguous project name")

// Project finds a project by id or (case-insensitive) name. An exact id match
// always wins. A name shared by several projects is ambiguous and reports
// (nil, false) rather than silently picking one; use the id in that case
// (ProjectPath distinguishes the two outcomes via ErrAmbiguousProject). The
// returned pointer aliases the slice element so callers may edit it in place.
func (c *Config) Project(idOrName string) (*ProjectConfig, bool) {
	p, n := c.lookupProject(idOrName)
	if n != 1 {
		return nil, false
	}
	return p, true
}

// lookupProject returns the first project matching idOrName and how many
// matched: 1 for an exact id match (ids are unique after Validate) or a
// unique name, >1 for a name shared by several projects, 0 for none.
func (c *Config) lookupProject(idOrName string) (*ProjectConfig, int) {
	if c == nil || idOrName == "" {
		return nil, 0
	}
	for i := range c.Projects {
		if c.Projects[i].ID == idOrName {
			return &c.Projects[i], 1
		}
	}
	var first *ProjectConfig
	n := 0
	for i := range c.Projects {
		if c.Projects[i].Name != "" && strings.EqualFold(c.Projects[i].Name, idOrName) {
			if first == nil {
				first = &c.Projects[i]
			}
			n++
		}
	}
	return first, n
}

// ProjectPath returns the expanded local path of a project, looked up by id
// or unique name (see Project). The error wraps ErrUnknownProject or
// ErrAmbiguousProject so callers can tell the two apart.
func (c *Config) ProjectPath(id string) (string, error) {
	if c == nil {
		return "", errNilConfig
	}
	p, n := c.lookupProject(id)
	switch {
	case n == 0:
		return "", fmt.Errorf("%w %s", ErrUnknownProject, id)
	case n > 1:
		return "", fmt.Errorf("%w %q matches %d projects; use the project id", ErrAmbiguousProject, id, n)
	}
	return paths.ExpandHome(p.Path)
}

// AddProject appends or replaces (by id) a project mapping. Callers must
// supply a non-empty id: a project whose id is empty or whitespace-only is
// ignored (nothing is added or replaced), because the vault project id is the
// only identity and an empty id would otherwise let a later call silently
// overwrite an earlier one. Validate reports the missing id for anything that
// still reaches it.
func (c *Config) AddProject(p ProjectConfig) {
	if c == nil || strings.TrimSpace(p.ID) == "" {
		return
	}
	for i := range c.Projects {
		if c.Projects[i].ID == p.ID {
			c.Projects[i] = p
			return
		}
	}
	c.Projects = append(c.Projects, p)
}

// RemoveProject removes a mapping by id; reports whether it existed.
func (c *Config) RemoveProject(id string) bool {
	if c == nil {
		return false
	}
	for i := range c.Projects {
		if c.Projects[i].ID == id {
			c.Projects = append(c.Projects[:i], c.Projects[i+1:]...)
			return true
		}
	}
	return false
}

// Example returns the annotated example configuration from the design spec,
// suitable for `config show --example` and documentation.
func Example() string { return exampleYAML }

const exampleYAML = `version: 1
machine:
  name: macbook-pro               # human label only (the id lives in the state dir)
vault:
  path: ~/.local/share/private-sync/vault   # local vault directory (git clone when remote is git)
  remote:
    type: git                     # none | git | rclone
    git:
      url: git@github.com:me/private-sync-vault.git
      branch: main
    rclone:
      remote: gdrive              # rclone remote name (e.g. Google Drive)
      path: private-sync-vault    # path inside the remote
key:
  source: bitwarden               # prompt | file | bitwarden
  file:
    path: ~/.config/private-sync/key     # passphrase file, must be 0600 and owned by the user
  bitwarden:
    item: private-sync vault      # item name or id
    field: password               # password | notes | <custom field name>
scan:
  # include: [".env", ".env.*", "*.pem"]   # optional: a non-empty list replaces the built-in defaults
  # exclude_dirs: [".git", "node_modules"]
  # exclude_files: ["package-lock.json"]
  max_file_size: 2MiB             # plain bytes, KB/MB/GB (x1000) or KiB/MiB/GiB (x1024); "2M" is rejected as ambiguous
projects:
  - id: 3f2a9c1e5b7d0a46          # vault project id (opaque)
    name: myapp                   # informational, written by the program, never read for identity
    path: ~/Work/myapp            # local path on THIS machine
`

// isStablePath reports whether p resolves to the same directory no matter
// where private-sync is launched from: it is absolute, or it is "~" / starts
// with "~/" (which paths.ExpandHome resolves against the home directory).
// Anything else ("vault", "./vault", "~bob/vault") is resolved by
// paths.ExpandHome against the current working directory, so the same config
// would point at different places from a shell and from cron.
func isStablePath(p string) bool {
	return p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) || filepath.IsAbs(p)
}

// caseInsensitivePaths reports whether the platform's default filesystem
// ignores case (macOS APFS/HFS+ and Windows NTFS). On such systems
// "~/Vault/key" and "~/vault" name the same directory, so containment checks
// must fold case or a key file could slip into the vault unnoticed. The rare
// case-sensitive volume on these platforms gets a false positive at worst,
// which is the safer failure for a check that guards the passphrase file.
var caseInsensitivePaths = runtime.GOOS == "darwin" || runtime.GOOS == "windows"

// isWithin reports whether child equals dir or lies beneath it (both absolute,
// cleaned). On platforms whose filesystems are case-insensitive by default the
// comparison folds case (see caseInsensitivePaths); elsewhere it is byte-wise.
func isWithin(dir, child string) bool {
	if caseInsensitivePaths {
		dir, child = strings.ToLower(dir), strings.ToLower(child)
	}
	rel, err := filepath.Rel(dir, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func projectLabel(p ProjectConfig) string {
	switch {
	case p.Name != "" && p.ID != "":
		return p.Name + " [" + p.ID + "]"
	case p.Name != "":
		return p.Name
	case p.ID != "":
		return p.ID
	}
	return "<unnamed>"
}

func defaultMachineName() string { return "machine-" + time.Now().Format("0102") }
