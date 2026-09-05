//go:build unix

package keysource

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/sanbiv/private-sync/internal/config"
)

func TestFileSourceForeignOwner(t *testing.T) {
	dir := t.TempDir()
	path := writeTemp(t, dir, "key", "secret\n", 0o600)

	orig := getuid
	t.Cleanup(func() { getuid = orig })
	getuid = func() int { return os.Getuid() + 1 }

	src, err := FromConfig(config.KeyConfig{Source: config.KeyFile, File: config.FileKey{Path: path}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = src.Passphrase(context.Background(), nil)
	if err == nil {
		t.Fatal("expected ownership error")
	}
	if !errors.Is(err, ErrKeyFilePerms) {
		t.Fatalf("err = %v, want errors.Is ErrKeyFilePerms", err)
	}
	if !strings.Contains(err.Error(), "is owned by uid") || !strings.Contains(err.Error(), "chmod 600 ") {
		t.Fatalf("err = %q", err.Error())
	}

	// Same file, correct uid: accepted.
	getuid = orig
	got, err := src.Passphrase(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "secret" {
		t.Fatalf("got %q", got)
	}
}

// TestOpenKeyFileRefusesSymlink pins the open-once hardening: ReadKeyFile resolves
// symlinks first, then opens the resolved path with O_NOFOLLOW, so a symlink
// planted at the resolved path between resolution and open is refused instead of
// being followed to a file whose permissions were never checked.
func TestOpenKeyFileRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := writeTemp(t, dir, "target", "secret\n", 0o600)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	// The low-level open (what ReadKeyFile uses after resolution) refuses the link,
	// and says so instead of surfacing the raw ELOOP text.
	f, err := openKeyFile(link)
	if err == nil {
		_ = f.Close()
		t.Fatal("openKeyFile followed a symlink")
	}
	if !strings.Contains(err.Error(), "is a symlink") || !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("openKeyFile(symlink) err = %v, want a clear symlink message wrapping ELOOP", err)
	}
	// The exported check resolves first, exactly like ReadKeyFile.
	if err := CheckKeyFile(link); err != nil {
		t.Fatalf("CheckKeyFile(symlink to good file) = %v", err)
	}

	// The public path still works because ResolveKeyFile resolves the link first.
	got, err := ReadKeyFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "secret" {
		t.Fatalf("got %q", got)
	}
	// And the bytes come from the very descriptor that was checked: a 0644 target
	// behind an otherwise fine link is rejected with the target's path.
	bad := writeTemp(t, dir, "badtarget", "x\n", 0o644)
	badLink := filepath.Join(dir, "badlink")
	if err := os.Symlink(bad, badLink); err != nil {
		t.Fatal(err)
	}
	_, err = ReadKeyFile(badLink)
	if !errors.Is(err, ErrKeyFilePerms) || !strings.Contains(err.Error(), "badtarget has mode 0644") {
		t.Fatalf("err = %v", err)
	}
}

// TestCheckKeyFileNotReadable: a file we cannot open is reported as an open error
// rather than passing a stat-only check and failing later on read.
func TestCheckKeyFileNotReadable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can read everything")
	}
	dir := t.TempDir()
	p := writeTemp(t, dir, "unreadable", "secret\n", 0o000)
	t.Cleanup(func() { _ = os.Chmod(p, 0o600) })
	if err := CheckKeyFile(p); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("CheckKeyFile err = %v, want fs.ErrPermission", err)
	}
	if _, err := ReadKeyFile(p); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("ReadKeyFile err = %v, want fs.ErrPermission", err)
	}
}

