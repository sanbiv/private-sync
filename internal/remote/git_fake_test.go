package remote

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/execx"
)

func gitCfg(url string) config.GitRemote { return config.GitRemote{URL: url, Branch: "main"} }

// emptyRemoteScript answers Prepare's probes as an empty remote on a fresh
// repository whose HEAD already points at main.
func emptyRemoteScript(tail []string, _ int) (execx.Result, bool) {
	switch {
	case hasPrefixWords(tail, []string{"symbolic-ref", "-q", "HEAD"}):
		return out("refs/heads/main\n"), true
	case hasPrefixWords(tail, []string{"remote", "get-url", "origin"}):
		return exit(2, "error: No such remote 'origin'\n"), true
	case hasPrefixWords(tail, []string{"rev-parse", "--verify", "-q"}):
		return exit(1, ""), true
	}
	return execx.Result{}, false
}

func TestGitCmdEnvAndArgs(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", "ssh -i /tmp/key")
	// An IDE terminal / hook environment that must never reach git.
	t.Setenv("GIT_ASKPASS", "/ide/bin/gui-askpass")
	t.Setenv("SSH_ASKPASS", "/ide/bin/gui-askpass")
	t.Setenv("GIT_OBJECT_DIRECTORY", "/elsewhere/.git/objects")
	t.Setenv("GIT_ALTERNATE_OBJECT_DIRECTORIES", "/elsewhere/.git/objects")
	t.Setenv("GIT_COMMON_DIR", "/elsewhere/.git")
	f := newFakeGit(t)
	f.script = emptyRemoteScript
	dir := filepath.Join(t.TempDir(), "vault")
	r, err := NewGit(config.GitRemote{URL: "git@example.com:me/vault.git"}, dir, Options{MachineID: "m1", Runner: f.runner()})
	if err != nil {
		t.Fatal(err)
	}
	if r.Name() != "git" {
		t.Errorf("Name = %q", r.Name())
	}
	if err := r.Prepare(context.Background(), nil); err != nil {
		t.Fatalf("Prepare: %v\n%s", err, joinTails(f.tails))
	}
	if len(f.cmds) == 0 {
		t.Fatal("no git commands recorded")
	}
	for i, c := range f.cmds {
		if c.Name != "git" {
			t.Errorf("cmd %d name = %q", i, c.Name)
		}
		if c.Dir != dir {
			t.Errorf("cmd %d dir = %q, want %q", i, c.Dir, dir)
		}
		if c.Stdin != nil {
			t.Errorf("cmd %d has stdin", i)
		}
		if c.OnStderr == nil {
			t.Errorf("cmd %d has no OnStderr", i)
		}
		gitDir := filepath.Join(dir, ".git")
		for _, want := range []string{
			"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=", "GCM_INTERACTIVE=never",
			"LC_ALL=C", "GIT_SSH_COMMAND=ssh -i /tmp/key -o BatchMode=yes",
			"GIT_DIR=" + gitDir, "GIT_WORK_TREE=" + dir, "GIT_INDEX_FILE=" + filepath.Join(gitDir, "index"),
			"GIT_COMMON_DIR=" + gitDir, "GIT_OBJECT_DIRECTORY=" + filepath.Join(gitDir, "objects"),
			"GIT_ALTERNATE_OBJECT_DIRECTORIES=",
		} {
			if !slices.Contains(c.Env, want) {
				t.Errorf("cmd %d env lacks %q: %v", i, want, c.Env)
			}
		}
		// The real runner appends c.Env to the inherited environment and
		// os/exec lets the last duplicate win: the overrides must come last.
		effective := execx.SanitizedEnv(c.Env)
		for key, want := range map[string]string{
			"GIT_ASKPASS": "", "SSH_ASKPASS": "", "GIT_ALTERNATE_OBJECT_DIRECTORIES": "",
			"GIT_OBJECT_DIRECTORY": filepath.Join(gitDir, "objects"), "GIT_COMMON_DIR": gitDir, "GIT_DIR": gitDir,
		} {
			if got, ok := lastEnv(effective, key); !ok || got != want {
				t.Errorf("cmd %d effective %s = %q (present %v), want %q", i, key, got, ok, want)
			}
		}
		if i := slices.Index(c.Args, "core.askPass="); i < 1 || c.Args[i-1] != "-c" {
			t.Errorf("cmd %d lacks -c core.askPass=: %q", i, c.Args)
		}
		gitTail(t, c) // asserts the -c prefix
	}
	want := [][]string{
		{"init", "-b", "main"},
		{"symbolic-ref", "-q", "HEAD"},
		{"remote", "get-url", "origin"},
		{"remote", "add", "origin", "git@example.com:me/vault.git"},
		{"fetch", "origin"},
		{"rev-parse", "--verify", "-q", "refs/remotes/origin/main"},
		{"config", "branch.main.remote", "origin"},
		{"config", "branch.main.merge", "refs/heads/main"},
	}
	if len(f.tails) != len(want) {
		t.Fatalf("command sequence:\n%s\nwant:\n%s", joinTails(f.tails), joinTails(want))
	}
	for i := range want {
		if !slices.Equal(f.tails[i], want[i]) {
			t.Errorf("cmd %d = %v, want %v", i, f.tails[i], want[i])
		}
	}
	if got := mustRead(t, dir, ".gitattributes"); got != gitAttributesContent {
		t.Errorf(".gitattributes = %q", got)
	}
	if got := mustRead(t, dir, ".gitignore"); got != gitIgnoreContent {
		t.Errorf(".gitignore = %q", got)
	}
}

