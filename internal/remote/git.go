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
		"user interactivity has been disabled", // Git Credential Manager with GCM_INTERACTIVE=never
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
	// gitRejectPatterns (lower-case) mark a push rejected because the remote
	// branch moved: only those are worth a fetch + rebase + retry. The bare
	// token "[rejected]" is git's own marker for a local non-fast-forward
	// refusal; "[remote rejected]" (hooks, protected branches) does not match
	// it and is not retried.
	gitRejectPatterns = []string{"non-fast-forward", "fetch first", "stale info", "[rejected]"}
	// gitInitNoBranchPatterns (lower-case) mean `git init -b` is unsupported.
	gitInitNoBranchPatterns = []string{"unknown switch", "unknown option"}
	// gitNoSuchRemotePatterns (lower-case) mean `git remote get-url` found no remote.
	gitNoSuchRemotePatterns = []string{"no such remote"}

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
		"-c", "core.askPass=", // see gitEnv: no askpass program, ever
	}
}

// gitEnv is the extra environment every git invocation gets: no prompts,
// English messages, and ssh in batch mode (never asks for keys or host keys).
//
// GIT_TERMINAL_PROMPT=0 alone is not enough: git consults GIT_ASKPASS, then
// core.askPass, then SSH_ASKPASS before it even looks at that variable, and
// IDE terminals (VS Code, JetBrains, GitHub Desktop) export GIT_ASKPASS so an
// HTTPS fetch would pop a GUI prompt. An empty askpass program disables the
// whole chain (git treats "" as "no program"), and later entries win over the
// inherited environment. GCM_INTERACTIVE=never keeps Git Credential Manager
// from opening a browser; its refusal is classified as ErrAuth.
func gitEnv() []string {
	return []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"GCM_INTERACTIVE=never",
		"LC_ALL=C",
		"GIT_SSH_COMMAND=" + sshCommand(os.Getenv("GIT_SSH_COMMAND")),
	}
}

// sshBatchModeRe matches an ssh BatchMode option in any spelling the user may
// have put into GIT_SSH_COMMAND (-o BatchMode=no, -oBatchMode=NO,
// -o "BatchMode no"), together with the whitespace before it.
var sshBatchModeRe = regexp.MustCompile(`(?i)\s*-o\s*["']?batchmode[=\s]+[^\s"']*["']?`)

// sshCommand returns the user's GIT_SSH_COMMAND (or "ssh") forced into batch
// mode. ssh honours the FIRST value given for an option, so an existing
// "-o BatchMode=no" would win over an appended "-o BatchMode=yes": every
// BatchMode option the user set is stripped first, then ours is appended.
func sshCommand(userCmd string) string {
	ssh := strings.TrimSpace(sshBatchModeRe.ReplaceAllString(strings.TrimSpace(userCmd), ""))
	if ssh == "" {
		ssh = "ssh"
	}
	return ssh + " -o BatchMode=yes"
}

// env is gitEnv plus the explicit location of the vault repository:
// GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE, GIT_COMMON_DIR and
// GIT_OBJECT_DIRECTORY always name the vault and GIT_ALTERNATE_OBJECT_DIRECTORIES
// is empty, so a git environment inherited from a hook (receive-pack's
// quarantine exports the object directories) or an outer git command can never
// redirect `git add`/`commit`/`fetch` into another repository's object store.
// Later entries win over inherited ones.
func (g *gitRemote) env() []string {
	gitDir := resolveGitDir(g.dir)
	common := resolveCommonDir(gitDir)
	return append(gitEnv(),
		"GIT_DIR="+gitDir,
		"GIT_WORK_TREE="+g.dir,
		"GIT_INDEX_FILE="+filepath.Join(gitDir, "index"),
		"GIT_COMMON_DIR="+common,
		"GIT_OBJECT_DIRECTORY="+filepath.Join(common, "objects"),
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=",
	)
}

// resolveCommonDir returns the directory holding the repository's shared
// files (objects, refs, config): gitDir itself, or the target of
// <gitDir>/commondir (relative to gitDir) when the vault is a linked worktree.
func resolveCommonDir(gitDir string) string {
	data, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return gitDir
	}
	line, _, _ := strings.Cut(string(data), "\n")
	target := strings.TrimSpace(filepath.FromSlash(line))
	if target == "" {
		return gitDir
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(gitDir, target)
	}
	return filepath.Clean(target)
}

