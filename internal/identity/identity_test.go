package identity

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sanbiv/private-sync/internal/execx"
)

func TestNormalizeGitURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{"github ssh scp", "git@github.com:foo/bar.git", "github.com/foo/bar", true},
		{"github https", "https://github.com/foo/bar.git", "github.com/foo/bar", true},
		{"github https no suffix", "https://github.com/foo/bar", "github.com/foo/bar", true},
		{"github ssh scheme", "ssh://git@github.com/foo/bar.git", "github.com/foo/bar", true},
		{"ssh scheme with port", "ssh://git@github.com:22/foo/bar.git", "github.com/foo/bar", true},
		{"ssh scheme with odd port", "ssh://user@git.example.com:2222/team/repo.git", "git.example.com/team/repo", true},
		{"https with port", "https://git.example.com:8443/team/repo.git", "git.example.com/team/repo", true},
		{"git protocol", "git://github.com/foo/bar.git", "github.com/foo/bar", true},
		{"http", "http://github.com/foo/bar", "github.com/foo/bar", true},
		{"uppercase", "HTTPS://GitHub.com/Foo/Bar.GIT", "github.com/foo/bar", true},
		{"uppercase scp", "git@GitHub.com:Foo/Bar.git", "github.com/foo/bar", true},
		{"trailing slash", "https://github.com/foo/bar/", "github.com/foo/bar", true},
		{"trailing slash and git", "https://github.com/foo/bar.git/", "github.com/foo/bar", true},
		{"double trailing slash", "https://github.com/foo/bar//", "github.com/foo/bar", true},
		{"gitlab subgroups scp", "git@gitlab.com:group/sub/sub2/repo.git", "gitlab.com/group/sub/sub2/repo", true},
		{"gitlab subgroups https", "https://gitlab.com/group/sub/sub2/repo.git", "gitlab.com/group/sub/sub2/repo", true},
		{"gitlab subgroups ssh scheme", "ssh://git@gitlab.com:2222/group/sub/repo.git", "gitlab.com/group/sub/repo", true},
		{"azure https", "https://org@dev.azure.com/org/project/_git/repo", "dev.azure.com/org/project/repo", true},
		{"azure ssh", "git@ssh.dev.azure.com:v3/org/project/repo", "dev.azure.com/org/project/repo", true},
		{"azure ssh scheme with port", "ssh://git@ssh.dev.azure.com:22/v3/org/project/repo", "dev.azure.com/org/project/repo", true},
		{"azure v3 only on other hosts kept", "git@ssh.example.com:v3/org/repo.git", "example.com/v3/org/repo", true},
		{"scp user with password", "user:pass@host:path/repo.git", "host/path/repo", true},
		{"scp user with password and port-like path", "deploy:s3cr3t@git.example.com:team/repo", "git.example.com/team/repo", true},
		{"scp at sign in path", "git@github.com:foo/b@r.git", "github.com/foo/b@r", true},
		{"azure legacy visualstudio", "https://org.visualstudio.com/project/_git/repo", "org.visualstudio.com/project/repo", true},
		{"www prefix", "https://www.example.com/foo/bar.git", "example.com/foo/bar", true},
		{"ssh prefix", "ssh://git@ssh.example.com/foo/bar.git", "example.com/foo/bar", true},
		{"host alias scp", "github-work:foo/bar.git", "github-work/foo/bar", true},
		{"host alias with user", "me@work-alias:foo/bar", "work-alias/foo/bar", true},
		{"user with password", "https://user:secret@github.com/foo/bar.git", "github.com/foo/bar", true},
		{"whitespace around", "  git@github.com:foo/bar.git \n", "github.com/foo/bar", true},
		{"bitbucket", "https://user@bitbucket.org/team/repo.git", "bitbucket.org/team/repo", true},
		{"sourcehut tilde", "git@git.sr.ht:~user/repo", "git.sr.ht/~user/repo", true},
		{"tilde path ssh scheme", "ssh://git@host/~user/repo.git", "host/~user/repo", true},
		{"leading slash scp", "git@github.com:/foo/bar.git", "github.com/foo/bar", true},
		{"ipv6 bracket with port", "ssh://git@[::1]:2222/foo/bar.git", "[::1]/foo/bar", true},

		{"empty", "", "", false},
		{"whitespace", "   ", "", false},
		{"file scheme", "file:///home/me/repo.git", "", false},
		{"absolute path", "/home/me/repo", "", false},
		{"relative path", "../repo", "", false},
		{"dot path", "./repo", "", false},
		{"bare name", "repo", "", false},
		{"windows drive", `C:\Users\me\repo`, "", false},
		{"windows drive forward", "c:/users/me/repo", "", false},
		{"scheme only host", "https://github.com", "", false},
		{"scheme only host slash", "https://github.com/", "", false},
		{"scp empty path", "git@github.com:", "", false},
		{"host with slash before colon", "host/path:thing", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := NormalizeGitURL(tt.raw)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("NormalizeGitURL(%q) = (%q, %v), want (%q, %v)", tt.raw, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestNormalizeGitURL_Equivalence(t *testing.T) {
	forms := []string{
		"git@github.com:foo/bar.git",
		"https://github.com/foo/bar.git",
		"https://github.com/foo/bar",
		"ssh://git@github.com/foo/bar.git",
		"ssh://git@github.com:22/foo/bar.git",
		"git://github.com/foo/bar.git",
		"https://www.github.com/Foo/Bar/",
	}
	for _, f := range forms {
		got, ok := NormalizeGitURL(f)
		if !ok || got != "github.com/foo/bar" {
			t.Errorf("%q → (%q, %v)", f, got, ok)
		}
	}

	// Azure DevOps: ssh and https clones of one repository must agree.
	azure := []string{
		"https://org@dev.azure.com/org/project/_git/repo",
		"https://dev.azure.com/org/project/_git/repo",
		"git@ssh.dev.azure.com:v3/org/project/repo",
		"ssh://git@ssh.dev.azure.com:22/v3/org/project/repo",
	}
	for _, f := range azure {
		got, ok := NormalizeGitURL(f)
		if !ok || got != "dev.azure.com/org/project/repo" {
			t.Errorf("%q → (%q, %v)", f, got, ok)
		}
	}
}

// ---- Detect helpers ----------------------------------------------------------

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeGit returns a runner that answers rev-parse with top/prefix and config
// with the given remote lines. It records how many calls it received.
func fakeGit(top, prefix string, remotes map[string]string, calls *int) execx.Runner {
	return execx.Fake(func(c execx.Cmd) (execx.Result, error) {
		if calls != nil {
			*calls++
		}
		if c.Name != "git" {
			return execx.Result{}, errors.New("unexpected binary " + c.Name)
		}
		args := strings.Join(c.Args, " ")
		switch {
		case strings.Contains(args, "rev-parse --show-toplevel --show-prefix"):
			return execx.Result{Stdout: []byte(top + "\n" + prefix + "\n")}, nil
		case strings.Contains(args, "config --get-regexp"):
			if len(remotes) == 0 {
				return execx.Result{ExitCode: 1}, nil
			}
			var sb strings.Builder
			// deterministic order: origin first, then the rest sorted
			if u, ok := remotes["origin"]; ok {
				sb.WriteString("remote.origin.url " + u + "\n")
			}
			for _, n := range sortedKeys(remotes) {
				if n == "origin" {
					continue
				}
				sb.WriteString("remote." + n + ".url " + remotes[n] + "\n")
			}
			return execx.Result{Stdout: []byte(sb.String())}, nil
		}
		return execx.Result{ExitCode: 128, Stderr: []byte("fatal: unknown")}, nil
	})
}

func sortedKeys(m map[string]string) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	for i := 1; i < len(ks); i++ {
		for j := i; j > 0 && ks[j] < ks[j-1]; j-- {
			ks[j], ks[j-1] = ks[j-1], ks[j]
		}
	}
	return ks
}

func failingRunner(calls *int) execx.Runner {
	return execx.Fake(func(c execx.Cmd) (execx.Result, error) {
		if calls != nil {
			*calls++
		}
		return execx.Result{}, &exec.Error{Name: "git", Err: exec.ErrNotFound}
	})
}

func strs(fps []Fingerprint) []string {
	var out []string
	for _, f := range fps {
		out = append(out, f.String())
	}
	return out
}

func assertFps(t *testing.T, got []Fingerprint, want []string) {
	t.Helper()
	if g := strs(got); !reflect.DeepEqual(g, want) {
		t.Fatalf("fingerprints:\n got  %v\n want %v", g, want)
	}
}

// ---- Detect with the runner --------------------------------------------------

func TestDetect_GitViaRunner_Root(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	r := fakeGit(dir, "", map[string]string{
		"origin":   "git@github.com:Foo/Bar.git",
		"upstream": "https://github.com/other/bar.git",
	}, &calls)
	got, err := Detect(context.Background(), dir, r)
	if err != nil {
		t.Fatal(err)
	}
	assertFps(t, got, []string{
		"git:github.com/foo/bar",
		"git:github.com/other/bar",
		"dir:" + filepath.Base(dir),
	})
	if calls != 2 {
		t.Fatalf("runner calls = %d, want 2", calls)
	}
	for _, f := range got[:2] {
		if f.Level != LevelStrong || f.Kind != "git" {
			t.Errorf("%v: level %d kind %q", f, f.Level, f.Kind)
		}
	}
	if last := got[len(got)-1]; last.Level != LevelDir {
		t.Errorf("last fingerprint %v has level %d", last, last.Level)
	}
}

func TestDetect_GitViaRunner_Prefix(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "services", "api")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	r := fakeGit(root, "services/api/", map[string]string{"origin": "git@github.com:acme/mono.git"}, nil)
	got, err := Detect(context.Background(), sub, r)
	if err != nil {
		t.Fatal(err)
	}
	assertFps(t, got, []string{"git:github.com/acme/mono#services/api", "dir:api"})
}