func TestGitEnvSSHCommand(t *testing.T) {
	cases := []struct{ env, want string }{
		{"", "ssh -o BatchMode=yes"},
		{"ssh -i /k", "ssh -i /k -o BatchMode=yes"},
		{"ssh -o BatchMode=yes", "ssh -o BatchMode=yes"},
		{"  plink -batch  ", "plink -batch -o BatchMode=yes"},
		// ssh honours the first value of an option: a user-supplied
		// BatchMode=no must be removed, not merely followed by ours.
		{"ssh -o BatchMode=no", "ssh -o BatchMode=yes"},
		{"ssh -o batchmode=NO -i /k", "ssh -i /k -o BatchMode=yes"},
		{"ssh -oBatchMode=no", "ssh -o BatchMode=yes"},
		{"ssh -i /k -o BatchMode=no -o ConnectTimeout=5", "ssh -i /k -o ConnectTimeout=5 -o BatchMode=yes"},
		{`ssh -o "BatchMode=no"`, "ssh -o BatchMode=yes"},
		{`ssh -o "BatchMode no"`, "ssh -o BatchMode=yes"},
		{"ssh -o BatchMode=yes -o BatchMode=no", "ssh -o BatchMode=yes"},
		{"-o BatchMode=no", "ssh -o BatchMode=yes"},
		// Options that merely contain the word are untouched.
		{"ssh -F /cfg/batchmode=no", "ssh -F /cfg/batchmode=no -o BatchMode=yes"},
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv("GIT_SSH_COMMAND", tc.env)
			env := gitEnv()
			if !slices.Contains(env, "GIT_SSH_COMMAND="+tc.want) {
				t.Errorf("env = %v, want GIT_SSH_COMMAND=%q", env, tc.want)
			}
			if !slices.Contains(env, "GIT_TERMINAL_PROMPT=0") || !slices.Contains(env, "LC_ALL=C") {
				t.Errorf("env = %v", env)
			}
			// Exactly one -o BatchMode option must remain, and it must say yes
			// (the word may legitimately survive elsewhere, e.g. in a path).
			got, _ := lastEnv(env, "GIT_SSH_COMMAND")
			lower := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(got), `"`, ""), "-obatchmode", "-o batchmode")
			if n := strings.Count(lower, "-o batchmode"); n != 1 {
				t.Errorf("GIT_SSH_COMMAND=%q carries %d -o BatchMode options, want 1", got, n)
			}
			for _, bad := range []string{"-o batchmode=no", "-o batchmode no"} {
				if strings.Contains(lower, bad) {
					t.Errorf("GIT_SSH_COMMAND=%q still disables batch mode", got)
				}
			}
		})
	}
}

func TestGitInitFallbackWithoutDashB(t *testing.T) {
	f := newFakeGit(t)
	f.script = func(tail []string, call int) (execx.Result, bool) {
		if hasPrefixWords(tail, []string{"init", "-b"}) {
			return exit(129, "error: unknown switch `b'\nusage: git init [-q | --quiet] ...\n"), true
		}
		if hasPrefixWords(tail, []string{"symbolic-ref", "-q", "HEAD"}) {
			return out("refs/heads/master\n"), true
		}
		return emptyRemoteScript(tail, call)
	}
	r, err := NewGit(gitCfg("u"), filepath.Join(t.TempDir(), "v"), Options{Runner: f.runner()})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Prepare(context.Background(), nil); err != nil {
		t.Fatalf("Prepare: %v\n%s", err, joinTails(f.tails))
	}
	head := [][]string{
		{"init", "-b", "main"},
		{"init"},
		{"symbolic-ref", "HEAD", "refs/heads/main"},
		{"symbolic-ref", "-q", "HEAD"},
		{"rev-parse", "--verify", "-q", "HEAD"},
		{"symbolic-ref", "HEAD", "refs/heads/main"},
	}
	for i := range head {
		if i >= len(f.tails) || !slices.Equal(f.tails[i], head[i]) {
			t.Fatalf("command %d mismatch; sequence:\n%s", i, joinTails(f.tails))
		}
	}
}

// nonFastForwardStderr is git's output for a push the remote branch outran.
const nonFastForwardStderr = "To github.com:me/vault.git\n ! [rejected]        main -> main (fetch first)\nerror: failed to push some refs to 'github.com:me/vault.git'\n"

// pushScript simulates a repository with local commits and a remote branch,
// rejecting the first rejects pushes as non-fast-forward.
func pushScript(rejects int, pushes *int) func(tail []string, call int) (execx.Result, bool) {
	return pushScriptWith(rejects, nonFastForwardStderr, pushes)
}

