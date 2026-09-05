//go:build !windows

package keysource

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

	// The low-level open (what ReadKeyFile uses after resolution) refuses the link.
	if f, err := openKeyFile(link); err == nil {
		_ = f.Close()
		t.Fatal("openKeyFile followed a symlink")
	}
	if err := CheckKeyFile(link); err == nil {
		t.Fatal("CheckKeyFile followed a symlink")
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
