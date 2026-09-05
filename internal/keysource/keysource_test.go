package keysource

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/execx"
	"github.com/sanbiv/private-sync/internal/ui"
)

// fakePrompter records prompts and answers each Password call from a queue.
type fakePrompter struct {
	answers [][]byte
	err     error
	titles  []string
	handed  [][]byte // the exact slices returned, so tests can verify wiping
}

func (f *fakePrompter) Password(_ context.Context, title string) ([]byte, error) {
	f.titles = append(f.titles, title)
	if f.err != nil {
		return nil, f.err
	}
	if len(f.answers) == 0 {
		return nil, errors.New("fakePrompter: no answer queued")
	}
	a := f.answers[0]
	f.answers = f.answers[1:]
	out := make([]byte, len(a))
	copy(out, a)
	f.handed = append(f.handed, out)
	return out, nil
}

func (f *fakePrompter) Confirm(_ context.Context, _ string, def bool) (bool, error) {
	return def, nil
}

// recordedCmd is a deep copy of an execx.Cmd taken inside the fake handler.
type recordedCmd struct {
	Name string
	Args []string
	Env  []string
}

func record(c execx.Cmd) recordedCmd {
	return recordedCmd{
		Name: c.Name,
		Args: append([]string(nil), c.Args...),
		Env:  append([]string(nil), c.Env...),
	}
}

func (r recordedCmd) is(args ...string) bool {
	if len(r.Args) < len(args) {
		return false
	}
	for i, a := range args {
		if r.Args[i] != a {
			return false
		}
	}
	return true
}

// unsetBWSession removes BW_SESSION for the duration of the test and restores the
// developer's exported value afterwards (t.Setenv registers the restore).
func unsetBWSession(t *testing.T) {
	t.Helper()
	t.Setenv(BWSessionEnvVar, "")
	if err := os.Unsetenv(BWSessionEnvVar); err != nil {
		t.Fatal(err)
	}
}

// unsetEnvVar removes PRIVATE_SYNC_PASSPHRASE for the duration of the test and
// restores the developer's exported value afterwards (t.Setenv registers the
// restore); a bare os.Unsetenv would silently drop it for the rest of the binary.
func unsetEnvVar(t *testing.T) {
	t.Helper()
	t.Setenv(EnvVar, "")
	if err := os.Unsetenv(EnvVar); err != nil {
		t.Fatal(err)
	}
}

