// Package app wires the core packages for both front ends (spec §12).
package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/crypto"
	"github.com/sanbiv/private-sync/internal/execx"
	"github.com/sanbiv/private-sync/internal/identity"
	"github.com/sanbiv/private-sync/internal/keysource"
	"github.com/sanbiv/private-sync/internal/paths"
	"github.com/sanbiv/private-sync/internal/remote"
	"github.com/sanbiv/private-sync/internal/scan"
	"github.com/sanbiv/private-sync/internal/state"
	"github.com/sanbiv/private-sync/internal/sync"
	"github.com/sanbiv/private-sync/internal/ui"
	"github.com/sanbiv/private-sync/internal/vault"
)

// ErrNoConfig signals the first run (setup wizard needed).
var ErrNoConfig = errors.New("no configuration found")

// ErrVaultRace is returned by Setup when another machine created the vault
// between our create and our verification fetch (spec §10.1).
var ErrVaultRace = errors.New("another machine created the vault first; re-run init and choose open")

// ErrNotLinked is returned when a project id or name is not mapped on this machine.
var ErrNotLinked = errors.New("project is not linked on this machine")

// ErrAmbiguousProject is returned by ResolveProject when a name matches more
// than one linked project (ids are the only identity; names may repeat).
var ErrAmbiguousProject = errors.New("ambiguous project name")

// ErrKeyFileMissing is returned by Setup when the vault already exists, the
// key source is a file and that file does not exist: only the user knows the
// existing vault's passphrase, so the file is never generated in that case.
var ErrKeyFileMissing = errors.New("key file does not exist")

// ErrStalePin is returned by Setup when a vault id is pinned for the vault
// path, no vault.json is there any more, and the user declined to create a
// new vault in its place.
var ErrStalePin = errors.New("vault path is pinned to a vault that is missing")

// ErrKeyFileEnvMismatch is returned by Setup when it generated a key file but
// the passphrase actually used came from PRIVATE_SYNC_PASSPHRASE: the file
// would not open the vault, so it is removed again.
var ErrKeyFileEnvMismatch = errors.New("PRIVATE_SYNC_PASSPHRASE is set but key.source is file and the key file does not exist")

// kdfParams yields the KDF parameters used when creating a vault. It is a
// variable so tests can pick cheaper (but still valid) costs.
var kdfParams = crypto.DefaultKDFParams

// now is the clock used for timestamps (overridable in tests).
var now = func() time.Time { return time.Now().UTC() }

// RemoteFactory builds a remote for a vault directory (remote.New by default).
type RemoteFactory func(cfg config.RemoteConfig, vaultDir string, o remote.Options) (remote.Remote, error)

// App is the loaded configuration without any key material.
type App struct {
	Dirs       paths.Dirs
	ConfigPath string
	Config     *config.Config
	Machine    state.Machine
	Runner     execx.Runner

	// NewRemote replaces remote.New when set (tests, alternative transports).
	NewRemote RemoteFactory
}

// Load reads the config (ErrNoConfig when absent) and the machine identity.
func Load(configPath string, dirs paths.Dirs, r execx.Runner) (*App, error) {
	if strings.TrimSpace(configPath) == "" {
		configPath = dirs.ConfigFile()
	}
	if strings.TrimSpace(configPath) == "" {
		return nil, errors.New("app.Load: empty config path")
	}
	if strings.TrimSpace(dirs.State) == "" {
		return nil, errors.New("app.Load: empty state directory")
	}
	if r == nil {
		r = execx.Real()
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			return nil, fmt.Errorf("%w: %w", ErrNoConfig, err)
		}
		return nil, fmt.Errorf("load config %s: %w", configPath, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", configPath, err)
	}
	machine, err := state.LoadMachine(dirs.State)
	if err != nil {
		return nil, fmt.Errorf("machine identity: %w", err)
	}
	return &App{
		Dirs:       dirs,
		ConfigPath: configPath,
		Config:     cfg,
		Machine:    machine,
		Runner:     r,
	}, nil
}

