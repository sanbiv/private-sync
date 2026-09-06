package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/execx"
	"github.com/sanbiv/private-sync/internal/paths"
	"github.com/sanbiv/private-sync/internal/remote"
	"github.com/sanbiv/private-sync/internal/state"
	"github.com/sanbiv/private-sync/internal/vault"
)

// newGitFixture writes a config with a git remote pointing at url (key prompt).
func newGitFixture(t *testing.T, url string) fixture {
	t.Helper()
	root := t.TempDir()
	dirs := paths.Dirs{
		Config: filepath.Join(root, "config"),
		State:  filepath.Join(root, "state"),
	}
	cfg := config.Default(dirs)
	cfg.Machine.Name = "git-machine"
	cfg.Vault.Path = filepath.Join(root, "vault")
	cfg.Vault.Remote = config.RemoteConfig{Type: config.RemoteGit, Git: config.GitRemote{URL: url, Branch: "main"}}
	cfg.Key = config.KeyConfig{Source: config.KeyPrompt}
	if err := cfg.Save(dirs.ConfigFile()); err != nil {
		t.Fatalf("save config: %v", err)
	}
	return fixture{root: root, dirs: dirs, cfg: cfg}
}

// TestSetupCreateDiscardsGitHistory covers discardLocalHistory through Setup
// with a scripted remote: the git backend commits vault.json locally before
// pushing, so a failed creation must not leave a repository whose history the
// next Prepare would replay onto the winner's vault.json.
func TestSetupCreateDiscardsGitHistory(t *testing.T) {
	// simulateInit is what the real backend's Prepare does to a fresh vault.
	simulateInit := func(vaultDir string) func() error {
		return func() error {
			return os.WriteFile(filepath.Join(vaultDir, gitDirName, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600)
		}
	}
	tests := []struct {
		name        string
		remoteType  config.RemoteType
		preexisting bool  // .git exists before Setup
		pushErr     error // failure injected into Push
		wantGitKept bool
		wantHint    string // substring the error must contain ("" = must not mention moving aside)
		wantRemoved bool   // a "removed .../.git" log line
	}{
		{
			name:        "repository created this run is removed after a lost race",
			remoteType:  config.RemoteGit,
			pushErr:     &remote.VaultConflict{Dir: "d", LocalID: "a", RemoteID: "b"},
			wantRemoved: true,
		},
		{
			name:        "repository created this run is removed after a plain push failure",
			remoteType:  config.RemoteGit,
			pushErr:     remote.ErrNetwork,
			wantRemoved: true,
		},
		{
			name:        "pre-existing repository is kept; race error names the directory",
			remoteType:  config.RemoteGit,
			preexisting: true,
			pushErr:     &remote.VaultConflict{Dir: "d", LocalID: "a", RemoteID: "b"},
			wantGitKept: true,
			wantHint:    "if init keeps failing",
		},
		{
			name:        "pre-existing repository is kept; plain failure gets no hint",
			remoteType:  config.RemoteGit,
			preexisting: true,
			pushErr:     remote.ErrNetwork,
			wantGitKept: true,
		},
		{
			name:        "none remote never touches a .git directory",
			remoteType:  config.RemoteNone,
			pushErr:     &remote.VaultConflict{Dir: "d", LocalID: "a", RemoteID: "b"},
			wantGitKept: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var f fixture
			if tc.remoteType == config.RemoteGit {
				f = newGitFixture(t, filepath.Join(t.TempDir(), "unused.git"))
			} else {
				f = newFixture(t)
			}
			a := f.load(t)
			vaultDir, _ := a.Config.VaultPath()
			gitDir := filepath.Join(vaultDir, gitDirName)
			if err := os.MkdirAll(gitDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if tc.preexisting {
				writeFile(t, filepath.Join(gitDir, "HEAD"), "ref: refs/heads/main\n")
			} else {
				// Only Prepare creates it in this case.
				if err := os.RemoveAll(gitDir); err != nil {
					t.Fatal(err)
				}
			}
			rem := &fakeRemote{pushErr: tc.pushErr, onPrepare: func() error {
				if err := os.MkdirAll(gitDir, 0o700); err != nil {
					return err
				}
				return simulateInit(vaultDir)()
			}}
			useRemote(a, rem)
			var logs []string
			_, err := a.Setup(context.Background(), &fixedPrompter{pass: "correct horse"}, func(l string) { logs = append(logs, l) })
			if err == nil {
				t.Fatal("Setup succeeded, want failure")
			}
			if !errors.Is(err, tc.pushErr) && !errors.Is(err, ErrVaultRace) {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, isConflict := tc.pushErr.(*remote.VaultConflict); isConflict != errors.Is(err, ErrVaultRace) {
				t.Fatalf("race classification wrong for %v: %v", tc.pushErr, err)
			}
			if got := exists(gitDir); got != tc.wantGitKept {
				t.Fatalf(".git kept = %v, want %v (err %v, logs %v)", got, tc.wantGitKept, err, logs)
			}
			if tc.wantHint != "" {
				if !strings.Contains(err.Error(), tc.wantHint) || !strings.Contains(err.Error(), vaultDir) {
					t.Fatalf("error should tell the user to move %s aside: %v", vaultDir, err)
				}
			} else if strings.Contains(err.Error(), "if init keeps failing") {
				t.Fatalf("unexpected move-aside hint: %v", err)
			}
			removedLogged := false
			for _, l := range logs {
				if strings.Contains(l, "removed "+gitDir) {
					removedLogged = true
				}
			}
			if removedLogged != tc.wantRemoved {
				t.Fatalf("removed log = %v, want %v: %v", removedLogged, tc.wantRemoved, logs)
			}
			// The rest of the cleanup still happens.
			if vault.Exists(vaultDir) {
				t.Error("vault.json left behind")
			}
			if _, ok, _ := state.PinnedVault(f.dirs.State, vaultDir); ok {
				t.Error("pinned an unverified vault")
			}
		})
	}
}

func TestDiscardLocalHistory(t *testing.T) {
	base := errors.New("boom")
	race := errors.New("race: " + ErrVaultRace.Error())
	raceWrapped := errors.Join(ErrVaultRace, race)
	t.Run("nothing to remove keeps the error as is", func(t *testing.T) {
		dir := t.TempDir()
		if got := discardLocalHistory(dir, false, base, nil); got != base {
			t.Fatalf("got %v, want the original error", got)
		}
	})
	t.Run("removes the repository and logs it", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, gitDirName, "objects", "x"), "")
		var logs []string
		if got := discardLocalHistory(dir, false, base, func(l string) { logs = append(logs, l) }); got != base {
			t.Fatalf("got %v, want the original error", got)
		}
		if exists(filepath.Join(dir, gitDirName)) {
			t.Fatal(".git still there")
		}
		if len(logs) != 1 || !strings.Contains(logs[0], "removed") {
			t.Fatalf("logs = %v", logs)
		}
	})
	t.Run("pre-existing repository: race gets a hint, other errors do not", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, gitDirName, "HEAD"), "")
		got := discardLocalHistory(dir, true, raceWrapped, nil)
		if !errors.Is(got, ErrVaultRace) || !strings.Contains(got.Error(), dir) {
			t.Fatalf("race hint missing: %v", got)
		}
		if got := discardLocalHistory(dir, true, base, nil); got != base {
			t.Fatalf("non-race error changed: %v", got)
		}
		if !exists(filepath.Join(dir, gitDirName)) {
			t.Fatal("pre-existing .git was removed")
		}
	})
	t.Run("nil log is tolerated", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, gitDirName, "HEAD"), "")
		_ = discardLocalHistory(dir, false, base, nil)
	})
}

