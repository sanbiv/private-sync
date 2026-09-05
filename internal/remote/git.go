package remote

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/execx"
)

const (
	defaultGitBranch = "main"
	// maxPushRetries is how many times Push re-fetches and re-pushes after a
	// non-fast-forward rejection before giving up.
	maxPushRetries = 3

	gitAttributesContent = "*.enc binary\nvault.json text\n"
	gitIgnoreContent     = ".psv-tmp-*\n.DS_Store\ndesktop.ini\nThumbs.db\n.tmp.driveupload/\n.tmp.drivedownload/\n"
)

var (
	// gitAuthPatterns (lower-case) mark failures that need a one-time
	// interactive authentication.
	gitAuthPatterns = []string{
		"permission denied (publickey)",
		"host key verification failed",
		"could not read username",
		"could not read password",
		"authentication failed",
		"terminal prompts disabled",
		"invalid username or password",
		"invalid username or token",
		"access denied",
		"returned error: 401",
		"returned error: 403",
	}
	// gitNetworkPatterns (lower-case) mark failures that mean the remote host
	// could not be reached.
	gitNetworkPatterns = []string{
		"could not resolve host",
		"could not resolve hostname",
		"connection refused",
		"connection timed out",
		"connection reset",
		"operation timed out",
		"network is unreachable",
		"network is down",
		"no route to host",
		"temporary failure in name resolution",
		"name or service not known",
		"nodename nor servname provided",
		"failed to connect",
		"couldn't connect to server",
		"ssh: connect to host",
	}
	// gitRejectPatterns (lower-case) mark a push rejected as non-fast-forward.
	gitRejectPatterns = []string{"rejected", "non-fast-forward", "fetch first"}

	mergeConflictRe = regexp.MustCompile(`(?m)Merge conflict in (.+?)\s*$`)
)

// bootstrapFile is a plain-text file Prepare writes into the vault when absent.
type bootstrapFile struct{ name, content string }

// bootstrapFiles lists the vault bootstrap files in a fixed order.
func bootstrapFiles() []bootstrapFile {
	return []bootstrapFile{
		{".gitattributes", gitAttributesContent},
		{".gitignore", gitIgnoreContent},
	}
}

// gitRemote is the git backend (spec §10, "git").
type gitRemote struct {
	url    string
	branch string
	dir    string
	opts   Options
	runner execx.Runner
	// nullDevice is the value of core.hooksPath that disables hooks.
	nullDevice string
}

// NewGit builds the git backend.
func NewGit(cfg config.GitRemote, vaultDir string, o Options) (Remote, error) {
	url := strings.TrimSpace(cfg.URL)
	if url == "" {
		return nil, errors.New("remote: git url is required")
	}
	if strings.HasPrefix(url, "-") {
		return nil, fmt.Errorf("remote: invalid git url %q", cfg.URL)
	}
	if strings.TrimSpace(vaultDir) == "" {
		return nil, errors.New("remote: vault directory is required")
	}
	branch := strings.TrimSpace(cfg.Branch)
	if branch == "" {
		branch = defaultGitBranch
	}
	if strings.HasPrefix(branch, "-") || strings.ContainsAny(branch, " \t\r\n~^:?*[\\") || strings.Contains(branch, "..") {
		return nil, fmt.Errorf("remote: invalid git branch %q", cfg.Branch)
	}
	abs, err := filepath.Abs(vaultDir)
	if err != nil {
		return nil, fmt.Errorf("remote: vault directory: %w", err)
	}
	runner := o.Runner
	if runner == nil {
		runner = execx.Real()
	}
	null := os.DevNull
	if runtime.GOOS == "windows" {
		null = "NUL"
	}
	return &gitRemote{url: url, branch: branch, dir: abs, opts: o, runner: runner, nullDevice: null}, nil
}

func (g *gitRemote) Name() string { return "git" }

// configArgs is the -c prefix every git invocation carries.
func (g *gitRemote) configArgs() []string {
	return []string{
		"-c", "user.name=private-sync",
		"-c", "user.email=private-sync@localhost",
		"-c", "commit.gpgsign=false",
		"-c", "core.autocrlf=false",
		"-c", "core.hooksPath=" + g.nullDevice,
	}
}

// gitEnv is the extra environment every git invocation gets: no prompts,
// English messages, and ssh in batch mode (never asks for keys or host keys).
func gitEnv() []string {
	ssh := strings.TrimSpace(os.Getenv("GIT_SSH_COMMAND"))
	if ssh == "" {
		ssh = "ssh"
	}
	if !strings.Contains(ssh, "BatchMode=") {
		ssh += " -o BatchMode=yes"
	}
	return []string{"GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_SSH_COMMAND=" + ssh}
}

