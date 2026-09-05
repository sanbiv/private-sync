package remote

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/execx"
)

const (
	mid   = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	other = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
)

// fakeRclone records rclone invocations and captures every --files-from list
// while it still exists.
type fakeRclone struct {
	cmds   []execx.Cmd
	lists  []string // content of the --files-from file per call ("" when absent)
	paths  []string // the --files-from path per call
	script func(args []string, call int) (execx.Result, bool)
}

func newFakeRclone() *fakeRclone {
	return &fakeRclone{script: func([]string, int) (execx.Result, bool) { return execx.Result{}, false }}
}

func filesFromArg(args []string) string {
	for i, a := range args {
		if a == "--files-from" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func (f *fakeRclone) runner() execx.Runner {
	return execx.Fake(func(c execx.Cmd) (execx.Result, error) {
		f.cmds = append(f.cmds, c)
		list := ""
		p := filesFromArg(c.Args)
		if p != "" {
			data, err := os.ReadFile(p)
			if err != nil {
				return exit(1, "files-from unreadable: "+err.Error()), nil
			}
			list = string(data)
		}
		f.lists = append(f.lists, list)
		f.paths = append(f.paths, p)
		if res, ok := f.script(c.Args, len(f.cmds)); ok {
			return res, nil
		}
		return execx.Result{}, nil
	})
}

func newRcloneVault(t *testing.T, f *fakeRclone, cfg config.RcloneRemote) (Remote, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "vault")
	r, err := NewRclone(cfg, dir, Options{MachineID: mid, MachineName: "mac", Runner: f.runner()})
	if err != nil {
		t.Fatal(err)
	}
	return r, dir
}

func assertCmd(t *testing.T, c execx.Cmd, dir string, want []string) {
	t.Helper()
	if c.Name != "rclone" {
		t.Errorf("name = %q", c.Name)
	}
	if c.Stdin != nil {
		t.Errorf("stdin set")
	}
	if c.OnStderr == nil {
		t.Errorf("no OnStderr")
	}
	if c.Dir != dir {
		t.Errorf("dir = %q, want %q", c.Dir, dir)
	}
	if !slices.Equal(c.Args, want) {
		t.Errorf("args =\n  %q\nwant\n  %q", c.Args, want)
	}
}

func TestRcloneFetchArgs(t *testing.T) {
	f := newFakeRclone()
	r, dir := newRcloneVault(t, f, config.RcloneRemote{Remote: "gdrive", Path: "backup/vault"})
	if r.Name() != "rclone" {
		t.Errorf("Name = %q", r.Name())
	}
	if err := r.Fetch(context.Background(), testLog(t)); err != nil {
		t.Fatal(err)
	}
	if len(f.cmds) != 2 {
		t.Fatalf("got %d commands, want 2", len(f.cmds))
	}
	assertCmd(t, f.cmds[0], dir, []string{"copy", "gdrive:backup/vault/blobs", filepath.Join(dir, "blobs"), "--ignore-existing"})
	assertCmd(t, f.cmds[1], dir, []string{
		"copy", "gdrive:backup/vault", dir,
		"--exclude", "/blobs/**",
		"--exclude", "machines/" + mid + ".json.enc",
		"--exclude", "projects/*/meta/" + mid + ".json.enc",
		"--exclude", "projects/*/state/" + mid + ".json.enc",
		"--exclude", ".psv-tmp-*",
	})
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("vault dir not created: %v", err)
	}
}

