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
		for _, want := range []string{"GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_SSH_COMMAND=ssh -i /tmp/key -o BatchMode=yes"} {
			if !slices.Contains(c.Env, want) {
				t.Errorf("cmd %d env lacks %q: %v", i, want, c.Env)
			}
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

// pushScript simulates a repository with local commits and a remote branch,
// rejecting the first rejects pushes as non-fast-forward.
func pushScript(rejects int, pushes *int) func(tail []string, call int) (execx.Result, bool) {
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
				return exit(1, "To github.com:me/vault.git\n ! [rejected]        main -> main (fetch first)\nerror: failed to push some refs to 'github.com:me/vault.git'\n"), true
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