// TestCheckKeyFileResolvesLikeReadKeyFile pins the exported CheckKeyFile contract:
// it accepts the same user-facing paths ReadKeyFile does (~, symlinks) and reports
// problems against the resolved target, never the opaque "too many levels of
// symbolic links" error from the O_NOFOLLOW open.
func TestCheckKeyFileResolvesLikeReadKeyFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	good := writeTemp(t, home, "good", "secret\n", 0o600)
	bad := writeTemp(t, home, "bad", "secret\n", 0o644)
	badResolved, err := filepath.EvalSymlinks(bad)
	if err != nil {
		t.Fatal(err)
	}
	mkLink := func(name, target string) string {
		link := filepath.Join(home, name)
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		return link
	}
	goodLink := mkLink("goodlink", good)
	badLink := mkLink("badlink", bad)
	chain := mkLink("chain", goodLink)
	dangling := mkLink("dangling", filepath.Join(home, "gone"))
	if err := os.Mkdir(filepath.Join(home, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		path    string
		wantErr error
		errText string
	}{
		{name: "plain file", path: good},
		{name: "symlink", path: goodLink},
		{name: "symlink chain", path: chain},
		{name: "tilde", path: "~/good"},
		{name: "tilde symlink", path: "~/goodlink"},
		{name: "symlink to 0644 reports target", path: badLink, wantErr: ErrKeyFilePerms, errText: "key file " + badResolved + " has mode 0644"},
		{name: "dangling symlink", path: dangling, wantErr: fs.ErrNotExist, errText: "does not exist"},
		{name: "missing", path: filepath.Join(home, "nope"), wantErr: fs.ErrNotExist, errText: "does not exist"},
		{name: "directory", path: filepath.Join(home, "dir"), errText: "not a regular file"},
		{name: "empty path", path: "", errText: "empty path"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckKeyFile(tc.path)
			if tc.wantErr == nil && tc.errText == "" {
				if err != nil {
					t.Fatalf("CheckKeyFile(%q) = %v", tc.path, err)
				}
				// Agreement with ReadKeyFile on success.
				if _, rerr := ReadKeyFile(tc.path); rerr != nil {
					t.Fatalf("ReadKeyFile(%q) = %v but CheckKeyFile passed", tc.path, rerr)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckKeyFile(%q) = nil, want error", tc.path)
			}
			if strings.Contains(err.Error(), "too many levels of symbolic links") {
				t.Fatalf("opaque ELOOP surfaced: %v", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want errors.Is %v", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.errText) {
				t.Fatalf("err = %q, want substring %q", err.Error(), tc.errText)
			}
			// Agreement with ReadKeyFile on failure (same sentinel).
			_, rerr := ReadKeyFile(tc.path)
			if rerr == nil {
				t.Fatalf("ReadKeyFile(%q) passed but CheckKeyFile failed: %v", tc.path, err)
			}
			if tc.wantErr != nil && !errors.Is(rerr, tc.wantErr) {
				t.Fatalf("ReadKeyFile err = %v, want errors.Is %v", rerr, tc.wantErr)
			}
		})
	}
}

// TestFileSourcePermsHintShellQuoted: the remedy after "run:" must be a command
// that works when pasted, so paths with spaces or shell metacharacters are quoted
// there (and only there: the descriptive prefix keeps the verbatim path).
func TestFileSourcePermsHintShellQuoted(t *testing.T) {
	tests := []struct {
		name string
		dir  string
	}{
		{"space", "My Keys"},
		{"single quote", "it's"},
		{"dollar", "$HOME"},
		{"glob and parens", "k*(1)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			dir := filepath.Join(base, tc.dir)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			p := writeTemp(t, dir, "key", "s", 0o644)
			resolved, err := filepath.EvalSymlinks(p)
			if err != nil {
				t.Fatal(err)
			}
			q := shellQuote(resolved)
			if q == resolved {
				t.Fatalf("test path %q needs no quoting; pick a nastier one", resolved)
			}

			// mode hint
			_, err = ReadKeyFile(p)
			if !errors.Is(err, ErrKeyFilePerms) {
				t.Fatalf("err = %v", err)
			}
			want := fmt.Sprintf("key file %s has mode 0644; run: chmod 600 %s", resolved, q)
			if err.Error() != want {
				t.Fatalf("err = %q\nwant %q", err.Error(), want)
			}
			if err := CheckKeyFile(p); err == nil || err.Error() != want {
				t.Fatalf("CheckKeyFile err = %v, want %q", err, want)
			}

			// ownership hint
			if err := os.Chmod(p, 0o600); err != nil {
				t.Fatal(err)
			}
			orig := getuid
			t.Cleanup(func() { getuid = orig })
			fake := os.Getuid() + 1
			getuid = func() int { return fake }
			_, err = ReadKeyFile(p)
			if !errors.Is(err, ErrKeyFilePerms) {
				t.Fatalf("err = %v", err)
			}
			want = fmt.Sprintf("key file %s is owned by uid %d, not the current user (uid %d); run: chown %d %s && chmod 600 %s", resolved, os.Getuid(), fake, fake, q, q)
			if err.Error() != want {
				t.Fatalf("err = %q\nwant %q", err.Error(), want)
			}
		})
	}
}

// TestPermsHintRoundTripsThroughShell executes the quoted remedy with /bin/sh to
// prove the hint is actually pasteable: after running it the file is 0600 and
// ReadKeyFile succeeds.
func TestPermsHintRoundTripsThroughShell(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	for _, sub := range []string{"My Keys", "it's a $dir", "a;b(c)*"} {
		t.Run(sub, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), sub)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			p := writeTemp(t, dir, "key", "secret\n", 0o644)
			_, err := ReadKeyFile(p)
			if !errors.Is(err, ErrKeyFilePerms) {
				t.Fatalf("err = %v", err)
			}
			_, remedy, ok := strings.Cut(err.Error(), "; run: ")
			if !ok {
				t.Fatalf("no remedy in %q", err.Error())
			}
			cmd := exec.Command(sh, "-c", remedy)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("remedy %q failed: %v\n%s", remedy, err, out)
			}
			got, err := ReadKeyFile(p)
			if err != nil {
				t.Fatalf("after remedy: %v", err)
			}
			if string(got) != "secret" {
				t.Fatalf("got %q", got)
			}
		})
	}
}