// resolveGitDir returns the repository directory of the vault: <dir>/.git,
// or the target of a gitfile ("gitdir: <path>", relative to dir) when .git
// is a file. A missing or unreadable .git yields <dir>/.git.
func resolveGitDir(dir string) string {
	def := filepath.Join(dir, ".git")
	st, err := os.Stat(def)
	if err != nil || st.IsDir() {
		return def
	}
	data, err := os.ReadFile(def)
	if err != nil {
		return def
	}
	line, _, _ := strings.Cut(string(data), "\n")
	target, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	target = strings.TrimSpace(filepath.FromSlash(target))
	if !ok || target == "" {
		return def
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	return filepath.Clean(target)
}

// urlUserinfoRe matches the userinfo of a URL ("https://user:token@host"):
// git URLs may embed tokens that must not reach logs or error messages.
var urlUserinfoRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/@\s]+@`)

// redactText masks credentials embedded in URLs.
func redactText(s string) string { return urlUserinfoRe.ReplaceAllString(s, "${1}***@") }

// redactedError presents an error with URL credentials masked while still
// unwrapping to the original (errors.Is / errors.As keep working).
type redactedError struct{ cause error }

func (e *redactedError) Error() string { return redactText(e.cause.Error()) }

// Unwrap exposes the original error.
func (e *redactedError) Unwrap() error { return e.cause }

// redactErr wraps err when its message embeds URL credentials.
func redactErr(err error) error {
	if err == nil {
		return nil
	}
	if msg := err.Error(); redactText(msg) != msg {
		return &redactedError{cause: err}
	}
	return err
}

// cmd builds the execx.Cmd for a git invocation inside the vault.
func (g *gitRemote) cmd(log func(string), args ...string) execx.Cmd {
	full := append(g.configArgs(), args...)
	return execx.Cmd{
		Name:  "git",
		Args:  full,
		Dir:   g.dir,
		Env:   g.env(),
		Stdin: nil,
		OnStderr: func(line string) {
			if strings.TrimSpace(line) != "" {
				log("git: " + redactText(line))
			}
		},
	}
}

// run executes git with args and classifies failures (auth / network);
// credentials embedded in URLs never reach the log or the error message.
func (g *gitRemote) run(ctx context.Context, log func(string), args ...string) (execx.Result, error) {
	log("$ git " + redactText(strings.Join(args, " ")))
	res, err := g.runner.Run(ctx, g.cmd(log, args...))
	return res, redactErr(g.classify(err))
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

// isRejected reports whether a push failure is a non-fast-forward rejection
// (the remote branch moved), i.e. one that a fetch + rebase can resolve.
// Server-side refusals ("[remote rejected]", hooks, protected branches) are not.
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

// refExists runs `git rev-parse --verify -q <ref>` (ref may be "<rev>:<path>").
// Only exit status 1 means "absent"; anything else (128: not a repository,
// corrupt refs, bad objects) is an error, so a broken repository is never
// mistaken for an empty remote or an unborn branch.
func (g *gitRemote) refExists(ctx context.Context, log func(string), ref string) (bool, error) {
	_, err := g.run(ctx, log, "rev-parse", "--verify", "-q", ref)
	switch {
	case err == nil:
		return true, nil
	case exitedWith(err, 1):
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
	adopting := true
	if _, err := os.Stat(filepath.Join(g.dir, ".git")); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remote: %w", err)
		}
		adopting = false
		if err := g.initRepo(ctx, log); err != nil {
			return err
		}
	}
	if adopting {
		if err := g.checkAdoptable(ctx, log); err != nil {
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
	// Spec §10 lists the bootstrap files right after `git init`; they are
	// deliberately written after integration instead: a machine adopting an
	// existing vault then takes the committed copies, whereas untracked ones
	// would make `checkout -B` refuse ("would be overwritten") and force the
	// reset --hard fallback. The result is identical for a fresh vault.
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
	} else if !exitedWith(err, 129) && !containsAny(stderrOf(err), gitInitNoBranchPatterns) {
		return err // git missing, context cancelled, permission denied, ...
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

// vaultTopLevel are the top-level names a vault directory may contain. A
// repository at the vault path holding anything else is somebody else's.
var vaultTopLevel = []string{
	".git", vaultFileName, blobsDirName, "machines", "projects",
	".gitattributes", ".gitignore",
	".DS_Store", "desktop.ini", "Thumbs.db", // the noise .gitignore lists
	".tmp.driveupload", ".tmp.drivedownload",
}

// isVaultEntry reports whether a top-level directory entry belongs to a vault.
func isVaultEntry(name string) bool {
	for _, v := range vaultTopLevel {
		if name == v {
			return true
		}
	}
	return strings.HasPrefix(name, ".psv-tmp-")
}

// foreignEntries lists the top-level names in dir that are not vault files,
// in directory order (os.ReadDir sorts them).
func foreignEntries(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if !isVaultEntry(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// checkAdoptable refuses to take over a git repository that already exists at
// the vault path but is not the vault's own.
//
// Everything Prepare and Push do afterwards assumes the repository is the
// vault's: ensureOrigin repoints origin at the vault url, ensureBranch may
// create and check out the vault branch, and Push runs `git add -A` plus a
// push. Pointed at a project directory (an easy mistake in the setup wizard,
// where a path is typed or tab-completed) that would repoint the project's
// real origin and publish its whole working tree — including uncommitted work
// and whatever secrets it holds — to the sync remote in plaintext.
//
// A repository counts as the vault's when origin already names the configured
// url, or when the working tree holds nothing but vault files (a vault whose
// first Prepare was interrupted before origin was set).
func (g *gitRemote) checkAdoptable(ctx context.Context, log func(string)) error {
	origin := ""
	res, err := g.run(ctx, log, "remote", "get-url", "origin")
	switch {
	case err == nil:
		origin = strings.TrimSpace(string(res.Stdout))
		if origin == g.url {
			return nil
		}
	case exitedWith(err, 2), containsAny(stderrOf(err), gitNoSuchRemotePatterns):
		// No origin yet; the working tree has to vouch for the repository.
	default:
		return err
	}
	foreign, ferr := foreignEntries(g.dir)
	if ferr != nil {
		return fmt.Errorf("remote: %w", ferr)
	}
	if len(foreign) == 0 {
		return nil
	}
	shown := foreign
	if len(shown) > 5 {
		shown = append(append([]string(nil), shown[:5]...), "...")
	}
	has := "it has no origin"
	if origin != "" {
		has = "its origin is " + redactText(origin)
	}
	return fmt.Errorf("remote: %s is already a git repository that is not this vault's (%s, and it contains %s); "+
		"point the vault at an empty directory, or configure the remote url of that repository",
		g.dir, has, strings.Join(shown, ", "))
}

// ensureOrigin adds origin or repoints it at the configured url. `git remote
// get-url` exits 2 ("No such remote") when origin is missing; any other
// failure (128: not a repository) is reported as is.
func (g *gitRemote) ensureOrigin(ctx context.Context, log func(string)) error {
	res, err := g.run(ctx, log, "remote", "get-url", "origin")
	switch {
	case err == nil:
		if strings.TrimSpace(string(res.Stdout)) == g.url {
			return nil
		}
		_, err = g.run(ctx, log, "remote", "set-url", "origin", g.url)
		return err
	case exitedWith(err, 2), containsAny(stderrOf(err), gitNoSuchRemotePatterns):
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
	if err := g.clearInterruptedRebase(ctx, log); err != nil {
		return err
	}
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
// A local vault.json is only ever replaced by the remote's when both ids can
// be read and match; a file that cannot be shown to belong to the remote's
// vault (different id, or unreadable on either side) is an error, so
// adoption never silently overwrites it.
func (g *gitRemote) checkVaultID(ctx context.Context, log func(string)) error {
	localPath := filepath.Join(g.dir, vaultFileName)
	localID, present, err := localVaultID(g.dir)
	if err != nil {
		if !present {
			return fmt.Errorf("remote: read %s: %w", localPath, err)
		}
		return fmt.Errorf("remote: local %s is unreadable (%w); move it aside to open the remote vault", localPath, err)
	}
	if !present {
		return nil
	}
	remotePath := g.originRef() + ":" + vaultFileName
	exists, err := g.refExists(ctx, log, remotePath)
	if err != nil {
		return err
	}
	if !exists {
		return nil // remote history has no vault.json (yet)
	}
	res, err := g.run(ctx, log, "show", remotePath)
	if err != nil {
		return err
	}
	remoteID, err := vaultID(res.Stdout)
	if err != nil {
		return fmt.Errorf("remote: vault.json on origin/%s is unreadable (%w); cannot verify it belongs to the same vault as %s",
			g.branch, err, localPath)
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

// rebaseInProgress reports whether the repository is stopped in the middle of
// a rebase: git leaves rebase-merge (interactive/merge backend) or
// rebase-apply (am backend) in the repository directory until it finishes,
// is continued or is aborted.
func (g *gitRemote) rebaseInProgress() bool {
	gitDir := resolveGitDir(g.dir)
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(gitDir, name)); err == nil {
			return true
		}
	}
	return false
}

// clearInterruptedRebase aborts a rebase a previous run left behind.
//
// It must run before anything is committed: on a stopped rebase HEAD is
// detached and the working tree may hold conflict markers, so `git add -A`
// would commit corrupt .enc files into the vault — and the abort that
// eventually follows would throw that commit away together with any new vault
// file it swept up (a blob written since would simply disappear).
func (g *gitRemote) clearInterruptedRebase(ctx context.Context, log func(string)) error {
	if !g.rebaseInProgress() {
		return nil
	}
	log("a previous rebase was interrupted; aborting it before touching the working tree")
	if _, err := g.run(ctx, log, "rebase", "--abort"); err != nil {
		return fmt.Errorf("remote: %s is stopped in the middle of a rebase and it could not be aborted (%w); "+
			"run: git -C %s rebase --abort", g.dir, err, g.dir)
	}
	return nil
}

// abortRebase runs `git rebase --abort`, reporting a failure to the log only:
// callers are already returning an error of their own.
func (g *gitRemote) abortRebase(ctx context.Context, log func(string)) {
	if _, err := g.run(ctx, log, "rebase", "--abort"); err != nil {
		log("warning: git rebase --abort: " + err.Error())
	}
}

// rebase replays local commits onto origin/<branch>; on failure the rebase is
// aborted and the conflicting files are reported.
func (g *gitRemote) rebase(ctx context.Context, log func(string)) error {
	_, err := g.run(ctx, log, "rebase", g.originRef())
	if err == nil {
		return nil
	}
	if execx.ExitCode(err) < 0 {
		// git was killed rather than exiting on its own: exec.CommandContext
		// SIGKILLs it when the context is cancelled (esc during a fetch) and
		// ProcessState.ExitCode() is -1 for a signalled process. The rebase
		// may have stopped on a conflict first, so it is aborted here too --
		// on a context that cannot be cancelled, or the abort process would
		// never start.
		g.abortRebase(context.WithoutCancel(ctx), log)
		return err
	}
	files := g.conflictFiles(ctx, log, err)
	g.abortRebase(ctx, log)
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
	// `git add -A` would happily stage a half-merged file, conflict markers
	// and all, and commit it into the vault. clearInterruptedRebase has
	// already dealt with a stopped rebase, so anything unmerged left here is
	// a merge the user has to finish.
	if files := unmergedPaths(string(res.Stdout)); len(files) > 0 {
		return fmt.Errorf("remote: %s has unmerged paths (%s) from an interrupted merge; "+
			"resolve them or run: git -C %s merge --abort", g.dir, strings.Join(files, ", "), g.dir)
	}
	log("committing uncommitted local changes before rebasing")
	return g.stageAndCommit(ctx, log)
}

// unmergedPaths lists the conflicted paths in `git status --porcelain` output:
// the status codes with a U on either side, plus AA (both added) and DD (both
// deleted), which git reports without one.
func unmergedPaths(porcelain string) []string {
	var out []string
	for _, line := range strings.Split(porcelain, "\n") {
		if len(line) < 4 {
			continue
		}
		x, y := line[0], line[1]
		if x == 'U' || y == 'U' || (x == 'A' && y == 'A') || (x == 'D' && y == 'D') {
			out = append(out, strings.TrimSpace(line[2:]))
		}
	}
	return out
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
	// A fetch that was cancelled mid-rebase is only a warning to the caller,
	// which then pushes anyway: never commit the half-merged tree it left.
	if err := g.clearInterruptedRebase(ctx, log); err != nil {
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