// --- real git ---------------------------------------------------------------

// requireGit skips without a git binary and isolates git from the user's
// global/system configuration, home directory and inherited repository or
// askpass variables.
func requireGit(t *testing.T) {
	t.Helper()
	if _, ok := execx.LookPath("git"); !ok {
		t.Skip("git not installed")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_SSH_COMMAND", "")
	for _, k := range []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE",
		"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR",
		"GIT_ASKPASS", "SSH_ASKPASS",
	} {
		if v, ok := os.LookupEnv(k); ok {
			t.Cleanup(func() { _ = os.Setenv(k, v) })
			_ = os.Unsetenv(k)
		}
	}
}

// rawGit runs git directly (outside the backend) for setup and assertions.
func rawGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
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

// interposed runs hook after the first Fetch: between Setup's initial fetch
// (remote empty) and the create branch, i.e. exactly where another machine
// can win the §10.1 race.
type interposed struct {
	remote.Remote
	fetches int
	hook    func()
}

func (r *interposed) Fetch(ctx context.Context, log func(string)) error {
	r.fetches++
	err := r.Remote.Fetch(ctx, log)
	if r.fetches == 1 && r.hook != nil {
		r.hook()
	}
	return err
}

// TestSetupGitLostRaceRecovery reproduces the §10.1 race against a real bare
// repository: B loses to A, gets ErrVaultRace, and following the remedy
// ("re-run init and choose open") must open A's vault instead of failing on
// a vault.json rebase conflict forever.
func TestSetupGitLostRaceRecovery(t *testing.T) {
	requireGit(t)
	bare := newBare(t)
	fa := newGitFixture(t, bare)
	fb := newGitFixture(t, bare)
	loadReal := func(f fixture) *App {
		t.Helper()
		a, err := Load(f.dirs.ConfigFile(), f.dirs, execx.Real())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return a
	}

	b := loadReal(fb)
	bVault, _ := b.Config.VaultPath()
	var aID string
	b.NewRemote = func(cfg config.RemoteConfig, dir string, o remote.Options) (remote.Remote, error) {
		real, err := remote.New(cfg, dir, o)
		if err != nil {
			return nil, err
		}
		return &interposed{Remote: real, hook: func() {
			a := loadReal(fa)
			sa, err := a.Setup(context.Background(), &fixedPrompter{pass: "pw-a"}, func(l string) { t.Log("A: " + l) })
			if err != nil {
				t.Fatalf("A Setup: %v", err)
			}
			aID = sa.Vault.ID()
			sa.Close()
		}}, nil
	}
	var logs1 []string
	_, err := b.Setup(context.Background(), &fixedPrompter{pass: "pw-b"}, func(l string) { logs1 = append(logs1, l) })
	if !errors.Is(err, ErrVaultRace) {
		t.Fatalf("B first Setup: want ErrVaultRace, got %v\nlogs:\n%s", err, strings.Join(logs1, "\n"))
	}
	if !strings.Contains(err.Error(), "re-run init") {
		t.Fatalf("race error lacks the remedy: %v", err)
	}
	if exists(filepath.Join(bVault, gitDirName)) {
		t.Fatalf("B's losing git history was kept; the next init would conflict on vault.json\nlogs:\n%s", strings.Join(logs1, "\n"))
	}
	if vault.Exists(bVault) {
		t.Fatal("B's unverified vault.json was kept")
	}
	if _, ok, _ := state.PinnedVault(fb.dirs.State, bVault); ok {
		t.Fatal("B pinned an unverified vault")
	}

	// The remedy: re-run init; the vault now exists on the remote, so open
	// is the only option and A's passphrase is what unlocks it.
	b2 := loadReal(fb)
	var logs2 []string
	sb, err := b2.Setup(context.Background(), &fixedPrompter{pass: "pw-a"}, func(l string) { logs2 = append(logs2, l) })
	if err != nil {
		t.Fatalf("B re-run init after lost race failed:\n  err: %v\n  logs:\n%s", err, strings.Join(logs2, "\n"))
	}
	defer sb.Close()
	if sb.Vault.ID() != aID {
		t.Fatalf("B opened %s, want A's vault %s", sb.Vault.ID(), aID)
	}
	if pinned, ok, _ := state.PinnedVault(fb.dirs.State, bVault); !ok || pinned != aID {
		t.Fatalf("B pin = %q, %v; want %s", pinned, ok, aID)
	}
	// B's local branch is A's history plus B's machine file, nothing from
	// the failed creation.
	if head, remoteHead := rawGit(t, bVault, "rev-parse", "HEAD"), rawGit(t, bare, "rev-parse", "refs/heads/main"); head == "" || remoteHead == "" {
		t.Fatal("cannot read heads")
	}
	if got := rawGit(t, bVault, "show", "HEAD:vault.json"); !strings.Contains(got, aID) {
		t.Fatalf("B's vault.json at HEAD is not A's: %s", got)
	}
	// And a wrong passphrase on the re-run is still refused (open, not create).
	b3 := loadReal(fb)
	if _, err := b3.Setup(context.Background(), &fixedPrompter{pass: "pw-b"}, nil); !errors.Is(err, vault.ErrWrongPassphrase) {
		t.Fatalf("B re-run with its own passphrase: want ErrWrongPassphrase, got %v", err)
	}
}

