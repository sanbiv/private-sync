package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/sanbiv/private-sync/internal/paths"
)

// setHome isolates ~ expansion in a temp directory and returns it.
func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	}
	return home
}

func testDirs(home string) paths.Dirs {
	return paths.Dirs{
		Config: filepath.Join(home, ".config", "private-sync"),
		State:  filepath.Join(home, ".local", "state", "private-sync"),
	}
}

// abs turns a rooted test path such as "/srv/other" into a path that
// isStablePath accepts on every platform. On Unix it is returned cleaned; on
// Windows a volume-less rooted path is drive-relative (filepath.IsAbs is false),
// so filepath.Abs prepends the current drive exactly like paths.ExpandHome
// does. It never fails on the fixtures used here; a failure would mean the
// working directory is unreadable, which every test would report anyway.
func abs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return a
}

// valid returns a fully populated, valid config rooted under home.
func valid() *Config {
	return &Config{
		Version: CurrentVersion,
		Machine: MachineConfig{Name: "macbook-pro"},
		Vault: VaultConfig{
			Path: "~/.local/share/private-sync/vault",
			Remote: RemoteConfig{
				Type:   RemoteGit,
				Git:    GitRemote{URL: "git@github.com:me/vault.git", Branch: "main"},
				Rclone: RcloneRemote{Remote: "gdrive", Path: "private-sync-vault"},
			},
		},
		Key: KeyConfig{
			Source:    KeyBitwarden,
			File:      FileKey{Path: "~/.config/private-sync/key"},
			Bitwarden: BitwardenKey{Item: "private-sync vault", Field: "password"},
		},
		Scan: ScanConfig{
			Include:      []string{".env", "*.pem"},
			ExcludeDirs:  []string{".git"},
			ExcludeFiles: []string{"package-lock.json"},
			MaxFileSize:  "2MiB",
		},
		Projects: []ProjectConfig{
			{ID: "3f2a9c1e5b7d0a46", Name: "myapp", Path: "~/Work/myapp"},
			{ID: "aaaa000011112222", Name: "Other", Path: abs("/srv/other")},
		},
	}
}

func TestDefault(t *testing.T) {
	home := setHome(t)
	t.Setenv("XDG_DATA_HOME", "")
	c := Default(testDirs(home))
	if c.Version != CurrentVersion {
		t.Errorf("Version = %d, want %d", c.Version, CurrentVersion)
	}
	if !strings.HasPrefix(c.Machine.Name, "machine-") {
		t.Errorf("Machine.Name = %q, want machine-* prefix", c.Machine.Name)
	}
	if c.Vault.Remote.Type != RemoteNone {
		t.Errorf("remote type = %q, want none", c.Vault.Remote.Type)
	}
	if c.Vault.Remote.Git.Branch != "main" {
		t.Errorf("git branch = %q, want main", c.Vault.Remote.Git.Branch)
	}
	if c.Key.Source != KeyPrompt {
		t.Errorf("key source = %q, want prompt", c.Key.Source)
	}
	if c.Key.Bitwarden.Field != "password" {
		t.Errorf("bitwarden field = %q, want password", c.Key.Bitwarden.Field)
	}
	if c.Vault.Path != "~/.local/share/private-sync/vault" {
		t.Errorf("vault path = %q, want contracted default", c.Vault.Path)
	}
	if c.Key.File.Path != "~/.config/private-sync/key" {
		t.Errorf("key file path = %q, want contracted default", c.Key.File.Path)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Default config must validate: %v", err)
	}
	if w := c.Warnings(); len(w) != 0 {
		t.Errorf("Default config warnings = %v, want none", w)
	}
	n, err := c.MaxFileSize()
	if err != nil || n != DefaultMaxFileSize {
		t.Errorf("MaxFileSize = %d, %v; want %d", n, err, DefaultMaxFileSize)
	}
	vp, err := c.VaultPath()
	if err != nil || !filepath.IsAbs(vp) || !strings.HasPrefix(vp, home) {
		t.Errorf("VaultPath = %q, %v; want absolute under %s", vp, err, home)
	}
	kp, err := c.KeyFilePath()
	if err != nil || kp != filepath.Join(home, ".config", "private-sync", "key") {
		t.Errorf("KeyFilePath = %q, %v", kp, err)
	}
}

func TestLoadNotFound(t *testing.T) {
	setHome(t)
	_, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load missing: err = %v, want ErrNotFound", err)
	}
	if _, err := Load(""); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Load empty path: err = %v, want a non-ErrNotFound error", err)
	}
}

func TestLoadDirectory(t *testing.T) {
	setHome(t)
	_, err := Load(t.TempDir())
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Load directory: err = %v, want read error", err)
	}
}

func TestLoadParsesSpecExample(t *testing.T) {
	setHome(t)
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(Example()), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load(Example): %v", err)
	}
	if c.Version != 1 || c.Machine.Name != "macbook-pro" {
		t.Errorf("header = %+v", c.Machine)
	}
	if c.Vault.Path != "~/.local/share/private-sync/vault" {
		t.Errorf("vault path not kept typed: %q", c.Vault.Path)
	}
	if c.Vault.Remote.Type != RemoteGit || c.Vault.Remote.Git.URL != "git@github.com:me/private-sync-vault.git" || c.Vault.Remote.Git.Branch != "main" {
		t.Errorf("git remote = %+v", c.Vault.Remote.Git)
	}
	if c.Vault.Remote.Rclone != (RcloneRemote{Remote: "gdrive", Path: "private-sync-vault"}) {
		t.Errorf("rclone remote = %+v", c.Vault.Remote.Rclone)
	}
	if c.Key.Source != KeyBitwarden || c.Key.Bitwarden != (BitwardenKey{Item: "private-sync vault", Field: "password"}) {
		t.Errorf("key = %+v", c.Key)
	}
	if c.Key.File.Path != "~/.config/private-sync/key" {
		t.Errorf("key file = %q", c.Key.File.Path)
	}
	if c.Scan.MaxFileSize != "2MiB" {
		t.Errorf("max_file_size = %q", c.Scan.MaxFileSize)
	}
	if len(c.Projects) != 1 || c.Projects[0] != (ProjectConfig{ID: "3f2a9c1e5b7d0a46", Name: "myapp", Path: "~/Work/myapp"}) {
		t.Errorf("projects = %+v", c.Projects)
	}
}