// pushScriptWith is pushScript with a custom stderr for the rejected pushes.
func pushScriptWith(rejects int, stderr string, pushes *int) func(tail []string, call int) (execx.Result, bool) {
	return func(tail []string, _ int) (execx.Result, bool) {
		switch {
		case hasPrefixWords(tail, []string{"diff", "--cached", "--quiet"}):
			return exit(1, ""), true
		case hasPrefixWords(tail, []string{"rev-parse", "--verify", "-q"}):
			return execx.Result{}, true // HEAD and origin/main exist
		case hasPrefixWords(tail, []string{"status", "--porcelain"}):
			return out(""), true
		case hasPrefixWords(tail, []string{"push"}):
			*pushes++
			if *pushes <= rejects {
				return exit(1, stderr), true
			}
			return execx.Result{}, true
		}
		return execx.Result{}, false
	}
}

func TestGitPushRetriesAfterRejection(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Run("succeeds within retries", func(t *testing.T) {
		f := newFakeGit(t)
		pushes := 0
		f.script = pushScript(2, &pushes)
		r, err := NewGit(gitCfg("u"), dir, Options{Runner: f.runner()})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Push(context.Background(), []string{"x"}, nil); err != nil {
			t.Fatalf("Push: %v\n%s", err, joinTails(f.tails))
		}
		if pushes != 3 {
			t.Errorf("pushes = %d, want 3", pushes)
		}
		if n := f.count("fetch", "origin"); n != 2 {
			t.Errorf("fetches = %d, want 2", n)
		}
		if n := f.count("rebase", "refs/remotes/origin/main"); n != 2 {
			t.Errorf("rebases = %d, want 2", n)
		}
		if n := f.count("commit", "--no-verify", "-m", "sync"); n < 1 {
			t.Errorf("commit not run")
		}
		if f.index("push", "-u", "origin", "main") < 0 {
			t.Errorf("push -u origin main not run:\n%s", joinTails(f.tails))
		}
		if f.index("add", "-A") > f.index("commit") || f.index("commit") > f.index("push") {
			t.Errorf("wrong order:\n%s", joinTails(f.tails))
		}
	})
	t.Run("gives up after max retries", func(t *testing.T) {
		f := newFakeGit(t)
		pushes := 0
		f.script = pushScript(100, &pushes)
		r, err := NewGit(gitCfg("u"), dir, Options{Runner: f.runner()})
		if err != nil {
			t.Fatal(err)
		}
		err = r.Push(context.Background(), nil, nil)
		if err == nil {
			t.Fatal("Push succeeded despite permanent rejection")
		}
		if pushes != maxPushRetries+1 {
			t.Errorf("pushes = %d, want %d", pushes, maxPushRetries+1)
		}
		if !strings.Contains(err.Error(), "rejected") {
			t.Errorf("error = %v", err)
		}
		var ee *execx.ExitError
		if !errors.As(err, &ee) {
			t.Errorf("underlying ExitError not preserved: %v", err)
		}
	})
	t.Run("non-rejection failure is not retried", func(t *testing.T) {
		f := newFakeGit(t)
		pushes := 0
		f.script = func(tail []string, call int) (execx.Result, bool) {
			if hasPrefixWords(tail, []string{"push"}) {
				pushes++
				return exit(128, "fatal: unable to access 'https://x/': Could not resolve host: x\n"), true
			}
			return pushScript(0, new(int))(tail, call)
		}
		r, err := NewGit(gitCfg("u"), dir, Options{Runner: f.runner()})
		if err != nil {
			t.Fatal(err)
		}
		err = r.Push(context.Background(), nil, nil)
		if !errors.Is(err, ErrNetwork) {
			t.Errorf("err = %v, want ErrNetwork", err)
		}
		if pushes != 1 {
			t.Errorf("pushes = %d, want 1", pushes)
		}
	})
}

func TestGitClassify(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	g := &gitRemote{dir: dir}
	cases := []struct {
		name   string
		stderr string
		want   error // sentinel, nil for raw
	}{
		{"publickey", "git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository.\n", ErrAuth},
		{"host key", "Host key verification failed.\nfatal: Could not read from remote repository.\n", ErrAuth},
		{"https username", "fatal: could not read Username for 'https://github.com': terminal prompts disabled\n", ErrAuth},
		{"auth failed", "remote: Invalid username or password.\nfatal: Authentication failed for 'https://x/'\n", ErrAuth},
		{"forbidden", "fatal: unable to access 'https://x/': The requested URL returned error: 403\n", ErrAuth},
		{"resolve", "fatal: unable to access 'https://x/': Could not resolve host: x\n", ErrNetwork},
		{"refused", "ssh: connect to host example.com port 22: Connection refused\nfatal: Could not read from remote repository.\n", ErrNetwork},
		{"timeout", "fatal: unable to access 'https://x/': Failed to connect to x port 443 after 75000 ms: Connection timed out\n", ErrNetwork},
		{"unreachable", "ssh: connect to host x port 22: Network is unreachable\n", ErrNetwork},
		{"rejected", " ! [rejected]        main -> main (non-fast-forward)\n", nil},
		{"other", "fatal: not a git repository\n", nil},
		{"local permission denied", "fatal: could not create work tree dir 'x': Permission denied\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := &execx.ExitError{Cmd: execx.Cmd{Name: "git", Args: []string{"fetch"}}, Result: execx.Result{ExitCode: 128, Stderr: []byte(tc.stderr)}}
			err := g.classify(raw)
			var ee *execx.ExitError
			if !errors.As(err, &ee) {
				t.Errorf("ExitError lost: %v", err)
			}
			for _, s := range []error{ErrAuth, ErrNetwork} {
				if errors.Is(err, s) != (tc.want == s) {
					t.Errorf("errors.Is(err, %v) = %v; err = %v", s, errors.Is(err, s), err)
				}
			}
			if tc.want == ErrAuth {
				hint := "authenticate once in a terminal: git -C " + dir + " fetch"
				if !strings.Contains(err.Error(), hint) {
					t.Errorf("auth error lacks hint %q: %v", hint, err)
				}
			}
			if tc.want == nil && err != raw {
				t.Errorf("raw error was wrapped: %v", err)
			}
			if tc.want != nil && len(err.Error()) > 300 {
				t.Errorf("classified message too long (%d chars): %v", len(err.Error()), err)
			}
		})
	}
	if g.classify(nil) != nil {
		t.Error("classify(nil) != nil")
	}
	plain := errors.New("context canceled")
	if g.classify(plain) != plain {
		t.Error("non-exit error was altered")
	}
}