func TestDetect_GitViaRunner_DedupesRemotes(t *testing.T) {
	dir := t.TempDir()
	r := fakeGit(dir, "", map[string]string{
		"origin": "git@github.com:foo/bar.git",
		"mirror": "https://github.com/foo/bar",
	}, nil)
	got, err := Detect(context.Background(), dir, r)
	if err != nil {
		t.Fatal(err)
	}
	assertFps(t, got, []string{"git:github.com/foo/bar", "dir:" + filepath.Base(dir)})
}

func TestDetect_GitViaRunner_NoRemotes(t *testing.T) {
	dir := t.TempDir()
	// A .git config with a remote exists on disk, but git (authoritative) says
	// there are none: the filesystem fallback must not be consulted.
	writeFile(t, filepath.Join(dir, ".git", "config"), "[remote \"origin\"]\n\turl = git@github.com:foo/bar.git\n")
	calls := 0
	r := fakeGit(dir, "", nil, &calls)
	got, err := Detect(context.Background(), dir, r)
	if err != nil {
		t.Fatal(err)
	}
	assertFps(t, got, []string{"dir:" + filepath.Base(dir)})
	if calls != 2 {
		t.Fatalf("runner calls = %d, want 2", calls)
	}
}

func TestDetect_GitViaRunner_SkipsLocalRemotes(t *testing.T) {
	dir := t.TempDir()
	r := fakeGit(dir, "", map[string]string{
		"origin": "/srv/git/repo.git",
		"backup": "file:///mnt/backup/repo.git",
		"cloud":  "https://gitlab.com/g/s/repo.git",
	}, nil)
	got, err := Detect(context.Background(), dir, r)
	if err != nil {
		t.Fatal(err)
	}
	assertFps(t, got, []string{"git:gitlab.com/g/s/repo", "dir:" + filepath.Base(dir)})
}

