package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/crypto"
	"github.com/sanbiv/private-sync/internal/execx"
	"github.com/sanbiv/private-sync/internal/identity"
	"github.com/sanbiv/private-sync/internal/paths"
	"github.com/sanbiv/private-sync/internal/remote"
	"github.com/sanbiv/private-sync/internal/scan"
	"github.com/sanbiv/private-sync/internal/state"
	"github.com/sanbiv/private-sync/internal/vault"
)

// TestMain makes vault creation cheap: the smallest KDF costs the spec allows.
func TestMain(m *testing.M) {
	kdfParams = func() (crypto.KDFParams, error) {
		return crypto.KDFParams{
			Algo:    crypto.KDFAlgo,
			Time:    crypto.KDFMinTime,
			Memory:  crypto.KDFMinMemory,
			Threads: crypto.KDFMinThreads,
		}, nil
	}
	os.Exit(m.Run())
}

// fixedPrompter answers every password prompt with a fresh copy of pass
// (vault.Open zeroes the buffer it receives). Confirm answers with the
// default unless confirm is set; every question asked is recorded.
type fixedPrompter struct {
	pass      string
	calls     int
	confirm   func(question string, def bool) (bool, error)
	questions []string
}

func (f *fixedPrompter) Password(context.Context, string) ([]byte, error) {
	f.calls++
	return []byte(f.pass), nil
}

func (f *fixedPrompter) Confirm(_ context.Context, question string, def bool) (bool, error) {
	f.questions = append(f.questions, question)
	if f.confirm != nil {
		return f.confirm(question, def)
	}
	return def, nil
}

func answer(yes bool) func(string, bool) (bool, error) {
	return func(string, bool) (bool, error) { return yes, nil }
}

// fakeRemote is a scriptable Remote: pushErr fails Push, onFetch runs on
// every Fetch (1-based call number) and may fail it or rewrite the vault dir.
type fakeRemote struct {
	pushErr error
	onFetch func(call int) error
	pushes  [][]string
	fetches int
}

func (r *fakeRemote) Name() string                                { return "fake" }
func (r *fakeRemote) Prepare(context.Context, func(string)) error { return nil }
func (r *fakeRemote) Fetch(context.Context, func(string)) error {
	r.fetches++
	if r.onFetch != nil {
		return r.onFetch(r.fetches)
	}
	return nil
}
func (r *fakeRemote) Push(_ context.Context, written []string, _ func(string)) error {
	r.pushes = append(r.pushes, append([]string(nil), written...))
	return r.pushErr
}

// useRemote makes every remote the App builds be rem.
func useRemote(a *App, rem remote.Remote) {
	a.NewRemote = func(config.RemoteConfig, string, remote.Options) (remote.Remote, error) { return rem, nil }
}

// noGit is a runner where every subprocess fails (no git, no bw, no rclone).
func noGit() execx.Runner {
	return execx.Fake(func(c execx.Cmd) (execx.Result, error) {
		return execx.Result{ExitCode: 127}, errors.New("fake runner: " + c.Name + " not available")
	})
}

type fixture struct {
	root string
	dirs paths.Dirs
	cfg  *config.Config
}

// newFixture writes a config with remote none / key prompt below a temp dir.
func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	dirs := paths.Dirs{
		Config: filepath.Join(root, "config"),
		State:  filepath.Join(root, "state"),
	}
	cfg := config.Default(dirs)
	cfg.Machine.Name = "test-machine"
	cfg.Vault.Path = filepath.Join(root, "vault")
	cfg.Vault.Remote = config.RemoteConfig{Type: config.RemoteNone}
	cfg.Key = config.KeyConfig{Source: config.KeyPrompt, File: config.FileKey{Path: filepath.Join(dirs.Config, "key")}}
	if err := cfg.Save(dirs.ConfigFile()); err != nil {
		t.Fatalf("save config: %v", err)
	}
	return fixture{root: root, dirs: dirs, cfg: cfg}
}

func (f fixture) load(t *testing.T) *App {
	t.Helper()
	a, err := Load(f.dirs.ConfigFile(), f.dirs, noGit())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return a
}