func TestRcloneFetchEmptyRemote(t *testing.T) {
	cases := []struct {
		name   string
		codes  []int
		wantOK bool
		calls  int
	}{
		{"both dirs missing", []int{3, 3}, true, 2},
		{"only blobs missing", []int{3, 0}, true, 2},
		{"blobs fail hard", []int{2, 0}, false, 1},
		{"vault fails hard", []int{0, 7}, false, 2},
		{"all good", []int{0, 0}, true, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeRclone()
			f.script = func(args []string, call int) (execx.Result, bool) {
				code := tc.codes[call-1]
				if code == 3 {
					return exit(3, "2026/09/05 10:00:00 ERROR : : error reading source directory: directory not found\n"), true
				}
				if code != 0 {
					return exit(code, "2026/09/05 10:00:00 Failed to copy: something broke\n"), true
				}
				return execx.Result{}, true
			}
			r, _ := newRcloneVault(t, f, config.RcloneRemote{Remote: "gdrive", Path: "v"})
			err := r.Fetch(context.Background(), nil)
			if (err == nil) != tc.wantOK {
				t.Fatalf("err = %v, wantOK %v", err, tc.wantOK)
			}
			if len(f.cmds) != tc.calls {
				t.Errorf("calls = %d, want %d", len(f.cmds), tc.calls)
			}
		})
	}
}

func TestRclonePushBlobsFirstThenOwnFiles(t *testing.T) {
	f := newFakeRclone()
	r, dir := newRcloneVault(t, f, config.RcloneRemote{Remote: "gdrive:", Path: "/vault/"})
	mustWrite(t, dir, "blobs/aa/x.enc", "x")
	mustWrite(t, dir, "blobs/bb/y.enc", "y")
	mustWrite(t, dir, "blobs/cc/unwritten.enc", "z")
	mustWrite(t, dir, "machines/"+mid+".json.enc", "me")
	mustWrite(t, dir, "machines/"+other+".json.enc", "them")
	mustWrite(t, dir, "projects/p1/meta/"+mid+".json.enc", "meta")
	mustWrite(t, dir, "projects/p1/state/"+mid+".json.enc", "state")
	mustWrite(t, dir, "projects/p1/state/"+other+".json.enc", "their state")
	mustWrite(t, dir, "projects/p2/state/"+mid+".json.enc", "state2")
	mustWrite(t, dir, "vault.json", `{"id":"v"}`)
	mustWrite(t, dir, ".gitattributes", "*.enc binary\n")
	mustWrite(t, dir, ".psv-tmp-abc", "temp")

	written := []string{
		"blobs/bb/y.enc",
		"blobs/aa/x.enc",
		"blobs/aa/x.enc", // duplicate
		"./blobs/bb/y.enc",
		filepath.Join(dir, "blobs", "aa", "x.enc"), // absolute inside the vault
		"blobs/zz/missing.enc",                     // not on disk
		"machines/" + mid + ".json.enc",            // not a blob
		"../outside/blobs/q.enc",                   // escapes the vault
		"",
	}
	if err := r.Push(context.Background(), written, testLog(t)); err != nil {
		t.Fatal(err)
	}
	if len(f.cmds) != 2 {
		t.Fatalf("got %d commands, want 2:\n%v", len(f.cmds), f.cmds)
	}
	// Blobs first.
	assertCmd(t, f.cmds[0], dir, []string{"copy", dir, "gdrive:vault", "--files-from", f.paths[0], "--no-traverse", "--ignore-existing"})
	if f.lists[0] != "blobs/aa/x.enc\nblobs/bb/y.enc\n" {
		t.Errorf("blob list = %q", f.lists[0])
	}
	// Then the own files.
	assertCmd(t, f.cmds[1], dir, []string{"copy", dir, "gdrive:vault", "--files-from", f.paths[1], "--no-traverse"})
	wantOwn := strings.Join([]string{
		".gitattributes",
		"machines/" + mid + ".json.enc",
		"projects/p1/meta/" + mid + ".json.enc",
		"projects/p1/state/" + mid + ".json.enc",
		"projects/p2/state/" + mid + ".json.enc",
		"vault.json",
	}, "\n") + "\n"
	if f.lists[1] != wantOwn {
		t.Errorf("own list =\n%q\nwant\n%q", f.lists[1], wantOwn)
	}
	for i, p := range f.paths {
		if p == "" {
			t.Fatalf("call %d has no --files-from", i)
		}
		if !strings.HasPrefix(p, os.TempDir()) {
			t.Errorf("list %d not in os.TempDir(): %q", i, p)
		}
		if _, err := os.Stat(p); err == nil {
			t.Errorf("list %d not removed after Push: %q", i, p)
		}
	}
}