func TestDetect_RunnerRevParseFails_FallsBack(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".git", "config"), "[remote \"origin\"]\n\turl = git@github.com:foo/fallback.git\n")
	r := execx.Fake(func(c execx.Cmd) (execx.Result, error) {
		return execx.Result{ExitCode: 128, Stderr: []byte("fatal: not a git repository")}, nil
	})
	got, err := Detect(context.Background(), dir, r)
	if err != nil {
		t.Fatal(err)
	}
	assertFps(t, got, []string{"git:github.com/foo/fallback", "dir:" + filepath.Base(dir)})
}

// ---- Detect without git (filesystem fallback) -------------------------------

const sampleConfig = `[core]
	repositoryformatversion = 0
	filemode = true
	bare = false
[remote "origin"]
	url = git@github.com:Acme/Widgets.git
	fetch = +refs/heads/*:refs/remotes/origin/*
; a comment
[branch "main"]
	remote = origin
	merge = refs/heads/main
[remote "upstream"]
	url = "https://gitlab.com/acme/widgets.git"
	fetch = +refs/heads/*:refs/remotes/upstream/*
[remote "local"]
	url = /tmp/widgets
`

func TestDetect_NoGitBinary_GitDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".git", "config"), sampleConfig)
	calls := 0
	got, err := Detect(context.Background(), dir, failingRunner(&calls))
	if err != nil {
		t.Fatal(err)
	}
	assertFps(t, got, []string{
		"git:github.com/acme/widgets",
		"git:gitlab.com/acme/widgets",
		"dir:" + filepath.Base(dir),
	})
	if calls != 1 {
		t.Fatalf("runner calls = %d, want 1", calls)
	}
}

func TestDetect_NilRunner_GitDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".git", "config"), sampleConfig)
	got, err := Detect(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertFps(t, got, []string{
		"git:github.com/acme/widgets",
		"git:gitlab.com/acme/widgets",
		"dir:" + filepath.Base(dir),
	})
}

func TestDetect_NoGitBinary_SubdirPrefix(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "config"), "[remote \"origin\"]\n\turl = git@github.com:acme/mono.git\n")
	sub := filepath.Join(root, "packages", "web")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Detect(context.Background(), sub, failingRunner(nil))
	if err != nil {
		t.Fatal(err)
	}
	assertFps(t, got, []string{"git:github.com/acme/mono#packages/web", "dir:web"})
}

func TestDetect_NoGitBinary_GitFilePointer(t *testing.T) {
	tests := []struct {
		name     string
		absolute bool
	}{
		{"relative gitdir", false},
		{"absolute gitdir", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			real := filepath.Join(base, "store", "repo.git")
			writeFile(t, filepath.Join(real, "config"), "[remote \"origin\"]\n\turl = https://github.com/acme/pointer.git\n")
			work := filepath.Join(base, "work")
			if err := os.MkdirAll(work, 0o755); err != nil {
				t.Fatal(err)
			}
			pointer := "gitdir: ../store/repo.git\n"
			if tt.absolute {
				pointer = "gitdir: " + real + "\n"
			}
			writeFile(t, filepath.Join(work, ".git"), pointer)

			got, err := Detect(context.Background(), work, failingRunner(nil))
			if err != nil {
				t.Fatal(err)
			}
			assertFps(t, got, []string{"git:github.com/acme/pointer", "dir:work"})

			// And from a nested directory, with prefix.
			nested := filepath.Join(work, "a", "b")
			if err := os.MkdirAll(nested, 0o755); err != nil {
				t.Fatal(err)
			}
			got, err = Detect(context.Background(), nested, failingRunner(nil))
			if err != nil {
				t.Fatal(err)
			}
			assertFps(t, got, []string{"git:github.com/acme/pointer#a/b", "dir:b"})
		})
	}
}