func TestLoadVersions(t *testing.T) {
	setHome(t)
	tests := []struct {
		name    string
		yaml    string
		wantVer int
		wantErr string
	}{
		{name: "missing version treated as 1", yaml: "vault:\n  path: ~/v\n", wantVer: 1},
		{name: "explicit zero treated as 1", yaml: "version: 0\nvault:\n  path: ~/v\n", wantVer: 1},
		{name: "current", yaml: "version: 1\nvault:\n  path: ~/v\n", wantVer: 1},
		{name: "newer refused", yaml: "version: 2\nvault:\n  path: ~/v\n", wantErr: "newer"},
		{name: "negative refused", yaml: "version: -1\nvault:\n  path: ~/v\n", wantErr: "version"},
		{name: "non-integer version", yaml: "version: abc\nvault:\n  path: ~/v\n", wantErr: "parse yaml"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(p)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.Version != tc.wantVer {
				t.Errorf("Version = %d, want %d", c.Version, tc.wantVer)
			}
		})
	}
}

func TestLoadRejectsBadInput(t *testing.T) {
	setHome(t)
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"empty file", "", "empty"},
		{"only comments", "# nothing here\n", "empty"},
		{"malformed yaml", "vault: [\n", "parse yaml"},
		{"unknown key", "vault:\n  path: ~/v\nscan:\n  max_filesize: 2MiB\n", "max_filesize"},
		{"wrong type", "vault:\n  path: ~/v\nprojects: notalist\n", "parse yaml"},
		{"invalid content", "vault:\n  path: ~/v\n  remote:\n    type: ftp\n", "unknown type"},
		// A second document must be reported as such, not as a misleading
		// "vault.path: required" from decoding only the first one.
		{"multi-document", "version: 1\n---\nvault:\n  path: ~/v\n", "single YAML document"},
		{"multi-document trailing empty doc", "vault:\n  path: ~/v\n---\n---\n", "single YAML document"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(p)
			if err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	setHome(t)
	src := "vault:\n  path: ~/v\n  remote:\n    type: git\n    git:\n      url: git@example.com:v.git\nkey:\n  source: bitwarden\n  bitwarden:\n    item: it\n"
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Vault.Remote.Git.Branch != "main" {
		t.Errorf("branch = %q, want main", c.Vault.Remote.Git.Branch)
	}
	if c.Key.Bitwarden.Field != "password" {
		t.Errorf("field = %q, want password", c.Key.Bitwarden.Field)
	}
	if n, err := c.MaxFileSize(); err != nil || n != DefaultMaxFileSize {
		t.Errorf("MaxFileSize = %d, %v", n, err)
	}
	if len(c.Scan.Include) != 0 || len(c.Scan.ExcludeDirs) != 0 || len(c.Scan.ExcludeFiles) != 0 {
		t.Errorf("scan lists should stay empty (= defaults): %+v", c.Scan)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	home := setHome(t)
	want := valid()
	dir := filepath.Join(home, ".config", "private-sync")
	p := filepath.Join(dir, "config.yaml")

	if err := want.Save(p); err != nil {
		t.Fatalf("Save: %v", err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if st.Mode().Perm() != FileMode {
			t.Errorf("file mode = %o, want %o (FileMode)", st.Mode().Perm(), FileMode)
		}
		dst, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if dst.Mode().Perm() != DirMode {
			t.Errorf("dir mode = %o, want %o (DirMode)", dst.Mode().Perm(), DirMode)
		}
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "path: ~/Work/myapp") || !strings.Contains(string(raw), "path: ~/.local/share/private-sync/vault") {
		t.Errorf("Save must keep ~ untouched, got:\n%s", raw)
	}
	if strings.Contains(string(raw), home) {
		t.Errorf("Save must not expand the home directory, got:\n%s", raw)
	}
	if !strings.HasPrefix(string(raw), "#") {
		t.Errorf("Save output should start with a comment header, got:\n%s", raw)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".psv-tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}

	got, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !equalConfig(got, want) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}

	// Overwrite in place: the second Save replaces the first atomically.
	got.Machine.Name = "other"
	if err := got.Save(p); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	again, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if again.Machine.Name != "other" {
		t.Errorf("after overwrite Machine.Name = %q, want other", again.Machine.Name)
	}
}

// The exported mode constants are the spec'd values (0600 file, 0700 dir) and
// Save must create the whole missing parent chain with DirMode, not with
// whatever a helper happens to hard-code.
func TestSaveModes(t *testing.T) {
	if FileMode != 0o600 {
		t.Errorf("FileMode = %o, want 0600", FileMode)
	}
	if DirMode != 0o700 {
		t.Errorf("DirMode = %o, want 0700", DirMode)
	}
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not honoured on Windows")
	}
	setHome(t)
	root := t.TempDir()
	dir := filepath.Join(root, "nested", "private-sync")
	if err := valid().Save(filepath.Join(dir, "config.yaml")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	for _, d := range []string{filepath.Join(root, "nested"), dir} {
		st, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != DirMode {
			t.Errorf("%s mode = %o, want %o (DirMode)", d, st.Mode().Perm(), DirMode)
		}
	}
	st, err := os.Stat(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != FileMode {
		t.Errorf("file mode = %o, want %o (FileMode)", st.Mode().Perm(), FileMode)
	}
}

func TestSaveDefaultRoundTrip(t *testing.T) {
	home := setHome(t)
	t.Setenv("XDG_DATA_HOME", "")
	d := testDirs(home)
	c := Default(d)
	c.Machine.Name = "box"
	if err := c.Save(d.ConfigFile()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(d.ConfigFile())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !equalConfig(got, c) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, c)
	}
}

func TestSaveErrors(t *testing.T) {
	setHome(t)
	var nilCfg *Config
	if err := nilCfg.Save(filepath.Join(t.TempDir(), "c.yaml")); err == nil {
		t.Error("Save on nil config should fail")
	}
	if err := valid().Save(""); err == nil {
		t.Error("Save with empty path should fail")
	}
	// Parent path is a file, so the directory cannot be created.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := valid().Save(filepath.Join(blocker, "config.yaml")); err == nil {
		t.Error("Save under a file should fail")
	}
}

// Save must refuse a config that Load would reject, and leave nothing behind.
func TestSaveRejectsInvalidConfig(t *testing.T) {
	setHome(t)
	tests := []struct {
		name string
		mut  func(c *Config)
		want string
	}{
		{"unknown remote type", func(c *Config) { c.Vault.Remote.Type = "ftp" }, "unknown type"},
		{"empty vault path", func(c *Config) { c.Vault.Path = "" }, "vault.path"},
		{"key file inside vault", func(c *Config) { c.Key.File.Path = c.Vault.Path + "/key" }, "inside vault.path"},
		{"duplicate project id", func(c *Config) { c.Projects = append(c.Projects, c.Projects[0]) }, "duplicate project id"},
		{"bad max_file_size", func(c *Config) { c.Scan.MaxFileSize = "lots" }, "scan.max_file_size"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "cfg")
			p := filepath.Join(dir, "config.yaml")
			c := valid()
			tc.mut(c)
			err := c.Save(p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Save err = %v, want containing %q", err, tc.want)
			}
			if _, statErr := os.Stat(p); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("Save of an invalid config must not create %s (stat err = %v)", p, statErr)
			}
			if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("Save of an invalid config must not create the directory %s (stat err = %v)", dir, statErr)
			}
		})
	}
	// Save also fills type-dependent defaults, exactly like Load does.
	c := valid()
	c.Vault.Remote.Git.Branch = ""
	c.Key.Bitwarden.Field = ""
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := c.Save(p); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if c.Vault.Remote.Git.Branch != DefaultBranch || c.Key.Bitwarden.Field != DefaultBitwardenField {
		t.Errorf("Save should fill defaults in place: branch=%q field=%q", c.Vault.Remote.Git.Branch, c.Key.Bitwarden.Field)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "branch: main") || !strings.Contains(string(raw), "field: password") {
		t.Errorf("saved file should contain the filled defaults, got:\n%s", raw)
	}
}