func TestGitRebaseConflictFake(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		unmerged string
		wantFile string
	}{
		{"from index", "vault.json\n", "vault.json"},
		{"from output", "", "projects/p/state/x.json.enc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGit(t)
			f.script = func(tail []string, _ int) (execx.Result, bool) {
				switch {
				case hasPrefixWords(tail, []string{"rev-parse", "--verify", "-q"}):
					return execx.Result{}, true
				case hasPrefixWords(tail, []string{"status", "--porcelain"}):
					return out(""), true
				case hasPrefixWords(tail, []string{"rebase", "refs/remotes/origin/main"}):
					return execx.Result{
						ExitCode: 1,
						Stdout:   []byte("Auto-merging " + tc.wantFile + "\nCONFLICT (add/add): Merge conflict in " + tc.wantFile + "\n"),
						Stderr:   []byte("error: could not apply 640d65b... sync\nhint: Resolve all conflicts manually\n"),
					}, true
				case hasPrefixWords(tail, []string{"diff", "--name-only", "--diff-filter=U"}):
					return out(tc.unmerged), true
				}
				return execx.Result{}, false
			}
			r, err := NewGit(gitCfg("u"), dir, Options{Runner: f.runner()})
			if err != nil {
				t.Fatal(err)
			}
			err = r.Fetch(context.Background(), nil)
			var rc *RebaseConflictError
			if !errors.As(err, &rc) {
				t.Fatalf("err = %T %v", err, err)
			}
			if !slices.Equal(rc.Files, []string{tc.wantFile}) {
				t.Errorf("files = %v, want [%s]", rc.Files, tc.wantFile)
			}
			if f.index("rebase", "--abort") < 0 {
				t.Errorf("rebase --abort not run:\n%s", joinTails(f.tails))
			}
			if f.index("rebase", "--abort") < f.index("rebase", "refs/remotes/origin/main") {
				t.Errorf("abort ran before rebase")
			}
		})
	}
}

func TestGitAdoptFallsBackToReset(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := newFakeGit(t)
	checkouts := 0
	f.script = func(tail []string, _ int) (execx.Result, bool) {
		switch {
		case hasPrefixWords(tail, []string{"rev-parse", "--verify", "-q", "HEAD"}):
			return exit(1, ""), true // unborn
		case hasPrefixWords(tail, []string{"rev-parse", "--verify", "-q"}):
			return execx.Result{}, true // origin/main exists
		case hasPrefixWords(tail, []string{"checkout", "-B", "main"}):
			checkouts++
			if checkouts == 1 {
				return exit(1, "error: The following untracked working tree files would be overwritten by checkout:\n\tblobs/aa/x.enc\nAborting\n"), true
			}
			return execx.Result{}, true
		}
		return execx.Result{}, false
	}
	r, err := NewGit(gitCfg("u"), dir, Options{Runner: f.runner()})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Fetch(context.Background(), nil); err != nil {
		t.Fatalf("Fetch: %v\n%s", err, joinTails(f.tails))
	}
	reset := f.index("reset", "--hard", "refs/remotes/origin/main")
	if reset < 0 || checkouts != 2 || reset < f.index("checkout", "-B") {
		t.Errorf("expected checkout -B, reset --hard, checkout -B:\n%s", joinTails(f.tails))
	}
	if f.count("commit") != 0 {
		t.Errorf("commit must not run on an unborn branch during Fetch:\n%s", joinTails(f.tails))
	}
}

func TestNewGitValidation(t *testing.T) {
	cases := []struct {
		name   string
		cfg    config.GitRemote
		dir    string
		wantOK bool
	}{
		{"ok", config.GitRemote{URL: "git@h:r.git"}, "v", true},
		{"empty url", config.GitRemote{}, "v", false},
		{"option-looking url", config.GitRemote{URL: "--upload-pack=x"}, "v", false},
		{"empty dir", config.GitRemote{URL: "u"}, "", false},
		{"bad branch", config.GitRemote{URL: "u", Branch: "a b"}, "v", false},
		{"option-looking branch", config.GitRemote{URL: "u", Branch: "-x"}, "v", false},
		{"dotdot branch", config.GitRemote{URL: "u", Branch: "a..b"}, "v", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewGit(tc.cfg, tc.dir, Options{Runner: newFakeGit(t).runner()})
			if (err == nil) != tc.wantOK {
				t.Fatalf("err = %v, wantOK %v", err, tc.wantOK)
			}
			if tc.wantOK && r.(*gitRemote).branch != "main" {
				t.Errorf("default branch = %q", r.(*gitRemote).branch)
			}
		})
	}
	r, err := NewGit(config.GitRemote{URL: "u", Branch: "  release  "}, "v", Options{Runner: newFakeGit(t).runner()})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.(*gitRemote).branch; got != "release" {
		t.Errorf("branch = %q", got)
	}
	if !filepath.IsAbs(r.(*gitRemote).dir) {
		t.Errorf("dir not absolute: %q", r.(*gitRemote).dir)
	}
}

