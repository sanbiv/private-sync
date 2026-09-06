// Package keysource obtains the vault passphrase (spec §11).
package keysource

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"

	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/execx"
	"github.com/sanbiv/private-sync/internal/paths"
	"github.com/sanbiv/private-sync/internal/ui"
)

// EnvVar is the environment variable that overrides every source.
const EnvVar = "PRIVATE_SYNC_PASSPHRASE"

// BWSessionEnvVar is the Bitwarden CLI session variable reused when already set.
const BWSessionEnvVar = "BW_SESSION"

// Source produces a passphrase.
//
// Passphrase blocks (it may prompt and run subprocesses) and is meant to be
// called before any tea.Program exists (spec §11). The sources returned by
// FromConfig are nevertheless safe for concurrent use: the bitwarden source
// serialises its status/unlock step so a shared Source never unlocks twice or
// races on the cached session.
type Source interface {
	Name() string
	Passphrase(ctx context.Context, p ui.Prompter) ([]byte, error)
}

// ErrKeyFilePerms is returned when the key file is readable by others / not owned.
var ErrKeyFilePerms = errors.New("insecure key file permissions")

// ErrEmptyPassphrase is returned when a source yields an empty passphrase.
var ErrEmptyPassphrase = errors.New("empty passphrase")

// ErrKeyFileTooLarge is returned when the key file exceeds MaxKeyFileSize: a
// passphrase file is a single short line, so anything bigger is a misconfigured
// path (a database, a log, a special file) and is refused before being read.
var ErrKeyFileTooLarge = errors.New("key file too large")

// MaxKeyFileSize bounds how many bytes ReadKeyFile will read from a key file.
const MaxKeyFileSize = 64 << 10

// envPassphrase is captured once by CaptureEnv.
var envPassphrase []byte

// CaptureEnv reads EnvVar once into memory and unsets it so children never see it.
// Call at process start.
func CaptureEnv() {
	if v, ok := os.LookupEnv(EnvVar); ok {
		envPassphrase = []byte(v)
		_ = os.Unsetenv(EnvVar)
	}
}

// EnvCaptured reports whether a passphrase was captured from the environment,
// which Obtain returns in preference to the configured source. Front ends use
// it to refuse a passphrase change that would leave that captured value stale
// and lock the environment out of the vault.
func EnvCaptured() bool { return len(envPassphrase) > 0 }

// FromConfig builds the configured source.
func FromConfig(cfg config.KeyConfig, r execx.Runner) (Source, error) {
	switch cfg.Source {
	case config.KeyPrompt, "":
		return &promptSource{}, nil
	case config.KeyFile:
		path := strings.TrimSpace(cfg.File.Path)
		if path == "" {
			dirs, err := paths.Default()
			if err != nil {
				return nil, fmt.Errorf("key.file.path is empty and the default cannot be resolved: %w", err)
			}
			path = dirs.DefaultKeyFile()
		}
		return &fileSource{path: path}, nil
	case config.KeyBitwarden:
		item := strings.TrimSpace(cfg.Bitwarden.Item)
		if item == "" {
			return nil, errors.New("key.bitwarden.item is empty")
		}
		if r == nil {
			return nil, errors.New("keysource: bitwarden source needs a command runner")
		}
		field := strings.TrimSpace(cfg.Bitwarden.Field)
		if field == "" {
			field = "password"
		}
		return &bitwardenSource{runner: r, item: item, field: field}, nil
	default:
		return nil, fmt.Errorf("unknown key source %q (want prompt, file or bitwarden)", cfg.Source)
	}
}

// Obtain returns the passphrase: captured env first, then the configured source.
func Obtain(ctx context.Context, cfg config.KeyConfig, p ui.Prompter, r execx.Runner) ([]byte, error) {
	if len(envPassphrase) > 0 {
		out := make([]byte, len(envPassphrase))
		copy(out, envPassphrase)
		return out, nil
	}
	src, err := FromConfig(cfg, r)
	if err != nil {
		return nil, err
	}
	return src.Passphrase(ctx, p)
}