// Session is an opened vault with its state, remote and engine.
type Session struct {
	*App
	Vault  *vault.Vault
	State  *state.Store
	Remote remote.Remote
	Engine *sync.Engine

	// Warn receives the vault's warnings about skipped files (spec §5: foreign
	// files such as Drive "(1)" copies are reported by name) from the helpers
	// that list projects internally (Identify, ResolveProject, UnlinkedProjects,
	// LinkProject). VaultProjects returns the warnings instead. Setup wires it
	// to its log; front ends may replace it. Never nil after Open/Setup.
	Warn func(string)
}

// Open prepares the remote (no fetch), obtains the key, opens the vault and checks the pin.
//
// The passphrase is only requested once the remote is prepared and vault.json
// is known to exist, so an unreachable remote or a missing vault never costs
// the user a prompt or a Bitwarden unlock.
func (a *App) Open(ctx context.Context, p ui.Prompter) (*Session, error) {
	if err := a.check(); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	vaultDir, err := a.Config.VaultPath()
	if err != nil {
		return nil, fmt.Errorf("vault path: %w", err)
	}
	rem, err := a.newRemote(vaultDir)
	if err != nil {
		return nil, err
	}
	if err := rem.Prepare(ctx, discard); err != nil {
		return nil, fmt.Errorf("prepare remote %s: %w", rem.Name(), err)
	}
	if !vault.Exists(vaultDir) {
		return nil, noVaultError(vaultDir)
	}
	passphrase, err := keysource.Obtain(ctx, a.Config.Key, p, a.runner())
	if err != nil {
		return nil, err
	}
	defer crypto.Zero(passphrase)

	v, err := vault.Open(vaultDir, passphrase, a.Machine.ID)
	if err != nil {
		if errors.Is(err, vault.ErrNoVault) {
			return nil, noVaultError(vaultDir)
		}
		return nil, err
	}
	if err := a.checkPin(vaultDir, v.ID()); err != nil {
		v.Close()
		return nil, err
	}
	return a.session(v, rem, discard)
}

// noVaultError wraps vault.ErrNoVault with the path and the remedy.
func noVaultError(vaultDir string) error {
	return fmt.Errorf("%w: %s; run `private-sync init` to create or open a vault", vault.ErrNoVault, vaultDir)
}

// Setup runs the vault creation protocol (spec §10.1): prepare, fetch, then
// open (vault.json present) or create+push+verify.
//
// The key is obtained only after the fetch reveals whether a vault exists:
// with a file key source, a missing file is generated only when a NEW vault
// is created (an existing vault needs its real passphrase, ErrKeyFileMissing).
func (a *App) Setup(ctx context.Context, p ui.Prompter, log func(string)) (*Session, error) {
	if err := a.check(); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if log == nil {
		log = discard
	}

	vaultDir, err := a.Config.VaultPath()
	if err != nil {
		return nil, fmt.Errorf("vault path: %w", err)
	}
	if err := paths.EnsureDir(vaultDir); err != nil {
		return nil, fmt.Errorf("create vault directory: %w", err)
	}
	rem, err := a.newRemote(vaultDir)
	if err != nil {
		return nil, err
	}
	// Whether Prepare is about to create the git repository (as opposed to
	// finding one): a repository this run created may be discarded again when
	// the creation protocol fails (see setupCreate).
	hadGit := exists(filepath.Join(vaultDir, gitDirName))
	log(fmt.Sprintf("preparing remote %s", rem.Name()))
	if err := rem.Prepare(ctx, log); err != nil {
		return nil, fmt.Errorf("prepare remote %s: %w", rem.Name(), err)
	}
	log("fetching remote")
	if err := rem.Fetch(ctx, log); err != nil {
		return nil, fmt.Errorf("fetch remote %s: %w", rem.Name(), err)
	}

	if vault.Exists(vaultDir) {
		return a.setupOpen(ctx, p, log, rem, vaultDir)
	}
	return a.setupCreate(ctx, p, log, rem, vaultDir, hadGit)
}

// gitDirName is the repository directory the git backend creates in the vault.
const gitDirName = ".git"