func TestResolveGitDir(t *testing.T) {
	base := t.TempDir()
	worktreeDir := filepath.Join(base, "main", ".git", "worktrees", "v")
	dotGit := func(dir string) string { return filepath.Join(dir, ".git") }
	cases := []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  func(dir string) string
	}{
		{"missing", func(*testing.T, string) {}, dotGit},
		{"directory", func(t *testing.T, dir string) {
			if err := os.MkdirAll(dotGit(dir), 0o700); err != nil {
				t.Fatal(err)
			}
		}, dotGit},
		{"gitfile absolute", func(t *testing.T, dir string) { mustWrite(t, dir, ".git", "gitdir: "+worktreeDir+"\n") },
			func(string) string { return worktreeDir }},
		{"gitfile relative", func(t *testing.T, dir string) { mustWrite(t, dir, ".git", "gitdir: ../main/.git/worktrees/v\n") },
			func(dir string) string {
				return filepath.Clean(filepath.Join(dir, "..", "main", ".git", "worktrees", "v"))
			}},
		{"gitfile garbage", func(t *testing.T, dir string) { mustWrite(t, dir, ".git", "nonsense\n") }, dotGit},
		{"gitfile empty target", func(t *testing.T, dir string) { mustWrite(t, dir, ".git", "gitdir:   \n") }, dotGit},
		{"gitfile empty file", func(t *testing.T, dir string) { mustWrite(t, dir, ".git", "") }, dotGit},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(base, fmt.Sprintf("v%d", i))
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, dir)
			if got := resolveGitDir(dir); got != tc.want(dir) {
				t.Errorf("resolveGitDir = %q, want %q", got, tc.want(dir))
			}
		})
	}
}

func TestRedactText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://user:s3cret@github.com/me/vault.git", "https://***@github.com/me/vault.git"},
		{"https://ghp_token@github.com/me/vault.git", "https://***@github.com/me/vault.git"},
		{"remote add origin https://u:p@h/r.git and https://x@h2/r", "remote add origin https://***@h/r.git and https://***@h2/r"},
		{"ssh://git@github.com/me/vault.git", "ssh://***@github.com/me/vault.git"},
		{"git@github.com:me/vault.git", "git@github.com:me/vault.git"},
		{"https://github.com/me/vault.git", "https://github.com/me/vault.git"},
		{"/local/path.git", "/local/path.git"},
		{"user@host", "user@host"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := redactText(tc.in); got != tc.want {
			t.Errorf("redactText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if redactErr(nil) != nil {
		t.Error("redactErr(nil) != nil")
	}
	plain := errors.New("no url here")
	if redactErr(plain) != plain {
		t.Error("error without credentials was wrapped")
	}
}

func TestGitRedactsCredentials(t *testing.T) {
	const url = "https://me:ghp_secret123@github.com/me/vault.git"
	f := newFakeGit(t)
	f.script = func(tail []string, call int) (execx.Result, bool) {
		if hasPrefixWords(tail, []string{"remote", "add", "origin"}) {
			return exit(128, "fatal: could not add remote '"+url+"'\n"), true
		}
		return emptyRemoteScript(tail, call)
	}
	r, err := NewGit(config.GitRemote{URL: url}, filepath.Join(t.TempDir(), "v"), Options{Runner: f.runner()})
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	err = r.Prepare(context.Background(), func(s string) { lines = append(lines, s) })
	if err == nil {
		t.Fatal("Prepare succeeded")
	}
	if strings.Contains(err.Error(), "ghp_secret123") {
		t.Errorf("error leaks the token: %v", err)
	}
	if !strings.Contains(err.Error(), "https://***@github.com/me/vault.git") {
		t.Errorf("error lacks the redacted url: %v", err)
	}
	var ee *execx.ExitError
	if !errors.As(err, &ee) || ee.Result.ExitCode != 128 {
		t.Errorf("ExitError lost through redaction: %v", err)
	}
	i := f.index("remote", "add", "origin")
	if i < 0 || f.tails[i][3] != url {
		t.Fatalf("git did not receive the real url:\n%s", joinTails(f.tails))
	}
	// Stderr lines are redacted as they are streamed.
	f.cmds[i].OnStderr("fatal: unable to access '" + url + "': bad")
	for _, l := range lines {
		if strings.Contains(l, "ghp_secret123") {
			t.Errorf("log leaks the token: %q", l)
		}
	}
	for _, want := range []string{
		"$ git remote add origin https://***@github.com/me/vault.git",
		"git: fatal: unable to access 'https://***@github.com/me/vault.git': bad",
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("log lacks %q:\n%s", want, strings.Join(lines, "\n"))
		}
	}
}