// cmd builds the execx.Cmd for a git invocation inside the vault.
func (g *gitRemote) cmd(log func(string), args ...string) execx.Cmd {
	full := append(g.configArgs(), args...)
	return execx.Cmd{
		Name:  "git",
		Args:  full,
		Dir:   g.dir,
		Env:   gitEnv(),
		Stdin: nil,
		OnStderr: func(line string) {
			if strings.TrimSpace(line) != "" {
				log("git: " + line)
			}
		},
	}
}

// run executes git with args and classifies failures (auth / network).
func (g *gitRemote) run(ctx context.Context, log func(string), args ...string) (execx.Result, error) {
	log("$ git " + strings.Join(args, " "))
	res, err := g.runner.Run(ctx, g.cmd(log, args...))
	return res, g.classify(err)
}

// classify wraps auth and network failures with their sentinels and a hint.
func (g *gitRemote) classify(err error) error {
	if err == nil {
		return nil
	}
	text := stderrOf(err)
	if text == "" {
		return err
	}
	if line := matchLine(text, gitAuthPatterns); line != "" {
		return &classified{
			kind:  ErrAuth,
			msg:   fmt.Sprintf("%s (authenticate once in a terminal: git -C %s fetch)", line, g.dir),
			cause: err,
		}
	}
	if line := matchLine(text, gitNetworkPatterns); line != "" {
		return &classified{kind: ErrNetwork, msg: line, cause: err}
	}
	return err
}

// isRejected reports whether a push failure is a non-fast-forward rejection.
func isRejected(err error) bool {
	return containsAny(stderrOf(err), gitRejectPatterns)
}

// exitedWith reports whether err is a subprocess exit with the given code.
func exitedWith(err error, code int) bool { return err != nil && execx.ExitCode(err) == code }

func (g *gitRemote) originRef() string { return "refs/remotes/origin/" + g.branch }

// requireRepo fails when Prepare has not created the repository yet.
func (g *gitRemote) requireRepo() error {
	if _, err := os.Stat(filepath.Join(g.dir, ".git")); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remote: %s is not a git repository yet (run setup / Prepare first)", g.dir)
		}
		return fmt.Errorf("remote: %w", err)
	}
	return nil
}

// refExists runs `git rev-parse --verify -q <ref>`; exit 1 means absent.
func (g *gitRemote) refExists(ctx context.Context, log func(string), ref string) (bool, error) {
	_, err := g.run(ctx, log, "rev-parse", "--verify", "-q", ref)
	switch {
	case err == nil:
		return true, nil
	case execx.ExitCode(err) > 0:
		return false, nil
	default:
		return false, err
	}
}

// Prepare bootstraps the repository idempotently (spec §10):
// init + origin + bootstrap files, fetch, adopt or rebase onto origin/<branch>,
// and record the upstream so plain git commands in the vault behave.
func (g *gitRemote) Prepare(ctx context.Context, log func(string)) error {
	log = logger(log)
	if err := os.MkdirAll(g.dir, 0o700); err != nil {
		return fmt.Errorf("remote: create vault directory: %w", err)
	}
	if _, err := os.Stat(filepath.Join(g.dir, ".git")); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remote: %w", err)
		}
		if err := g.initRepo(ctx, log); err != nil {
			return err
		}
	}
	if err := g.ensureBranch(ctx, log); err != nil {
		return err
	}
	if err := g.ensureOrigin(ctx, log); err != nil {
		return err
	}
	if _, err := g.run(ctx, log, "fetch", "origin"); err != nil {
		return err
	}
	if err := g.integrate(ctx, log); err != nil {
		return err
	}
	// Written after integration so that a machine adopting an existing vault
	// takes the committed copies instead of tripping checkout on untracked ones.
	for _, f := range bootstrapFiles() {
		wrote, err := writeFileIfAbsent(filepath.Join(g.dir, f.name), []byte(f.content))
		if err != nil {
			return fmt.Errorf("remote: write %s: %w", f.name, err)
		}
		if wrote {
			log("wrote " + f.name)
		}
	}
	if _, err := g.run(ctx, log, "config", "branch."+g.branch+".remote", "origin"); err != nil {
		return err
	}
	if _, err := g.run(ctx, log, "config", "branch."+g.branch+".merge", "refs/heads/"+g.branch); err != nil {
		return err
	}
	return nil
}