func TestMarshalOmitsEmptyOptionalSections(t *testing.T) {
	setHome(t)
	c := &Config{Vault: VaultConfig{Path: "~/v", Remote: RemoteConfig{Type: RemoteNone}}, Key: KeyConfig{Source: KeyPrompt}}
	data, err := c.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, absent := range []string{"git:", "rclone:", "bitwarden:", "include:", "max_file_size:"} {
		if strings.Contains(s, absent) {
			t.Errorf("Marshal should omit empty %q, got:\n%s", absent, s)
		}
	}
	if !strings.Contains(s, "version: 1") {
		t.Errorf("Marshal should default version to 1, got:\n%s", s)
	}
	// Output must decode back into a valid config.
	if _, err := Parse(data); err != nil {
		t.Errorf("Parse(Marshal()) failed: %v\n%s", err, s)
	}
}

func TestValidate(t *testing.T) {
	setHome(t)
	tests := []struct {
		name string
		mut  func(c *Config)
		want string // substring of the error, "" = valid
	}{
		{"valid", func(c *Config) {}, ""},
		{"nil-safe empty projects", func(c *Config) { c.Projects = nil }, ""},
		{"empty vault path", func(c *Config) { c.Vault.Path = "" }, "vault.path"},
		{"blank vault path", func(c *Config) { c.Vault.Path = "   " }, "vault.path"},
		{"relative vault path", func(c *Config) { c.Vault.Path = "vault" }, "vault.path: \"vault\" must be absolute or start with ~/"},
		{"dot-relative vault path", func(c *Config) { c.Vault.Path = "./vault" }, "vault.path"},
		{"parent-relative vault path", func(c *Config) { c.Vault.Path = "../vault" }, "vault.path"},
		{"tilde-user vault path is not expanded", func(c *Config) { c.Vault.Path = "~bob/vault" }, "vault.path"},
		{"bare tilde vault path", func(c *Config) {
			c.Vault.Path = "~"
			c.Key.File.Path = abs("/etc/private-sync/key") // otherwise it sits inside the vault
		}, ""},
		{"absolute vault path", func(c *Config) { c.Vault.Path = abs("/srv/vault") }, ""},
		{"relative key file path", func(c *Config) { c.Key.File.Path = "key" }, "key.file.path: \"key\" must be absolute or start with ~/"},
		{"relative key file path with prompt source", func(c *Config) {
			c.Key.Source = KeyPrompt
			c.Key.File.Path = "./key"
		}, "key.file.path"},
		{"absolute key file path", func(c *Config) { c.Key.File.Path = abs("/etc/private-sync/key") }, ""},
		{"relative project path", func(c *Config) { c.Projects[0].Path = "Work/myapp" }, "projects[0] (myapp [3f2a9c1e5b7d0a46]): path \"Work/myapp\" must be absolute or start with ~/"},
		{"dot-relative project path", func(c *Config) { c.Projects[1].Path = "./other" }, "projects[1]"},
		{"absolute project path", func(c *Config) { c.Projects[0].Path = abs("/opt/myapp") }, ""},
		{"remote none ok without details", func(c *Config) {
			c.Vault.Remote = RemoteConfig{Type: RemoteNone}
		}, ""},
		{"empty remote type = none", func(c *Config) { c.Vault.Remote = RemoteConfig{} }, ""},
		{"unknown remote type", func(c *Config) { c.Vault.Remote.Type = "ftp" }, "unknown type \"ftp\""},
		{"remote type case-sensitive", func(c *Config) { c.Vault.Remote.Type = "Git" }, "unknown type"},
		{"git without url", func(c *Config) { c.Vault.Remote.Git.URL = "" }, "vault.remote.git.url"},
		{"git branch defaults", func(c *Config) { c.Vault.Remote.Git.Branch = "" }, ""},
		{"rclone ok", func(c *Config) { c.Vault.Remote.Type = RemoteRclone }, ""},
		{"rclone without remote", func(c *Config) {
			c.Vault.Remote.Type = RemoteRclone
			c.Vault.Remote.Rclone.Remote = ""
		}, "vault.remote.rclone.remote"},
		{"rclone without path", func(c *Config) {
			c.Vault.Remote.Type = RemoteRclone
			c.Vault.Remote.Rclone.Path = ""
		}, "vault.remote.rclone.path"},
		{"none ignores missing git url", func(c *Config) {
			c.Vault.Remote.Type = RemoteNone
			c.Vault.Remote.Git.URL = ""
		}, ""},
		{"empty key source = prompt", func(c *Config) { c.Key.Source = "" }, ""},
		{"unknown key source", func(c *Config) { c.Key.Source = "keychain" }, "unknown source \"keychain\""},
		{"prompt ok", func(c *Config) { c.Key = KeyConfig{Source: KeyPrompt} }, ""},
		{"file ok", func(c *Config) { c.Key.Source = KeyFile }, ""},
		{"file without path", func(c *Config) {
			c.Key.Source = KeyFile
			c.Key.File.Path = ""
		}, "key.file.path"},
		{"bitwarden without item", func(c *Config) { c.Key.Bitwarden.Item = "" }, "key.bitwarden.item"},
		{"bitwarden field defaults", func(c *Config) { c.Key.Bitwarden.Field = "" }, ""},
		{"bitwarden custom field", func(c *Config) { c.Key.Bitwarden.Field = "my field" }, ""},
		{"key file inside vault", func(c *Config) {
			c.Key.File.Path = "~/.local/share/private-sync/vault/key"
		}, "inside vault.path"},
		{"key file inside vault via unclean path", func(c *Config) {
			c.Key.File.Path = "~/.local/share/private-sync/vault/../vault/sub/key"
		}, "inside vault.path"},
		{"key file inside vault even with prompt source", func(c *Config) {
			c.Key.Source = KeyPrompt
			c.Key.File.Path = "~/.local/share/private-sync/vault/key"
		}, "inside vault.path"},
		{"key file equals vault path", func(c *Config) {
			c.Key.File.Path = c.Vault.Path
		}, "inside vault.path"},
		{"key file next to vault is fine", func(c *Config) {
			c.Key.File.Path = "~/.local/share/private-sync/vault-key"
		}, ""},
		{"key file is a parent of the vault", func(c *Config) {
			c.Key.File.Path = "~/.local/share"
		}, ""},
		{"max_file_size unparsable", func(c *Config) { c.Scan.MaxFileSize = "two megs" }, "scan.max_file_size"},
		{"max_file_size zero", func(c *Config) { c.Scan.MaxFileSize = "0" }, "greater than zero"},
		{"max_file_size decimal unit", func(c *Config) { c.Scan.MaxFileSize = "1.5MB" }, ""},
		{"max_file_size empty = default", func(c *Config) { c.Scan.MaxFileSize = "" }, ""},
		{"duplicate project id", func(c *Config) {
			c.Projects = append(c.Projects, ProjectConfig{ID: c.Projects[0].ID, Path: "~/elsewhere"})
		}, "duplicate project id"},
		{"empty project id", func(c *Config) { c.Projects[0].ID = "" }, "projects[0]: id is required"},
		{"whitespace-only project id", func(c *Config) { c.Projects[0].ID = "  " }, "projects[0]: id is required"},
		{"project id with leading whitespace", func(c *Config) { c.Projects[0].ID = " " + c.Projects[0].ID }, "leading or trailing whitespace"},
		{"project id with trailing whitespace", func(c *Config) { c.Projects[0].ID = c.Projects[0].ID + "\t" }, "leading or trailing whitespace"},
		{"duplicate id differing only by whitespace", func(c *Config) {
			c.Projects = append(c.Projects, ProjectConfig{ID: " " + c.Projects[0].ID, Path: "~/elsewhere"})
		}, "duplicate project id"},
		{"inner whitespace in id is not trimmed", func(c *Config) { c.Projects[0].ID = "my project" }, ""},
		{"empty project path", func(c *Config) { c.Projects[1].Path = "" }, "projects[1] (Other [aaaa000011112222]): path is required"},
		{"blank project path", func(c *Config) { c.Projects[1].Path = " " }, "path is required"},
		{"duplicate names are allowed", func(c *Config) { c.Projects[1].Name = c.Projects[0].Name }, ""},
		{"multiple errors joined", func(c *Config) {
			c.Vault.Remote.Type = "ftp"
			c.Key.Source = "keychain"
		}, "unknown source"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			tc.mut(c)
			err := c.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
			if !strings.HasPrefix(err.Error(), "invalid config:") {
				t.Errorf("error should be prefixed with 'invalid config:', got %q", err)
			}
		})
	}
}