func (f fixture) setup(t *testing.T, pass string) (*Session, []string) {
	t.Helper()
	a := f.load(t)
	var logs []string
	s, err := a.Setup(context.Background(), &fixedPrompter{pass: pass}, func(l string) { logs = append(logs, l) })
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(s.Close)
	return s, logs
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoad(t *testing.T) {
	t.Run("missing config is ErrNoConfig", func(t *testing.T) {
		root := t.TempDir()
		dirs := paths.Dirs{Config: filepath.Join(root, "config"), State: filepath.Join(root, "state")}
		_, err := Load(dirs.ConfigFile(), dirs, noGit())
		if !errors.Is(err, ErrNoConfig) {
			t.Fatalf("want ErrNoConfig, got %v", err)
		}
		if !errors.Is(err, config.ErrNotFound) {
			t.Fatalf("want wrapped config.ErrNotFound, got %v", err)
		}
	})
	t.Run("empty path falls back to dirs.ConfigFile", func(t *testing.T) {
		f := newFixture(t)
		a, err := Load("", f.dirs, noGit())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if a.ConfigPath != f.dirs.ConfigFile() {
			t.Fatalf("ConfigPath = %q, want %q", a.ConfigPath, f.dirs.ConfigFile())
		}
	})
	t.Run("loads config and machine identity", func(t *testing.T) {
		f := newFixture(t)
		a := f.load(t)
		if a.Config == nil || a.Config.Machine.Name != "test-machine" {
			t.Fatalf("config not loaded: %+v", a.Config)
		}
		if !vault.ValidMachineID(a.Machine.ID) {
			t.Fatalf("machine id %q is not a uuid", a.Machine.ID)
		}
		if a.Runner == nil {
			t.Fatal("runner is nil")
		}
		// Stable across loads.
		b := f.load(t)
		if b.Machine.ID != a.Machine.ID {
			t.Fatalf("machine id changed: %s vs %s", a.Machine.ID, b.Machine.ID)
		}
	})
	t.Run("nil runner defaults to Real", func(t *testing.T) {
		f := newFixture(t)
		a, err := Load(f.dirs.ConfigFile(), f.dirs, nil)
		if err != nil {
			t.Fatal(err)
		}
		if a.Runner == nil {
			t.Fatal("runner is nil")
		}
	})
	t.Run("invalid config is an error", func(t *testing.T) {
		f := newFixture(t)
		writeFile(t, f.dirs.ConfigFile(), "version: 1\nvault:\n  path: relative/path\n")
		_, err := Load(f.dirs.ConfigFile(), f.dirs, noGit())
		if err == nil || errors.Is(err, ErrNoConfig) {
			t.Fatalf("want validation error, got %v", err)
		}
	})
	t.Run("empty state dir is an error", func(t *testing.T) {
		f := newFixture(t)
		_, err := Load(f.dirs.ConfigFile(), paths.Dirs{Config: f.dirs.Config}, noGit())
		if err == nil {
			t.Fatal("want error for empty state dir")
		}
	})
}

func TestSetupCreatesAndPins(t *testing.T) {
	f := newFixture(t)
	s, logs := f.setup(t, "correct horse")
	vaultDir, _ := s.Config.VaultPath()

	if !vault.Exists(vaultDir) {
		t.Fatal("vault.json not created")
	}
	id, err := vault.ReadID(vaultDir)
	if err != nil || id != s.Vault.ID() {
		t.Fatalf("ReadID = %q, %v; want %q", id, err, s.Vault.ID())
	}
	pinned, ok, err := state.PinnedVault(f.dirs.State, vaultDir)
	if err != nil || !ok || pinned != s.Vault.ID() {
		t.Fatalf("pin = %q, %v, %v; want %q", pinned, ok, err, s.Vault.ID())
	}
	if s.State == nil || s.Engine == nil || s.Remote == nil {
		t.Fatalf("session incomplete: %+v", s)
	}
	if s.Remote.Name() == "" {
		t.Fatal("remote has no name")
	}
	machines, _, err := s.Vault.ListMachines()
	if err != nil {
		t.Fatal(err)
	}
	if len(machines) != 1 || machines[0].ID != s.Machine.ID || machines[0].Name != "test-machine" {
		t.Fatalf("machines = %+v", machines)
	}
	joined := strings.Join(logs, "\n")
	for _, want := range []string{"creating", "pushing", "verifying", "created vault"} {
		if !strings.Contains(joined, want) {
			t.Errorf("logs lack %q:\n%s", want, joined)
		}
	}
}

func TestSetupOpensExistingVault(t *testing.T) {
	f := newFixture(t)
	first, _ := f.setup(t, "correct horse")
	firstID := first.Vault.ID()
	first.Close()

	second, logs := f.setup(t, "correct horse")
	if second.Vault.ID() != firstID {
		t.Fatalf("second Setup opened %s, want %s", second.Vault.ID(), firstID)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "opening") {
		t.Fatalf("logs do not mention opening:\n%s", strings.Join(logs, "\n"))
	}
	// Nil log and nil ctx are tolerated.
	second.Close()
	a := f.load(t)
	third, err := a.Setup(nil, &fixedPrompter{pass: "correct horse"}, nil) //nolint:staticcheck // nil ctx on purpose
	if err != nil {
		t.Fatalf("Setup with nil log: %v", err)
	}
	third.Close()
}

func TestSetupWrongPassphrase(t *testing.T) {
	f := newFixture(t)
	s, _ := f.setup(t, "correct horse")
	s.Close()

	a := f.load(t)
	_, err := a.Setup(context.Background(), &fixedPrompter{pass: "wrong"}, nil)
	if !errors.Is(err, vault.ErrWrongPassphrase) {
		t.Fatalf("want ErrWrongPassphrase, got %v", err)
	}
	_, err = a.Open(context.Background(), &fixedPrompter{pass: "wrong"})
	if !errors.Is(err, vault.ErrWrongPassphrase) {
		t.Fatalf("Open: want ErrWrongPassphrase, got %v", err)
	}
}

func TestOpen(t *testing.T) {
	t.Run("no vault suggests init", func(t *testing.T) {
		f := newFixture(t)
		a := f.load(t)
		p := &fixedPrompter{pass: "x"}
		_, err := a.Open(context.Background(), p)
		if !errors.Is(err, vault.ErrNoVault) {
			t.Fatalf("want ErrNoVault, got %v", err)
		}
		if !strings.Contains(err.Error(), "init") {
			t.Fatalf("error should suggest init: %v", err)
		}
		// The passphrase is never requested when there is nothing to open.
		if p.calls != 0 {
			t.Fatalf("prompted %d times for a missing vault, want 0", p.calls)
		}
	})
	t.Run("remote failure costs no prompt", func(t *testing.T) {
		f := newFixture(t)
		s, _ := f.setup(t, "correct horse")
		s.Close()
		a := f.load(t)
		boom := errors.New("network down")
		a.NewRemote = func(config.RemoteConfig, string, remote.Options) (remote.Remote, error) { return nil, boom }
		p := &fixedPrompter{pass: "correct horse"}
		if _, err := a.Open(context.Background(), p); !errors.Is(err, boom) {
			t.Fatalf("want remote error, got %v", err)
		}
		if p.calls != 0 {
			t.Fatalf("prompted %d times although the remote failed, want 0", p.calls)
		}
	})
	t.Run("check does not mutate the App", func(t *testing.T) {
		f := newFixture(t)
		a := f.load(t)
		a.Runner = nil
		if _, err := a.Open(context.Background(), &fixedPrompter{pass: "x"}); !errors.Is(err, vault.ErrNoVault) {
			t.Fatalf("want ErrNoVault, got %v", err)
		}
		if a.Runner != nil {
			t.Fatal("Open wrote App.Runner; defaulting belongs to Load")
		}
		if a.runner() == nil {
			t.Fatal("runner() should default to a real runner")
		}
	})
	t.Run("opens after setup and keeps the pin", func(t *testing.T) {
		f := newFixture(t)
		s, _ := f.setup(t, "correct horse")
		id := s.Vault.ID()
		s.Close()

		a := f.load(t)
		p := &fixedPrompter{pass: "correct horse"}
		o, err := a.Open(context.Background(), p)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer o.Close()
		if o.Vault.ID() != id {
			t.Fatalf("opened %s, want %s", o.Vault.ID(), id)
		}
		if p.calls != 1 {
			t.Fatalf("prompted %d times, want 1", p.calls)
		}
		if o.State == nil || o.Engine == nil || o.Remote == nil {
			t.Fatal("session incomplete")
		}
	})
	t.Run("open pins when no pin exists", func(t *testing.T) {
		f := newFixture(t)
		s, _ := f.setup(t, "correct horse")
		vaultDir, _ := s.Config.VaultPath()
		id := s.Vault.ID()
		s.Close()
		if err := state.UnpinVault(f.dirs.State, vaultDir); err != nil {
			t.Fatal(err)
		}
		a := f.load(t)
		o, err := a.Open(context.Background(), &fixedPrompter{pass: "correct horse"})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		o.Close()
		pinned, ok, _ := state.PinnedVault(f.dirs.State, vaultDir)
		if !ok || pinned != id {
			t.Fatalf("pin after Open = %q, %v; want %q", pinned, ok, id)
		}
	})
	t.Run("nil app", func(t *testing.T) {
		var a *App
		if _, err := a.Open(context.Background(), &fixedPrompter{pass: "x"}); err == nil {
			t.Fatal("want error on nil App")
		}
		if _, err := a.Setup(context.Background(), &fixedPrompter{pass: "x"}, nil); err == nil {
			t.Fatal("want error on nil App")
		}
	})
}

func TestPinMismatch(t *testing.T) {
	f := newFixture(t)
	s, _ := f.setup(t, "correct horse")
	vaultDir, _ := s.Config.VaultPath()
	firstID := s.Vault.ID()
	s.Close()

	// Replace the vault at the same path with a different one.
	if err := os.Remove(filepath.Join(vaultDir, vault.VaultFileName)); err != nil {
		t.Fatal(err)
	}
	params, _ := kdfParams()
	other, err := vault.Create(vaultDir, []byte("correct horse"), params, s.Machine.ID)
	if err != nil {
		t.Fatal(err)
	}
	otherID := other.ID()
	other.Close()
	if otherID == firstID {
		t.Fatal("test bug: new vault has the same id")
	}

	a := f.load(t)
	_, err = a.Open(context.Background(), &fixedPrompter{pass: "correct horse"})
	if !errors.Is(err, state.ErrVaultMismatch) {
		t.Fatalf("Open: want ErrVaultMismatch, got %v", err)
	}
	if !strings.Contains(err.Error(), firstID) || !strings.Contains(err.Error(), otherID) {
		t.Fatalf("error should name both ids: %v", err)
	}
	_, err = a.Setup(context.Background(), &fixedPrompter{pass: "correct horse"}, nil)
	if !errors.Is(err, state.ErrVaultMismatch) {
		t.Fatalf("Setup: want ErrVaultMismatch, got %v", err)
	}
	// The pin was not touched.
	pinned, ok, _ := state.PinnedVault(f.dirs.State, vaultDir)
	if !ok || pinned != firstID {
		t.Fatalf("pin = %q, %v; want %q", pinned, ok, firstID)
	}
}

func TestSetupCreateStalePin(t *testing.T) {
	// A pin for the path whose vault.json is gone (dehydrated Drive folder,
	// clone not yet synced, wrong path) must never be replaced silently.
	pinnedFixture := func(t *testing.T) (fixture, string, string) {
		t.Helper()
		f := newFixture(t)
		s, _ := f.setup(t, "correct horse")
		vaultDir, _ := s.Config.VaultPath()
		firstID := s.Vault.ID()
		s.Close()
		if err := os.RemoveAll(vaultDir); err != nil {
			t.Fatal(err)
		}
		return f, vaultDir, firstID
	}
	assertUntouched := func(t *testing.T, f fixture, vaultDir, firstID string) {
		t.Helper()
		pinned, ok, _ := state.PinnedVault(f.dirs.State, vaultDir)
		if !ok || pinned != firstID {
			t.Fatalf("pin = %q, %v; want untouched %q", pinned, ok, firstID)
		}
		if vault.Exists(vaultDir) {
			t.Fatal("a vault was created although the user did not confirm")
		}
	}

	t.Run("confirmed: new vault replaces the pin", func(t *testing.T) {
		f, vaultDir, firstID := pinnedFixture(t)
		a := f.load(t)
		p := &fixedPrompter{pass: "correct horse", confirm: answer(true)}
		var logs []string
		s2, err := a.Setup(context.Background(), p, func(l string) { logs = append(logs, l) })
		if err != nil {
			t.Fatalf("Setup: %v", err)
		}
		defer s2.Close()
		if s2.Vault.ID() == firstID {
			t.Fatal("expected a new vault id")
		}
		pinned, ok, _ := state.PinnedVault(f.dirs.State, vaultDir)
		if !ok || pinned != s2.Vault.ID() {
			t.Fatalf("pin = %q, %v; want %q", pinned, ok, s2.Vault.ID())
		}
		if len(p.questions) != 1 || !strings.Contains(p.questions[0], firstID) || !strings.Contains(p.questions[0], vaultDir) {
			t.Fatalf("questions = %q, want one naming %s and %s", p.questions, firstID, vaultDir)
		}
		if !strings.Contains(strings.Join(logs, "\n"), "replacing stale pin") {
			t.Fatalf("logs do not mention the replacement:\n%s", strings.Join(logs, "\n"))
		}
	})
	t.Run("declined: ErrStalePin and nothing changes", func(t *testing.T) {
		f, vaultDir, firstID := pinnedFixture(t)
		a := f.load(t)
		p := &fixedPrompter{pass: "correct horse", confirm: answer(false)}
		_, err := a.Setup(context.Background(), p, nil)
		if !errors.Is(err, ErrStalePin) {
			t.Fatalf("want ErrStalePin, got %v", err)
		}
		if !strings.Contains(err.Error(), firstID) {
			t.Fatalf("error should name the pinned vault: %v", err)
		}
		if p.calls != 0 {
			t.Fatalf("prompted for the passphrase %d times before the decision", p.calls)
		}
		assertUntouched(t, f, vaultDir, firstID)
	})
	t.Run("default answer is no", func(t *testing.T) {
		f, vaultDir, firstID := pinnedFixture(t)
		a := f.load(t)
		_, err := a.Setup(context.Background(), &fixedPrompter{pass: "correct horse"}, nil)
		if !errors.Is(err, ErrStalePin) {
			t.Fatalf("want ErrStalePin, got %v", err)
		}
		assertUntouched(t, f, vaultDir, firstID)
	})
	t.Run("confirm error aborts", func(t *testing.T) {
		f, vaultDir, firstID := pinnedFixture(t)
		a := f.load(t)
		boom := errors.New("tty gone")
		p := &fixedPrompter{pass: "correct horse", confirm: func(string, bool) (bool, error) { return true, boom }}
		_, err := a.Setup(context.Background(), p, nil)
		if !errors.Is(err, ErrStalePin) || !errors.Is(err, boom) {
			t.Fatalf("want ErrStalePin wrapping the prompt error, got %v", err)
		}
		assertUntouched(t, f, vaultDir, firstID)
	})
	t.Run("nil prompter cannot confirm", func(t *testing.T) {
		f, vaultDir, firstID := pinnedFixture(t)
		a := f.load(t)
		_, err := a.Setup(context.Background(), nil, nil)
		if !errors.Is(err, ErrStalePin) {
			t.Fatalf("want ErrStalePin, got %v", err)
		}
		assertUntouched(t, f, vaultDir, firstID)
	})
	t.Run("no pin: no question asked", func(t *testing.T) {
		f := newFixture(t)
		a := f.load(t)
		p := &fixedPrompter{pass: "correct horse"}
		s, err := a.Setup(context.Background(), p, nil)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
		if len(p.questions) != 0 {
			t.Fatalf("unexpected questions %q", p.questions)
		}
	})
}

func TestSetupExistingVaultMissingKeyFile(t *testing.T) {
	// Second machine: the vault exists (synced folder) and key.source is
	// file, but the file has not been written yet. Setup must not generate a
	// random passphrase that can never open the vault.
	f := newFixture(t)
	s, _ := f.setup(t, "correct horse")
	vaultDir, _ := s.Config.VaultPath()
	id := s.Vault.ID()
	s.Close()

	keyPath := filepath.Join(f.dirs.Config, "key")
	f.cfg.Key = config.KeyConfig{Source: config.KeyFile, File: config.FileKey{Path: keyPath}}
	if err := f.cfg.Save(f.dirs.ConfigFile()); err != nil {
		t.Fatal(err)
	}
	a := f.load(t)
	p := &fixedPrompter{pass: "never used"}
	var logs []string
	_, err := a.Setup(context.Background(), p, func(l string) { logs = append(logs, l) })
	if !errors.Is(err, ErrKeyFileMissing) {
		t.Fatalf("want ErrKeyFileMissing, got %v", err)
	}
	for _, want := range []string{keyPath, "chmod 600", "init"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if _, statErr := os.Lstat(keyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("key file was created: %v", statErr)
	}
	if strings.Contains(strings.Join(logs, "\n"), "created key file") {
		t.Fatalf("logs claim a key file was created:\n%s", strings.Join(logs, "\n"))
	}
	if p.calls != 0 {
		t.Fatalf("prompted %d times with a file source", p.calls)
	}
	// The vault and its pin are untouched; writing the real passphrase to
	// the file makes the same Setup open it.
	pinned, ok, _ := state.PinnedVault(f.dirs.State, vaultDir)
	if !ok || pinned != id {
		t.Fatalf("pin = %q, %v; want %q", pinned, ok, id)
	}
	writeFile(t, keyPath, "correct horse\n")
	s2, err := a.Setup(context.Background(), p, nil)
	if err != nil {
		t.Fatalf("Setup after writing the key file: %v", err)
	}
	defer s2.Close()
	if s2.Vault.ID() != id {
		t.Fatalf("opened %s, want %s", s2.Vault.ID(), id)
	}
	// Open behaves the same way (no file ⇒ keysource error, never creates it).
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Open(context.Background(), p); err == nil {
		t.Fatal("Open without the key file should fail")
	}
	if _, statErr := os.Lstat(keyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Open created the key file: %v", statErr)
	}
}

func TestSetupCreateFailures(t *testing.T) {
	// Every failure after vault.Create must leave no unverified local vault
	// behind (and no pin), so the next init starts from the remote's state.
	type check func(t *testing.T, f fixture, vaultDir string, rem *fakeRemote, err error)
	assertCleaned := func(t *testing.T, f fixture, vaultDir string, machineID string) {
		t.Helper()
		if vault.Exists(vaultDir) {
			t.Error("local vault.json left behind after a failed creation")
		}
		if _, statErr := os.Lstat(filepath.Join(vaultDir, "machines", machineID+".json.enc")); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("machine file left behind: %v", statErr)
		}
		if _, ok, _ := state.PinnedVault(f.dirs.State, vaultDir); ok {
			t.Error("vault pinned although creation failed")
		}
	}
	tests := []struct {
		name   string
		remote func(vaultDir string) *fakeRemote
		want   error // errors.Is target
		race   bool
		check  check
	}{
		{
			name: "push rebase conflict on vault.json is a race",
			remote: func(string) *fakeRemote {
				return &fakeRemote{pushErr: &remote.RebaseConflictError{Files: []string{"vault.json"}, Cause: errors.New("git")}}
			},
			want: ErrVaultRace,
			race: true,
		},
		{
			name: "push vault id conflict is a race",
			remote: func(dir string) *fakeRemote {
				return &fakeRemote{pushErr: &remote.VaultConflict{Dir: dir, LocalID: "a", RemoteID: "b"}}
			},
			want: ErrVaultRace,
			race: true,
		},
		{
			name: "push rebase conflict on another file is not a race",
			remote: func(string) *fakeRemote {
				return &fakeRemote{pushErr: &remote.RebaseConflictError{Files: []string{"machines/x.json.enc"}, Cause: errors.New("git")}}
			},
			check: func(t *testing.T, _ fixture, _ string, _ *fakeRemote, err error) {
				if errors.Is(err, ErrVaultRace) {
					t.Fatalf("plain rebase conflict reported as a race: %v", err)
				}
				var rc *remote.RebaseConflictError
				if !errors.As(err, &rc) || !strings.Contains(err.Error(), "push to remote fake") {
					t.Fatalf("want wrapped push error, got %v", err)
				}
			},
		},
		{
			name:   "plain push error",
			remote: func(string) *fakeRemote { return &fakeRemote{pushErr: remote.ErrNetwork} },
			want:   remote.ErrNetwork,
		},
		{
			name: "verification fetch conflict on vault.json is a race",
			remote: func(string) *fakeRemote {
				return &fakeRemote{onFetch: func(call int) error {
					if call == 2 {
						return &remote.RebaseConflictError{Files: []string{"vault.json"}, Cause: errors.New("git")}
					}
					return nil
				}}
			},
			want: ErrVaultRace,
			race: true,
		},
		{
			name: "verification fetch fails",
			remote: func(string) *fakeRemote {
				return &fakeRemote{onFetch: func(call int) error {
					if call == 2 {
						return remote.ErrNetwork
					}
					return nil
				}}
			},
			want: remote.ErrNetwork,
		},
		{
			name: "another machine's vault.json wins the race and is kept",
			remote: func(dir string) *fakeRemote {
				return &fakeRemote{onFetch: func(call int) error {
					if call != 2 {
						return nil
					}
					// Simulate the fetch bringing the other machine's vault.
					if err := os.Remove(filepath.Join(dir, vault.VaultFileName)); err != nil {
						return err
					}
					params, _ := kdfParams()
					other, err := vault.Create(dir, []byte("their passphrase"), params, "11111111-1111-4111-8111-111111111111")
					if err != nil {
						return err
					}
					other.Close()
					return nil
				}}
			},
			want: ErrVaultRace,
			check: func(t *testing.T, f fixture, vaultDir string, _ *fakeRemote, err error) {
				// The winner's vault.json stays; our machine file and pin are gone.
				if !vault.Exists(vaultDir) {
					t.Fatal("the other machine's vault.json was removed")
				}
				if id, _ := vault.ReadID(vaultDir); !strings.Contains(err.Error(), id) {
					t.Fatalf("error should name the remote id %s: %v", id, err)
				}
				if _, ok, _ := state.PinnedVault(f.dirs.State, vaultDir); ok {
					t.Fatal("pinned an unverified vault")
				}
				entries, _ := os.ReadDir(filepath.Join(vaultDir, "machines"))
				if len(entries) != 0 {
					t.Fatalf("machine files left behind: %v", entries)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			a := f.load(t)
			vaultDir, _ := a.Config.VaultPath()
			rem := tc.remote(vaultDir)
			useRemote(a, rem)
			var logs []string
			_, err := a.Setup(context.Background(), &fixedPrompter{pass: "correct horse"}, func(l string) { logs = append(logs, l) })
			if err == nil {
				t.Fatal("Setup succeeded, want failure")
			}
			if tc.check != nil {
				tc.check(t, f, vaultDir, rem, err)
			} else {
				if !errors.Is(err, tc.want) {
					t.Fatalf("want %v, got %v", tc.want, err)
				}
				if errors.Is(err, ErrVaultRace) != tc.race {
					t.Fatalf("race=%v mismatch: %v", tc.race, err)
				}
				if tc.race && !strings.Contains(err.Error(), "re-run init") {
					t.Fatalf("race error lacks the remedy: %v", err)
				}
				assertCleaned(t, f, vaultDir, a.Machine.ID)
			}
			if len(rem.pushes) != 1 || !contains(rem.pushes[0], vault.VaultFileName) {
				t.Fatalf("pushes = %v, want one including vault.json", rem.pushes)
			}
			// After a cleaned-up failure the next Setup starts fresh (create
			// branch again, not open of a stale local vault).
			if !vault.Exists(vaultDir) {
				useRemote(a, &fakeRemote{})
				s, err := a.Setup(context.Background(), &fixedPrompter{pass: "correct horse"}, nil)
				if err != nil {
					t.Fatalf("Setup after cleanup: %v", err)
				}
				s.Close()
			}
		})
	}
}

func TestSetupCreateFailureRemovesGeneratedKeyFile(t *testing.T) {
	// The key file generated for a vault that was never verified is removed
	// again: leaving it would make the next init "open" the other machine's
	// vault with a random passphrase that cannot work.
	f := newFixture(t)
	keyPath := filepath.Join(f.dirs.Config, "key")
	f.cfg.Key = config.KeyConfig{Source: config.KeyFile, File: config.FileKey{Path: keyPath}}
	if err := f.cfg.Save(f.dirs.ConfigFile()); err != nil {
		t.Fatal(err)
	}
	a := f.load(t)
	useRemote(a, &fakeRemote{pushErr: &remote.RebaseConflictError{Files: []string{"vault.json"}, Cause: errors.New("git")}})
	var logs []string
	_, err := a.Setup(context.Background(), &fixedPrompter{pass: "unused"}, func(l string) { logs = append(logs, l) })
	if !errors.Is(err, ErrVaultRace) {
		t.Fatalf("want ErrVaultRace, got %v", err)
	}
	if _, statErr := os.Lstat(keyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("generated key file kept after the failed creation: %v", statErr)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "created key file") || !strings.Contains(joined, "removed generated key file") {
		t.Fatalf("logs should report creation and removal:\n%s", joined)
	}

	// A pre-existing key file is never removed.
	writeFile(t, keyPath, "my own passphrase\n")
	_, err = a.Setup(context.Background(), &fixedPrompter{pass: "unused"}, nil)
	if !errors.Is(err, ErrVaultRace) {
		t.Fatalf("want ErrVaultRace, got %v", err)
	}
	if content, readErr := os.ReadFile(keyPath); readErr != nil || string(content) != "my own passphrase\n" {
		t.Fatalf("pre-existing key file touched: %q, %v", content, readErr)
	}
}

func TestIsVaultRace(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain", errors.New("x"), false},
		{"network", remote.ErrNetwork, false},
		{"vault conflict sentinel", remote.ErrVaultConflict, true},
		{"vault conflict typed and wrapped", fmt.Errorf("push: %w", &remote.VaultConflict{Dir: "d", LocalID: "a", RemoteID: "b"}), true},
		{"rebase conflict on vault.json", &remote.RebaseConflictError{Files: []string{"machines/a.json.enc", "vault.json"}}, true},
		{"rebase conflict wrapped", fmt.Errorf("push: %w", &remote.RebaseConflictError{Files: []string{"vault.json"}}), true},
		{"rebase conflict elsewhere", &remote.RebaseConflictError{Files: []string{"machines/a.json.enc"}}, false},
		{"rebase conflict no files", &remote.RebaseConflictError{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isVaultRace(tc.err); got != tc.want {
				t.Fatalf("isVaultRace(%v) = %v, want %v", tc.err, got, tc.want)
			}
			if tc.want && !errors.Is(raceOr(tc.err, errors.New("fallback")), ErrVaultRace) {
				t.Fatal("raceOr should wrap ErrVaultRace")
			}
			if !tc.want {
				fallback := errors.New("fallback")
				if raceOr(tc.err, fallback) != fallback {
					t.Fatal("raceOr should return the fallback")
				}
			}
		})
	}
}

func TestRemoveLocalVault(t *testing.T) {
	t.Run("removes own files and vault.json with our id", func(t *testing.T) {
		dir := t.TempDir()
		params, _ := kdfParams()
		v, err := vault.Create(dir, []byte("pw"), params, "22222222-2222-4222-8222-222222222222")
		if err != nil {
			t.Fatal(err)
		}
		if err := v.WriteMachine(vault.MachineInfo{ID: "22222222-2222-4222-8222-222222222222", Name: "m", LastSeen: time.Now()}); err != nil {
			t.Fatal(err)
		}
		written := v.Written()
		id := v.ID()
		v.Close()
		if !contains(written, "vault.json") || len(written) < 2 {
			t.Fatalf("written = %v", written)
		}
		var logs []string
		removeLocalVault(dir, id, written, func(l string) { logs = append(logs, l) })
		if vault.Exists(dir) {
			t.Fatal("vault.json kept")
		}
		for _, rel := range written {
			if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(rel))); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s kept: %v", rel, err)
			}
		}
		if len(logs) != len(written) {
			t.Fatalf("logs = %q", logs)
		}
		// Idempotent and nil-log safe.
		removeLocalVault(dir, id, written, nil)
	})
	t.Run("keeps vault.json of another id and ignores escaping paths", func(t *testing.T) {
		dir := t.TempDir()
		params, _ := kdfParams()
		v, err := vault.Create(dir, []byte("pw"), params, "22222222-2222-4222-8222-222222222222")
		if err != nil {
			t.Fatal(err)
		}
		id := v.ID()
		v.Close()
		outside := filepath.Join(filepath.Dir(dir), "outside-"+filepath.Base(dir))
		writeFile(t, outside, "keep")
		t.Cleanup(func() { os.Remove(outside) })
		removeLocalVault(dir, "not-"+id, []string{"../" + filepath.Base(outside), "", ".", "..", outside}, nil)
		if !vault.Exists(dir) {
			t.Fatal("another machine's vault.json was removed")
		}
		if _, err := os.Stat(outside); err != nil {
			t.Fatalf("file outside the vault dir removed: %v", err)
		}
	})
}