func TestGitPushRejectionClassification(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		stderr string
		retry  bool // a fetch + rebase + second push is worth trying
	}{
		{"fetch first", " ! [rejected]        main -> main (fetch first)\nerror: failed to push some refs to 'x'\nhint: Updates were rejected because the remote contains work that you do not have locally.\n", true},
		{"non-fast-forward", " ! [rejected]        main -> main (non-fast-forward)\nerror: failed to push some refs to 'x'\n", true},
		{"stale info", " ! [rejected]        main -> main (stale info)\nerror: failed to push some refs to 'x'\n", true},
		{"pre-receive hook", "remote: error: rejected by pre-receive hook\nTo x\n ! [remote rejected] main -> main (pre-receive hook declined)\nerror: failed to push some refs to 'x'\n", false},
		{"protected branch", "remote: error: GH006: Protected branch update failed for refs/heads/main.\nremote: error: Changes must be made through a pull request.\nTo x\n ! [remote rejected] main -> main (protected branch hook declined)\nerror: failed to push some refs to 'x'\n", false},
		{"update hook", "remote: error: hook declined to update refs/heads/main\nTo x\n ! [remote rejected] main -> main (hook declined)\nerror: failed to push some refs to 'x'\n", false},
		{"unrelated failure", "error: src refspec main does not match any\nerror: failed to push some refs to 'x'\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGit(t)
			pushes := 0
			f.script = pushScriptWith(1, tc.stderr, &pushes)
			r, err := NewGit(gitCfg("u"), dir, Options{Runner: f.runner()})
			if err != nil {
				t.Fatal(err)
			}
			err = r.Push(context.Background(), nil, nil)
			if tc.retry {
				if err != nil {
					t.Fatalf("Push: %v\n%s", err, joinTails(f.tails))
				}
				if pushes != 2 || f.count("fetch", "origin") != 1 || f.count("rebase", "refs/remotes/origin/main") != 1 {
					t.Errorf("pushes = %d; expected one rejection, one fetch+rebase, one retry:\n%s", pushes, joinTails(f.tails))
				}
				return
			}
			if err == nil {
				t.Fatalf("Push succeeded despite the server refusing it:\n%s", joinTails(f.tails))
			}
			var ee *execx.ExitError
			if !errors.As(err, &ee) {
				t.Errorf("underlying ExitError not preserved: %v", err)
			}
			if pushes != 1 || f.count("fetch", "origin") != 0 || f.count("rebase") != 0 {
				t.Errorf("server-side refusal was retried (pushes = %d):\n%s", pushes, joinTails(f.tails))
			}
		})
	}
}

func TestGitRefExistsOnlyExit1MeansAbsent(t *testing.T) {
	cases := []struct {
		name    string
		res     execx.Result
		wantErr string // "" = Prepare succeeds with an empty remote
	}{
		{"exit 1 is absent", exit(1, ""), ""},
		{"not a repository", exit(128, "fatal: not a git repository: '/v/.git'\n"), "not a git repository"},
		{"bad ref", exit(128, "error: refs/remotes/origin/main: not a valid SHA1\nfatal: Needed a single revision\n"), "not a valid SHA1"},
		{"usage error", exit(129, "usage: git rev-parse [<options>] -- [<args>...]\n"), "usage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGit(t)
			f.script = func(tail []string, call int) (execx.Result, bool) {
				if hasPrefixWords(tail, []string{"rev-parse", "--verify", "-q"}) {
					return tc.res, true
				}
				return emptyRemoteScript(tail, call)
			}
			r, err := NewGit(gitCfg("u"), filepath.Join(t.TempDir(), "v"), Options{Runner: f.runner()})
			if err != nil {
				t.Fatal(err)
			}
			var lines []string
			err = r.Prepare(context.Background(), func(s string) { lines = append(lines, s) })
			empty := slices.Contains(lines, "remote branch main is empty")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Prepare: %v\n%s", err, joinTails(f.tails))
				}
				if !empty {
					t.Errorf("missing origin/main not reported as an empty remote:\n%s", strings.Join(lines, "\n"))
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Prepare err = %v, want one containing %q\n%s", err, tc.wantErr, joinTails(f.tails))
			}
			if empty {
				t.Error("a broken repository was reported as an empty remote")
			}
			if n := f.count("checkout") + f.count("reset") + f.count("rebase") + f.count("config"); n != 0 {
				t.Errorf("acted on a broken repository:\n%s", joinTails(f.tails))
			}
		})
	}

	t.Run("push", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "vault")
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name     string
			res      execx.Result
			wantPush bool
			wantErr  bool
			wantLog  string
		}{
			{"unborn HEAD", exit(1, ""), false, false, "nothing to push"},
			{"broken repository", exit(128, "fatal: not a git repository: '/v/.git'\n"), false, true, ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := newFakeGit(t)
				f.script = func(tail []string, _ int) (execx.Result, bool) {
					switch {
					case hasPrefixWords(tail, []string{"rev-parse", "--verify", "-q", "HEAD"}):
						return tc.res, true
					case hasPrefixWords(tail, []string{"diff", "--cached", "--quiet"}):
						return execx.Result{}, true // nothing staged
					}
					return execx.Result{}, false
				}
				r, err := NewGit(gitCfg("u"), dir, Options{Runner: f.runner()})
				if err != nil {
					t.Fatal(err)
				}
				var lines []string
				err = r.Push(context.Background(), nil, func(s string) { lines = append(lines, s) })
				if (err != nil) != tc.wantErr {
					t.Fatalf("Push err = %v, wantErr %v\n%s", err, tc.wantErr, joinTails(f.tails))
				}
				if got := f.count("push") > 0; got != tc.wantPush {
					t.Errorf("push run = %v, want %v:\n%s", got, tc.wantPush, joinTails(f.tails))
				}
				if tc.wantLog != "" && !slices.Contains(lines, tc.wantLog) {
					t.Errorf("log lacks %q: %v", tc.wantLog, lines)
				}
				if tc.wantErr && slices.Contains(lines, "nothing to push") {
					t.Error("a broken repository was reported as 'nothing to push'")
				}
			})
		}
	})
}