// TestSetupGitCreateThenOpen checks the plain (race-free) git flow end to end:
// create on A, open on B, and Open on A afterwards without any leftover.
func TestSetupGitCreateThenOpen(t *testing.T) {
	requireGit(t)
	bare := newBare(t)
	fa := newGitFixture(t, bare)
	fb := newGitFixture(t, bare)
	a, err := Load(fa.dirs.ConfigFile(), fa.dirs, execx.Real())
	if err != nil {
		t.Fatal(err)
	}
	sa, err := a.Setup(context.Background(), &fixedPrompter{pass: "pw"}, nil)
	if err != nil {
		t.Fatalf("A Setup: %v", err)
	}
	id := sa.Vault.ID()
	sa.Close()
	aVault, _ := a.Config.VaultPath()
	if !exists(filepath.Join(aVault, gitDirName)) {
		t.Fatal("A's repository is missing after a successful creation")
	}

	b, err := Load(fb.dirs.ConfigFile(), fb.dirs, execx.Real())
	if err != nil {
		t.Fatal(err)
	}
	sb, err := b.Setup(context.Background(), &fixedPrompter{pass: "pw"}, nil)
	if err != nil {
		t.Fatalf("B Setup: %v", err)
	}
	defer sb.Close()
	if sb.Vault.ID() != id {
		t.Fatalf("B opened %s, want %s", sb.Vault.ID(), id)
	}
	oa, err := a.Open(context.Background(), &fixedPrompter{pass: "pw"})
	if err != nil {
		t.Fatalf("A Open: %v", err)
	}
	oa.Close()
}