func TestWarnForwarding(t *testing.T) {
	f := newFixture(t)
	s, _ := f.setup(t, "correct horse")
	vaultDir, _ := s.Config.VaultPath()
	var warned []string
	s.Warn = func(w string) { warned = append(warned, w) }

	// A foreign file (Drive-style duplicate) in projects/ is reported by name.
	junk := "meta (1).json.enc"
	writeFile(t, filepath.Join(vaultDir, "projects", junk), "junk")

	_, warnings, err := s.VaultProjects()
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], junk) {
		t.Fatalf("VaultProjects warnings = %v, %v", warnings, err)
	}
	if len(warned) != 0 {
		t.Fatalf("VaultProjects must return, not forward: %v", warned)
	}
	if _, err := s.UnlinkedProjects(); err != nil {
		t.Fatal(err)
	}
	if len(warned) != 1 || !strings.Contains(warned[0], junk) {
		t.Fatalf("UnlinkedProjects forwarded %v", warned)
	}
	warned = nil
	if _, err := s.ResolveProject("nobody"); !errors.Is(err, ErrNotLinked) {
		t.Fatalf("ResolveProject: %v", err)
	}
	if len(warned) != 1 {
		t.Fatalf("ResolveProject forwarded %v", warned)
	}
	warned = nil
	dir := filepath.Join(f.root, "p")
	writeFile(t, filepath.Join(dir, "x"), "")
	if _, _, err := s.Identify(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if len(warned) != 1 {
		t.Fatalf("Identify forwarded %v", warned)
	}
	// Setup wires Warn to its log; Open wires it to a no-op; nil is tolerated.
	s.Close()
	a := f.load(t)
	var logs []string
	s2, err := a.Setup(context.Background(), &fixedPrompter{pass: "correct horse"}, func(l string) { logs = append(logs, l) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.UnlinkedProjects(); err != nil {
		t.Fatal(err)
	}
	s2.Close()
	if !strings.Contains(strings.Join(logs, "\n"), junk) {
		t.Fatalf("Setup log lacks the forwarded warning:\n%s", strings.Join(logs, "\n"))
	}
	o, err := a.Open(context.Background(), &fixedPrompter{pass: "correct horse"})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if o.Warn == nil {
		t.Fatal("Open left Warn nil")
	}
	o.Warn = nil
	if _, err := o.UnlinkedProjects(); err != nil {
		t.Fatalf("nil Warn must be tolerated: %v", err)
	}
}

func TestSetupKeyFile(t *testing.T) {
	f := newFixture(t)
	keyPath := filepath.Join(f.dirs.Config, "key")
	f.cfg.Key = config.KeyConfig{Source: config.KeyFile, File: config.FileKey{Path: keyPath}}
	if err := f.cfg.Save(f.dirs.ConfigFile()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("key file should not exist yet: %v", err)
	}

	a := f.load(t)
	var logs []string
	p := &fixedPrompter{pass: "never used"}
	s, err := a.Setup(context.Background(), p, func(l string) { logs = append(logs, l) })
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	id := s.Vault.ID()
	s.Close()
	if p.calls != 0 {
		t.Fatalf("prompter used %d times with a file key source", p.calls)
	}
	st, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("key file not created: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %o, want 600", st.Mode().Perm())
	}
	if !strings.Contains(strings.Join(logs, "\n"), "created key file") {
		t.Fatalf("logs lack key file creation:\n%s", strings.Join(logs, "\n"))
	}

	// Second Setup reads the same file and opens the same vault; the file is
	// left alone.
	before, _ := os.ReadFile(keyPath)
	logs = nil
	s2, err := a.Setup(context.Background(), p, func(l string) { logs = append(logs, l) })
	if err != nil {
		t.Fatalf("second Setup: %v", err)
	}
	defer s2.Close()
	if s2.Vault.ID() != id {
		t.Fatalf("second Setup opened %s, want %s", s2.Vault.ID(), id)
	}
	after, _ := os.ReadFile(keyPath)
	if string(before) != string(after) {
		t.Fatal("key file was rewritten")
	}
	if strings.Contains(strings.Join(logs, "\n"), "created key file") {
		t.Fatal("key file reported as created twice")
	}

	// Open works with the file source too.
	o, err := a.Open(context.Background(), p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	o.Close()
}

func TestSessionClose(t *testing.T) {
	var s *Session
	s.Close() // nil safe
	(&Session{}).Close()

	f := newFixture(t)
	s, _ = f.setup(t, "correct horse")
	s.Close()
	s.Close() // idempotent
	if _, _, err := s.Vault.ListProjects(); !errors.Is(err, vault.ErrClosed) {
		t.Fatalf("vault still usable after Close: %v", err)
	}
}

func TestScanOptions(t *testing.T) {
	f := newFixture(t)
	s, _ := f.setup(t, "correct horse")
	vaultDir, _ := s.Config.VaultPath()
	keyPath, _ := s.Config.KeyFilePath()

	tests := []struct {
		name         string
		scan         config.ScanConfig
		projectID    string
		wantInclude  []string
		wantDirs     []string
		wantFiles    []string
		wantMax      int64
		wantTracked  bool
		wantErr      bool
		errSubstring string
	}{
		{
			name:        "defaults",
			wantInclude: scan.DefaultInclude(),
			wantDirs:    scan.DefaultExcludeDirs(),
			wantFiles:   scan.DefaultExcludeFiles(),
			wantMax:     2 * 1024 * 1024,
		},
		{
			name:        "custom lists replace defaults",
			scan:        config.ScanConfig{Include: []string{"*.secret"}, ExcludeDirs: []string{"tmp"}, ExcludeFiles: []string{"*.bak"}, MaxFileSize: "1KiB"},
			wantInclude: []string{"*.secret"},
			wantDirs:    []string{"tmp"},
			wantFiles:   []string{"*.bak"},
			wantMax:     1024,
		},
		{
			name:        "tracked set requested for a project",
			projectID:   "0123456789abcdef",
			wantInclude: scan.DefaultInclude(),
			wantDirs:    scan.DefaultExcludeDirs(),
			wantFiles:   scan.DefaultExcludeFiles(),
			wantMax:     2 * 1024 * 1024,
			wantTracked: true,
		},
		{
			name:         "bad size",
			scan:         config.ScanConfig{MaxFileSize: "lots"},
			wantErr:      true,
			errSubstring: "max_file_size",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s.Config.Scan = tc.scan
			opts, err := s.ScanOptions(tc.projectID)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), tc.errSubstring) {
					t.Fatalf("want error containing %q, got %v", tc.errSubstring, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ScanOptions: %v", err)
			}
			if !equalStrings(opts.Include, tc.wantInclude) {
				t.Errorf("Include = %v, want %v", opts.Include, tc.wantInclude)
			}
			if !equalStrings(opts.ExcludeDirs, tc.wantDirs) {
				t.Errorf("ExcludeDirs = %v, want %v", opts.ExcludeDirs, tc.wantDirs)
			}
			if !equalStrings(opts.ExcludeFiles, tc.wantFiles) {
				t.Errorf("ExcludeFiles = %v, want %v", opts.ExcludeFiles, tc.wantFiles)
			}
			if opts.MaxFileSize != tc.wantMax {
				t.Errorf("MaxFileSize = %d, want %d", opts.MaxFileSize, tc.wantMax)
			}
			if !contains(opts.HardExclude, vaultDir) {
				t.Errorf("HardExclude %v lacks vault dir %s", opts.HardExclude, vaultDir)
			}
			if !contains(opts.HardExclude, keyPath) {
				t.Errorf("HardExclude %v lacks key file %s", opts.HardExclude, keyPath)
			}
			if tc.wantTracked && opts.Tracked == nil {
				t.Error("Tracked is nil, want a map")
			}
			if !tc.wantTracked && opts.Tracked != nil {
				t.Error("Tracked set without a project id")
			}
			// Options are copies: mutating them must not touch the config.
			if len(opts.Include) > 0 {
				opts.Include[0] = "mutated"
				if len(s.Config.Scan.Include) > 0 && s.Config.Scan.Include[0] == "mutated" {
					t.Error("ScanOptions aliases config slices")
				}
			}
		})
	}

	t.Run("no key file path", func(t *testing.T) {
		s.Config.Scan = config.ScanConfig{}
		s.Config.Key.File.Path = ""
		opts, err := s.ScanOptions("")
		if err != nil {
			t.Fatal(err)
		}
		if len(opts.HardExclude) == 0 || !contains(opts.HardExclude, vaultDir) {
			t.Fatalf("HardExclude = %v", opts.HardExclude)
		}
		for _, h := range opts.HardExclude {
			if h == "" {
				t.Fatal("empty hard exclude entry")
			}
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestIdentifyAndProjectIDDeterminism(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.root, "myapp")
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/myapp\n\ngo 1.27\n")

	s1, _ := f.setup(t, "correct horse")
	fps1, matches, err := s1.Identify(context.Background(), dir)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("empty vault should give no matches, got %+v", matches)
	}
	var hasGo, hasDir bool
	for _, fp := range fps1 {
		switch fp.Kind {
		case "go":
			hasGo = fp.Value == "example.com/myapp" && fp.Level == identity.LevelPackage
		case "dir":
			hasDir = fp.Value == "myapp" && fp.Level == identity.LevelDir
		}
	}
	if !hasGo || !hasDir {
		t.Fatalf("fingerprints = %+v", fps1)
	}
	id1, err := s1.ProjectID(fps1, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(id1) != 16 || !isHex(id1) {
		t.Fatalf("id %q is not 16 hex chars", id1)
	}
	strongest, _ := identity.Strongest(fps1)
	if want := s1.Vault.Keys().KeyedID(crypto.ProjectIDPrefix, []byte(strongest.String()))[:16]; id1 != want {
		t.Fatalf("id = %s, want KeyedID prefix %s", id1, want)
	}
	if want := s1.Vault.Keys().ProjectID(strongest.String()); id1 != want {
		t.Fatalf("id = %s, want Keys.ProjectID %s", id1, want)
	}

	// Random ids differ, are 16 hex and are not deterministic.
	r1, err := s1.ProjectID(fps1, true)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s1.ProjectID(fps1, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(r1) != 16 || !isHex(r1) || r1 == r2 || r1 == id1 {
		t.Fatalf("random ids %s %s (deterministic %s)", r1, r2, id1)
	}
	// dir: only ⇒ random even without forceRandom.
	dirOnly := []identity.Fingerprint{{Kind: "dir", Value: "x", Level: identity.LevelDir}}
	d1, _ := s1.ProjectID(dirOnly, false)
	d2, _ := s1.ProjectID(dirOnly, false)
	if len(d1) != 16 || d1 == d2 {
		t.Fatalf("dir-only ids should be random: %s %s", d1, d2)
	}
	// Empty fingerprints ⇒ random, no panic.
	if e, err := s1.ProjectID(nil, false); err != nil || len(e) != 16 {
		t.Fatalf("empty fps: %q %v", e, err)
	}

	// Link and re-identify from a second session on the same vault.
	if err := s1.LinkProject(context.Background(), id1, "myapp", dir, fps1); err != nil {
		t.Fatalf("LinkProject: %v", err)
	}
	s1.Close()

	s2, _ := f.setup(t, "correct horse")
	fps2, matches, err := s2.Identify(context.Background(), dir)
	if err != nil {
		t.Fatalf("Identify(2): %v", err)
	}
	id2, err := s2.ProjectID(fps2, false)
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id1 {
		t.Fatalf("project id differs across sessions: %s vs %s", id1, id2)
	}
	if len(matches) != 1 || matches[0].ProjectID != id1 || matches[0].Strength != identity.StrengthStrong {
		t.Fatalf("matches = %+v, want strong match on %s", matches, id1)
	}

	// Errors.
	if _, _, err := s2.Identify(context.Background(), ""); err == nil {
		t.Fatal("want error for empty dir")
	}
	if _, _, err := s2.Identify(context.Background(), filepath.Join(f.root, "nope")); err == nil {
		t.Fatal("want error for a missing dir")
	}
}

func isHex(s string) bool {
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func TestLinkProject(t *testing.T) {
	f := newFixture(t)
	s, _ := f.setup(t, "correct horse")
	dir := filepath.Join(f.root, "proj")
	writeFile(t, filepath.Join(dir, "package.json"), `{"name":"proj"}`)
	fps := []identity.Fingerprint{
		{Kind: "npm", Value: "proj", Level: identity.LevelPackage},
		{Kind: "dir", Value: "proj", Level: identity.LevelDir},
	}
	id, err := s.ProjectID(fps, false)
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	now = func() time.Time { return fixed }
	t.Cleanup(func() { now = func() time.Time { return time.Now().UTC() } })

	if err := s.LinkProject(context.Background(), id, "Proj", dir, fps); err != nil {
		t.Fatalf("LinkProject: %v", err)
	}

	// Vault meta.
	vp, _, err := s.Vault.ReadProject(id)
	if err != nil {
		t.Fatalf("ReadProject: %v", err)
	}
	if vp.Name != "Proj" || len(vp.Fingerprints) != 2 || !vp.CreatedAt.Equal(fixed) {
		t.Fatalf("vault project = %+v", vp)
	}
	if len(vp.Machines) != 1 || vp.Machines[0] != s.Machine.ID {
		t.Fatalf("machines = %v", vp.Machines)
	}
	meta, err := s.Vault.ReadProjectMeta(id, s.Machine.ID)
	if err != nil || meta.Name != "Proj" {
		t.Fatalf("meta = %+v, %v", meta, err)
	}

	// Config in memory and on disk.
	pc, ok := s.Config.Project(id)
	if !ok || pc.Name != "Proj" || pc.Path != paths.ContractHome(dir) {
		t.Fatalf("config project = %+v, %v", pc, ok)
	}
	reloaded, err := config.Load(f.dirs.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if rp, ok := reloaded.Project(id); !ok || rp.Path != paths.ContractHome(dir) {
		t.Fatalf("saved config project = %+v, %v", rp, ok)
	}
	if got, err := reloaded.ProjectPath(id); err != nil || got != dir {
		t.Fatalf("ProjectPath = %q, %v; want %q", got, err, dir)
	}

	// Relinking with more fingerprints and no name keeps name + created_at and unions.
	later := fixed.Add(time.Hour)
	now = func() time.Time { return later }
	more := []identity.Fingerprint{{Kind: "git", Value: "github.com/me/proj", Level: identity.LevelStrong}}
	if err := s.LinkProject(context.Background(), id, "", dir, more); err != nil {
		t.Fatalf("relink: %v", err)
	}
	vp, _, err = s.Vault.ReadProject(id)
	if err != nil {
		t.Fatal(err)
	}
	if vp.Name != "Proj" || !vp.CreatedAt.Equal(fixed) {
		t.Fatalf("relinked project = %+v", vp)
	}
	if len(vp.Fingerprints) != 3 {
		t.Fatalf("fingerprints after union = %+v", vp.Fingerprints)
	}
	if n := len(s.Config.Projects); n != 1 {
		t.Fatalf("config has %d projects, want 1", n)
	}

	// Name defaults to the directory basename for a fresh project.
	other := filepath.Join(f.root, "other")
	writeFile(t, filepath.Join(other, "README"), "")
	if err := s.LinkProject(context.Background(), "aaaaaaaaaaaaaaaa", "", other, nil); err != nil {
		t.Fatalf("link other: %v", err)
	}
	if op, ok := s.Config.Project("aaaaaaaaaaaaaaaa"); !ok || op.Name != "other" {
		t.Fatalf("other = %+v, %v", op, ok)
	}
	if vo, _, err := s.Vault.ReadProject("aaaaaaaaaaaaaaaa"); err != nil || vo.Name != "other" || len(vo.Fingerprints) != 0 {
		t.Fatalf("other vault project = %+v, %v", vo, err)
	}

	// Errors.
	if err := s.LinkProject(context.Background(), "", "x", dir, fps); err == nil {
		t.Fatal("want error for empty id")
	}
	if err := s.LinkProject(context.Background(), "bbbbbbbbbbbbbbbb", "x", "", fps); err == nil {
		t.Fatal("want error for empty dir")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.LinkProject(cancelled, "bbbbbbbbbbbbbbbb", "x", dir, fps); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestUnlinkAndResolve(t *testing.T) {
	f := newFixture(t)
	s, _ := f.setup(t, "correct horse")
	dir := filepath.Join(f.root, "proj")
	writeFile(t, filepath.Join(dir, "x"), "")
	const id = "0123456789abcdef"
	if err := s.LinkProject(context.Background(), id, "VaultName", dir, nil); err != nil {
		t.Fatal(err)
	}
	// Seed a base and a journal seq in the state for this project.
	s.State.SetBase(id, "a.env", state.BaseEntry{Blob: "ab", Kind: vault.KindFile, Clock: vault.Clock{s.Machine.ID: 1}})
	if err := s.State.Save(); err != nil {
		t.Fatal(err)
	}

	t.Run("resolve by id and by config name", func(t *testing.T) {
		for _, q := range []string{id, "VaultName", "vaultname"} {
			p, err := s.ResolveProject(q)
			if err != nil || p.ID != id {
				t.Fatalf("ResolveProject(%q) = %+v, %v", q, p, err)
			}
		}
	})
	t.Run("resolve by vault name when the config name differs", func(t *testing.T) {
		pc, _ := s.Config.Project(id)
		pc.Name = "local-alias"
		defer func() { pc.Name = "VaultName" }()
		p, err := s.ResolveProject("vaultname")
		if err != nil || p.ID != id {
			t.Fatalf("ResolveProject by vault name = %+v, %v", p, err)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		if _, err := s.ResolveProject("nope"); !errors.Is(err, ErrNotLinked) {
			t.Fatalf("want ErrNotLinked, got %v", err)
		}
		if _, err := s.ResolveProject(""); err == nil {
			t.Fatal("want error for empty query")
		}
	})
	t.Run("vault project not linked is not resolved by name", func(t *testing.T) {
		meta := vault.ProjectMeta{Name: "Elsewhere", CreatedAt: time.Now()}
		if err := s.Vault.WriteProjectMeta("fedcba9876543210", s.Machine.ID, meta); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ResolveProject("Elsewhere"); !errors.Is(err, ErrNotLinked) {
			t.Fatalf("want ErrNotLinked, got %v", err)
		}
	})
	t.Run("unlink", func(t *testing.T) {
		if err := s.UnlinkProject(id); err != nil {
			t.Fatalf("UnlinkProject: %v", err)
		}
		if _, ok := s.Config.Project(id); ok {
			t.Fatal("project still in config")
		}
		reloaded, err := config.Load(f.dirs.ConfigFile())
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := reloaded.Project(id); ok {
			t.Fatal("project still in saved config")
		}
		if bases := s.State.Bases(id); len(bases) != 0 {
			t.Fatalf("state bases remain: %v", bases)
		}
		if contains(s.State.Projects(), id) {
			t.Fatal("state still lists the project")
		}
		// Vault untouched.
		if _, _, err := s.Vault.ReadProject(id); err != nil {
			t.Fatalf("vault project gone: %v", err)
		}
		if err := s.UnlinkProject(id); !errors.Is(err, ErrNotLinked) {
			t.Fatalf("second unlink: want ErrNotLinked, got %v", err)
		}
		if err := s.UnlinkProject(""); err == nil {
			t.Fatal("want error for empty id")
		}
	})
}

func TestUnlinkedProjects(t *testing.T) {
	f := newFixture(t)
	s, _ := f.setup(t, "correct horse")

	empty, err := s.UnlinkedProjects()
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty vault: %v, %v", empty, err)
	}

	dir := filepath.Join(f.root, "linked")
	writeFile(t, filepath.Join(dir, "x"), "")
	if err := s.LinkProject(context.Background(), "1111111111111111", "linked", dir, nil); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"2222222222222222", "3333333333333333"} {
		if err := s.Vault.WriteProjectMeta(id, s.Machine.ID, vault.ProjectMeta{Name: "p" + id[:1], CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.UnlinkedProjects()
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, p := range got {
		ids[p.ID] = true
	}
	if len(got) != 2 || !ids["2222222222222222"] || !ids["3333333333333333"] || ids["1111111111111111"] {
		t.Fatalf("UnlinkedProjects = %+v", got)
	}

	// Linking one of them removes it from the list.
	dir2 := filepath.Join(f.root, "two")
	writeFile(t, filepath.Join(dir2, "x"), "")
	if err := s.LinkProject(context.Background(), "2222222222222222", "", dir2, nil); err != nil {
		t.Fatal(err)
	}
	if pc, ok := s.Config.Project("2222222222222222"); !ok || pc.Name != "p2" {
		t.Fatalf("linked existing project took name %+v (want vault name p2)", pc)
	}
	got, err = s.UnlinkedProjects()
	if err != nil || len(got) != 1 || got[0].ID != "3333333333333333" {
		t.Fatalf("UnlinkedProjects after link = %+v, %v", got, err)
	}

	projects, warnings, err := s.VaultProjects()
	if err != nil || len(projects) != 3 || len(warnings) != 0 {
		t.Fatalf("VaultProjects = %d projects, %v, %v", len(projects), warnings, err)
	}
}

func TestSessionChecks(t *testing.T) {
	var s *Session
	if _, err := s.ScanOptions(""); err == nil {
		t.Fatal("nil session ScanOptions")
	}
	if _, _, err := s.Identify(context.Background(), "."); err == nil {
		t.Fatal("nil session Identify")
	}
	if _, err := s.ProjectID(nil, false); err == nil {
		t.Fatal("nil session ProjectID")
	}
	if err := s.LinkProject(context.Background(), "a", "b", ".", nil); err == nil {
		t.Fatal("nil session LinkProject")
	}
	if err := s.UnlinkProject("a"); err == nil {
		t.Fatal("nil session UnlinkProject")
	}
	if _, err := s.ResolveProject("a"); err == nil {
		t.Fatal("nil session ResolveProject")
	}
	if _, err := s.UnlinkedProjects(); err == nil {
		t.Fatal("nil session UnlinkedProjects")
	}
	f := newFixture(t)
	a := f.load(t)
	noVault := &Session{App: a}
	if _, err := noVault.UnlinkedProjects(); err == nil {
		t.Fatal("session without vault should error")
	}
}

func TestWithVaultFile(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, []string{"vault.json"}},
		{"already present", []string{"vault.json", "machines/a.json.enc"}, []string{"vault.json", "machines/a.json.enc"}},
		{"appended and deduped", []string{"machines/a.json.enc", "", "machines/a.json.enc"}, []string{"machines/a.json.enc", "vault.json"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := withVaultFile(tc.in); !equalStrings(got, tc.want) {
				t.Fatalf("withVaultFile(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