// WriteKeyFile creates a new passphrase file (O_EXCL, 0600) with a random passphrase.
func WriteKeyFile(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("keysource: empty key file path")
	}
	full, err := paths.ExpandHome(path)
	if err != nil {
		return fmt.Errorf("key file %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return fmt.Errorf("create key file directory: %w", err)
	}
	f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("key file %s already exists: %w", full, err)
		}
		return fmt.Errorf("create key file %s: %w", full, err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		_ = f.Close()
		_ = os.Remove(full)
		return fmt.Errorf("generate passphrase: %w", err)
	}
	line := base64.StdEncoding.EncodeToString(raw) + "\n"
	if _, err := f.WriteString(line); err != nil {
		_ = f.Close()
		_ = os.Remove(full)
		return fmt.Errorf("write key file %s: %w", full, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(full)
		return fmt.Errorf("sync key file %s: %w", full, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(full)
		return fmt.Errorf("close key file %s: %w", full, err)
	}
	return nil
}

// Zero overwrites b with zeros (best effort wipe of secret material).
func Zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// ---------------------------------------------------------------------------
// prompt
// ---------------------------------------------------------------------------

type promptSource struct{}

func (*promptSource) Name() string { return string(config.KeyPrompt) }

func (*promptSource) Passphrase(ctx context.Context, p ui.Prompter) ([]byte, error) {
	if p == nil {
		return nil, ui.ErrNonInteractive
	}
	pw, err := p.Password(ctx, "Vault passphrase")
	if err != nil {
		return nil, err
	}
	if len(pw) == 0 {
		return nil, fmt.Errorf("vault passphrase: %w", ErrEmptyPassphrase)
	}
	return pw, nil
}

// ---------------------------------------------------------------------------
// file
// ---------------------------------------------------------------------------

type fileSource struct {
	path string
}

func (*fileSource) Name() string { return string(config.KeyFile) }

// permsError carries the exact OpenSSH-style remedy and unwraps to ErrKeyFilePerms.
type permsError struct {
	msg string
}

func (e *permsError) Error() string { return e.msg }
func (e *permsError) Unwrap() error { return ErrKeyFilePerms }

// ResolveKeyFile expands ~, makes the path absolute and resolves symlinks.
func ResolveKeyFile(path string) (string, error) {
	full, err := paths.ExpandHome(path)
	if err != nil {
		return "", fmt.Errorf("key file %s: %w", path, err)
	}
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("key file %s does not exist: %w", full, err)
		}
		return "", fmt.Errorf("key file %s: %w", full, err)
	}
	return resolved, nil
}