// A relative key file (or vault) path is reported once, as a relative-path
// error, and must never also trigger the inside-vault check: that check would
// expand the relative path against the working directory, so running the
// command from inside the vault would add a second, misleading error.
func TestValidateRelativePathsNotResolvedAgainstCwd(t *testing.T) {
	home := setHome(t)
	vault := filepath.Join(home, ".local", "share", "private-sync", "vault")
	if err := os.MkdirAll(vault, 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		cwd  string
		mut  func(c *Config)
		want string // the one expected error key
	}{
		{"relative key file, cwd inside vault", vault, func(c *Config) { c.Key.File.Path = "key" }, "key.file.path"},
		{"relative key file, cwd is vault", vault, func(c *Config) { c.Key.File.Path = "." }, "key.file.path"},
		{"relative vault, cwd is parent of the key file", filepath.Dir(vault), func(c *Config) {
			c.Vault.Path = "."
			c.Key.File.Path = filepath.Join(vault, "key")
		}, "vault.path"},
		{"both relative", vault, func(c *Config) {
			c.Vault.Path = "vault"
			c.Key.File.Path = "vault/key"
		}, "vault.path"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(tc.cwd)
			c := valid()
			tc.mut(c)
			err := c.Validate()
			if err == nil {
				t.Fatal("expected an error")
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.want) || !strings.Contains(msg, "must be absolute or start with ~/") {
				t.Errorf("err = %v, want a relative-path error for %s", err, tc.want)
			}
			if strings.Contains(msg, "inside vault.path") {
				t.Errorf("err = %v, must not contain the cwd-dependent inside-vault error", err)
			}
		})
	}
	// Control: with both paths stable the inside-vault check still fires,
	// regardless of the working directory.
	t.Chdir(home)
	c := valid()
	c.Key.File.Path = "~/.local/share/private-sync/vault/key"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "inside vault.path") {
		t.Errorf("stable paths: err = %v, want inside-vault error", err)
	}
}