func TestGitEnsureOriginStrict(t *testing.T) {
	cases := []struct {
		name    string
		res     execx.Result
		wantAdd bool
		wantErr string
	}{
		{"exit 2 adds origin", exit(2, "error: No such remote 'origin'\n"), true, ""},
		{"message without exit 2 adds origin", exit(1, "error: No such remote 'origin'\n"), true, ""},
		{"not a repository", exit(128, "fatal: not a git repository (or any of the parent directories): .git\n"), false, "not a git repository"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGit(t)
			f.script = func(tail []string, call int) (execx.Result, bool) {
				if hasPrefixWords(tail, []string{"remote", "get-url", "origin"}) {
					return tc.res, true
				}
				return emptyRemoteScript(tail, call)
			}
			r, err := NewGit(gitCfg("u"), filepath.Join(t.TempDir(), "v"), Options{Runner: f.runner()})
			if err != nil {
				t.Fatal(err)
			}
			err = r.Prepare(context.Background(), nil)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("Prepare: %v\n%s", err, joinTails(f.tails))
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("Prepare err = %v, want %q\n%s", err, tc.wantErr, joinTails(f.tails))
			}
			if got := f.count("remote", "add", "origin", "u") > 0; got != tc.wantAdd {
				t.Errorf("remote add run = %v, want %v:\n%s", got, tc.wantAdd, joinTails(f.tails))
			}
			if tc.wantErr != "" && f.count("fetch") != 0 {
				t.Errorf("fetched from a broken repository:\n%s", joinTails(f.tails))
			}
		})
	}
}

func TestGitInitFailureIsNotRetriedWithoutDashB(t *testing.T) {
	f := newFakeGit(t)
	f.script = func(tail []string, call int) (execx.Result, bool) {
		if hasPrefixWords(tail, []string{"init"}) {
			return exit(128, "fatal: cannot mkdir /v: Permission denied\n"), true
		}
		return emptyRemoteScript(tail, call)
	}
	r, err := NewGit(gitCfg("u"), filepath.Join(t.TempDir(), "v"), Options{Runner: f.runner()})
	if err != nil {
		t.Fatal(err)
	}
	err = r.Prepare(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("Prepare err = %v\n%s", err, joinTails(f.tails))
	}
	if f.count("init") != 1 {
		t.Errorf("a failure unrelated to -b triggered the fallback:\n%s", joinTails(f.tails))
	}
}

func TestGitCheckVaultIDFake(t *testing.T) {
	const ref = "refs/remotes/origin/main"
	cases := []struct {
		name         string
		pathRes      execx.Result // rev-parse --verify -q origin/main:vault.json
		showRes      execx.Result // show origin/main:vault.json
		wantErr      error        // sentinel, nil for success
		wantContains string
		wantAdopt    bool
	}{
		{"remote has no vault.json yet", exit(1, ""), execx.Result{}, nil, "", true},
		{"same id", execx.Result{}, out(vaultJSONv1), nil, "", true},
		{"different id", execx.Result{}, out(vaultJSONv2), ErrVaultConflict, "22222222", false},
		{"remote vault.json unreadable", execx.Result{}, out("{broken"), errors.New("x"), "unreadable", false},
		{"remote vault.json without id", execx.Result{}, out(`{"version":1}`), errors.New("x"), "unreadable", false},
		{"probe fails", exit(128, "fatal: not a git repository\n"), execx.Result{}, errors.New("x"), "not a git repository", false},
		{"show fails", execx.Result{}, exit(128, "fatal: bad object\n"), errors.New("x"), "bad object", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "vault")
			if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
			mustWrite(t, dir, "vault.json", vaultJSONv1)
			f := newFakeGit(t)
			f.script = func(tail []string, _ int) (execx.Result, bool) {
				switch {
				case slices.Equal(tail, []string{"rev-parse", "--verify", "-q", ref}):
					return execx.Result{}, true // remote has history
				case slices.Equal(tail, []string{"rev-parse", "--verify", "-q", ref + ":vault.json"}):
					return tc.pathRes, true
				case slices.Equal(tail, []string{"rev-parse", "--verify", "-q", "HEAD"}):
					return exit(1, ""), true // unborn
				case slices.Equal(tail, []string{"show", ref + ":vault.json"}):
					return tc.showRes, true
				}
				return execx.Result{}, false
			}
			r, err := NewGit(gitCfg("u"), dir, Options{Runner: f.runner()})
			if err != nil {
				t.Fatal(err)
			}
			err = r.Fetch(context.Background(), nil)
			switch {
			case tc.wantErr == nil && err != nil:
				t.Fatalf("Fetch: %v\n%s", err, joinTails(f.tails))
			case tc.wantErr != nil && err == nil:
				t.Fatalf("Fetch succeeded\n%s", joinTails(f.tails))
			case tc.wantErr == ErrVaultConflict && !errors.Is(err, ErrVaultConflict):
				t.Fatalf("err = %v, want ErrVaultConflict", err)
			case tc.wantErr != nil && tc.wantErr != ErrVaultConflict && errors.Is(err, ErrVaultConflict):
				t.Fatalf("err = %v must not be an id conflict", err)
			}
			if tc.wantContains != "" && !strings.Contains(err.Error(), tc.wantContains) {
				t.Errorf("err = %v, want one containing %q", err, tc.wantContains)
			}
			if got := f.count("checkout", "-B", "main", ref) > 0; got != tc.wantAdopt {
				t.Errorf("adopted = %v, want %v:\n%s", got, tc.wantAdopt, joinTails(f.tails))
			}
			if tc.pathRes.ExitCode == 1 && f.count("show") != 0 {
				t.Errorf("show run although the remote has no vault.json:\n%s", joinTails(f.tails))
			}
			if got := mustRead(t, dir, "vault.json"); got != vaultJSONv1 {
				t.Errorf("local vault.json modified: %q", got)
			}
		})
	}
}