// openKeyFile opens path (already resolved) exactly once and validates the file
// behind the returned descriptor: a regular file with safe permissions and
// ownership (checks skipped on Windows). Checking the opened descriptor rather
// than the path closes the check-then-read window in which the file could be
// swapped; on Unix the open also refuses to follow a symlink planted at path.
// The caller owns the returned file and must close it.
func openKeyFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, openFlags, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("key file %s does not exist: %w", path, err)
		}
		if isSymlinkOpenError(err) {
			return nil, fmt.Errorf("key file %s is a symlink; pass the resolved path (see ResolveKeyFile): %w", path, err)
		}
		return nil, fmt.Errorf("key file %s: %w", path, err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("key file %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("key file %s is not a regular file", path)
	}
	if err := checkKeyFilePerms(path, fi); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// CheckKeyFile verifies that the key file at path is a regular file with safe
// permissions and ownership (checks skipped on Windows) without reading it. Like
// ReadKeyFile it accepts any user-facing path: ~ is expanded and symlinks are
// resolved first, so the checked (and reported) path is the final target. Callers
// such as config validation or the wizard can pass the configured path as-is.
func CheckKeyFile(path string) error {
	resolved, err := ResolveKeyFile(path)
	if err != nil {
		return err
	}
	f, err := openKeyFile(resolved)
	if err != nil {
		return err
	}
	return f.Close()
}

// shellQuote returns path quoted for copy-pasting into a POSIX shell. Paths made
// only of "safe" characters are returned unchanged (so the common case reads
// exactly like OpenSSH's hint); anything else is wrapped in single quotes, with
// embedded single quotes spelled '\”.
func shellQuote(path string) string {
	if path != "" && !strings.ContainsFunc(path, func(r rune) bool { return !isShellSafe(r) }) {
		return path
	}
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// isShellSafe reports whether r never needs quoting in a POSIX shell word.
func isShellSafe(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("/._-+:@,%=", r)
}

// ReadKeyFile resolves, checks and reads a key file, trimming trailing whitespace
// (every Unicode white space, so a stray \v, \f or NBSP cannot silently change the
// passphrase). The file is opened once: the bytes returned are the ones whose
// permissions were checked. Files larger than MaxKeyFileSize are refused with
// ErrKeyFileTooLarge; at most MaxKeyFileSize+1 bytes are ever read into memory.
func ReadKeyFile(path string) ([]byte, error) {
	resolved, err := ResolveKeyFile(path)
	if err != nil {
		return nil, err
	}
	f, err := openKeyFile(resolved)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxKeyFileSize+1))
	_ = f.Close()
	if err != nil {
		Zero(data)
		return nil, fmt.Errorf("read key file %s: %w", resolved, err)
	}
	if len(data) > MaxKeyFileSize {
		Zero(data)
		return nil, fmt.Errorf("key file %s is larger than %d bytes; not a passphrase file: %w", resolved, MaxKeyFileSize, ErrKeyFileTooLarge)
	}
	pw := bytes.TrimRightFunc(data, unicode.IsSpace)
	if len(pw) == 0 {
		Zero(data)
		return nil, fmt.Errorf("key file %s is empty: %w", resolved, ErrEmptyPassphrase)
	}
	out := make([]byte, len(pw))
	copy(out, pw)
	Zero(data)
	return out, nil
}

func (s *fileSource) Passphrase(_ context.Context, _ ui.Prompter) ([]byte, error) {
	return ReadKeyFile(s.path)
}

// ---------------------------------------------------------------------------
// bitwarden
// ---------------------------------------------------------------------------

type bitwardenSource struct {
	runner execx.Runner
	item   string
	field  string

	session string // unlock session kept in memory for this process

	mu sync.Mutex // guards session: status/unlock runs at most once per source
}

func (*bitwardenSource) Name() string { return string(config.KeyBitwarden) }

type bwStatus struct {
	Status string `json:"status"`
}

type bwItem struct {
	Notes *string `json:"notes"`
	Login *struct {
		Password *string `json:"password"`
	} `json:"login"`
	Fields []struct {
		Name  *string `json:"name"`
		Value *string `json:"value"`
	} `json:"fields"`
}

func (s *bitwardenSource) run(ctx context.Context, env []string, args ...string) ([]byte, error) {
	res, err := s.runner.Run(ctx, execx.Cmd{Name: "bw", Args: args, Env: env})
	if err != nil {
		return nil, err
	}
	return res.Stdout, nil
}

// status returns the "status" field of `bw status`. env is the extra environment
// for that single command: the CLI only reports "unlocked" when it can see the
// session key, and execx strips BW_SESSION from every child, so the caller passes
// BW_SESSION=<value> explicitly when the user has one exported.
func (s *bitwardenSource) status(ctx context.Context, env []string) (string, error) {
	out, err := s.run(ctx, env, "status")
	if err != nil {
		return "", fmt.Errorf("bw status: %w", err)
	}
	var st bwStatus
	if err := json.Unmarshal(bytes.TrimSpace(out), &st); err != nil {
		return "", fmt.Errorf("bw status: unparsable output: %w", err)
	}
	if st.Status == "" {
		return "", errors.New("bw status: missing \"status\" field in output")
	}
	return st.Status, nil
}