// --- project resolution -----------------------------------------------------

func TestResolveProjectAmbiguous(t *testing.T) {
	f := newFixture(t)
	s, _ := f.setup(t, "correct horse")
	link := func(id, name, sub string) {
		t.Helper()
		dir := filepath.Join(f.root, sub)
		writeFile(t, filepath.Join(dir, "x"), "")
		if err := s.LinkProject(context.Background(), id, name, dir, nil); err != nil {
			t.Fatal(err)
		}
	}
	const id1, id2, id3 = "1111111111111111", "2222222222222222", "3333333333333333"
	link(id1, "Dup", "one")
	link(id2, "dup", "two")
	link(id3, "Solo", "three")

	t.Run("ids always resolve", func(t *testing.T) {
		for _, id := range []string{id1, id2, id3} {
			p, err := s.ResolveProject(id)
			if err != nil || p.ID != id {
				t.Fatalf("ResolveProject(%s) = %+v, %v", id, p, err)
			}
		}
	})
	t.Run("unique name resolves", func(t *testing.T) {
		p, err := s.ResolveProject("solo")
		if err != nil || p.ID != id3 {
			t.Fatalf("ResolveProject(solo) = %+v, %v", p, err)
		}
	})
	t.Run("shared config name is ambiguous and lists the candidates", func(t *testing.T) {
		for _, q := range []string{"Dup", "dup", "DUP"} {
			_, err := s.ResolveProject(q)
			if !errors.Is(err, ErrAmbiguousProject) {
				t.Fatalf("ResolveProject(%q): want ErrAmbiguousProject, got %v", q, err)
			}
			if errors.Is(err, ErrNotLinked) {
				t.Fatalf("ambiguous reported as not linked: %v", err)
			}
			if msg := err.Error(); !strings.Contains(msg, id1) || !strings.Contains(msg, id2) || strings.Contains(msg, id3) {
				t.Fatalf("candidates not listed correctly: %v", err)
			}
		}
	})
	t.Run("vault name of one and config name of another are ambiguous", func(t *testing.T) {
		// id2 keeps vault name "dup" but its config name is changed locally;
		// id1's config name still says "Dup": both are candidates.
		pc, _ := s.Config.Project(id2)
		pc.Name = "local-two"
		defer func() { pc.Name = "dup" }()
		if _, err := s.ResolveProject("dup"); !errors.Is(err, ErrAmbiguousProject) {
			t.Fatalf("want ErrAmbiguousProject, got %v", err)
		}
		// The new local name itself is unique.
		if p, err := s.ResolveProject("LOCAL-TWO"); err != nil || p.ID != id2 {
			t.Fatalf("ResolveProject(local-two) = %+v, %v", p, err)
		}
	})
	t.Run("unlinking one candidate makes the name unique again", func(t *testing.T) {
		if err := s.UnlinkProject(id2); err != nil {
			t.Fatal(err)
		}
		p, err := s.ResolveProject("dup")
		if err != nil || p.ID != id1 {
			t.Fatalf("ResolveProject(dup) after unlink = %+v, %v", p, err)
		}
	})
}