func envValue(env []string, key string) (string, bool) {
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok && k == key {
			return v, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// FromConfig
// ---------------------------------------------------------------------------

func TestFromConfig(t *testing.T) {
	r := execx.Fake(func(c execx.Cmd) (execx.Result, error) { return execx.Result{}, nil })
	tests := []struct {
		name     string
		cfg      config.KeyConfig
		runner   execx.Runner
		wantName string
		wantErr  string
	}{
		{name: "prompt", cfg: config.KeyConfig{Source: config.KeyPrompt}, wantName: "prompt"},
		{name: "empty source defaults to prompt", cfg: config.KeyConfig{}, wantName: "prompt"},
		{name: "file", cfg: config.KeyConfig{Source: config.KeyFile, File: config.FileKey{Path: "/x/key"}}, wantName: "file"},
		{name: "file default path", cfg: config.KeyConfig{Source: config.KeyFile}, wantName: "file"},
		{name: "bitwarden", cfg: config.KeyConfig{Source: config.KeyBitwarden, Bitwarden: config.BitwardenKey{Item: "vault"}}, runner: r, wantName: "bitwarden"},
		{name: "bitwarden no item", cfg: config.KeyConfig{Source: config.KeyBitwarden}, runner: r, wantErr: "item is empty"},
		{name: "bitwarden nil runner", cfg: config.KeyConfig{Source: config.KeyBitwarden, Bitwarden: config.BitwardenKey{Item: "vault"}}, wantErr: "runner"},
		{name: "unknown", cfg: config.KeyConfig{Source: "keychain"}, wantErr: `unknown key source "keychain"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src, err := FromConfig(tc.cfg, tc.runner)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if src.Name() != tc.wantName {
				t.Fatalf("Name() = %q, want %q", src.Name(), tc.wantName)
			}
		})
	}
}

func TestFromConfigDefaults(t *testing.T) {
	src, err := FromConfig(config.KeyConfig{Source: config.KeyFile}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fsrc := src.(*fileSource)
	if !strings.HasSuffix(fsrc.path, filepath.Join("private-sync", "key")) {
		t.Fatalf("default key path = %q", fsrc.path)
	}
	src, err = FromConfig(config.KeyConfig{Source: config.KeyBitwarden, Bitwarden: config.BitwardenKey{Item: " it "}},
		execx.Fake(func(execx.Cmd) (execx.Result, error) { return execx.Result{}, nil }))
	if err != nil {
		t.Fatal(err)
	}
	bw := src.(*bitwardenSource)
	if bw.field != "password" || bw.item != "it" {
		t.Fatalf("bitwarden defaults: field=%q item=%q", bw.field, bw.item)
	}
}

// ---------------------------------------------------------------------------
// prompt source
// ---------------------------------------------------------------------------

func TestPromptSource(t *testing.T) {
	tests := []struct {
		name    string
		p       *fakePrompter
		want    string
		wantErr error
		errText string
	}{
		{name: "ok", p: &fakePrompter{answers: [][]byte{[]byte("hunter2")}}, want: "hunter2"},
		{name: "empty", p: &fakePrompter{answers: [][]byte{[]byte("")}}, wantErr: ErrEmptyPassphrase},
		{name: "prompter error", p: &fakePrompter{err: ui.ErrNonInteractive}, wantErr: ui.ErrNonInteractive},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src, err := FromConfig(config.KeyConfig{Source: config.KeyPrompt}, nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := src.Passphrase(context.Background(), tc.p)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if len(tc.p.titles) != 1 || tc.p.titles[0] != "Vault passphrase" {
				t.Fatalf("titles = %v", tc.p.titles)
			}
		})
	}
	t.Run("nil prompter", func(t *testing.T) {
		src, _ := FromConfig(config.KeyConfig{Source: config.KeyPrompt}, nil)
		if _, err := src.Passphrase(context.Background(), nil); !errors.Is(err, ui.ErrNonInteractive) {
			t.Fatalf("err = %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// file source
// ---------------------------------------------------------------------------

func writeTemp(t *testing.T, dir, name, content string, mode fs.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFileSource(t *testing.T) {
	unix := runtime.GOOS != "windows"
	dir := t.TempDir()

	tests := []struct {
		name    string
		setup   func(t *testing.T) string // returns config path
		want    string
		wantErr error
		errText string
		unixOK  bool // on Windows the perms check is skipped: expect success instead
	}{
		{
			name:  "0600 ok",
			setup: func(t *testing.T) string { return writeTemp(t, dir, "k600", "secret", 0o600) },
			want:  "secret",
		},
		{
			name:  "0400 ok",
			setup: func(t *testing.T) string { return writeTemp(t, dir, "k400", "secret", 0o400) },
			want:  "secret",
		},
		{
			name:    "0644 rejected with chmod hint",
			setup:   func(t *testing.T) string { return writeTemp(t, dir, "k644", "secret", 0o644) },
			wantErr: ErrKeyFilePerms,
			errText: "has mode 0644; run: chmod 600 ",
			unixOK:  true,
		},
		{
			name:    "0660 rejected",
			setup:   func(t *testing.T) string { return writeTemp(t, dir, "k660", "secret", 0o660) },
			wantErr: ErrKeyFilePerms,
			errText: "has mode 0660; run: chmod 600 ",
			unixOK:  true,
		},
		{
			name:    "0601 rejected",
			setup:   func(t *testing.T) string { return writeTemp(t, dir, "k601", "secret", 0o601) },
			wantErr: ErrKeyFilePerms,
			errText: "has mode 0601; run: chmod 600 ",
			unixOK:  true,
		},
		{
			name: "trailing newline and whitespace trimmed",
			setup: func(t *testing.T) string {
				return writeTemp(t, dir, "knl", "  se cret \t\r\n\n", 0o600)
			},
			want: "  se cret",
		},
		{
			// Spec §11: "trimmed of trailing whitespace" means every white space
			// character, not just space/tab/CR/LF: \v, \f, NBSP and NEL must go too.
			name: "trailing vertical tab, form feed and NBSP trimmed",
			setup: func(t *testing.T) string {
				return writeTemp(t, dir, "kvt", "se cret\n\v\f\u00a0\u0085 \t", 0o600)
			},
			want: "se cret",
		},
		{
			name: "interior unicode whitespace kept, only the tail trimmed",
			setup: func(t *testing.T) string {
				return writeTemp(t, dir, "kmid", "se\u00a0cret\v\f", 0o600)
			},
			want: "se\u00a0cret",
		},
		{
			name:    "vertical tab and form feed only is empty",
			setup:   func(t *testing.T) string { return writeTemp(t, dir, "kvtonly", "\v\f\u00a0", 0o600) },
			wantErr: ErrEmptyPassphrase,
		},
		{
			name: "exactly MaxKeyFileSize bytes accepted",
			setup: func(t *testing.T) string {
				return writeTemp(t, dir, "kmax", strings.Repeat("a", MaxKeyFileSize), 0o600)
			},
			want: strings.Repeat("a", MaxKeyFileSize),
		},
		{
			name: "larger than MaxKeyFileSize rejected",
			setup: func(t *testing.T) string {
				return writeTemp(t, dir, "kbig", strings.Repeat("a", 70<<10)+"\n", 0o600)
			},
			wantErr: ErrKeyFileTooLarge,
			errText: "is larger than 65536 bytes; not a passphrase file",
		},
		{
			// One byte over the limit, even when the excess is whitespace: the
			// limit is on the file, not on the trimmed passphrase.
			name: "MaxKeyFileSize plus one trailing newline rejected",
			setup: func(t *testing.T) string {
				return writeTemp(t, dir, "kmax1", strings.Repeat("a", MaxKeyFileSize)+"\n", 0o600)
			},
			wantErr: ErrKeyFileTooLarge,
		},
		{
			name:    "whitespace only is empty",
			setup:   func(t *testing.T) string { return writeTemp(t, dir, "kws", "\n\n \t", 0o600) },
			wantErr: ErrEmptyPassphrase,
		},
		{
			name:    "empty file",
			setup:   func(t *testing.T) string { return writeTemp(t, dir, "kempty", "", 0o600) },
			wantErr: ErrEmptyPassphrase,
		},
		{
			name:    "missing file",
			setup:   func(t *testing.T) string { return filepath.Join(dir, "nope") },
			wantErr: fs.ErrNotExist,
			errText: "does not exist",
		},
		{
			name: "symlink to 0600 target",
			setup: func(t *testing.T) string {
				target := writeTemp(t, dir, "ktarget", "linked\n", 0o600)
				link := filepath.Join(dir, "klink")
				if err := os.Symlink(target, link); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
				return link
			},
			want: "linked",
		},
		{
			name: "symlink to 0644 target reports target path",
			setup: func(t *testing.T) string {
				target := writeTemp(t, dir, "kbadtarget", "linked\n", 0o644)
				link := filepath.Join(dir, "kbadlink")
				if err := os.Symlink(target, link); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
				return link
			},
			wantErr: ErrKeyFilePerms,
			errText: "kbadtarget has mode 0644; run: chmod 600 ",
			unixOK:  true,
		},
		{
			name: "dangling symlink",
			setup: func(t *testing.T) string {
				link := filepath.Join(dir, "kdangling")
				if err := os.Symlink(filepath.Join(dir, "gone"), link); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
				return link
			},
			wantErr: fs.ErrNotExist,
		},
		{
			name: "directory is not a regular file",
			setup: func(t *testing.T) string {
				d := filepath.Join(dir, "adir")
				if err := os.Mkdir(d, 0o700); err != nil {
					t.Fatal(err)
				}
				return d
			},
			errText: "not a regular file",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.setup(t)
			src, err := FromConfig(config.KeyConfig{Source: config.KeyFile, File: config.FileKey{Path: path}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := src.Passphrase(context.Background(), nil)
			expectErr := tc.wantErr != nil || tc.errText != ""
			if tc.unixOK && !unix {
				expectErr = false
			}
			if expectErr {
				if err == nil {
					t.Fatalf("expected error, got passphrase %q", got)
				}
				if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want errors.Is %v", err, tc.wantErr)
				}
				if tc.errText != "" && !strings.Contains(err.Error(), tc.errText) {
					t.Fatalf("err = %q, want containing %q", err.Error(), tc.errText)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.want != "" && string(got) != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFileSourcePermsMessageExact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission checks are skipped on Windows")
	}
	dir := t.TempDir()
	p := writeTemp(t, dir, "key", "s", 0o644)
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ReadKeyFile(p)
	if err == nil {
		t.Fatal("expected error")
	}
	want := fmt.Sprintf("key file %s has mode 0644; run: chmod 600 %s", resolved, resolved)
	if err.Error() != want {
		t.Fatalf("err = %q\nwant %q", err.Error(), want)
	}
	if !errors.Is(err, ErrKeyFilePerms) {
		t.Fatalf("err does not unwrap to ErrKeyFilePerms: %v", err)
	}
}

func TestFileSourceExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	writeTemp(t, home, "key", "fromhome\n", 0o600)
	src, err := FromConfig(config.KeyConfig{Source: config.KeyFile, File: config.FileKey{Path: "~/key"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := src.Passphrase(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fromhome" {
		t.Fatalf("got %q", got)
	}
}

// ---------------------------------------------------------------------------
// WriteKeyFile
// ---------------------------------------------------------------------------

func TestWriteKeyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "deeper", "key")
	if err := WriteKeyFile(path); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %04o, want 0600", fi.Mode().Perm())
		}
		di, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if di.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode = %04o, want 0700", di.Mode().Perm())
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("expected trailing newline, got %q", data)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("content is not base64: %v", err)
	}
	if len(raw) != 32 {
		t.Fatalf("decoded %d bytes, want 32", len(raw))
	}

	// Readable through the file source, trimmed.
	got, err := ReadKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != strings.TrimSpace(string(data)) {
		t.Fatalf("ReadKeyFile = %q, file = %q", got, data)
	}

	// O_EXCL: second write fails and leaves the content untouched.
	err = WriteKeyFile(path)
	if err == nil {
		t.Fatal("expected error on existing file")
	}
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("err = %v, want fs.ErrExist", err)
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %q", err.Error())
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(data) {
		t.Fatal("existing key file was modified")
	}

	// Two generated files differ.
	other := filepath.Join(dir, "key2")
	if err := WriteKeyFile(other); err != nil {
		t.Fatal(err)
	}
	od, _ := os.ReadFile(other)
	if string(od) == string(data) {
		t.Fatal("two generated passphrases are identical")
	}

	if err := WriteKeyFile(""); err == nil {
		t.Fatal("expected error for empty path")
	}
	if err := WriteKeyFile("   "); err == nil {
		t.Fatal("expected error for blank path")
	}
}

func TestWriteKeyFileExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := WriteKeyFile("~/.config/private-sync/key"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "private-sync", "key")); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// CaptureEnv + Obtain
// ---------------------------------------------------------------------------

func TestCaptureEnvAndObtain(t *testing.T) {
	t.Cleanup(func() { envPassphrase = nil })

	t.Run("env wins over configured source", func(t *testing.T) {
		envPassphrase = nil
		t.Setenv(EnvVar, "from-env")
		CaptureEnv()
		if v, ok := os.LookupEnv(EnvVar); ok {
			t.Fatalf("%s still set to %q after CaptureEnv", EnvVar, v)
		}
		// A prompter that fails and a runner that fails: neither may be consulted.
		p := &fakePrompter{err: errors.New("must not prompt")}
		r := execx.Fake(func(c execx.Cmd) (execx.Result, error) {
			t.Fatalf("runner must not be called, got %v", c.Args)
			return execx.Result{}, nil
		})
		for _, cfg := range []config.KeyConfig{
			{Source: config.KeyPrompt},
			{Source: config.KeyFile, File: config.FileKey{Path: filepath.Join(t.TempDir(), "missing")}},
			{Source: config.KeyBitwarden, Bitwarden: config.BitwardenKey{Item: "x"}},
			{Source: "bogus"},
		} {
			got, err := Obtain(context.Background(), cfg, p, r)
			if err != nil {
				t.Fatalf("%s: %v", cfg.Source, err)
			}
			if string(got) != "from-env" {
				t.Fatalf("%s: got %q", cfg.Source, got)
			}
			// Returned copy is independent of the captured buffer.
			got[0] = 'X'
			if string(envPassphrase) != "from-env" {
				t.Fatal("Obtain returned the internal buffer, not a copy")
			}
		}
		if len(p.titles) != 0 {
			t.Fatalf("prompter was consulted: %v", p.titles)
		}
	})

	t.Run("empty env value falls through to source", func(t *testing.T) {
		envPassphrase = nil
		t.Setenv(EnvVar, "")
		CaptureEnv()
		p := &fakePrompter{answers: [][]byte{[]byte("prompted")}}
		got, err := Obtain(context.Background(), config.KeyConfig{Source: config.KeyPrompt}, p, nil)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "prompted" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("unset env uses configured source", func(t *testing.T) {
		envPassphrase = nil
		unsetEnvVar(t)
		CaptureEnv()
		if envPassphrase != nil {
			t.Fatal("envPassphrase captured from nothing")
		}
		dir := t.TempDir()
		path := writeTemp(t, dir, "key", "from-file\n", 0o600)
		got, err := Obtain(context.Background(), config.KeyConfig{Source: config.KeyFile, File: config.FileKey{Path: path}}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "from-file" {
			t.Fatalf("got %q", got)
		}
		if _, err := Obtain(context.Background(), config.KeyConfig{Source: "bogus"}, nil, nil); err == nil {
			t.Fatal("expected error for unknown source")
		}
	})
}

// TestCaptureEnvTestHygiene is the regression test for the test suite itself: a
// developer who has PRIVATE_SYNC_PASSPHRASE exported must find it untouched after
// the CaptureEnv tests ran (CaptureEnv unsets it, and the tests clear it), so the
// helpers must register a restore rather than call os.Unsetenv directly.
func TestCaptureEnvTestHygiene(t *testing.T) {
	t.Cleanup(func() { envPassphrase = nil })
	t.Setenv(EnvVar, "developer-value")
	t.Setenv(BWSessionEnvVar, "developer-session")

	t.Run("inner test clears both variables", func(t *testing.T) {
		envPassphrase = nil
		unsetEnvVar(t)
		unsetBWSession(t)
		if _, ok := os.LookupEnv(EnvVar); ok {
			t.Fatalf("%s still set inside the inner test", EnvVar)
		}
		if _, ok := os.LookupEnv(BWSessionEnvVar); ok {
			t.Fatalf("%s still set inside the inner test", BWSessionEnvVar)
		}
		CaptureEnv()
		if envPassphrase != nil {
			t.Fatal("envPassphrase captured from nothing")
		}
	})
	t.Run("inner test captures and unsets", func(t *testing.T) {
		envPassphrase = nil
		t.Setenv(EnvVar, "inner")
		CaptureEnv()
		if string(envPassphrase) != "inner" {
			t.Fatalf("captured %q", envPassphrase)
		}
		if _, ok := os.LookupEnv(EnvVar); ok {
			t.Fatalf("%s still set after CaptureEnv", EnvVar)
		}
	})

	if got := os.Getenv(EnvVar); got != "developer-value" {
		t.Fatalf("%s = %q after inner tests, want the developer's value restored", EnvVar, got)
	}
	if got := os.Getenv(BWSessionEnvVar); got != "developer-session" {
		t.Fatalf("%s = %q after inner tests, want the developer's value restored", BWSessionEnvVar, got)
	}
}

// ---------------------------------------------------------------------------
// shellQuote
// ---------------------------------------------------------------------------

func TestShellQuote(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", "''"},
		{"/home/me/.config/private-sync/key", "/home/me/.config/private-sync/key"},
		{"C:/Users/me/key", "C:/Users/me/key"},
		{"/tmp/a-b_c.d+e:f@g,h%i=j", "/tmp/a-b_c.d+e:f@g,h%i=j"},
		{"/Users/me/My Keys/key", "'/Users/me/My Keys/key'"},
		{"/tmp/it's/key", `'/tmp/it'\''s/key'`},
		{"/tmp/$HOME/key", "'/tmp/$HOME/key'"},
		{"/tmp/a;rm -rf /", "'/tmp/a;rm -rf /'"},
		{"/tmp/a\tb", "'/tmp/a\tb'"},
		{"/tmp/a\nb", "'/tmp/a\nb'"},
		{"/tmp/key*", "'/tmp/key*'"},
		{"/tmp/key(1)", "'/tmp/key(1)'"},
		{"/tmp/\u00e9/key", "'/tmp/\u00e9/key'"},
		{"~/key", "'~/key'"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := shellQuote(tc.in); got != tc.want {
				t.Fatalf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// bitwarden source
// ---------------------------------------------------------------------------

const bwItemJSON = `{
  "object": "item", "id": "abc", "name": "private-sync vault",
  "notes": "note-pass\n\n",
  "login": {"username": "me", "password": "login-pass"},
  "fields": [
    {"name": "empty", "value": "", "type": 1},
    {"name": "custom", "value": "custom-pass", "type": 1},
    {"name": "trailing", "value": "trail-pass \t\r\n\n\u000b\u000c\u00a0", "type": 1},
    {"name": "vtonly", "value": "\u000b\u000c\u00a0", "type": 1},
    {"name": "nullval", "value": null, "type": 1}
  ]
}`

// bwFake builds a fake bw runner for the given status; it records every command.
type bwFake struct {
	status   string
	session  string // returned by unlock
	item     string // JSON returned by get item
	cmds     []recordedCmd
	unlockRC int
	getRC    int
	statusRC int

	// unlockedIf, when set, makes `bw status` behave like the real CLI: it reports
	// "unlocked" only when the status command's own Env carries
	// BW_SESSION=<unlockedIf>, and "locked" otherwise (status is then ignored).
	unlockedIf string
}

func (b *bwFake) statusFor(env []string) string {
	if b.unlockedIf == "" {
		return b.status
	}
	if v, ok := envValue(env, BWSessionEnvVar); ok && v == b.unlockedIf {
		return "unlocked"
	}
	return "locked"
}

func (b *bwFake) runner(t *testing.T) execx.Runner {
	return execx.Fake(func(c execx.Cmd) (execx.Result, error) {
		rc := record(c)
		b.cmds = append(b.cmds, rc)
		if c.Name != "bw" {
			t.Errorf("unexpected binary %q", c.Name)
		}
		if c.Stdin != nil {
			t.Errorf("bw must not receive stdin, got %q", c.Stdin)
		}
		switch {
		case rc.is("status"):
			if len(rc.Args) != 1 {
				t.Errorf("status args = %v", rc.Args)
			}
			return execx.Result{Stdout: []byte(fmt.Sprintf(`{"serverUrl":"https://x","status":%q}`, b.statusFor(rc.Env)) + "\n"), Stderr: []byte("nag\n"), ExitCode: b.statusRC}, nil
		case rc.is("unlock", "--raw", "--passwordenv", "BW_PASSWORD"):
			if len(rc.Args) != 4 {
				t.Errorf("unlock args = %v", rc.Args)
			}
			return execx.Result{Stdout: []byte(b.session + "\n"), Stderr: []byte("update nag\n"), ExitCode: b.unlockRC}, nil
		case rc.is("get", "item"):
			if len(rc.Args) != 3 {
				t.Errorf("get item args = %v", rc.Args)
			}
			return execx.Result{Stdout: []byte(b.item), Stderr: []byte("Please update\n"), ExitCode: b.getRC}, nil
		default:
			t.Errorf("unexpected bw command %v", rc.Args)
			return execx.Result{ExitCode: 1}, nil
		}
	})
}

func (b *bwFake) find(args ...string) []recordedCmd {
	var out []recordedCmd
	for _, c := range b.cmds {
		if c.is(args...) {
			out = append(out, c)
		}
	}
	return out
}

// assertEnvIsolation checks that BW_PASSWORD appears only on unlock commands,
// BW_SESSION only on get commands (the in-memory session) and on the status
// command (exactly the exported value wantStatusSession, or no env at all when
// the user has none exported), and that nothing else carries any env.
func assertEnvIsolation(t *testing.T, cmds []recordedCmd, wantMaster, wantSession, wantStatusSession string) {
	t.Helper()
	for _, c := range cmds {
		pw, hasPW := envValue(c.Env, "BW_PASSWORD")
		sess, hasSess := envValue(c.Env, "BW_SESSION")
		switch {
		case c.is("status"):
			if hasPW {
				t.Errorf("status: BW_PASSWORD leaked: %v", c.Env)
			}
			if wantStatusSession == "" {
				if len(c.Env) != 0 {
					t.Errorf("status: env = %v, want empty (no exported BW_SESSION)", c.Env)
				}
				break
			}
			if !hasSess || sess != wantStatusSession {
				t.Errorf("status: BW_SESSION = %q,%v want %q", sess, hasSess, wantStatusSession)
			}
			if len(c.Env) != 1 {
				t.Errorf("status: env = %v, want exactly one entry", c.Env)
			}
		case c.is("unlock"):
			if !hasPW || pw != wantMaster {
				t.Errorf("unlock: BW_PASSWORD = %q,%v want %q", pw, hasPW, wantMaster)
			}
			if hasSess {
				t.Errorf("unlock: BW_SESSION leaked: %v", c.Env)
			}
			if len(c.Env) != 1 {
				t.Errorf("unlock: env = %v, want exactly one entry", c.Env)
			}
		case c.is("get", "item"):
			if !hasSess || sess != wantSession {
				t.Errorf("get item: BW_SESSION = %q,%v want %q", sess, hasSess, wantSession)
			}
			if hasPW {
				t.Errorf("get item: BW_PASSWORD leaked: %v", c.Env)
			}
			if len(c.Env) != 1 {
				t.Errorf("get item: env = %v, want exactly one entry", c.Env)
			}
		default:
			if len(c.Env) != 0 {
				t.Errorf("%v: env must be empty, got %v", c.Args, c.Env)
			}
		}
	}
}

func bwCfg(item, field string) config.KeyConfig {
	return config.KeyConfig{Source: config.KeyBitwarden, Bitwarden: config.BitwardenKey{Item: item, Field: field}}
}

func TestBitwardenFieldSelection(t *testing.T) {
	tests := []struct {
		name    string
		field   string
		item    string
		want    string
		errText string
	}{
		{name: "default password", field: "", item: bwItemJSON, want: "login-pass"},
		{name: "password", field: "password", item: bwItemJSON, want: "login-pass"},
		{name: "notes trimmed", field: "notes", item: bwItemJSON, want: "note-pass"},
		{name: "custom field", field: "custom", item: bwItemJSON, want: "custom-pass"},
		{name: "custom field trailing whitespace trimmed", field: "trailing", item: bwItemJSON, want: "trail-pass"},
		{name: "custom field vertical tab, form feed and NBSP only is empty", field: "vtonly", item: bwItemJSON, errText: "empty passphrase"},
		{name: "notes trailing vertical tab and form feed trimmed", field: "notes", item: `{"notes":"note\u00a0pass\n\u000b\u000c"}`, want: "note\u00a0pass"},
		{name: "password trailing vertical tab and form feed trimmed", field: "password", item: `{"login":{"password":"pw\u000b\u000c\u00a0\u0085"}}`, want: "pw"},
		{name: "custom field whitespace only is empty", field: "ws", item: `{"fields":[{"name":"ws","value":" \n"}]}`, errText: "empty passphrase"},
		{name: "password trailing whitespace trimmed", field: "password", item: `{"login":{"password":"  pw one \t\r\n"}}`, want: "  pw one"},
		{name: "password whitespace only is empty", field: "password", item: `{"login":{"password":"\n"}}`, errText: "empty passphrase"},
		{name: "custom field empty", field: "empty", item: bwItemJSON, errText: "empty passphrase"},
		{name: "custom field null value", field: "nullval", item: bwItemJSON, errText: "empty passphrase"},
		{name: "custom field missing", field: "nope", item: bwItemJSON, errText: `no field named "nope"`},
		{name: "no login", field: "password", item: `{"notes":"n"}`, errText: "no login password"},
		{name: "null password", field: "password", item: `{"login":{"password":null}}`, errText: "no login password"},
		{name: "empty password", field: "password", item: `{"login":{"password":""}}`, errText: "empty passphrase"},
		{name: "no notes", field: "notes", item: `{"login":{"password":"x"}}`, errText: "no notes"},
		{name: "whitespace notes", field: "notes", item: `{"notes":"  \n"}`, errText: "empty passphrase"},
		{name: "no fields", field: "custom", item: `{"login":{"password":"x"}}`, errText: `no field named "custom"`},
		{name: "invalid json", field: "password", item: `not json`, errText: "unparsable"},
		{name: "empty output", field: "password", item: ``, errText: "unparsable"},
		{name: "json array", field: "password", item: `[]`, errText: "unparsable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			unsetBWSession(t)
			fake := &bwFake{status: "locked", session: "sess-1", item: tc.item}
			src, err := FromConfig(bwCfg("private-sync vault", tc.field), fake.runner(t))
			if err != nil {
				t.Fatal(err)
			}
			p := &fakePrompter{answers: [][]byte{[]byte("master")}}
			got, err := src.Passphrase(context.Background(), p)
			if tc.errText != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errText) {
					t.Fatalf("err = %v, want containing %q", err, tc.errText)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			assertEnvIsolation(t, fake.cmds, "master", "sess-1", "")
			gets := fake.find("get", "item")
			if len(gets) != 1 || gets[0].Args[2] != "private-sync vault" {
				t.Fatalf("get item calls = %v", gets)
			}
		})
	}
}

func TestBitwardenStatusFlows(t *testing.T) {
	tests := []struct {
		name        string
		status      string
		statusRC    int
		envSession  string
		answers     [][]byte
		promptErr   error
		unlockRC    int
		unlockOut   string
		getRC       int
		want        string
		errText     string
		wantTitles  []string
		wantUnlock  int
		wantGet     int
		wantSession string
	}{
		{
			name: "unauthenticated", status: "unauthenticated",
			errText: "run `bw login` first",
		},
		{
			name: "locked prompts and unlocks", status: "locked",
			answers: [][]byte{[]byte("master")}, unlockOut: "sess-A",
			want: "login-pass", wantTitles: []string{"Bitwarden master password"}, wantUnlock: 1, wantGet: 1, wantSession: "sess-A",
		},
		{
			name: "unlocked with BW_SESSION reuses it", status: "unlocked", envSession: "env-sess",
			want: "login-pass", wantTitles: nil, wantUnlock: 0, wantGet: 1, wantSession: "env-sess",
		},
		{
			name: "unlocked without BW_SESSION behaves like locked", status: "unlocked",
			answers: [][]byte{[]byte("master")}, unlockOut: "sess-B",
			want: "login-pass", wantTitles: []string{"Bitwarden master password"}, wantUnlock: 1, wantGet: 1, wantSession: "sess-B",
		},
		{
			name: "locked ignores BW_SESSION in env", status: "locked", envSession: "stale",
			answers: [][]byte{[]byte("master")}, unlockOut: "sess-C",
			want: "login-pass", wantTitles: []string{"Bitwarden master password"}, wantUnlock: 1, wantGet: 1, wantSession: "sess-C",
		},
		{
			name: "status non-zero exit", status: "locked", statusRC: 1,
			errText: "bw status",
		},
		{
			name: "unknown status", status: "weird",
			errText: `unexpected status "weird"`,
		},
		{
			name: "empty master password", status: "locked",
			answers: [][]byte{[]byte("")},
			errText: "master password is empty", wantTitles: []string{"Bitwarden master password"},
		},
		{
			name: "prompter error", status: "locked",
			promptErr: ui.ErrNonInteractive,
			errText:   "non-interactive", wantTitles: []string{"Bitwarden master password"},
		},
		{
			name: "unlock fails", status: "locked",
			answers: [][]byte{[]byte("wrong")}, unlockRC: 1, unlockOut: "",
			errText: "bw unlock", wantTitles: []string{"Bitwarden master password"}, wantUnlock: 1,
		},
		{
			name: "unlock returns empty session", status: "locked",
			answers: [][]byte{[]byte("master")}, unlockOut: "",
			errText: "empty session key", wantTitles: []string{"Bitwarden master password"}, wantUnlock: 1,
		},
		{
			name: "get item fails", status: "unlocked", envSession: "env-sess", getRC: 1,
			errText: "bw get item", wantGet: 1, wantSession: "env-sess",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envSession != "" {
				t.Setenv(BWSessionEnvVar, tc.envSession)
			} else {
				unsetBWSession(t)
			}
			fake := &bwFake{status: tc.status, session: tc.unlockOut, item: bwItemJSON, statusRC: tc.statusRC, unlockRC: tc.unlockRC, getRC: tc.getRC}
			src, err := FromConfig(bwCfg("vault", ""), fake.runner(t))
			if err != nil {
				t.Fatal(err)
			}
			p := &fakePrompter{answers: tc.answers, err: tc.promptErr}
			got, err := src.Passphrase(context.Background(), p)
			if tc.errText != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errText) {
					t.Fatalf("err = %v, want containing %q", err, tc.errText)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != tc.want {
					t.Fatalf("got %q, want %q", got, tc.want)
				}
			}
			if len(fake.find("status")) != 1 {
				t.Fatalf("status calls = %d", len(fake.find("status")))
			}
			if n := len(fake.find("unlock")); n != tc.wantUnlock {
				t.Fatalf("unlock calls = %d, want %d", n, tc.wantUnlock)
			}
			if n := len(fake.find("get", "item")); n != tc.wantGet {
				t.Fatalf("get item calls = %d, want %d", n, tc.wantGet)
			}
			if strings.Join(p.titles, ",") != strings.Join(tc.wantTitles, ",") {
				t.Fatalf("prompt titles = %v, want %v", p.titles, tc.wantTitles)
			}
			master := ""
			if len(tc.answers) > 0 {
				master = string(tc.answers[0])
			}
			assertEnvIsolation(t, fake.cmds, master, tc.wantSession, tc.envSession)
			// Master password bytes handed out by the prompter are wiped.
			for _, h := range p.handed {
				for _, b := range h {
					if b != 0 {
						t.Fatalf("master password not wiped: %q", h)
					}
				}
			}
		})
	}
}

// TestBitwardenStatusSeesExportedSession pins the fix for the unreachable
// "unlocked" branch: execx strips BW_SESSION from every child, so the exported
// value must be handed to `bw status` explicitly (and only to it) for the CLI to
// ever report "unlocked". The fake here mimics the real CLI and answers "unlocked"
// only when the status command's own Env carries the matching session.
func TestBitwardenStatusSeesExportedSession(t *testing.T) {
	tests := []struct {
		name        string
		envSession  string // exported BW_SESSION ("" = unset)
		prompter    *fakePrompter
		wantTitles  []string
		wantUnlock  int
		wantSession string // session expected on `get item`
		wantErr     error
	}{
		{
			name: "exported session is reused without prompting", envSession: "env-sess",
			prompter: nil, wantUnlock: 0, wantSession: "env-sess",
		},
		{
			name: "exported session works with a silent prompter", envSession: "env-sess",
			prompter: &fakePrompter{err: ui.ErrNonInteractive}, wantUnlock: 0, wantSession: "env-sess",
		},
		{
			name:       "no exported session prompts and unlocks",
			prompter:   &fakePrompter{answers: [][]byte{[]byte("master")}},
			wantTitles: []string{"Bitwarden master password"}, wantUnlock: 1, wantSession: "fresh-sess",
		},
		{
			name: "stale exported session is not reused", envSession: "stale",
			prompter:   &fakePrompter{answers: [][]byte{[]byte("master")}},
			wantTitles: []string{"Bitwarden master password"}, wantUnlock: 1, wantSession: "fresh-sess",
		},
		{
			name:     "no exported session and nil prompter is non-interactive",
			prompter: nil, wantErr: ui.ErrNonInteractive,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envSession != "" {
				t.Setenv(BWSessionEnvVar, tc.envSession)
			} else {
				unsetBWSession(t)
			}
			fake := &bwFake{unlockedIf: "env-sess", session: "fresh-sess", item: bwItemJSON}
			src, err := FromConfig(bwCfg("vault", "custom"), fake.runner(t))
			if err != nil {
				t.Fatal(err)
			}
			var p ui.Prompter
			if tc.prompter != nil {
				p = tc.prompter
			}
			got, err := src.Passphrase(context.Background(), p)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if len(fake.find("get", "item")) != 0 {
					t.Fatal("get item must not run after a failed unlock")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "custom-pass" {
				t.Fatalf("got %q", got)
			}
			if n := len(fake.find("status")); n != 1 {
				t.Fatalf("status calls = %d, want 1", n)
			}
			if n := len(fake.find("unlock")); n != tc.wantUnlock {
				t.Fatalf("unlock calls = %d, want %d", n, tc.wantUnlock)
			}
			gets := fake.find("get", "item")
			if len(gets) != 1 {
				t.Fatalf("get item calls = %d, want 1", len(gets))
			}
			if sess, _ := envValue(gets[0].Env, BWSessionEnvVar); sess != tc.wantSession {
				t.Fatalf("get item BW_SESSION = %q, want %q", sess, tc.wantSession)
			}
			var titles []string
			if tc.prompter != nil {
				titles = tc.prompter.titles
			}
			if strings.Join(titles, ",") != strings.Join(tc.wantTitles, ",") {
				t.Fatalf("prompt titles = %v, want %v", titles, tc.wantTitles)
			}
			master := ""
			if tc.wantUnlock > 0 {
				master = "master"
			}
			assertEnvIsolation(t, fake.cmds, master, tc.wantSession, tc.envSession)
			// The exported value must never leak to the process environment of the
			// unlock command, and the source must not have modified our own env.
			if v := os.Getenv(BWSessionEnvVar); v != tc.envSession {
				t.Fatalf("BW_SESSION in process env changed to %q", v)
			}
		})
	}
}

func TestBitwardenSessionReusedWithinProcess(t *testing.T) {
	unsetBWSession(t)
	fake := &bwFake{status: "locked", session: "sess-once", item: bwItemJSON}
	src, err := FromConfig(bwCfg("vault", "custom"), fake.runner(t))
	if err != nil {
		t.Fatal(err)
	}
	p := &fakePrompter{answers: [][]byte{[]byte("master")}}
	for i := 0; i < 3; i++ {
		got, err := src.Passphrase(context.Background(), p)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if string(got) != "custom-pass" {
			t.Fatalf("call %d: got %q", i, got)
		}
	}
	if n := len(fake.find("status")); n != 1 {
		t.Fatalf("status calls = %d, want 1", n)
	}
	if n := len(fake.find("unlock")); n != 1 {
		t.Fatalf("unlock calls = %d, want 1", n)
	}
	if n := len(fake.find("get", "item")); n != 3 {
		t.Fatalf("get item calls = %d, want 3", n)
	}
	if len(p.titles) != 1 {
		t.Fatalf("prompted %d times, want 1", len(p.titles))
	}
	assertEnvIsolation(t, fake.cmds, "master", "sess-once", "")
}

func TestBitwardenRunnerError(t *testing.T) {
	unsetBWSession(t)
	boom := errors.New("exec: \"bw\": executable file not found in $PATH")
	r := execx.Fake(func(c execx.Cmd) (execx.Result, error) { return execx.Result{}, boom })
	src, err := FromConfig(bwCfg("vault", ""), r)
	if err != nil {
		t.Fatal(err)
	}
	_, err = src.Passphrase(context.Background(), &fakePrompter{})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapping %v", err, boom)
	}
}

func TestBitwardenStatusParsing(t *testing.T) {
	unsetBWSession(t)
	for _, out := range []string{``, `garbage`, `{}`, `{"status":""}`, `[]`} {
		r := execx.Fake(func(c execx.Cmd) (execx.Result, error) {
			return execx.Result{Stdout: []byte(out)}, nil
		})
		src, _ := FromConfig(bwCfg("vault", ""), r)
		_, err := src.Passphrase(context.Background(), &fakePrompter{})
		if err == nil || !strings.Contains(err.Error(), "bw status") {
			t.Fatalf("output %q: err = %v", out, err)
		}
	}
}

func TestBitwardenNilPrompterWhenLocked(t *testing.T) {
	unsetBWSession(t)
	fake := &bwFake{status: "locked", session: "s", item: bwItemJSON}
	src, _ := FromConfig(bwCfg("vault", ""), fake.runner(t))
	_, err := src.Passphrase(context.Background(), nil)
	if !errors.Is(err, ui.ErrNonInteractive) {
		t.Fatalf("err = %v", err)
	}
	if len(fake.find("unlock")) != 0 {
		t.Fatal("unlock must not run without a prompter")
	}
}

func TestZero(t *testing.T) {
	Zero(nil)
	b := []byte("abc")
	Zero(b)
	if string(b) != "\x00\x00\x00" {
		t.Fatalf("Zero = %q", b)
	}
}

// ---------------------------------------------------------------------------
// concurrency
// ---------------------------------------------------------------------------

// syncBWFake is a goroutine-safe bw fake: `bw status` reports locked, unlock
// hands out one session, and every command is counted. It deliberately delays
// the unlock so concurrent callers pile up on the same source.
type syncBWFake struct {
	mu      sync.Mutex
	cmds    []recordedCmd
	unlocks atomic.Int32
	gets    atomic.Int32
	status  atomic.Int32
}

func (b *syncBWFake) runner(t *testing.T) execx.Runner {
	return execx.Fake(func(c execx.Cmd) (execx.Result, error) {
		rc := record(c)
		b.mu.Lock()
		b.cmds = append(b.cmds, rc)
		b.mu.Unlock()
		switch {
		case rc.is("status"):
			b.status.Add(1)
			return execx.Result{Stdout: []byte(`{"status":"locked"}`)}, nil
		case rc.is("unlock"):
			b.unlocks.Add(1)
			runtime.Gosched()
			return execx.Result{Stdout: []byte("sess-shared\n")}, nil
		case rc.is("get", "item"):
			b.gets.Add(1)
			return execx.Result{Stdout: []byte(bwItemJSON)}, nil
		default:
			t.Errorf("unexpected bw command %v", rc.Args)
			return execx.Result{ExitCode: 1}, nil
		}
	})
}

// countingPrompter is a goroutine-safe prompter that always answers "master".
type countingPrompter struct {
	calls atomic.Int32
}

func (c *countingPrompter) Password(_ context.Context, _ string) ([]byte, error) {
	c.calls.Add(1)
	runtime.Gosched()
	return []byte("master"), nil
}

func (*countingPrompter) Confirm(_ context.Context, _ string, def bool) (bool, error) {
	return def, nil
}

// TestBitwardenConcurrentPassphrase pins the session guard: N goroutines sharing
// one locked bitwarden source prompt and unlock exactly once, every call gets the
// passphrase, and the race detector (go test -race) sees no unsynchronised write
// to the cached session.
func TestBitwardenConcurrentPassphrase(t *testing.T) {
	unsetBWSession(t)
	fake := &syncBWFake{}
	src, err := FromConfig(bwCfg("vault", "custom"), fake.runner(t))
	if err != nil {
		t.Fatal(err)
	}
	p := &countingPrompter{}

	const n = 16
	var wg sync.WaitGroup
	results := make([]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			got, err := src.Passphrase(context.Background(), p)
			results[i], errs[i] = string(got), err
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
		if results[i] != "custom-pass" {
			t.Fatalf("call %d: got %q", i, results[i])
		}
	}
	if got := fake.status.Load(); got != 1 {
		t.Errorf("status calls = %d, want 1", got)
	}
	if got := fake.unlocks.Load(); got != 1 {
		t.Errorf("unlock calls = %d, want 1", got)
	}
	if got := p.calls.Load(); got != 1 {
		t.Errorf("prompted %d times, want 1", got)
	}
	if got := fake.gets.Load(); got != n {
		t.Errorf("get item calls = %d, want %d", got, n)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	assertEnvIsolation(t, fake.cmds, "master", "sess-shared", "")
}

// TestBitwardenConcurrentUnlockFailureDoesNotCacheSession: a failed unlock must
// leave the source unlocked-less so a later call retries instead of reusing junk.
func TestBitwardenUnlockFailureNotCached(t *testing.T) {
	unsetBWSession(t)
	fake := &bwFake{status: "locked", session: "", item: bwItemJSON}
	src, err := FromConfig(bwCfg("vault", "custom"), fake.runner(t))
	if err != nil {
		t.Fatal(err)
	}
	p := &fakePrompter{answers: [][]byte{[]byte("master"), []byte("master")}}
	if _, err := src.Passphrase(context.Background(), p); err == nil || !strings.Contains(err.Error(), "empty session key") {
		t.Fatalf("err = %v, want empty session key", err)
	}
	fake.session = "sess-2"
	got, err := src.Passphrase(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "custom-pass" {
		t.Fatalf("got %q", got)
	}
	if n := len(fake.find("unlock")); n != 2 {
		t.Fatalf("unlock calls = %d, want 2 (retry after failure)", n)
	}
	if len(fake.find("get", "item")) != 1 {
		t.Fatalf("get item calls = %v", fake.find("get", "item"))
	}
}

// TestReadKeyFileLimitDoesNotOverRead: the oversized check is enforced by a
// limiter, so at most MaxKeyFileSize+1 bytes are ever pulled into memory. We can
// only observe this indirectly: a file just over the limit fails identically to a
// far larger one, with the same wrapped sentinel and no partial passphrase.
func TestReadKeyFileLimitDoesNotOverRead(t *testing.T) {
	dir := t.TempDir()
	for _, size := range []int{MaxKeyFileSize + 1, MaxKeyFileSize + 4096, 1 << 20} {
		p := writeTemp(t, dir, fmt.Sprintf("k%d", size), strings.Repeat("z", size), 0o600)
		got, err := ReadKeyFile(p)
		if !errors.Is(err, ErrKeyFileTooLarge) {
			t.Fatalf("size %d: err = %v, want ErrKeyFileTooLarge", size, err)
		}
		if got != nil {
			t.Fatalf("size %d: returned %d bytes with error", size, len(got))
		}
		if !strings.Contains(err.Error(), p) {
			t.Fatalf("size %d: err %q should name the resolved path", size, err)
		}
	}
}