// unlock prompts for the master password and runs `bw unlock`, returning the session key.
func (s *bitwardenSource) unlock(ctx context.Context, p ui.Prompter) (string, error) {
	if p == nil {
		return "", ui.ErrNonInteractive
	}
	master, err := p.Password(ctx, "Bitwarden master password")
	if err != nil {
		return "", err
	}
	defer Zero(master)
	if len(master) == 0 {
		return "", errors.New("bitwarden master password is empty")
	}
	// exec.Cmd.Env is a []string, so the master password must be converted to an
	// immutable string here (and execx.realRunner copies it once more into the
	// child's env). Those copies cannot be wiped and live until GC; the prompter's
	// []byte is zeroed by the deferred Zero above. This residual copy is unavoidable
	// with the os/exec API, not an oversight.
	env := []string{"BW_PASSWORD=" + string(master)}
	out, err := s.run(ctx, env, "unlock", "--raw", "--passwordenv", "BW_PASSWORD")
	if err != nil {
		return "", fmt.Errorf("bw unlock: %w", err)
	}
	session := strings.TrimSpace(string(out))
	if session == "" {
		return "", errors.New("bw unlock: empty session key")
	}
	return session, nil
}

func (s *bitwardenSource) Passphrase(ctx context.Context, p ui.Prompter) ([]byte, error) {
	session, err := s.ensureSession(ctx, p)
	if err != nil {
		return nil, err
	}
	out, err := s.run(ctx, []string{BWSessionEnvVar + "=" + session}, "get", "item", s.item)
	if err != nil {
		return nil, fmt.Errorf("bw get item %s: %w", s.item, err)
	}
	return extractBWField(out, s.item, s.field)
}

// ensureSession returns the cached session key, establishing it on first use via
// `bw status` and, when needed, `bw unlock`. The whole status/unlock step runs
// under s.mu so concurrent callers sharing one source prompt and unlock once.
func (s *bitwardenSource) ensureSession(ctx context.Context, p ui.Prompter) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == "" {
		// A session the user exported is handed to `bw status` (and only to it, for
		// this one command) so the CLI can actually report "unlocked"; without it
		// execx's sanitised environment would make bw answer "locked" every time.
		envSession := os.Getenv(BWSessionEnvVar)
		var statusEnv []string
		if envSession != "" {
			statusEnv = []string{BWSessionEnvVar + "=" + envSession}
		}
		st, err := s.status(ctx, statusEnv)
		if err != nil {
			return "", err
		}
		switch st {
		case "unauthenticated":
			return "", errors.New("bitwarden: not logged in; run `bw login` first")
		case "unlocked":
			// "unlocked" without an exported session (e.g. a stale status from a
			// different environment) is treated like "locked" below.
			s.session = envSession
		case "locked":
		default:
			return "", fmt.Errorf("bw status: unexpected status %q", st)
		}
		if s.session == "" {
			session, err := s.unlock(ctx, p)
			if err != nil {
				return "", err
			}
			s.session = session
		}
	}
	return s.session, nil
}

// extractBWField parses `bw get item` JSON output and returns the requested field:
// "password" → login.password, "notes" → notes, anything else → the custom field
// with that name. Whichever field is chosen, trailing white space (any Unicode
// space, including \v, \f and NBSP) is trimmed exactly as the file source trims a
// key file, so a value pasted with a trailing newline unlocks the vault regardless
// of where it was stored.
func extractBWField(out []byte, item, field string) ([]byte, error) {
	var it bwItem
	if err := json.Unmarshal(bytes.TrimSpace(out), &it); err != nil {
		return nil, fmt.Errorf("bw get item %s: unparsable output: %w", item, err)
	}
	var value string
	switch field {
	case "password":
		if it.Login == nil || it.Login.Password == nil {
			return nil, fmt.Errorf("bitwarden item %q has no login password", item)
		}
		value = *it.Login.Password
	case "notes":
		if it.Notes == nil {
			return nil, fmt.Errorf("bitwarden item %q has no notes", item)
		}
		value = *it.Notes
	default:
		found := false
		for _, f := range it.Fields {
			if f.Name != nil && *f.Name == field {
				if f.Value != nil {
					value = *f.Value
				}
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("bitwarden item %q has no field named %q", item, field)
		}
	}
	value = strings.TrimRightFunc(value, unicode.IsSpace)
	if value == "" {
		return nil, fmt.Errorf("bitwarden item %q field %q: %w", item, field, ErrEmptyPassphrase)
	}
	return []byte(value), nil
}