// exists reports whether path exists (any type, symlinks not followed).
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// setupOpen is the "vault.json exists ⇒ open is the only option" branch of §10.1.
func (a *App) setupOpen(ctx context.Context, p ui.Prompter, log func(string), rem remote.Remote, vaultDir string) (*Session, error) {
	log(fmt.Sprintf("vault found at %s: opening", vaultDir))
	if a.Config.Key.Source == config.KeyFile {
		keyPath, exists, err := a.keyFileStatus()
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("%w: %s: the vault at %s already exists, so write its passphrase to that file (chmod 600) and re-run init",
				ErrKeyFileMissing, keyPath, vaultDir)
		}
	}
	passphrase, err := keysource.Obtain(ctx, a.Config.Key, p, a.runner())
	if err != nil {
		return nil, err
	}
	defer crypto.Zero(passphrase)

	v, err := vault.Open(vaultDir, passphrase, a.Machine.ID)
	if err != nil {
		return nil, err
	}
	if err := a.checkPin(vaultDir, v.ID()); err != nil {
		v.Close()
		return nil, err
	}
	if err := v.WriteMachine(a.machineInfo()); err != nil {
		v.Close()
		return nil, fmt.Errorf("write machine info: %w", err)
	}
	log(fmt.Sprintf("opened vault %s", v.ID()))
	return a.session(v, rem, log)
}

// setupCreate is the "no vault.json ⇒ create, push, fetch, verify, pin" branch of §10.1.
//
// hadGit reports whether <vaultDir>/.git existed before Prepare ran: when it
// did not, the git backend created it during this run, and a failed creation
// discards it again (see discardLocalHistory).
func (a *App) setupCreate(ctx context.Context, p ui.Prompter, log func(string), rem remote.Remote, vaultDir string, hadGit bool) (*Session, error) {
	// A pin for this path whose vault.json is gone is exactly what the pin
	// exists to catch (dehydrated Drive folder, not-yet-synced clone, wrong
	// path): never replace it silently.
	pinned, ok, err := state.PinnedVault(a.Dirs.State, vaultDir)
	if err != nil {
		return nil, fmt.Errorf("vault pin: %w", err)
	}
	if ok && pinned != "" {
		question := fmt.Sprintf("vault %s was previously used at %s but is missing; create a NEW vault here?", pinned, vaultDir)
		if p == nil {
			return nil, fmt.Errorf("%w: %s (no prompter to confirm)", ErrStalePin, question)
		}
		yes, err := p.Confirm(ctx, question, false)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrStalePin, question, err)
		}
		if !yes {
			return nil, fmt.Errorf("%w: %s at %s; make sure the vault files are present (synced, not dehydrated) or pick another path", ErrStalePin, pinned, vaultDir)
		}
		log(fmt.Sprintf("replacing stale pin %s for %s with the new vault", pinned, vaultDir))
	}

	log(fmt.Sprintf("no vault at %s: creating", vaultDir))

	// A file key source whose file does not exist yet: generate it (§11).
	var createdKeyFile string
	if a.Config.Key.Source == config.KeyFile {
		created, keyPath, err := a.ensureKeyFile()
		if err != nil {
			return nil, err
		}
		if created {
			createdKeyFile = keyPath
			log(fmt.Sprintf("created key file %s (0600) with a random passphrase; back it up", keyPath))
		}
	}
	// Everything below may fail; failures must not leave behind a generated
	// key file that no vault uses.
	discardKeyFile := func() {
		if createdKeyFile == "" {
			return
		}
		if err := os.Remove(createdKeyFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log(fmt.Sprintf("warning: could not remove generated key file %s: %v", createdKeyFile, err))
			return
		}
		log(fmt.Sprintf("removed generated key file %s (no vault uses it)", createdKeyFile))
		createdKeyFile = ""
	}

	passphrase, err := keysource.Obtain(ctx, a.Config.Key, p, a.runner())
	if err != nil {
		discardKeyFile()
		return nil, err
	}
	defer crypto.Zero(passphrase)
	if createdKeyFile != "" {
		// Obtain prefers the captured environment passphrase: make sure the
		// vault is created with what the file says, or do not keep the file.
		fromFile, err := keysource.ReadKeyFile(createdKeyFile)
		if err != nil {
			discardKeyFile()
			return nil, fmt.Errorf("read back generated key file: %w", err)
		}
		same := bytes.Equal(fromFile, passphrase)
		crypto.Zero(fromFile)
		if !same {
			path := createdKeyFile
			discardKeyFile()
			return nil, fmt.Errorf("%w (%s): unset PRIVATE_SYNC_PASSPHRASE, or write that passphrase to the file (chmod 600) and re-run init", ErrKeyFileEnvMismatch, path)
		}
	}

	params, err := kdfParams()
	if err != nil {
		discardKeyFile()
		return nil, fmt.Errorf("kdf parameters: %w", err)
	}
	v, err := vault.Create(vaultDir, passphrase, params, a.Machine.ID)
	if err != nil {
		discardKeyFile()
		return nil, err
	}
	// fail undoes the local side of a failed creation so the next init starts
	// from the remote's state instead of adopting an unverified local vault.
	fail := func(err error) (*Session, error) {
		written := v.Written()
		id := v.ID()
		v.Close()
		removeLocalVault(vaultDir, id, written, log)
		discardKeyFile()
		if a.Config.Vault.Remote.Type == config.RemoteGit {
			err = discardLocalHistory(vaultDir, hadGit, err, log)
		}
		return nil, err
	}
	if err := v.WriteMachine(a.machineInfo()); err != nil {
		return fail(fmt.Errorf("write machine info: %w", err))
	}
	log("pushing vault.json")
	if err := rem.Push(ctx, withVaultFile(v.Written()), log); err != nil {
		return fail(raceOr(err, fmt.Errorf("push to remote %s: %w", rem.Name(), err)))
	}
	log("verifying vault id")
	if err := rem.Fetch(ctx, log); err != nil {
		return fail(raceOr(err, fmt.Errorf("fetch remote %s: %w", rem.Name(), err)))
	}
	got, err := vault.ReadID(vaultDir)
	if err != nil {
		return fail(fmt.Errorf("verify vault id: %w", err))
	}
	if got != v.ID() {
		return fail(fmt.Errorf("%w (wrote %s, remote has %s)", ErrVaultRace, v.ID(), got))
	}
	// Verified on the remote: from here on the vault is real and stays.
	if err := state.PinVault(a.Dirs.State, vaultDir, v.ID()); err != nil {
		v.Close()
		return nil, fmt.Errorf("pin vault: %w", err)
	}
	log(fmt.Sprintf("created vault %s", v.ID()))
	return a.session(v, rem, log)
}