func TestDetect_NoGitBinary_LinkedWorktree(t *testing.T) {
	base := t.TempDir()
	main := filepath.Join(base, "main")
	writeFile(t, filepath.Join(main, ".git", "config"), "[remote \"origin\"]\n\turl = git@github.com:acme/wt.git\n")
	wtGitDir := filepath.Join(main, ".git", "worktrees", "feature")
	writeFile(t, filepath.Join(wtGitDir, "commondir"), "../..\n")
	wt := filepath.Join(base, "feature")
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+wtGitDir+"\n")

	got, err := Detect(context.Background(), wt, failingRunner(nil))
	if err != nil {
		t.Fatal(err)
	}
	assertFps(t, got, []string{"git:github.com/acme/wt", "dir:feature"})
}

// A ".git" symlink to a directory (repo metadata kept elsewhere) is the git
// dir itself, not a "gitdir:" pointer file.
func TestDetect_NoGitBinary_GitSymlinkToDir(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real.git")
	writeFile(t, filepath.Join(real, "config"), "[remote \"origin\"]\n\turl = git@github.com:acme/linked.git\n")
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(work, ".git")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	got, err := Detect(context.Background(), work, failingRunner(nil))
	if err != nil {
		t.Fatal(err)
	}
	assertFps(t, got, []string{"git:github.com/acme/linked", "dir:work"})

	// Nested directory: prefix is computed against the symlink's owner, not
	// the symlink target.
	nested := filepath.Join(work, "sub")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err = Detect(context.Background(), nested, failingRunner(nil))
	if err != nil {
		t.Fatal(err)
	}
	assertFps(t, got, []string{"git:github.com/acme/linked#sub", "dir:sub"})

	// Relative symlink target as well.
	work2 := filepath.Join(base, "work2")
	if err := os.MkdirAll(work2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "real.git"), filepath.Join(work2, ".git")); err != nil {
		t.Fatal(err)
	}
	got, err = Detect(context.Background(), work2, failingRunner(nil))
	if err != nil {
		t.Fatal(err)
	}
	assertFps(t, got, []string{"git:github.com/acme/linked", "dir:work2"})
}

// A ".git" symlink to a regular file is treated as a "gitdir:" pointer whose
// relative path resolves against the directory holding the symlink.
func TestDetect_NoGitBinary_GitSymlinkToPointerFile(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "store", "repo.git")
	writeFile(t, filepath.Join(real, "config"), "[remote \"origin\"]\n\turl = https://github.com/acme/symptr.git\n")
	writeFile(t, filepath.Join(base, "pointer.txt"), "gitdir: store/repo.git\n")
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "pointer.txt"), filepath.Join(work, ".git")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	// The relative pointer resolves against work/, so make it reachable there.
	writeFile(t, filepath.Join(work, "store", "repo.git", "config"), "[remote \"origin\"]\n\turl = https://github.com/acme/symptr.git\n")

	got, err := Detect(context.Background(), work, failingRunner(nil))
	if err != nil {
		t.Fatal(err)
	}
	assertFps(t, got, []string{"git:github.com/acme/symptr", "dir:work"})
}

// A dangling ".git" symlink is skipped and the walk continues upward.
func TestDetect_NoGitBinary_DanglingGitSymlink(t *testing.T) {
	base := t.TempDir()
	writeFile(t, filepath.Join(base, ".git", "config"), "[remote \"origin\"]\n\turl = git@github.com:acme/outer.git\n")
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "does-not-exist"), filepath.Join(work, ".git")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	got, err := Detect(context.Background(), work, failingRunner(nil))
	if err != nil {
		t.Fatal(err)
	}
	assertFps(t, got, []string{"git:github.com/acme/outer#work", "dir:work"})
}

func TestDetect_NoGitAtAll(t *testing.T) {
	dir := t.TempDir()
	got, err := Detect(context.Background(), dir, failingRunner(nil))
	if err != nil {
		t.Fatal(err)
	}
	assertFps(t, got, []string{"dir:" + filepath.Base(dir)})
}

func TestDetect_BrokenGitPointer(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".git"), "this is not a pointer\n")
	got, err := Detect(context.Background(), dir, failingRunner(nil))
	if err != nil {
		t.Fatal(err)
	}
	assertFps(t, got, []string{"dir:" + filepath.Base(dir)})
}

func TestDetect_Errors(t *testing.T) {
	dir := t.TempDir()
	if _, err := Detect(context.Background(), filepath.Join(dir, "missing"), failingRunner(nil)); err == nil {
		t.Error("missing dir: expected error")
	}
	f := filepath.Join(dir, "file.txt")
	writeFile(t, f, "x")
	if _, err := Detect(context.Background(), f, failingRunner(nil)); err == nil {
		t.Error("regular file: expected error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Detect(ctx, dir, failingRunner(nil)); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled ctx: err = %v", err)
	}
}

