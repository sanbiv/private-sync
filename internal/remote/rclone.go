package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/execx"
)

const (
	// rcloneExitDirNotFound is rclone's exit status for "directory not found":
	// an empty remote has no blobs/ (or nothing at all) yet, which Fetch tolerates.
	rcloneExitDirNotFound = 3
	// rcloneExitFileNotFound is rclone's exit status for "file not found".
	rcloneExitFileNotFound = 4
	// rcloneNoPromptFlag is appended to every rclone invocation: an encrypted
	// rclone.conf is unlocked through RCLONE_CONFIG_PASS or not at all, never
	// by a prompt (stdin is the null device anyway; this turns a hang or an
	// obscure EOF into a classified error with a hint).
	rcloneNoPromptFlag = "--ask-password=false"
)

var (
	rcloneConfigPatterns = []string{
		"didn't find section in config file",
		"didn't find backend",
		"config file not found",
	}
	// rcloneConfigPassPatterns (lower-case) mark an encrypted rclone.conf
	// that could not be unlocked (no RCLONE_CONFIG_PASS, or a wrong one).
	rcloneConfigPassPatterns = []string{
		"unable to decrypt configuration",
		"couldn't decrypt configuration",
		"not allowed to ask for password",
		"enter configuration password",
		"failed to read password",
	}
	// rcloneAuthPatterns (lower-case) mark failures that need the remote to
	// be re-authorised. Bare status numbers and "permission denied" are
	// deliberately absent: they also occur in byte counts and local file errors.
	rcloneAuthPatterns = []string{
		"token expired",
		"invalid_grant",
		"invalid_access_token",
		"expired_access_token",
		"couldn't fetch token",
		"failed to get token",
		"unauthorized",
		"unauthenticated",
		"authentication",
		"authenticate",
		"access denied",
		"accessdenied",
		"forbidden",
		"error 401",
		"error 403",
		"status 401",
		"status 403",
		"code 401",
		"code 403",
	}
	rcloneNetworkPatterns = []string{
		"no such host",
		"connection refused",
		"connection reset",
		"i/o timeout",
		"dial tcp",
		"network is unreachable",
		"no route to host",
		"tls handshake timeout",
		"context deadline exceeded",
		"temporary failure in name resolution",
	}
)

// rcloneRemote is the rclone backend (spec §10, "rclone").
type rcloneRemote struct {
	remote string // rclone remote name, without the trailing colon
	path   string // path inside the remote, slash separated, no leading/trailing slash
	dir    string // local vault directory (absolute)
	opts   Options
	runner execx.Runner
	// tempDir is where --files-from lists are created (os.TempDir by default).
	tempDir string
}

// NewRclone builds the rclone backend.
func NewRclone(cfg config.RcloneRemote, vaultDir string, o Options) (Remote, error) {
	name := strings.TrimSpace(cfg.Remote)
	name = strings.TrimSuffix(name, ":")
	if name == "" {
		return nil, errors.New("remote: rclone remote name is required")
	}
	if strings.HasPrefix(name, "-") || strings.ContainsAny(name, ":/\\ \t\r\n") {
		return nil, fmt.Errorf("remote: invalid rclone remote name %q", cfg.Remote)
	}
	if strings.TrimSpace(vaultDir) == "" {
		return nil, errors.New("remote: vault directory is required")
	}
	if strings.TrimSpace(o.MachineID) == "" {
		return nil, errors.New("remote: rclone backend needs the machine id")
	}
	if strings.ContainsAny(o.MachineID, "/\\*?[] \t\r\n") {
		return nil, fmt.Errorf("remote: invalid machine id %q", o.MachineID)
	}
	p := strings.Trim(strings.TrimSpace(filepath.ToSlash(cfg.Path)), "/")
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	abs, err := filepath.Abs(vaultDir)
	if err != nil {
		return nil, fmt.Errorf("remote: vault directory: %w", err)
	}
	runner := o.Runner
	if runner == nil {
		runner = execx.Real()
	}
	return &rcloneRemote{remote: name, path: p, dir: abs, opts: o, runner: runner, tempDir: os.TempDir()}, nil
}

func (r *rcloneRemote) Name() string { return "rclone" }

// target returns "<remote>:<path>[/sub]".
func (r *rcloneRemote) target(sub string) string {
	p := r.path
	if sub != "" {
		if p == "" {
			p = sub
		} else {
			p += "/" + sub
		}
	}
	return r.remote + ":" + p
}