// raceOr maps a remote error that means "another machine's vault.json won"
// (a rebase conflict on vault.json, or a vault id conflict detected by the
// backend) to ErrVaultRace; any other error is returned as fallback.
func raceOr(err, fallback error) error {
	if isVaultRace(err) {
		return fmt.Errorf("%w: %v", ErrVaultRace, err)
	}
	return fallback
}

// isVaultRace reports whether err is the §10.1 race surfaced by a remote backend.
func isVaultRace(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, remote.ErrVaultConflict) {
		return true
	}
	var rc *remote.RebaseConflictError
	return errors.As(err, &rc) && rc != nil && slices.Contains(rc.Files, vault.VaultFileName)
}

// discardLocalHistory undoes the git repository a failed creation leaves
// behind. The git backend commits vault.json locally before pushing, so after
// a lost race (or any failure once another machine's vault reached the remote)
// the next Prepare would replay that commit onto the winner's history and hit
// an add/add conflict on vault.json on every re-run of init. When this run
// created <vaultDir>/.git it is removed, so the next Prepare re-initialises
// the repository and adopts origin/<branch>. A repository that existed before
// (it may carry history that is not ours) is kept, and a race error then
// names the directory to move aside so "re-run init" can succeed.
func discardLocalHistory(vaultDir string, hadGit bool, err error, log func(string)) error {
	if log == nil {
		log = discard
	}
	gitDir := filepath.Join(vaultDir, gitDirName)
	if hadGit {
		if isVaultRaceErr(err) {
			return fmt.Errorf("%w; if init keeps failing with a vault.json conflict, move %s aside (it keeps the git history of this failed creation) and re-run init", err, vaultDir)
		}
		return err
	}
	if !exists(gitDir) {
		return err
	}
	if rmErr := os.RemoveAll(gitDir); rmErr != nil {
		log(fmt.Sprintf("warning: could not remove %s after failed vault creation: %v", gitDir, rmErr))
		return fmt.Errorf("%w; remove %s (the git history of this failed creation) before re-running init", err, gitDir)
	}
	log(fmt.Sprintf("removed %s (git history of a vault creation that did not complete)", gitDir))
	return err
}