func TestRclonePushWithoutBlobs(t *testing.T) {
	f := newFakeRclone()
	r, dir := newRcloneVault(t, f, config.RcloneRemote{Remote: "gdrive", Path: "v"})
	mustWrite(t, dir, "machines/"+mid+".json.enc", "me")
	if err := r.Push(context.Background(), []string{"machines/" + mid + ".json.enc"}, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.cmds) != 1 {
		t.Fatalf("got %d commands, want 1", len(f.cmds))
	}
	assertCmd(t, f.cmds[0], dir, []string{"copy", dir, "gdrive:v", "--files-from", f.paths[0], "--no-traverse"})
	if f.lists[0] != "machines/"+mid+".json.enc\n" {
		t.Errorf("list = %q", f.lists[0])
	}
}

func TestRclonePushNothing(t *testing.T) {
	f := newFakeRclone()
	r, _ := newRcloneVault(t, f, config.RcloneRemote{Remote: "gdrive", Path: "v"})
	if err := r.Push(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.cmds) != 0 {
		t.Errorf("expected no rclone calls, got %d", len(f.cmds))
	}
}

func TestRclonePushStopsOnBlobFailure(t *testing.T) {
	f := newFakeRclone()
	f.script = func(args []string, call int) (execx.Result, bool) {
		if call == 1 {
			return exit(2, "2026/09/05 Failed to copy: boom\n"), true
		}
		return execx.Result{}, false
	}
	r, dir := newRcloneVault(t, f, config.RcloneRemote{Remote: "gdrive", Path: "v"})
	mustWrite(t, dir, "blobs/aa/x.enc", "x")
	mustWrite(t, dir, "machines/"+mid+".json.enc", "me")
	err := r.Push(context.Background(), []string{"blobs/aa/x.enc"}, nil)
	if err == nil {
		t.Fatal("Push succeeded despite blob failure")
	}
	if len(f.cmds) != 1 {
		t.Errorf("own files pushed after blob failure: %d calls", len(f.cmds))
	}
	if _, statErr := os.Stat(f.paths[0]); statErr == nil {
		t.Errorf("files-from list leaked after failure")
	}
}

func TestRclonePrepare(t *testing.T) {
	f := newFakeRclone()
	r, dir := newRcloneVault(t, f, config.RcloneRemote{Remote: "gdrive", Path: "backup/vault"})
	if err := r.Prepare(context.Background(), testLog(t)); err != nil {
		t.Fatal(err)
	}
	if len(f.cmds) != 2 {
		t.Fatalf("got %d commands, want 2", len(f.cmds))
	}
	assertCmd(t, f.cmds[0], dir, []string{"lsd", "gdrive:"})
	assertCmd(t, f.cmds[1], dir, []string{"mkdir", "gdrive:backup/vault"})
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("vault dir: %v mode %v", err, st)
	}
}