func TestParseGitConfigRemotes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"no remotes", "[core]\n\tbare = false\n", nil},
		{"quoted section", "[remote \"origin\"]\n\turl = a\n", []string{"a"}},
		{"dotted section", "[remote.origin]\nurl=b\n", []string{"b"}},
		{"case insensitive key", "[Remote \"x\"]\n\tURL = c\n", []string{"c"}},
		{"quoted value", "[remote \"x\"]\n\turl = \"d e\"\n", []string{"d e"}},
		{"trailing comment", "[remote \"x\"]\n\turl = f # comment\n", []string{"f"}},
		{"crlf", "[remote \"x\"]\r\n\turl = g\r\n", []string{"g"}},
		{"url outside remote", "[core]\n\turl = nope\n[remote \"x\"]\n\turl = h\n", []string{"h"}},
		{"unterminated section", "[remote \"x\"\n\turl = i\n", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseGitConfigRemotes(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

// ---- Manifests ---------------------------------------------------------------

func TestDetect_Manifests(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  []string // package fingerprints only
	}{
		{"go.mod", map[string]string{"go.mod": "module github.com/Acme/Tool // v2\n\ngo 1.27\n"}, []string{"go:github.com/Acme/Tool"}},
		{"go.mod quoted", map[string]string{"go.mod": "module \"example.com/x\"\n"}, []string{"go:example.com/x"}},
		{"go.mod no module", map[string]string{"go.mod": "go 1.27\n"}, nil},
		{"package.json", map[string]string{"package.json": `{"name": "@Acme/Web", "version": "1.0.0"}`}, []string{"npm:@acme/web"}},
		{"package.json invalid", map[string]string{"package.json": `{not json`}, nil},
		{"package.json no name", map[string]string{"package.json": `{"version": "1"}`}, nil},
		{"Cargo.toml", map[string]string{"Cargo.toml": "[package]\nname = \"my_crate\"\nversion = \"0.1.0\"\n\n[dependencies]\nserde = \"1\"\n"}, []string{"cargo:my_crate"}},
		{"Cargo.toml single quotes", map[string]string{"Cargo.toml": "[package]\nname = 'Quoted' # c\n"}, []string{"cargo:Quoted"}},
		{"Cargo.toml workspace only", map[string]string{"Cargo.toml": "[workspace]\nmembers = [\"a\"]\n"}, nil},
		{"Cargo.toml name in deps ignored", map[string]string{"Cargo.toml": "[dependencies]\nname = \"1\"\n"}, nil},
		{"pyproject project", map[string]string{"pyproject.toml": "[build-system]\nrequires = [\"x\"]\n\n[project]\nname = \"My-Package\"\nversion = \"1\"\n"}, []string{"py:my-package"}},
		{"pyproject poetry", map[string]string{"pyproject.toml": "[tool.poetry]\nname = \"PoetryPkg\"\n"}, []string{"py:poetrypkg"}},
		{"pyproject project wins", map[string]string{"pyproject.toml": "[tool.poetry]\nname = \"poetry\"\n[project]\nname = \"proj\"\n"}, []string{"py:proj"}},
		{"pyproject quoted table", map[string]string{"pyproject.toml": "[tool.\"poetry\"]\nname = \"q\"\n"}, []string{"py:q"}},
		{"composer.json", map[string]string{"composer.json": `{"name": "Vendor/Pkg"}`}, []string{"composer:vendor/pkg"}},
		{"pom.xml", map[string]string{"pom.xml": `<?xml version="1.0"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modelVersion>4.0.0</modelVersion>
  <groupId>com.acme</groupId>
  <artifactId>app</artifactId>
  <dependencies><dependency><groupId>x</groupId><artifactId>y</artifactId></dependency></dependencies>
</project>`}, []string{"maven:com.acme:app"}},
		{"pom.xml parent group", map[string]string{"pom.xml": `<project><parent><groupId>com.parent</groupId><artifactId>p</artifactId></parent><artifactId>child</artifactId></project>`}, []string{"maven:com.parent:child"}},
		{"pom.xml missing artifact", map[string]string{"pom.xml": `<project><groupId>g</groupId></project>`}, nil},
		{"pom.xml invalid", map[string]string{"pom.xml": `<project><groupId>g</groupId`}, nil},
		{"gemspec double", map[string]string{"foo.gemspec": "Gem::Specification.new do |s|\n  s.name = \"foo_gem\".freeze\n  s.version = '1'\nend\n"}, []string{"gem:foo_gem"}},
		{"gemspec single", map[string]string{"bar.gemspec": "Gem::Specification.new do |spec|\n  spec.name        = 'bar'\nend\n"}, []string{"gem:bar"}},
		{"gemspec first sorted", map[string]string{"b.gemspec": "s.name = 'b'", "a.gemspec": "s.name = 'a'"}, []string{"gem:a"}},
		{"gemspec no name", map[string]string{"x.gemspec": "s.version = '1'"}, nil},
		{"Package.swift", map[string]string{"Package.swift": "// swift-tools-version:5.9\nimport PackageDescription\n\nlet package = Package(\n    name: \"MyLib\",\n    products: [.library(name: \"Other\", targets: [\"MyLib\"])]\n)\n"}, []string{"swift:MyLib"}},
		{"Package.swift no name", map[string]string{"Package.swift": "import PackageDescription\n"}, nil},
		{"pubspec.yaml", map[string]string{"pubspec.yaml": "name: My_App\ndescription: x\nenvironment:\n  sdk: '>=3.0.0'\n"}, []string{"dart:my_app"}},
		{"pubspec.yaml invalid", map[string]string{"pubspec.yaml": "name: [unclosed\n"}, nil},
		{"pubspec.yaml no name", map[string]string{"pubspec.yaml": "description: x\n"}, nil},
		{"empty files", map[string]string{"go.mod": "", "package.json": "", "Cargo.toml": "", "pom.xml": "", "pubspec.yaml": ""}, nil},
		{"all kinds in order", map[string]string{
			"go.mod":         "module m\n",
			"package.json":   `{"name":"n"}`,
			"Cargo.toml":     "[package]\nname=\"c\"\n",
			"pyproject.toml": "[project]\nname=\"p\"\n",
			"composer.json":  `{"name":"v/c"}`,
			"pom.xml":        "<project><groupId>g</groupId><artifactId>a</artifactId></project>",
			"z.gemspec":      "s.name = 'g'",
			"Package.swift":  "Package(name: \"s\")",
			"pubspec.yaml":   "name: d\n",
		}, []string{"go:m", "npm:n", "cargo:c", "py:p", "composer:v/c", "maven:g:a", "gem:g", "swift:s", "dart:d"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range tt.files {
				writeFile(t, filepath.Join(dir, name), content)
			}
			got, err := Detect(context.Background(), dir, failingRunner(nil))
			if err != nil {
				t.Fatal(err)
			}
			want := append(append([]string{}, tt.want...), "dir:"+filepath.Base(dir))
			assertFps(t, got, want)
			for _, f := range got[:len(got)-1] {
				if f.Level != LevelPackage {
					t.Errorf("%v: level %d, want LevelPackage", f, f.Level)
				}
			}
		})
	}
}

func TestDetect_ManifestsOnlyInDir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module parent\n")
	sub := filepath.Join(root, "child")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Detect(context.Background(), sub, failingRunner(nil))
	if err != nil {
		t.Fatal(err)
	}
	assertFps(t, got, []string{"dir:child"})
}