// isVaultRaceErr reports whether err is (or wraps) ErrVaultRace.
func isVaultRaceErr(err error) bool { return errors.Is(err, ErrVaultRace) }

// removeLocalVault deletes the files a failed creation wrote below vaultDir
// (vault.json and this machine's own files). vault.json is only removed while
// it still carries our id: after a lost race it belongs to the other machine.
func removeLocalVault(vaultDir, vaultID string, written []string, log func(string)) {
	if log == nil {
		log = discard
	}
	for _, rel := range withVaultFile(written) {
		clean := filepath.Clean(filepath.FromSlash(rel))
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			continue
		}
		if rel == vault.VaultFileName {
			if got, err := vault.ReadID(vaultDir); err == nil && got != vaultID {
				continue
			}
		}
		full := filepath.Join(vaultDir, clean)
		if err := os.Remove(full); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				log(fmt.Sprintf("warning: could not remove %s after failed vault creation: %v", full, err))
			}
			continue
		}
		log(fmt.Sprintf("removed %s (vault creation did not complete)", rel))
	}
}

// SaveConfig writes the config back to ConfigPath.
func (a *App) SaveConfig() error { return a.Config.Save(a.ConfigPath) }

// check validates the receiver before any operation that needs it. It never
// writes to the receiver (Open/Setup may run while a TUI reads App fields).
func (a *App) check() error {
	switch {
	case a == nil:
		return errors.New("app: nil App")
	case a.Config == nil:
		return errors.New("app: no configuration loaded")
	case strings.TrimSpace(a.Dirs.State) == "":
		return errors.New("app: empty state directory")
	case a.Machine.ID == "":
		return errors.New("app: no machine identity")
	}
	return nil
}

// runner returns the configured subprocess runner, defaulting to execx.Real
// without storing it (Load already defaults it; a hand-built App may not).
func (a *App) runner() execx.Runner {
	if a.Runner != nil {
		return a.Runner
	}
	return execx.Real()
}

// newRemote builds the configured remote for vaultDir.
func (a *App) newRemote(vaultDir string) (remote.Remote, error) {
	factory := a.NewRemote
	if factory == nil {
		factory = remote.New
	}
	rem, err := factory(a.Config.Vault.Remote, vaultDir, remote.Options{
		MachineID:   a.Machine.ID,
		MachineName: a.Config.Machine.Name,
		Runner:      a.runner(),
	})
	if err != nil {
		return nil, fmt.Errorf("remote: %w", err)
	}
	if rem == nil {
		rem = remote.None{}
	}
	return rem, nil
}

// checkPin compares the opened vault id with the pin recorded for its path,
// pinning it when no pin exists yet.
func (a *App) checkPin(vaultDir, vaultID string) error {
	pinned, ok, err := state.PinnedVault(a.Dirs.State, vaultDir)
	if err != nil {
		return fmt.Errorf("vault pin: %w", err)
	}
	if ok {
		if pinned != vaultID {
			return fmt.Errorf("%w: %s was pinned to vault %s but now contains vault %s", state.ErrVaultMismatch, vaultDir, pinned, vaultID)
		}
		return nil
	}
	if err := state.PinVault(a.Dirs.State, vaultDir, vaultID); err != nil {
		return fmt.Errorf("pin vault: %w", err)
	}
	return nil
}

// session opens the state store and builds the engine for an opened vault.
func (a *App) session(v *vault.Vault, rem remote.Remote, warn func(string)) (*Session, error) {
	st, err := state.Open(a.Dirs.State, v.ID())
	if err != nil {
		v.Close()
		return nil, fmt.Errorf("open state: %w", err)
	}
	if warn == nil {
		warn = discard
	}
	return &Session{
		App:    a,
		Vault:  v,
		State:  st,
		Remote: rem,
		Engine: sync.New(v, st, a.Config, rem, a.Machine),
		Warn:   warn,
	}, nil
}