func TestResolveCommonDir(t *testing.T) {
	base := t.TempDir()
	cases := []struct {
		name    string
		content *string // nil = no commondir file
		want    func(gitDir string) string
	}{
		{"plain repository", nil, func(g string) string { return g }},
		{"linked worktree relative", ptr("../..\n"), func(g string) string { return filepath.Clean(filepath.Join(g, "..", "..")) }},
		{"absolute", ptr(filepath.Join(base, "main", ".git") + "\n"), func(string) string { return filepath.Join(base, "main", ".git") }},
		{"empty file", ptr(""), func(g string) string { return g }},
		{"whitespace", ptr("  \n"), func(g string) string { return g }},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gitDir := filepath.Join(base, fmt.Sprintf("main%d", i), ".git", "worktrees", "v")
			if err := os.MkdirAll(gitDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if tc.content != nil {
				mustWrite(t, gitDir, "commondir", *tc.content)
			}
			if got := resolveCommonDir(gitDir); got != tc.want(gitDir) {
				t.Errorf("resolveCommonDir = %q, want %q", got, tc.want(gitDir))
			}
		})
	}
	if got := resolveCommonDir(filepath.Join(base, "missing", ".git")); got != filepath.Join(base, "missing", ".git") {
		t.Errorf("missing gitdir: %q", got)
	}
}

func ptr(s string) *string { return &s }

// ctxGit is a runner that records the context state of every invocation.
type ctxGit struct {
	t     *testing.T
	tails [][]string
	ctxs  []error
	h     func(tail []string) execx.Result
}

func (r *ctxGit) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	tail := gitTail(r.t, c)
	r.tails = append(r.tails, tail)
	r.ctxs = append(r.ctxs, ctx.Err())
	res := r.h(tail)
	if res.ExitCode != 0 {
		return res, &execx.ExitError{Cmd: c, Result: res}
	}
	return res, nil
}

// TestGitRebaseKilledByCancellationIsAborted: cancelling a Fetch (esc in the
// TUI) SIGKILLs git, and a signalled process reports exit code -1. That branch
// used to return without aborting, leaving .git/rebase-merge and a detached
// HEAD for the next run to commit over. The abort also has to run on a context
// that is not cancelled, or the process would never start.
func TestGitRebaseKilledByCancellationIsAborted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &ctxGit{t: t}
	rec.h = func(tail []string) execx.Result {
		if hasPrefixWords(tail, []string{"rebase"}) && !hasPrefixWords(tail, []string{"rebase", "--abort"}) {
			cancel() // the user pressed esc while git rebase was running
			return exit(-1, "signal: killed")
		}
		return execx.Result{}
	}
	r, err := NewGit(gitCfg("u"), dir, Options{MachineID: "m1", Runner: rec})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Fetch(ctx, testLog(t)); err == nil {
		t.Fatal("Fetch succeeded although git was killed")
	}
	i := -1
	for n, tail := range rec.tails {
		if hasPrefixWords(tail, []string{"rebase", "--abort"}) {
			i = n
		}
	}
	if i < 0 {
		t.Fatalf("no `git rebase --abort` after the rebase was killed:\n%s", joinTails(rec.tails))
	}
	if rec.ctxs[i] != nil {
		t.Errorf("the abort ran on a cancelled context (%v); the process would never start", rec.ctxs[i])
	}
}

func TestUnmergedPaths(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"UU shared.txt\n", []string{"shared.txt"}},
		{"AA both.txt\nDD gone.txt\n", []string{"both.txt", "gone.txt"}},
		{"AU a\nUD b\nUA c\nDU d\n", []string{"a", "b", "c", "d"}},
		{" M edited.txt\n?? blobs/\nA  added.txt\n", nil},
		{"", nil},
		{"U\n", nil}, // too short to carry a path
	}
	for _, tc := range cases {
		if got := unmergedPaths(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("unmergedPaths(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
