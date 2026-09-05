package remote

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/execx"
)

// testLog forwards backend log lines to t.Log.
func testLog(t *testing.T) func(string) {
	t.Helper()
	return func(s string) { t.Log(s) }
}

// mustWrite writes a vault-relative file, creating parent directories.
func mustWrite(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// mustRead returns the content of a vault-relative file.
func mustRead(t *testing.T, dir, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// pathExists reports whether a vault-relative path exists.
func pathExists(dir, rel string) bool {
	_, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(rel)))
	return err == nil
}

// lastEnv returns the value of the last KEY= entry in env (os/exec semantics:
// a later duplicate wins) and whether the key is present at all.
func lastEnv(env []string, key string) (string, bool) {
	val, found := "", false
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			val, found = v, true
		}
	}
	return val, found
}

// countFiles counts the regular files below root (0 when root is missing).
func countFiles(t *testing.T, root string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			n++
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return n
}

// expectedGitPrefix is the -c prefix every git invocation must carry.
func expectedGitPrefix() []string {
	null := os.DevNull
	if runtime.GOOS == "windows" {
		null = "NUL"
	}
	return []string{
		"-c", "user.name=private-sync",
		"-c", "user.email=private-sync@localhost",
		"-c", "commit.gpgsign=false",
		"-c", "core.autocrlf=false",
		"-c", "core.hooksPath=" + null,
		"-c", "core.askPass=",
	}
}

// gitTail asserts the -c prefix and returns the arguments after it.
func gitTail(t *testing.T, c execx.Cmd) []string {
	t.Helper()
	prefix := expectedGitPrefix()
	if len(c.Args) < len(prefix) || !slices.Equal(c.Args[:len(prefix)], prefix) {
		t.Fatalf("git args lack the -c prefix: %q", c.Args)
	}
	return c.Args[len(prefix):]
}

// fakeGit is an execx.Fake for git that records every invocation and answers
// from a script keyed by the argument tail (after the -c prefix); unscripted
// commands succeed with empty output.
type fakeGit struct {
	t     *testing.T
	cmds  []execx.Cmd
	tails [][]string
	// script returns a result for the argument tail; ok=false means default.
	script func(tail []string, call int) (execx.Result, bool)
}

func newFakeGit(t *testing.T) *fakeGit {
	return &fakeGit{t: t, script: func([]string, int) (execx.Result, bool) { return execx.Result{}, false }}
}

func (f *fakeGit) runner() execx.Runner {
	return execx.Fake(func(c execx.Cmd) (execx.Result, error) {
		tail := gitTail(f.t, c)
		// gitTail strips the prefix from a copy; keep the raw Cmd too.
		f.cmds = append(f.cmds, c)
		f.tails = append(f.tails, tail)
		if res, ok := f.script(tail, len(f.tails)); ok {
			return res, nil
		}
		return execx.Result{}, nil
	})
}

// count returns how many recorded invocations start with the given words.
func (f *fakeGit) count(words ...string) int {
	n := 0
	for _, tail := range f.tails {
		if hasPrefixWords(tail, words) {
			n++
		}
	}
	return n
}

// index returns the position of the first invocation starting with words, or -1.
func (f *fakeGit) index(words ...string) int {
	for i, tail := range f.tails {
		if hasPrefixWords(tail, words) {
			return i
		}
	}
	return -1
}

func hasPrefixWords(tail, words []string) bool {
	if len(tail) < len(words) {
		return false
	}
	return slices.Equal(tail[:len(words)], words)
}

func joinTails(tails [][]string) string {
	var sb strings.Builder
	for _, t := range tails {
		sb.WriteString("  git " + strings.Join(t, " ") + "\n")
	}
	return sb.String()
}

// exit builds a failing Result with the given stderr.
func exit(code int, stderr string) execx.Result {
	return execx.Result{ExitCode: code, Stderr: []byte(stderr)}
}

// out builds a successful Result with the given stdout.
func out(stdout string) execx.Result { return execx.Result{Stdout: []byte(stdout)} }

// --- real git helpers -------------------------------------------------------

// inheritedGitVars are the environment variables a hook or an outer git
// command exports for *its* repository, plus the askpass helpers an IDE
// terminal exports; the backend must be immune to all of them.
var inheritedGitVars = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR",
	"GIT_ASKPASS", "SSH_ASKPASS",
}

// requireGit skips the test without a git binary and isolates git from the
// user's global/system configuration, home directory and inherited
// repository variables (tests that exercise immunity set them explicitly).
func requireGit(t *testing.T) {
	t.Helper()
	if _, ok := execx.LookPath("git"); !ok {
		t.Skip("git not installed")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_SSH_COMMAND", "")
	for _, k := range inheritedGitVars {
		if v, ok := os.LookupEnv(k); ok {
			// An empty value still counts as set for git; restore afterwards.
			t.Cleanup(func() { _ = os.Setenv(k, v) })
			_ = os.Unsetenv(k)
		}
	}
}

// cleanGitEnv returns os.Environ() without the inherited repository and
// askpass variables (a test may set them to simulate a hook or IDE
// environment), so helper git invocations always operate on their Dir.
func cleanGitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if slices.Contains(inheritedGitVars, k) {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
}

// rawGit runs git directly (outside the backend) for setup and assertions.
func rawGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	cmd.Env = cleanGitEnv()
	outb, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, outb)
	}
	return strings.TrimSpace(string(outb))
}

// newBare creates a local bare repository whose HEAD points at main.
func newBare(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	rawGit(t, t.TempDir(), "init", "--bare", "-q", dir)
	rawGit(t, dir, "symbolic-ref", "HEAD", "refs/heads/main")
	return dir
}

// newGitVault builds a git backend for a fresh vault directory (not created).
func newGitVault(t *testing.T, url, machineID string) (Remote, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "vault")
	r, err := NewGit(config.GitRemote{URL: url, Branch: "main"}, dir, Options{MachineID: machineID, MachineName: machineID})
	if err != nil {
		t.Fatal(err)
	}
	return r, dir
}

// lsFiles lists the tracked files at HEAD of a vault.
func lsFiles(t *testing.T, dir string) []string {
	t.Helper()
	outp := rawGit(t, dir, "ls-files")
	if outp == "" {
		return nil
	}
	return strings.Split(outp, "\n")
}