// machineInfo describes this machine for machines/<id>.json.enc.
func (a *App) machineInfo() vault.MachineInfo {
	host, _ := os.Hostname()
	return vault.MachineInfo{
		ID:       a.Machine.ID,
		Name:     a.Config.Machine.Name,
		Hostname: host,
		LastSeen: now(),
	}
}

// keyFileStatus returns the configured key file path and whether it exists.
func (a *App) keyFileStatus() (path string, exists bool, err error) {
	path, err = a.Config.KeyFilePath()
	if err != nil {
		return "", false, fmt.Errorf("key file path: %w", err)
	}
	if strings.TrimSpace(path) == "" {
		return "", false, errors.New("key.file.path is required when key.source is file")
	}
	_, statErr := os.Lstat(path)
	switch {
	case statErr == nil:
		return path, true, nil
	case errors.Is(statErr, fs.ErrNotExist):
		return path, false, nil
	default:
		return path, false, fmt.Errorf("key file %s: %w", path, statErr)
	}
}

// ensureKeyFile creates the configured key file when it does not exist
// (Setup, create branch only). It reports whether the file was created and its path.
func (a *App) ensureKeyFile() (created bool, path string, err error) {
	path, exists, err := a.keyFileStatus()
	if err != nil {
		return false, "", err
	}
	if exists {
		return false, path, nil
	}
	if err := keysource.WriteKeyFile(path); err != nil {
		return false, path, err
	}
	return true, path, nil
}