func TestDetect_ManifestDirectoryIgnored(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "go.mod"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Detect(context.Background(), dir, failingRunner(nil))
	if err != nil {
		t.Fatal(err)
	}
	assertFps(t, got, []string{"dir:" + filepath.Base(dir)})
}

// Gemspec detection must not treat the project path as a glob pattern: a
// directory whose name carries metacharacters still finds its gemspec, and
// never picks up a sibling directory that the pattern would also match.
func TestDetect_GemspecInGlobMetaDir(t *testing.T) {
	base := t.TempDir()
	tests := []struct {
		dirName string
		want    string
	}{
		{"foo[1]", "bracketed"},
		{"foo1", "sibling"},
		{"star*proj", "star"},
		{"q?proj", "question"},
		{"[archive]", "archive"},
	}
	for _, tt := range tests {
		writeFile(t, filepath.Join(base, tt.dirName, "x.gemspec"),
			"Gem::Specification.new do |s|\n  s.name = '"+tt.want+"'\nend\n")
	}
	for _, tt := range tests {
		t.Run(tt.dirName, func(t *testing.T) {
			dir := filepath.Join(base, tt.dirName)
			got, err := Detect(context.Background(), dir, failingRunner(nil))
			if err != nil {
				t.Fatal(err)
			}
			assertFps(t, got, []string{"gem:" + tt.want, "dir:" + tt.dirName})
		})
	}
}

// Several gemspecs: the lexically first one with a name wins; directories
// and unreadable entries named *.gemspec are ignored.
func TestDetect_GemspecOrderAndSkips(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "aaa.gemspec"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "bbb.gemspec"), "# no name here\n")
	writeFile(t, filepath.Join(dir, "ccc.gemspec"), "s.name = \"third\"\n")
	writeFile(t, filepath.Join(dir, "ddd.gemspec"), "s.name = \"fourth\"\n")
	writeFile(t, filepath.Join(dir, "notagemspec.txt"), "s.name = \"nope\"\n")
	got, err := Detect(context.Background(), dir, failingRunner(nil))
	if err != nil {
		t.Fatal(err)
	}
	assertFps(t, got, []string{"gem:third", "dir:" + filepath.Base(dir)})
}

func TestDetect_FullOrder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/app\n")
	writeFile(t, filepath.Join(dir, "package.json"), `{"name":"app"}`)
	r := fakeGit(dir, "", map[string]string{"origin": "git@github.com:acme/app.git"}, nil)
	got, err := Detect(context.Background(), dir, r)
	if err != nil {
		t.Fatal(err)
	}
	assertFps(t, got, []string{"git:github.com/acme/app", "go:example.com/app", "npm:app", "dir:" + filepath.Base(dir)})
	if s, ok := Strongest(got); !ok || s.String() != "git:github.com/acme/app" {
		t.Errorf("Strongest = %v, %v", s, ok)
	}
}