func TestValidateMultipleErrorsReported(t *testing.T) {
	setHome(t)
	c := valid()
	c.Vault.Remote.Type = "ftp"
	c.Key.Source = "keychain"
	c.Scan.MaxFileSize = "lots"
	err := c.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"vault.remote.type", "key.source", "scan.max_file_size"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestValidateFillsDefaults(t *testing.T) {
	setHome(t)
	c := valid()
	c.Vault.Remote.Type = ""
	c.Vault.Remote.Git.Branch = ""
	c.Key.Source = ""
	c.Key.Bitwarden.Field = ""
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Vault.Remote.Type != RemoteNone {
		t.Errorf("remote type = %q, want none", c.Vault.Remote.Type)
	}
	if c.Key.Source != KeyPrompt {
		t.Errorf("key source = %q, want prompt", c.Key.Source)
	}
	// Type-dependent defaults are only filled when that type is selected.
	c = valid()
	c.Vault.Remote.Git.Branch = ""
	c.Key.Bitwarden.Field = ""
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Vault.Remote.Git.Branch != "main" || c.Key.Bitwarden.Field != "password" {
		t.Errorf("defaults not filled: branch=%q field=%q", c.Vault.Remote.Git.Branch, c.Key.Bitwarden.Field)
	}
	var nilCfg *Config
	if err := nilCfg.Validate(); err == nil {
		t.Error("Validate on nil should error, not panic")
	}
}

func TestWarnings(t *testing.T) {
	setHome(t)
	tests := []struct {
		name string
		mut  func(c *Config)
		want []string // substrings, one per expected warning
	}{
		{"clean", func(c *Config) {}, nil},
		{"key file inside project", func(c *Config) {
			c.Key.File.Path = "~/Work/myapp/.secrets/key"
		}, []string{"key file ~/Work/myapp/.secrets/key is inside project myapp"}},
		{"key file inside project via absolute path", func(c *Config) {
			c.Key.File.Path = abs("/srv/other/key")
		}, []string{"inside project Other"}},
		{"key file equal to project dir", func(c *Config) {
			c.Key.File.Path = "~/Work/myapp"
		}, []string{"inside project myapp"}},
		{"key file next to project", func(c *Config) {
			c.Key.File.Path = "~/Work/myapp-key"
		}, nil},
		{"project inside vault", func(c *Config) {
			c.Projects[0].Path = "~/.local/share/private-sync/vault/proj"
		}, []string{"inside the vault directory"}},
		{"two projects same path", func(c *Config) {
			c.Projects[1].Path = "~/Work/myapp/"
		}, []string{"map to the same path"}},
		{"empty key file path", func(c *Config) {
			c.Key.File.Path = ""
		}, nil},
		{"unexpandable paths are skipped", func(c *Config) {
			c.Projects[0].Path = ""
			c.Vault.Path = ""
		}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			tc.mut(c)
			got := c.Warnings()
			if len(got) != len(tc.want) {
				t.Fatalf("Warnings = %v, want %d warning(s) %v", got, len(tc.want), tc.want)
			}
			for i, w := range tc.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("warning[%d] = %q, want containing %q", i, got[i], w)
				}
			}
		})
	}
	var nilCfg *Config
	if w := nilCfg.Warnings(); w != nil {
		t.Errorf("nil Warnings = %v", w)
	}
}

// Warnings must not resolve relative paths against the working directory:
// they are hard errors in Validate, and a cwd-dependent warning would be
// misleading (and differ between a shell and cron).
func TestWarningsSkipUnstablePaths(t *testing.T) {
	home := setHome(t)
	proj := filepath.Join(home, "Work", "myapp")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(proj)
	tests := []struct {
		name string
		mut  func(c *Config)
	}{
		{"relative key file inside cwd project", func(c *Config) { c.Key.File.Path = "key" }},
		{"relative project path equal to a stable one", func(c *Config) { c.Projects[1].Path = "." }},
		{"relative vault containing a project", func(c *Config) {
			c.Vault.Path = ".."
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			tc.mut(c)
			if w := c.Warnings(); len(w) != 0 {
				t.Errorf("Warnings = %v, want none for an unstable path", w)
			}
		})
	}
}

// Containment checks fold case on platforms whose default filesystem is
// case-insensitive (macOS, Windows), so a key file at ~/Vault/key is caught
// as living inside a vault at ~/vault; elsewhere the comparison is byte-wise.
func TestIsWithinCase(t *testing.T) {
	sep := string(filepath.Separator)
	root := filepath.Clean(sep + "Home" + sep + "Vault")
	upper := filepath.Join(root, "key")
	lower := filepath.Join(strings.ToLower(root), "key")
	if got := isWithin(root, lower); got != caseInsensitivePaths {
		t.Errorf("isWithin(%q, %q) = %v, want %v on %s", root, lower, got, caseInsensitivePaths, runtime.GOOS)
	}
	if got := isWithin(strings.ToLower(root), upper); got != caseInsensitivePaths {
		t.Errorf("isWithin(%q, %q) = %v, want %v on %s", strings.ToLower(root), upper, got, caseInsensitivePaths, runtime.GOOS)
	}
	// Exact-case containment holds everywhere; a sibling never does.
	if !isWithin(root, upper) {
		t.Errorf("isWithin(%q, %q) = false, want true", root, upper)
	}
	if isWithin(root, root+"2") {
		t.Errorf("isWithin(%q, %q) = true, want false", root, root+"2")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		return
	}
	// End to end: Validate reports the key file inside a differently-cased vault.
	setHome(t)
	c := valid()
	c.Vault.Path = "~/.local/share/private-sync/vault"
	c.Key.File.Path = "~/.local/share/private-sync/VAULT/key"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "inside vault.path") {
		t.Errorf("Validate = %v, want inside-vault error for a case-variant path", err)
	}
	c = valid()
	c.Key.File.Path = "~/WORK/MyApp/key"
	if w := c.Warnings(); len(w) != 1 || !strings.Contains(w[0], "inside project myapp") {
		t.Errorf("Warnings = %v, want key-inside-project warning for a case-variant path", w)
	}
}