// withVaultFile makes sure vault.json is part of the pushed list.
func withVaultFile(written []string) []string {
	out := make([]string, 0, len(written)+1)
	seen := map[string]bool{}
	for _, w := range written {
		if w == "" || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	if !seen[vault.VaultFileName] {
		out = append(out, vault.VaultFileName)
	}
	return out
}

func discard(string) {}

// Close releases key material.
func (s *Session) Close() {
	if s == nil || s.Vault == nil {
		return
	}
	s.Vault.Close()
}

// check validates the receiver before any operation that needs the vault.
func (s *Session) check() error {
	if s == nil {
		return errors.New("app: nil Session")
	}
	if err := s.App.check(); err != nil {
		return err
	}
	if s.Vault == nil {
		return errors.New("app: session has no vault")
	}
	return nil
}

// warn forwards vault warnings to Warn (nil safe).
func (s *Session) warn(warnings []string) {
	if s.Warn == nil {
		return
	}
	for _, w := range warnings {
		if w != "" {
			s.Warn(w)
		}
	}
}

// ScanOptions builds scanner options from the config (hard excludes, tracked set).
func (s *Session) ScanOptions(projectID string) (scan.Options, error) {
	if err := s.check(); err != nil {
		return scan.Options{}, err
	}
	cfg := s.Config
	opts := scan.Options{
		Include:      orDefault(cfg.Scan.Include, scan.DefaultInclude),
		ExcludeDirs:  orDefault(cfg.Scan.ExcludeDirs, scan.DefaultExcludeDirs),
		ExcludeFiles: orDefault(cfg.Scan.ExcludeFiles, scan.DefaultExcludeFiles),
	}
	maxSize, err := cfg.MaxFileSize()
	if err != nil {
		return scan.Options{}, fmt.Errorf("scan.max_file_size: %w", err)
	}
	if maxSize <= 0 {
		maxSize = scan.DefaultMaxFileSize
	}
	opts.MaxFileSize = maxSize

	var hard []string
	add := func(p string) {
		if p == "" {
			return
		}
		p = filepath.Clean(p)
		for _, h := range hard {
			if h == p {
				return
			}
		}
		hard = append(hard, p)
	}
	keyPath, err := cfg.KeyFilePath()
	if err != nil {
		return scan.Options{}, fmt.Errorf("key file path: %w", err)
	}
	if keyPath != "" {
		add(keyPath)
		if resolved, err := filepath.EvalSymlinks(keyPath); err == nil {
			add(resolved)
		}
	}
	vaultDir, err := cfg.VaultPath()
	if err != nil {
		return scan.Options{}, fmt.Errorf("vault path: %w", err)
	}
	add(vaultDir)
	if resolved, err := filepath.EvalSymlinks(vaultDir); err == nil {
		add(resolved)
	}
	opts.HardExclude = hard

	if projectID != "" {
		if s.Engine == nil {
			return scan.Options{}, errors.New("app: session has no engine")
		}
		tracked, err := s.Engine.TrackedPaths(projectID)
		if err != nil {
			return scan.Options{}, fmt.Errorf("tracked paths of %s: %w", projectID, err)
		}
		if tracked == nil {
			tracked = map[string]bool{}
		}
		opts.Tracked = tracked
	}
	return opts, nil
}

// orDefault copies list, or the defaults when list is empty.
func orDefault(list []string, def func() []string) []string {
	if len(list) == 0 {
		return append([]string(nil), def()...)
	}
	return append([]string(nil), list...)
}

// VaultProjects returns the merged view of every vault project with the
// warnings the vault reported for skipped files (returned, not forwarded to Warn).
func (s *Session) VaultProjects() ([]vault.Project, []string, error) {
	if err := s.check(); err != nil {
		return nil, nil, err
	}
	projects, warnings, err := s.Vault.ListProjects()
	if err != nil {
		return nil, warnings, fmt.Errorf("list vault projects: %w", err)
	}
	return projects, warnings, nil
}

// listProjects is VaultProjects for internal callers: warnings go to Warn.
func (s *Session) listProjects() ([]vault.Project, error) {
	projects, warnings, err := s.VaultProjects()
	s.warn(warnings)
	return projects, err
}

// Identify computes the fingerprints of dir and the vault matches.
func (s *Session) Identify(ctx context.Context, dir string) ([]identity.Fingerprint, []identity.Match, error) {
	if err := s.check(); err != nil {
		return nil, nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(dir) == "" {
		return nil, nil, errors.New("app.Identify: empty directory")
	}
	fps, err := identity.Detect(ctx, dir, s.runner())
	if err != nil {
		return nil, nil, fmt.Errorf("detect project identity of %s: %w", dir, err)
	}
	projects, err := s.listProjects()
	if err != nil {
		return fps, nil, err
	}
	byID := make(map[string][]identity.Fingerprint, len(projects))
	for _, p := range projects {
		byID[p.ID] = p.Fingerprints
	}
	return fps, identity.MatchProjects(fps, byID), nil
}

// ProjectID derives the deterministic project id for fingerprints (random when only dir:).
func (s *Session) ProjectID(fps []identity.Fingerprint, forceRandom bool) (string, error) {
	if err := s.check(); err != nil {
		return "", err
	}
	if !forceRandom {
		if fp, ok := identity.Strongest(fps); ok {
			keys := s.Vault.Keys()
			if keys == nil {
				return "", errors.New("app.ProjectID: vault keys unavailable")
			}
			id := keys.KeyedID(crypto.ProjectIDPrefix, []byte(fp.String()))
			if len(id) < crypto.ProjectIDLen {
				return "", errors.New("app.ProjectID: vault keys unavailable")
			}
			return id[:crypto.ProjectIDLen], nil
		}
	}
	id, err := crypto.RandomHex(crypto.ProjectIDLen / 2)
	if err != nil {
		return "", fmt.Errorf("random project id: %w", err)
	}
	return id, nil
}

// LinkProject records a project mapping in the config and writes this machine's meta.
func (s *Session) LinkProject(ctx context.Context, id, name, dir string, fps []identity.Fingerprint) error {
	if err := s.check(); err != nil {
		return err
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("app.LinkProject: empty project id")
	}
	if strings.TrimSpace(dir) == "" {
		return errors.New("app.LinkProject: empty project directory")
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("project directory %s: %w", dir, err)
	}
	absDir = filepath.Clean(absDir)
	// A typo'd path must fail here, not surface later as "path missing"
	// during sync after it was already written to the config.
	fi, err := os.Stat(absDir)
	if err != nil {
		return fmt.Errorf("project directory %s: %w", absDir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("project directory %s: not a directory", absDir)
	}

	existing, warnings, err := s.Vault.ReadProject(id)
	s.warn(warnings)
	if err != nil {
		if !errors.Is(err, vault.ErrNoProject) {
			return fmt.Errorf("read vault project %s: %w", id, err)
		}
		existing = nil
	}

	meta := vault.ProjectMeta{Name: strings.TrimSpace(name), CreatedAt: now()}
	if existing != nil {
		meta.Fingerprints = identity.Union(existing.Fingerprints, fps)
		if meta.Name == "" {
			meta.Name = existing.Name
		}
		if !existing.CreatedAt.IsZero() {
			meta.CreatedAt = existing.CreatedAt
		}
	} else {
		meta.Fingerprints = identity.Union(fps)
	}
	if meta.Name == "" {
		meta.Name = filepath.Base(absDir)
	}
	if meta.Fingerprints == nil {
		meta.Fingerprints = []identity.Fingerprint{}
	}
	if err := s.Vault.WriteProjectMeta(id, s.Machine.ID, meta); err != nil {
		return fmt.Errorf("write project meta: %w", err)
	}
	s.Config.AddProject(config.ProjectConfig{
		ID:   id,
		Name: meta.Name,
		Path: paths.ContractHome(absDir),
	})
	if err := s.SaveConfig(); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	return nil
}

// UnlinkProject removes the mapping (vault untouched).
func (s *Session) UnlinkProject(id string) error {
	if err := s.check(); err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("app.UnlinkProject: empty project id")
	}
	if !s.Config.RemoveProject(id) {
		return fmt.Errorf("%w: %s", ErrNotLinked, id)
	}
	if err := s.SaveConfig(); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	if s.State != nil {
		s.State.DeleteProject(id)
		if err := s.State.Save(); err != nil {
			return fmt.Errorf("save state: %w", err)
		}
	}
	return nil
}

// ResolveProject finds a linked project by id or name.
//
// An exact id match always wins. Otherwise the name is compared
// case-insensitively with the configured names and with the vault names of
// the linked projects; a name shared by several linked projects is reported
// as ErrAmbiguousProject (listing the candidate ids) rather than resolved to
// whichever comes first in the config.
func (s *Session) ResolveProject(idOrName string) (*config.ProjectConfig, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	idOrName = strings.TrimSpace(idOrName)
	if idOrName == "" {
		return nil, errors.New("app.ResolveProject: empty project id or name")
	}
	cfg := s.Config
	for i := range cfg.Projects {
		if cfg.Projects[i].ID == idOrName {
			return &cfg.Projects[i], nil
		}
	}
	// Candidate ids in config order, without duplicates.
	var candidates []string
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			candidates = append(candidates, id)
		}
	}
	for i := range cfg.Projects {
		if p := &cfg.Projects[i]; p.Name != "" && strings.EqualFold(p.Name, idOrName) {
			add(p.ID)
		}
	}
	projects, err := s.listProjects()
	if err != nil {
		return nil, err
	}
	linked := s.linkedIDs()
	for _, vp := range projects {
		if linked[vp.ID] && vp.Name != "" && strings.EqualFold(vp.Name, idOrName) {
			add(vp.ID)
		}
	}
	switch len(candidates) {
	case 0:
		return nil, fmt.Errorf("%w: %q", ErrNotLinked, idOrName)
	case 1:
		for i := range cfg.Projects {
			if cfg.Projects[i].ID == candidates[0] {
				return &cfg.Projects[i], nil
			}
		}
		return nil, fmt.Errorf("%w: %q", ErrNotLinked, idOrName)
	default:
		return nil, fmt.Errorf("%w: %q matches projects %s; use the project id", ErrAmbiguousProject, idOrName, strings.Join(candidates, ", "))
	}
}

// UnlinkedProjects lists vault projects not mapped on this machine.
func (s *Session) UnlinkedProjects() ([]vault.Project, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	projects, err := s.listProjects()
	if err != nil {
		return nil, err
	}
	linked := s.linkedIDs()
	out := make([]vault.Project, 0, len(projects))
	for _, p := range projects {
		if !linked[p.ID] {
			out = append(out, p)
		}
	}
	return out, nil
}

// linkedIDs returns the set of project ids mapped in the config.
func (s *Session) linkedIDs() map[string]bool {
	ids := make(map[string]bool, len(s.Config.Projects))
	for _, p := range s.Config.Projects {
		if id := strings.TrimSpace(p.ID); id != "" {
			ids[id] = true
		}
	}
	return ids
}