// root returns "<remote>:" (the remote itself).
func (r *rcloneRemote) root() string { return r.remote + ":" }

// cmd builds the execx.Cmd for an rclone invocation; every invocation ends
// with rcloneNoPromptFlag.
func (r *rcloneRemote) cmd(log func(string), args ...string) execx.Cmd {
	full := make([]string, 0, len(args)+1)
	full = append(full, args...)
	full = append(full, rcloneNoPromptFlag)
	return execx.Cmd{
		Name:  "rclone",
		Args:  full,
		Dir:   r.dir,
		Stdin: nil,
		OnStderr: func(line string) {
			if strings.TrimSpace(line) != "" {
				log("rclone: " + line)
			}
		},
	}
}

// run executes rclone with args and classifies failures.
func (r *rcloneRemote) run(ctx context.Context, log func(string), args ...string) (execx.Result, error) {
	c := r.cmd(log, args...)
	log("$ rclone " + strings.Join(c.Args, " "))
	res, err := r.runner.Run(ctx, c)
	return res, r.classify(err)
}

// classify wraps configuration, auth and network failures.
func (r *rcloneRemote) classify(err error) error {
	if err == nil {
		return nil
	}
	text := stderrOf(err)
	if text == "" {
		return err
	}
	if line := matchLine(text, rcloneConfigPassPatterns); line != "" {
		return fmt.Errorf("rclone configuration is encrypted and could not be unlocked (%s); "+
			"set RCLONE_CONFIG_PASS to the rclone configuration password: %w", line, err)
	}
	if line := matchLine(text, rcloneConfigPatterns); line != "" {
		return fmt.Errorf("rclone remote %q is not configured (%s); run: rclone config: %w", r.remote, line, err)
	}
	if line := matchLine(text, rcloneAuthPatterns); line != "" {
		return &classified{
			kind:  ErrAuth,
			msg:   fmt.Sprintf("%s (re-authenticate in a terminal: rclone config reconnect %s)", line, r.root()),
			cause: err,
		}
	}
	if line := matchLine(text, rcloneNetworkPatterns); line != "" {
		return &classified{kind: ErrNetwork, msg: line, cause: err}
	}
	return err
}

// Prepare verifies the remote is reachable and creates the vault folder.
//
// The probe is `rclone lsjson --stat <remote>:<path>` on the vault path
// itself rather than a listing of the remote's root: on bucket backends the
// root needs list-all-buckets permission and on a large Drive it lists every
// top-level folder, while <remote>:<path> is exactly what Fetch and Push use.
// Configuration, authentication and network failures are classified there;
// "not found" (exit 3/4) means the folder must be created.
func (r *rcloneRemote) Prepare(ctx context.Context, log func(string)) error {
	log = logger(log)
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return fmt.Errorf("remote: create vault directory: %w", err)
	}
	target := r.target("")
	res, err := r.run(ctx, log, "lsjson", "--stat", target)
	switch {
	case err == nil:
		isDir, ok := statIsDir(res.Stdout)
		if ok && !isDir {
			return fmt.Errorf("remote: %s is a file, not a folder", target)
		}
		if ok {
			log("vault folder exists on the remote")
			return nil
		}
		log("warning: unexpected rclone lsjson output; creating the vault folder anyway")
	case exitedWith(err, rcloneExitDirNotFound), exitedWith(err, rcloneExitFileNotFound):
		log("vault folder missing on the remote; creating it")
	default:
		return err
	}
	_, err = r.run(ctx, log, "mkdir", target)
	return err
}

// statIsDir decodes the IsDir field of `rclone lsjson --stat` output;
// ok is false when the output is not a stat document.
func statIsDir(data []byte) (isDir, ok bool) {
	var item struct {
		IsDir *bool `json:"IsDir"`
	}
	if err := json.Unmarshal(data, &item); err != nil || item.IsDir == nil {
		return false, false
	}
	return *item.IsDir, true
}