// initRepo runs `git init -b <branch>`, falling back to `git init` plus a
// symbolic-ref update for git versions without -b.
func (g *gitRemote) initRepo(ctx context.Context, log func(string)) error {
	if _, err := g.run(ctx, log, "init", "-b", g.branch); err == nil {
		return nil
	} else if execx.ExitCode(err) < 0 {
		return err // git missing, context cancelled, ...
	}
	log("git init -b unsupported; falling back to git init")
	if _, err := g.run(ctx, log, "init"); err != nil {
		return err
	}
	_, err := g.run(ctx, log, "symbolic-ref", "HEAD", "refs/heads/"+g.branch)
	return err
}

// ensureBranch makes HEAD point at <branch>: an unborn HEAD on another name is
// simply re-pointed; a born HEAD elsewhere is checked out (creating <branch>
// at HEAD when it does not exist yet).
func (g *gitRemote) ensureBranch(ctx context.Context, log func(string)) error {
	want := "refs/heads/" + g.branch
	res, err := g.run(ctx, log, "symbolic-ref", "-q", "HEAD")
	if err != nil && execx.ExitCode(err) < 0 {
		return err
	}
	current := strings.TrimSpace(string(res.Stdout))
	if err == nil && current == want {
		return nil
	}
	born, err := g.refExists(ctx, log, "HEAD")
	if err != nil {
		return err
	}
	if !born {
		_, err := g.run(ctx, log, "symbolic-ref", "HEAD", want)
		return err
	}
	exists, err := g.refExists(ctx, log, want)
	if err != nil {
		return err
	}
	if exists {
		_, err = g.run(ctx, log, "checkout", g.branch, "--")
		return err
	}
	_, err = g.run(ctx, log, "checkout", "-b", g.branch)
	return err
}

// ensureOrigin adds origin or repoints it at the configured url.
func (g *gitRemote) ensureOrigin(ctx context.Context, log func(string)) error {
	res, err := g.run(ctx, log, "remote", "get-url", "origin")
	switch {
	case err == nil:
		if strings.TrimSpace(string(res.Stdout)) == g.url {
			return nil
		}
		_, err = g.run(ctx, log, "remote", "set-url", "origin", g.url)
		return err
	case execx.ExitCode(err) > 0:
		_, err = g.run(ctx, log, "remote", "add", "origin", g.url)
		return err
	default:
		return err
	}
}

// integrate brings origin/<branch> into the local branch after a fetch:
// nothing when the remote is empty, adoption when the local branch is unborn,
// a rebase otherwise. A local vault.json for a different vault is refused.
func (g *gitRemote) integrate(ctx context.Context, log func(string)) error {
	have, err := g.refExists(ctx, log, g.originRef())
	if err != nil {
		return err
	}
	if !have {
		log("remote branch " + g.branch + " is empty")
		return nil
	}
	if err := g.checkVaultID(ctx, log); err != nil {
		return err
	}
	local, err := g.refExists(ctx, log, "HEAD")
	if err != nil {
		return err
	}
	if !local {
		return g.adopt(ctx, log)
	}
	// A rebase needs a clean tree: commit whatever a previous run left behind.
	if err := g.commitDirty(ctx, log); err != nil {
		return err
	}
	return g.rebase(ctx, log)
}

// checkVaultID compares the local vault.json id with the remote's (§10.1).
func (g *gitRemote) checkVaultID(ctx context.Context, log func(string)) error {
	localID, present, err := localVaultID(g.dir)
	if err != nil {
		if present {
			log("warning: local vault.json unreadable: " + err.Error())
		}
		return nil // the vault package reports unreadable files
	}
	if !present {
		return nil
	}
	res, err := g.run(ctx, log, "show", g.originRef()+":vault.json")
	if err != nil {
		if execx.ExitCode(err) > 0 {
			return nil // remote history has no vault.json (yet)
		}
		return err
	}
	remoteID, err := vaultID(res.Stdout)
	if err != nil {
		log("warning: remote vault.json unreadable: " + err.Error())
		return nil
	}
	if remoteID != localID {
		return &VaultConflict{Dir: g.dir, LocalID: localID, RemoteID: remoteID}
	}
	return nil
}

// adopt checks out origin/<branch> onto an unborn local branch; untracked
// local files survive. When an untracked file is also tracked remotely (only
// possible for content-addressed blobs or the bootstrap files, since the vault
// id was already verified) the remote copy wins.
func (g *gitRemote) adopt(ctx context.Context, log func(string)) error {
	_, err := g.run(ctx, log, "checkout", "-B", g.branch, g.originRef())
	if err == nil {
		return nil
	}
	if !strings.Contains(strings.ToLower(stderrOf(err)), "would be overwritten") {
		return err
	}
	log("local files are also present on the remote; adopting the remote copies")
	if _, err := g.run(ctx, log, "reset", "--hard", g.originRef()); err != nil {
		return err
	}
	_, err = g.run(ctx, log, "checkout", "-B", g.branch, g.originRef())
	return err
}