func TestDetect_RelativeDir(t *testing.T) {
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	got, err := Detect(context.Background(), ".", failingRunner(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != "dir" || got[0].Value == "." || got[0].Value == "" {
		t.Fatalf("got %v", got)
	}
}

func TestParseTOMLSections(t *testing.T) {
	in := `# comment
title = "root"
[package]
name = "a\"b"
edition = "2021"
authors = ["x"]
[[bin]]
name = "binname"
[tool.poetry]
name = 'lit'
[deep."quoted.key"]
name = """multi"""
`
	got := parseTOMLSections(in)
	tests := []struct{ section, key, want string }{
		{"", "title", "root"},
		{"package", "name", `a"b`},
		{"package", "edition", "2021"},
		{"package", "authors", ""},
		{"tool.poetry", "name", "lit"},
		{"deep.quoted.key", "name", "multi"},
		{"bin", "name", ""},
		{"missing", "name", ""},
	}
	for _, tt := range tests {
		if g := got.get(tt.section, tt.key); g != tt.want {
			t.Errorf("get(%q,%q) = %q, want %q", tt.section, tt.key, g, tt.want)
		}
	}
	if parseTOMLSections("") != nil {
		t.Error("empty input should yield nil")
	}
	if tomlSections(nil).get("a", "b") != "" {
		t.Error("nil map get should be empty")
	}
}

// ---- Real git (skipped when unavailable) ------------------------------------

func TestDetect_RealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "HOME="+root)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("remote", "add", "origin", "git@github.com:Real/Repo.git")
	run("remote", "add", "upstream", "https://gitlab.com/real/repo.git")
	sub := filepath.Join(root, "pkg", "x")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := Detect(context.Background(), sub, execx.Real())
	if err != nil {
		t.Fatal(err)
	}
	assertFps(t, got, []string{"git:github.com/real/repo#pkg/x", "git:gitlab.com/real/repo#pkg/x", "dir:x"})

	// The filesystem fallback must agree with git.
	fb, err := Detect(context.Background(), sub, failingRunner(nil))
	if err != nil {
		t.Fatal(err)
	}
	assertFps(t, fb, strs(got))
}

// ---- MatchProjects / Strongest / Union --------------------------------------

func fp(kind, value string, level Level) Fingerprint {
	return Fingerprint{Kind: kind, Value: value, Level: level}
}

func TestMatchProjects(t *testing.T) {
	local := []Fingerprint{
		fp("git", "github.com/acme/app", LevelStrong),
		fp("go", "example.com/app", LevelPackage),
		fp("dir", "app", LevelDir),
	}
	vault := map[string][]Fingerprint{
		"weak-b":     {fp("dir", "app", LevelDir), fp("git", "github.com/other/x", LevelStrong)},
		"strong-z":   {fp("go", "example.com/app", LevelPackage), fp("dir", "app", LevelDir)},
		"none":       {fp("git", "github.com/nope/nope", LevelStrong), fp("dir", "nope", LevelDir)},
		"strong-a":   {fp("git", "github.com/acme/app", LevelStrong)},
		"weak-a":     {fp("dir", "app", LevelDir)},
		"empty":      nil,
		"strong-git": {fp("dir", "app", LevelDir), fp("git", "github.com/acme/app", LevelStrong), fp("go", "example.com/app", LevelPackage)},
	}
	got := MatchProjects(local, vault)

	var ids []string
	for _, m := range got {
		ids = append(ids, m.ProjectID)
	}
	wantIDs := []string{"strong-a", "strong-git", "strong-z", "weak-a", "weak-b"}
	if !reflect.DeepEqual(ids, wantIDs) {
		t.Fatalf("order = %v, want %v", ids, wantIDs)
	}
	for _, m := range got {
		wantStrength := StrengthStrong
		if strings.HasPrefix(m.ProjectID, "weak") {
			wantStrength = StrengthWeak
		}
		if m.Strength != wantStrength {
			t.Errorf("%s: strength %d, want %d", m.ProjectID, m.Strength, wantStrength)
		}
		if len(m.Shared) == 0 {
			t.Errorf("%s: no shared fingerprints", m.ProjectID)
		}
	}
	// Shared keeps local order regardless of vault order.
	assertFps(t, got[1].Shared, []string{"git:github.com/acme/app", "go:example.com/app", "dir:app"})
	assertFps(t, got[3].Shared, []string{"dir:app"})
	assertFps(t, got[4].Shared, []string{"dir:app"})

	// Deterministic: repeated calls give the same order.
	for i := 0; i < 20; i++ {
		again := MatchProjects(local, vault)
		if !reflect.DeepEqual(again, got) {
			t.Fatalf("non-deterministic result on iteration %d", i)
		}
	}
}

func TestMatchProjects_Edges(t *testing.T) {
	if got := MatchProjects(nil, map[string][]Fingerprint{"a": {fp("dir", "x", LevelDir)}}); got != nil {
		t.Errorf("nil local: %v", got)
	}
	if got := MatchProjects([]Fingerprint{fp("dir", "x", LevelDir)}, nil); got != nil {
		t.Errorf("nil vault: %v", got)
	}
	if got := MatchProjects([]Fingerprint{fp("dir", "x", LevelDir)}, map[string][]Fingerprint{"a": {fp("dir", "y", LevelDir)}}); got != nil {
		t.Errorf("no overlap: %v", got)
	}
	// Level comes from the local side; kind+value decides equality.
	got := MatchProjects(
		[]Fingerprint{fp("npm", "x", LevelPackage)},
		map[string][]Fingerprint{"a": {fp("npm", "x", LevelDir)}},
	)
	if len(got) != 1 || got[0].Strength != StrengthStrong || got[0].Shared[0].Level != LevelPackage {
		t.Errorf("level mismatch handling: %+v", got)
	}
	// Duplicate local fingerprints are reported once.
	got = MatchProjects(
		[]Fingerprint{fp("dir", "x", LevelDir), fp("dir", "x", LevelDir)},
		map[string][]Fingerprint{"a": {fp("dir", "x", LevelDir)}},
	)
	if len(got) != 1 || len(got[0].Shared) != 1 || got[0].Strength != StrengthWeak {
		t.Errorf("duplicate local: %+v", got)
	}
}