func TestRclonePrepareErrors(t *testing.T) {
	cases := []struct {
		name    string
		stderr  string
		want    error
		contain string
	}{
		{"not configured", `2026/09/05 Failed to create file system for "gdrive:": didn't find section in config file` + "\n", nil, "not configured"},
		{"token expired", "2026/09/05 Failed to lsd: couldn't fetch token: invalid_grant: token expired\n", ErrAuth, "rclone config reconnect gdrive:"},
		{"forbidden", "2026/09/05 ERROR : : error listing: googleapi: Error 403: Forbidden\n", ErrAuth, ""},
		{"dns", "2026/09/05 Failed to lsd: Get \"https://www.googleapis.com/x\": dial tcp: lookup www.googleapis.com: no such host\n", ErrNetwork, ""},
		{"refused", "2026/09/05 Failed to lsd: dial tcp 1.2.3.4:443: connect: connection refused\n", ErrNetwork, ""},
		{"other", "2026/09/05 Failed to lsd: something odd\n", nil, "something odd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeRclone()
			f.script = func(args []string, call int) (execx.Result, bool) {
				return exit(1, tc.stderr), true
			}
			r, _ := newRcloneVault(t, f, config.RcloneRemote{Remote: "gdrive", Path: "v"})
			err := r.Prepare(context.Background(), nil)
			if err == nil {
				t.Fatal("Prepare succeeded")
			}
			if len(f.cmds) != 1 {
				t.Errorf("mkdir attempted after lsd failure")
			}
			for _, s := range []error{ErrAuth, ErrNetwork} {
				if errors.Is(err, s) != (tc.want == s) {
					t.Errorf("errors.Is(err, %v) = %v; err = %v", s, errors.Is(err, s), err)
				}
			}
			if tc.contain != "" && !strings.Contains(err.Error(), tc.contain) {
				t.Errorf("err %q lacks %q", err, tc.contain)
			}
			var ee *execx.ExitError
			if !errors.As(err, &ee) {
				t.Errorf("ExitError lost: %v", err)
			}
		})
	}
}

func TestRcloneStderrIsLogged(t *testing.T) {
	f := newFakeRclone()
	f.script = func(args []string, call int) (execx.Result, bool) {
		return execx.Result{Stderr: []byte("Transferred: 1 / 1\n\n")}, true
	}
	r, _ := newRcloneVault(t, f, config.RcloneRemote{Remote: "gdrive", Path: "v"})
	var lines []string
	if err := r.Prepare(context.Background(), func(s string) { lines = append(lines, s) }); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(lines, "rclone: Transferred: 1 / 1") {
		t.Errorf("stderr not streamed to log: %v", lines)
	}
	if !slices.Contains(lines, "$ rclone lsd gdrive:") {
		t.Errorf("command not logged: %v", lines)
	}
}

func TestNewRcloneValidation(t *testing.T) {
	cases := []struct {
		name       string
		cfg        config.RcloneRemote
		dir        string
		machine    string
		wantOK     bool
		wantTarget string
		wantBlobs  string
	}{
		{"ok", config.RcloneRemote{Remote: "gdrive", Path: "vault"}, "v", mid, true, "gdrive:vault", "gdrive:vault/blobs"},
		{"trailing colon and slashes", config.RcloneRemote{Remote: "gdrive:", Path: "/a//b/"}, "v", mid, true, "gdrive:a/b", "gdrive:a/b/blobs"},
		{"empty path", config.RcloneRemote{Remote: "gdrive"}, "v", mid, true, "gdrive:", "gdrive:blobs"},
		{"empty remote", config.RcloneRemote{Path: "v"}, "v", mid, false, "", ""},
		{"remote with path", config.RcloneRemote{Remote: "gdrive:vault"}, "v", mid, false, "", ""},
		{"option-looking remote", config.RcloneRemote{Remote: "--config"}, "v", mid, false, "", ""},
		{"empty dir", config.RcloneRemote{Remote: "gdrive"}, "", mid, false, "", ""},
		{"empty machine", config.RcloneRemote{Remote: "gdrive"}, "v", "", false, "", ""},
		{"bad machine", config.RcloneRemote{Remote: "gdrive"}, "v", "a/b", false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewRclone(tc.cfg, tc.dir, Options{MachineID: tc.machine, Runner: newFakeRclone().runner()})
			if (err == nil) != tc.wantOK {
				t.Fatalf("err = %v, wantOK %v", err, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			rr := r.(*rcloneRemote)
			if got := rr.target(""); got != tc.wantTarget {
				t.Errorf("target = %q, want %q", got, tc.wantTarget)
			}
			if got := rr.target("blobs"); got != tc.wantBlobs {
				t.Errorf("blobs target = %q, want %q", got, tc.wantBlobs)
			}
			if !filepath.IsAbs(rr.dir) {
				t.Errorf("dir not absolute: %q", rr.dir)
			}
		})
	}
}