func TestParseSize(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"512", 512, false},
		{"0", 0, false},
		{"2MiB", 2 * 1024 * 1024, false},
		{"2mib", 2 * 1024 * 1024, false},
		{"2MIB", 2 * 1024 * 1024, false},
		{"1.5MB", 1_500_000, false},
		{"1.5MiB", 1_572_864, false},
		{"300KiB", 300 * 1024, false},
		{"300KB", 300_000, false},
		{"1GiB", 1 << 30, false},
		{"1GB", 1_000_000_000, false},
		{"1TiB", 1 << 40, false},
		{"1TB", 1_000_000_000_000, false},
		{"2 mb", 2_000_000, false},
		{"2 MiB", 2 * 1024 * 1024, false},
		{"  2MiB  ", 2 * 1024 * 1024, false},
		// Bare letters are ambiguous (1000 vs 1024) and therefore rejected.
		{"2M", 0, true},
		{"2m", 0, true},
		{"2k", 0, true},
		{"2K", 0, true},
		{"1g", 0, true},
		{"1t", 0, true},
		{"2 m", 0, true},
		{"100B", 100, false},
		{"100 b", 100, false},
		{".5KiB", 512, false},
		{"0.5kb", 500, false},
		{"", 0, true},
		{"   ", 0, true},
		{"MiB", 0, true},
		{"-1", 0, true},
		{"-2MiB", 0, true},
		{"1.5", 0, true},
		{"1.5B", 0, true},
		// A fraction that does not land on a whole byte is rejected for every
		// unit, not silently rounded.
		{"1.0001KB", 0, true},
		{"1.0005KiB", 0, true},
		{"0.0000015MB", 0, true},
		{"1.00000001GiB", 0, true},
		// Fractions that do land on whole bytes are fine, including ones whose
		// binary float product carries rounding noise.
		{"0.1GB", 100_000_000, false},
		{"0.001MB", 1000, false},
		{"0.7KB", 700, false},
		{"1.25KiB", 1280, false},
		{"0.3GB", 300_000_000, false},
		{"1.2.3MB", 0, true},
		{"2 M B", 0, true},
		{"2MiBs", 0, true},
		{"2XB", 0, true},
		{"two", 0, true},
		{"2MiB extra", 0, true},
		{"1e3", 0, true},
		{"0x10", 0, true},
		{"99999999999999999999", 0, true},
		{"9223372036854775807", 9223372036854775807, false},
		{"9223372036854775808", 0, true},
		{"8589934592GiB", 0, true},
		{"9e18GiB", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseSize(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseSize(%q) = %d, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSize(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseSize(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestFormatSize(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0"}, {512, "512"}, {1024, "1KiB"}, {1536, "1536"}, {2 * 1024 * 1024, "2MiB"},
		{3 << 30, "3GiB"}, {1 << 40, "1TiB"}, {5 << 40, "5TiB"}, {1024 << 30, "1TiB"},
		{1_500_000, "1500000"}, {-5, "-5"},
	}
	for _, tc := range tests {
		if got := FormatSize(tc.in); got != tc.want {
			t.Errorf("FormatSize(%d) = %q, want %q", tc.in, got, tc.want)
		}
		if tc.in >= 0 {
			back, err := ParseSize(tc.want)
			if err != nil || back != tc.in {
				t.Errorf("ParseSize(FormatSize(%d)) = %d, %v", tc.in, back, err)
			}
		}
	}
}

func TestMaxFileSize(t *testing.T) {
	c := &Config{}
	if n, err := c.MaxFileSize(); err != nil || n != DefaultMaxFileSize {
		t.Errorf("empty: %d, %v", n, err)
	}
	c.Scan.MaxFileSize = "300KiB"
	if n, err := c.MaxFileSize(); err != nil || n != 300*1024 {
		t.Errorf("300KiB: %d, %v", n, err)
	}
	c.Scan.MaxFileSize = "bogus"
	if _, err := c.MaxFileSize(); err == nil {
		t.Error("bogus should error")
	}
}

func TestProjectLookup(t *testing.T) {
	c := valid()
	tests := []struct {
		q      string
		wantID string
		ok     bool
	}{
		{"3f2a9c1e5b7d0a46", "3f2a9c1e5b7d0a46", true},
		{"myapp", "3f2a9c1e5b7d0a46", true},
		{"MYAPP", "3f2a9c1e5b7d0a46", true},
		{"MyApp", "3f2a9c1e5b7d0a46", true},
		{"other", "aaaa000011112222", true},
		{"aaaa000011112222", "aaaa000011112222", true},
		{"AAAA000011112222", "", false}, // ids are exact
		{"nope", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.q, func(t *testing.T) {
			p, ok := c.Project(tc.q)
			if ok != tc.ok {
				t.Fatalf("Project(%q) ok = %v, want %v", tc.q, ok, tc.ok)
			}
			if ok && p.ID != tc.wantID {
				t.Errorf("Project(%q).ID = %q, want %q", tc.q, p.ID, tc.wantID)
			}
		})
	}
	// Id wins over a name that happens to match another project.
	c.AddProject(ProjectConfig{ID: "myapp", Name: "collision", Path: "/x"})
	if p, ok := c.Project("myapp"); !ok || p.ID != "myapp" {
		t.Errorf("id lookup should take precedence over name, got %+v", p)
	}
	// Returned pointer aliases the slice element.
	p, _ := c.Project("other")
	p.Path = "/changed"
	if c.Projects[1].Path != "/changed" {
		t.Error("Project should return a pointer into the slice")
	}
	// Empty-name projects never match an empty query.
	c.Projects = []ProjectConfig{{ID: "x", Path: "/x"}}
	if _, ok := c.Project(""); ok {
		t.Error("empty query must not match a project without a name")
	}
	var nilCfg *Config
	if _, ok := nilCfg.Project("x"); ok {
		t.Error("nil config should not find anything")
	}
}

func TestProjectPath(t *testing.T) {
	home := setHome(t)
	c := valid()
	got, err := c.ProjectPath("myapp")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "Work", "myapp"); got != want {
		t.Errorf("ProjectPath = %q, want %q", got, want)
	}
	// The fixture path is already absolute on every platform (see abs), and
	// ExpandHome must hand it back unchanged.
	wantAbs := abs("/srv/other")
	got, err = c.ProjectPath("aaaa000011112222")
	if err != nil || got != wantAbs {
		t.Errorf("ProjectPath(abs) = %q, %v; want %q", got, err, wantAbs)
	}
	if _, err := c.ProjectPath("missing"); err == nil || !strings.Contains(err.Error(), "unknown project missing") {
		t.Errorf("ProjectPath(missing) err = %v", err)
	}
	c.Projects[0].Path = ""
	if _, err := c.ProjectPath("myapp"); err == nil {
		t.Error("empty project path should error")
	}
}

func TestAddRemoveProject(t *testing.T) {
	c := &Config{}
	c.AddProject(ProjectConfig{ID: "a", Name: "A", Path: "/a"})
	c.AddProject(ProjectConfig{ID: "b", Name: "B", Path: "/b"})
	if len(c.Projects) != 2 {
		t.Fatalf("len = %d, want 2", len(c.Projects))
	}
	// Replace by id keeps position and updates every field.
	c.AddProject(ProjectConfig{ID: "a", Name: "A2", Path: "/a2"})
	if len(c.Projects) != 2 || c.Projects[0] != (ProjectConfig{ID: "a", Name: "A2", Path: "/a2"}) {
		t.Errorf("replace by id: %+v", c.Projects)
	}
	// Same name, different id → a new entry.
	c.AddProject(ProjectConfig{ID: "c", Name: "A2", Path: "/c"})
	if len(c.Projects) != 3 {
		t.Errorf("same name different id should append: %+v", c.Projects)
	}
	if !c.RemoveProject("a") {
		t.Error("RemoveProject(a) = false, want true")
	}
	if c.RemoveProject("a") {
		t.Error("second RemoveProject(a) = true, want false")
	}
	if c.RemoveProject("zzz") {
		t.Error("RemoveProject(unknown) = true")
	}
	if len(c.Projects) != 2 || c.Projects[0].ID != "b" || c.Projects[1].ID != "c" {
		t.Errorf("after remove: %+v", c.Projects)
	}
	if !c.RemoveProject("c") || !c.RemoveProject("b") || len(c.Projects) != 0 {
		t.Errorf("remove remaining: %+v", c.Projects)
	}
	if c.RemoveProject("b") {
		t.Error("remove from empty = true")
	}
	var nilCfg *Config
	nilCfg.AddProject(ProjectConfig{ID: "x"}) // must not panic
	if nilCfg.RemoveProject("x") {
		t.Error("nil RemoveProject = true")
	}
}

// An empty id must never be added: two empty-id projects would otherwise
// match each other in the replace-by-id loop and the second would silently
// overwrite the first.
func TestAddProjectIgnoresEmptyID(t *testing.T) {
	c := &Config{}
	c.AddProject(ProjectConfig{ID: "", Name: "first", Path: "/first"})
	c.AddProject(ProjectConfig{ID: "   ", Name: "second", Path: "/second"})
	if len(c.Projects) != 0 {
		t.Fatalf("empty-id projects must be ignored, got %+v", c.Projects)
	}
	c.AddProject(ProjectConfig{ID: "real", Name: "third", Path: "/third"})
	c.AddProject(ProjectConfig{ID: "", Name: "fourth", Path: "/fourth"})
	if len(c.Projects) != 1 || c.Projects[0].ID != "real" {
		t.Errorf("empty-id AddProject must not touch existing entries, got %+v", c.Projects)
	}
	if _, ok := c.Project(""); ok {
		t.Error("Project(\"\") should not match anything")
	}
}

// Every accessor must be nil-safe: app.Load returns a nil Config alongside
// ErrNoConfig, so a careless caller must get an error rather than a panic.
func TestNilConfigAccessors(t *testing.T) {
	var c *Config
	if _, err := c.VaultPath(); err == nil {
		t.Error("nil VaultPath should return an error")
	}
	if _, err := c.KeyFilePath(); err == nil {
		t.Error("nil KeyFilePath should return an error")
	}
	if n, err := c.MaxFileSize(); err == nil || n != 0 {
		t.Errorf("nil MaxFileSize = %d, %v; want 0 and an error", n, err)
	}
	if _, err := c.ProjectPath("x"); err == nil {
		t.Error("nil ProjectPath should return an error")
	}
	if err := c.Validate(); err == nil {
		t.Error("nil Validate should return an error")
	}
	if w := c.Warnings(); w != nil {
		t.Errorf("nil Warnings = %v, want nil", w)
	}
	if _, ok := c.Project("x"); ok {
		t.Error("nil Project should report not found")
	}
	if _, err := c.Marshal(); err == nil {
		t.Error("nil Marshal should return an error")
	}
}

func TestExample(t *testing.T) {
	setHome(t)
	ex := Example()
	if ex == "" {
		t.Fatal("Example() is empty")
	}
	for _, key := range []string{"version: 1", "machine:", "vault:", "remote:", "type: git", "rclone:", "key:", "source: bitwarden", "bitwarden:", "scan:", "max_file_size: 2MiB", "projects:", "# none | git | rclone", "# prompt | file | bitwarden", "KiB/MiB/GiB"} {
		if !strings.Contains(ex, key) {
			t.Errorf("Example() lacks %q", key)
		}
	}
	// It must be strictly parseable and valid.
	c, err := Parse([]byte(ex))
	if err != nil {
		t.Fatalf("Parse(Example()): %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Example config invalid: %v", err)
	}
	if w := c.Warnings(); len(w) != 0 {
		t.Errorf("Example config warnings: %v", w)
	}
	// Generic yaml decoding must succeed too (no tabs etc).
	var generic map[string]any
	if err := yaml.Unmarshal([]byte(ex), &generic); err != nil {
		t.Errorf("Example() is not valid YAML: %v", err)
	}
	if !strings.HasSuffix(ex, "\n") {
		t.Error("Example() should end with a newline")
	}
}

// Relative paths must be rejected on Load as well, with the offending key
// named, so a hand-edited config cannot silently depend on the working dir.
func TestLoadRejectsRelativePaths(t *testing.T) {
	setHome(t)
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"vault", "vault:\n  path: vault\n", "vault.path"},
		{"key file", "vault:\n  path: ~/v\nkey:\n  source: file\n  file:\n    path: key\n", "key.file.path"},
		{"project", "vault:\n  path: ~/v\nprojects:\n  - id: a\n    path: proj\n", "projects[0]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(p)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "must be absolute or start with ~/") {
				t.Fatalf("err = %v, want relative-path error mentioning %q", err, tc.want)
			}
		})
	}
}