// Strength is decided by an explicit level check (strong = level 1-2, weak =
// level 3), never by a "less than LevelDir" comparison that would let a zero
// or negative level count as strong. Unset (zero) levels fall back to the
// kind; unknown levels contribute nothing.
func TestMatchProjects_LevelClassification(t *testing.T) {
	vault := func(fps ...Fingerprint) map[string][]Fingerprint {
		return map[string][]Fingerprint{"p": fps}
	}
	tests := []struct {
		name     string
		local    []Fingerprint
		want     Strength // StrengthNone → project omitted
		wantSize int
	}{
		{"strong level", []Fingerprint{fp("git", "x", LevelStrong)}, StrengthStrong, 1},
		{"package level", []Fingerprint{fp("npm", "x", LevelPackage)}, StrengthStrong, 1},
		{"dir level", []Fingerprint{fp("dir", "x", LevelDir)}, StrengthWeak, 1},
		{"zero level dir kind is weak", []Fingerprint{fp("dir", "x", 0)}, StrengthWeak, 1},
		{"zero level git kind is strong", []Fingerprint{fp("git", "x", 0)}, StrengthStrong, 1},
		{"zero level package kind is strong", []Fingerprint{fp("cargo", "x", 0)}, StrengthStrong, 1},
		{"zero level unknown kind omitted", []Fingerprint{fp("odd", "x", 0)}, StrengthNone, 0},
		{"negative level never strong", []Fingerprint{fp("git", "x", -1)}, StrengthNone, 0},
		{"out of range level never strong", []Fingerprint{fp("git", "x", 7)}, StrengthNone, 0},
		{"negative plus dir is weak", []Fingerprint{fp("git", "x", -1), fp("dir", "d", LevelDir)}, StrengthWeak, 2},
		{"dir then package is strong", []Fingerprint{fp("dir", "d", LevelDir), fp("go", "m", LevelPackage)}, StrengthStrong, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchProjects(tt.local, vault(tt.local...))
			if tt.want == StrengthNone {
				if got != nil {
					t.Fatalf("expected omission, got %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Strength != tt.want || len(got[0].Shared) != tt.wantSize {
				t.Fatalf("got %+v, want strength %d with %d shared", got, tt.want, tt.wantSize)
			}
		})
	}
}

func TestStrongest(t *testing.T) {
	tests := []struct {
		name string
		in   []Fingerprint
		want string
		ok   bool
	}{
		{"empty", nil, "", false},
		{"dir only", []Fingerprint{fp("dir", "x", LevelDir)}, "", false},
		{"first strong", []Fingerprint{fp("dir", "x", LevelDir), fp("git", "a", LevelStrong), fp("git", "b", LevelStrong)}, "git:a", true},
		{"strong beats earlier package", []Fingerprint{fp("go", "m", LevelPackage), fp("git", "a", LevelStrong)}, "git:a", true},
		{"first package", []Fingerprint{fp("dir", "x", LevelDir), fp("npm", "n", LevelPackage), fp("go", "m", LevelPackage)}, "npm:n", true},
		{"unknown level ignored", []Fingerprint{fp("odd", "x", 7)}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Strongest(tt.in)
			if ok != tt.ok || (ok && got.String() != tt.want) {
				t.Fatalf("Strongest = (%v, %v), want (%q, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestUnion(t *testing.T) {
	a := []Fingerprint{fp("git", "x", LevelStrong), fp("dir", "d", LevelDir)}
	b := []Fingerprint{fp("dir", "d", LevelDir), fp("go", "m", LevelPackage), fp("git", "x", LevelStrong)}
	c := []Fingerprint{fp("npm", "n", LevelPackage), fp("go", "m", LevelPackage)}
	got := Union(a, b, c)
	assertFps(t, got, []string{"git:x", "dir:d", "go:m", "npm:n"})
	if got[0].Level != LevelStrong || got[1].Level != LevelDir {
		t.Errorf("levels of first-seen entries not preserved: %+v", got)
	}
	if Union() != nil {
		t.Error("Union() should be nil")
	}
	if Union(nil, nil) != nil {
		t.Error("Union(nil, nil) should be nil")
	}
	// Inputs are not mutated.
	if len(a) != 2 || len(b) != 3 {
		t.Error("inputs mutated")
	}
}

func TestFingerprintString(t *testing.T) {
	if s := (Fingerprint{Kind: "git", Value: "h/p#x"}).String(); s != "git:h/p#x" {
		t.Errorf("String = %q", s)
	}
	if s := (Fingerprint{}).String(); s != ":" {
		t.Errorf("empty String = %q", s)
	}
}