// Fetch copies blobs first (never overwriting existing content-addressed
// files), then everything else except blobs, this machine's own files and
// stale temp files. An empty remote (directory not found) is not an error.
func (r *rcloneRemote) Fetch(ctx context.Context, log func(string)) error {
	log = logger(log)
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return fmt.Errorf("remote: create vault directory: %w", err)
	}
	_, err := r.run(ctx, log, "copy", r.target("blobs"), filepath.Join(r.dir, "blobs"), "--ignore-existing")
	if err != nil && !exitedWith(err, rcloneExitDirNotFound) {
		return err
	}
	if err != nil {
		log("remote has no blobs yet")
	}
	args := []string{"copy", r.target(""), r.dir, "--exclude", "/blobs/**"}
	for _, own := range OwnFiles(r.opts.MachineID) {
		args = append(args, "--exclude", own)
	}
	args = append(args, "--exclude", ".psv-tmp-*")
	_, err = r.run(ctx, log, args...)
	if err != nil && !exitedWith(err, rcloneExitDirNotFound) {
		return err
	}
	if err != nil {
		log("remote vault folder is empty")
	}
	return nil
}

// Push uploads the written blobs first (content addressed, so existing remote
// copies are skipped), then this machine's own files plus the bootstrap files.
// Nothing is ever deleted on the remote.
func (r *rcloneRemote) Push(ctx context.Context, written []string, log func(string)) error {
	log = logger(log)
	blobs := r.writtenBlobs(written, log)
	if len(blobs) > 0 {
		if err := r.copyList(ctx, log, blobs, "--no-traverse", "--ignore-existing"); err != nil {
			return err
		}
	}
	own, err := r.ownFiles()
	if err != nil {
		return err
	}
	if len(own) == 0 {
		log("no own files to push")
		return nil
	}
	return r.copyList(ctx, log, own, "--no-traverse")
}

// copyList runs `rclone copy <vault> <target> --files-from <tmp> <extra...>`
// with the given vault-relative paths listed in a temporary file.
func (r *rcloneRemote) copyList(ctx context.Context, log func(string), paths []string, extra ...string) error {
	list, err := r.writeList(paths)
	if err != nil {
		return err
	}
	defer os.Remove(list)
	args := append([]string{"copy", r.dir, r.target(""), "--files-from", list}, extra...)
	_, err = r.run(ctx, log, args...)
	return err
}

// writeList writes one vault-relative path per line into a temp file.
func (r *rcloneRemote) writeList(paths []string) (string, error) {
	f, err := os.CreateTemp(r.tempDir, "private-sync-files-*.txt")
	if err != nil {
		return "", fmt.Errorf("remote: create file list: %w", err)
	}
	name := f.Name()
	var sb strings.Builder
	for _, p := range paths {
		sb.WriteString(p)
		sb.WriteByte('\n')
	}
	if _, err := f.WriteString(sb.String()); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", fmt.Errorf("remote: write file list: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("remote: write file list: %w", err)
	}
	return name, nil
}

// writtenBlobs selects the blob paths among written that exist locally,
// deduplicated and sorted.
func (r *rcloneRemote) writtenBlobs(written []string, log func(string)) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range written {
		rel, ok := vaultRel(r.dir, w)
		if !ok || !strings.HasPrefix(rel, "blobs/") || seen[rel] {
			continue
		}
		seen[rel] = true // examine (and log) each path once, present or not
		if !isRegularFile(filepath.Join(r.dir, filepath.FromSlash(rel))) {
			log("skipping missing blob " + rel)
			continue
		}
		out = append(out, rel)
	}
	sort.Strings(out)
	return out
}

// ownFiles expands OwnFiles(machineID) to the concrete files present locally
// and adds the bootstrap files when present.
//
// The patterns are matched relative to the vault (fs.Glob over os.DirFS)
// rather than joined onto the absolute vault path: a vault directory whose
// name contains glob metacharacters ("Vault [work]", "v*") would otherwise
// match nothing and this machine's own files would silently never be pushed.
func (r *rcloneRemote) ownFiles() ([]string, error) {
	var out []string
	vault := os.DirFS(r.dir)
	for _, pattern := range OwnFiles(r.opts.MachineID) {
		matches, err := fs.Glob(vault, pattern) // OwnFiles patterns are slash-separated
		if err != nil {
			return nil, fmt.Errorf("remote: expand %s: %w", pattern, err)
		}
		for _, m := range matches {
			if isRegularFile(filepath.Join(r.dir, filepath.FromSlash(m))) {
				out = append(out, m)
			}
		}
	}
	for _, name := range []string{"vault.json", ".gitattributes", ".gitignore"} {
		if isRegularFile(filepath.Join(r.dir, name)) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}