func TestIsStablePath(t *testing.T) {
	type row struct {
		in   string
		want bool
	}
	tests := []row{
		{"~", true},
		{"~/", true},
		{"~/x", true},
		{"", false},
		{"x", false},
		{"./x", false},
		{"../x", false},
		{"~x", false},
		{"~bob/x", false},
		{" /x", false},
		{" ~/x", false},
		// A path that is absolute on this platform is always stable.
		{abs("/srv/x"), true},
		{abs("/"), true},
	}
	if runtime.GOOS == "windows" {
		// Volume-less rooted paths are drive-relative on Windows, so they
		// depend on the current drive and are not stable there; the
		// drive-rooted and ~\ forms are the stable counterparts.
		tests = append(tests, row{"/x", false}, row{"/", false}, row{`C:\x`, true}, row{`~\x`, true})
	} else {
		tests = append(tests, row{"/x", true}, row{"/", true})
	}
	for _, tc := range tests {
		if got := isStablePath(tc.in); got != tc.want {
			t.Errorf("isStablePath(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// The temp file name must be unique per process and per call, otherwise
// two writers racing on config.yaml would truncate each other's temp file.
func TestTempSuffixUnique(t *testing.T) {
	a, b := tempSuffix(), tempSuffix()
	if a == b {
		t.Fatalf("tempSuffix returned the same value twice: %q", a)
	}
	pid := strconv.Itoa(os.Getpid())
	for _, s := range []string{a, b} {
		if !strings.HasPrefix(s, "cfg-"+pid+"-") {
			t.Errorf("tempSuffix = %q, want prefix cfg-%s-", s, pid)
		}
		if strings.ContainsAny(s, `/\ `) {
			t.Errorf("tempSuffix %q must be usable as a file name", s)
		}
	}
}

// Concurrent Saves to the same path must each write through their own temp
// file: the final config.yaml is one complete version and no temp files remain.
func TestSaveConcurrent(t *testing.T) {
	setHome(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := valid()
			c.Machine.Name = "writer-" + strconv.Itoa(i)
			errs[i] = c.Save(p)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("writer %d: %v", i, err)
		}
	}
	got, err := Load(p)
	if err != nil {
		t.Fatalf("Load after concurrent saves: %v", err)
	}
	if !strings.HasPrefix(got.Machine.Name, "writer-") {
		t.Errorf("Machine.Name = %q, want one of the writers", got.Machine.Name)
	}
	want := valid()
	want.Machine.Name = got.Machine.Name
	if !equalConfig(got, want) {
		t.Errorf("final file is not a complete config:\n got %+v\nwant %+v", got, want)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "config.yaml" {
			t.Errorf("unexpected file left behind: %s", e.Name())
		}
	}
}

func TestIsWithin(t *testing.T) {
	sep := string(filepath.Separator)
	root := filepath.Clean(sep + "a" + sep + "b")
	tests := []struct {
		dir, child string
		want       bool
	}{
		{root, root, true},
		{root, filepath.Join(root, "c"), true},
		{root, filepath.Join(root, "c", "d"), true},
		{root, filepath.Clean(sep + "a"), false},
		{root, filepath.Clean(sep + "a" + sep + "bc"), false},
		{root, filepath.Clean(sep + "a" + sep + "b2" + sep + "x"), false},
		{root, filepath.Clean(sep + "z"), false},
		{filepath.Join(root, "c"), root, false},
	}
	for _, tc := range tests {
		if got := isWithin(tc.dir, tc.child); got != tc.want {
			t.Errorf("isWithin(%q, %q) = %v, want %v", tc.dir, tc.child, got, tc.want)
		}
	}
}

func TestProjectLabel(t *testing.T) {
	tests := []struct {
		p    ProjectConfig
		want string
	}{
		{ProjectConfig{ID: "id1", Name: "n"}, "n [id1]"},
		{ProjectConfig{Name: "n"}, "n"},
		{ProjectConfig{ID: "id1"}, "id1"},
		{ProjectConfig{}, "<unnamed>"},
	}
	for _, tc := range tests {
		if got := projectLabel(tc.p); got != tc.want {
			t.Errorf("projectLabel(%+v) = %q, want %q", tc.p, got, tc.want)
		}
	}
}

// equalConfig compares two configs field by field, treating nil and empty
// slices as equal (YAML round-trips omit empty lists).
func equalConfig(a, b *Config) bool {
	if a.Version != b.Version || a.Machine != b.Machine || a.Vault != b.Vault || a.Key != b.Key || a.Scan.MaxFileSize != b.Scan.MaxFileSize {
		return false
	}
	eq := func(x, y []string) bool {
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	}
	if !eq(a.Scan.Include, b.Scan.Include) || !eq(a.Scan.ExcludeDirs, b.Scan.ExcludeDirs) || !eq(a.Scan.ExcludeFiles, b.Scan.ExcludeFiles) {
		return false
	}
	if len(a.Projects) != len(b.Projects) {
		return false
	}
	for i := range a.Projects {
		if a.Projects[i] != b.Projects[i] {
			return false
		}
	}
	return true
}