// rebase replays local commits onto origin/<branch>; on failure the rebase is
// aborted and the conflicting files are reported.
func (g *gitRemote) rebase(ctx context.Context, log func(string)) error {
	_, err := g.run(ctx, log, "rebase", g.originRef())
	if err == nil {
		return nil
	}
	if execx.ExitCode(err) < 0 {
		return err
	}
	files := g.conflictFiles(ctx, log, err)
	if _, aerr := g.run(ctx, log, "rebase", "--abort"); aerr != nil {
		log("warning: git rebase --abort: " + aerr.Error())
	}
	if len(files) == 0 {
		return fmt.Errorf("git rebase onto origin/%s failed: %w", g.branch, err)
	}
	return &RebaseConflictError{Files: files, Cause: err}
}

// conflictFiles lists the unmerged paths of a stopped rebase, falling back to
// parsing git's "Merge conflict in <file>" lines.
func (g *gitRemote) conflictFiles(ctx context.Context, log func(string), rebaseErr error) []string {
	var files []string
	seen := map[string]bool{}
	add := func(f string) {
		f = strings.TrimSpace(f)
		if f != "" && !seen[f] {
			seen[f] = true
			files = append(files, f)
		}
	}
	if res, err := g.run(ctx, log, "diff", "--name-only", "--diff-filter=U"); err == nil {
		for _, l := range strings.Split(string(res.Stdout), "\n") {
			add(l)
		}
	}
	for _, m := range mergeConflictRe.FindAllStringSubmatch(stderrOf(rebaseErr), -1) {
		add(m[1])
	}
	return files
}

// stageAndCommit runs `git add -A` and commits when anything is staged.
func (g *gitRemote) stageAndCommit(ctx context.Context, log func(string)) error {
	if _, err := g.run(ctx, log, "add", "-A"); err != nil {
		return err
	}
	_, err := g.run(ctx, log, "diff", "--cached", "--quiet")
	switch {
	case err == nil:
		return nil // nothing staged
	case exitedWith(err, 1):
		_, err = g.run(ctx, log, "commit", "--no-verify", "-m", "sync")
		return err
	default:
		return err
	}
}

// commitDirty commits uncommitted changes left behind by a previous run so
// that the working tree is clean before a rebase.
func (g *gitRemote) commitDirty(ctx context.Context, log func(string)) error {
	res, err := g.run(ctx, log, "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(res.Stdout)) == "" {
		return nil
	}
	log("committing uncommitted local changes before rebasing")
	return g.stageAndCommit(ctx, log)
}

// Fetch downloads origin/<branch> and rebases local history onto it
// (uncommitted leftovers from a previous run are committed first).
func (g *gitRemote) Fetch(ctx context.Context, log func(string)) error {
	log = logger(log)
	if err := g.requireRepo(); err != nil {
		return err
	}
	if _, err := g.run(ctx, log, "fetch", "origin"); err != nil {
		return err
	}
	return g.integrate(ctx, log)
}

// Push commits the working tree and pushes it; a non-fast-forward rejection
// triggers a Fetch (rebase) and a retry, at most maxPushRetries times.
// written is not needed: git pushes whatever was committed.
func (g *gitRemote) Push(ctx context.Context, written []string, log func(string)) error {
	_ = written
	log = logger(log)
	if err := g.requireRepo(); err != nil {
		return err
	}
	if err := g.stageAndCommit(ctx, log); err != nil {
		return err
	}
	born, err := g.refExists(ctx, log, "HEAD")
	if err != nil {
		return err
	}
	if !born {
		log("nothing to push")
		return nil
	}
	for attempt := 0; ; attempt++ {
		_, err := g.run(ctx, log, "push", "-u", "origin", g.branch)
		if err == nil {
			return nil
		}
		if !isRejected(err) {
			return err
		}
		if attempt >= maxPushRetries {
			return fmt.Errorf("git push rejected %d times (remote keeps changing): %w", attempt+1, err)
		}
		log(fmt.Sprintf("push rejected (remote has new commits); rebasing and retrying (%d/%d)", attempt+1, maxPushRetries))
		if err := g.Fetch(ctx, log); err != nil {
			return err
		}
	}
}
