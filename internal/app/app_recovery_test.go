package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/remote"
	"github.com/sanbiv/private-sync/internal/vault"
)

// storingRemote is a fake remote that behaves like a real one: Push copies the
// listed files out of the vault directory, Fetch copies back what it holds.
// failFetch fails the Fetch with that call number (1-based).
type storingRemote struct {
	dir       string // vault directory
	store     string // where pushed files live
	failFetch int
	fetchErr  error
	fetches   int
	pushes    [][]string
}

func (r *storingRemote) Name() string { return "storing" }

func (r *storingRemote) Prepare(context.Context, func(string)) error { return nil }

func (r *storingRemote) Fetch(context.Context, func(string)) error {
	r.fetches++
	if r.fetches == r.failFetch {
		return r.fetchErr
	}
	return filepath.WalkDir(r.store, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(r.store, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		dst := filepath.Join(r.dir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o600)
	})
}

func (r *storingRemote) Push(_ context.Context, written []string, _ func(string)) error {
	r.pushes = append(r.pushes, append([]string(nil), written...))
	for _, rel := range written {
		data, err := os.ReadFile(filepath.Join(r.dir, filepath.FromSlash(rel)))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		dst := filepath.Join(r.store, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// TestSetupCreateKeepsGeneratedKeyFileAfterPush covers the lockout bug: once
// vault.json (KDF salt + key wrapped under the generated random passphrase) is
// on the remote, deleting the generated key file destroys the only copy of a
// passphrase the user never saw. A failure of the post-push verification must
// keep the file, and re-running init must then open the vault that was pushed.
func TestSetupCreateKeepsGeneratedKeyFileAfterPush(t *testing.T) {
	f := newFixture(t)
	keyPath := filepath.Join(f.dirs.Config, "key")
	f.cfg.Key = config.KeyConfig{Source: config.KeyFile, File: config.FileKey{Path: keyPath}}
	if err := f.cfg.Save(f.dirs.ConfigFile()); err != nil {
		t.Fatal(err)
	}
	a := f.load(t)
	vaultDir, _ := a.Config.VaultPath()
	rem := &storingRemote{dir: vaultDir, store: t.TempDir(), failFetch: 2, fetchErr: remote.ErrNetwork}
	useRemote(a, rem)

	var logs []string
	_, err := a.Setup(context.Background(), nil, func(l string) { logs = append(logs, l) })
	if !errors.Is(err, remote.ErrNetwork) {
		t.Fatalf("want the verification fetch to fail with ErrNetwork, got %v", err)
	}
	if len(rem.pushes) != 1 || !contains(rem.pushes[0], vault.VaultFileName) {
		t.Fatalf("pushes = %v, want one including vault.json", rem.pushes)
	}
	if _, statErr := os.Lstat(filepath.Join(rem.store, vault.VaultFileName)); statErr != nil {
		t.Fatalf("vault.json did not reach the remote: %v", statErr)
	}
	key, readErr := os.ReadFile(keyPath)
	if readErr != nil {
		t.Fatalf("generated key file removed although vault.json is on the remote: %v", readErr)
	}
	if len(strings.TrimSpace(string(key))) == 0 {
		t.Fatal("generated key file is empty")
	}
	if !strings.Contains(err.Error(), keyPath) || !strings.Contains(err.Error(), "re-run init") {
		t.Fatalf("error should tell the user to keep %s and re-run init: %v", keyPath, err)
	}
	joined := strings.Join(logs, "\n")
	if strings.Contains(joined, "removed generated key file") {
		t.Fatalf("key file removed after a successful push:\n%s", joined)
	}
	if !strings.Contains(joined, "kept generated key file") {
		t.Fatalf("logs should report the kept key file:\n%s", joined)
	}

	// The documented recovery: re-run init. The remote still holds vault.json,
	// so this is the open branch, and the kept key file opens it.
	a2 := f.load(t)
	useRemote(a2, &storingRemote{dir: vaultDir, store: rem.store})
	s, err := a2.Setup(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("re-running init must open the pushed vault: %v", err)
	}
	defer s.Close()
	if got, err := vault.ReadID(vaultDir); err != nil || got != s.Vault.ID() {
		t.Fatalf("opened vault %s, remote holds %s (%v)", s.Vault.ID(), got, err)
	}
	if key2, err := os.ReadFile(keyPath); err != nil || string(key2) != string(key) {
		t.Fatalf("key file changed during the recovery: %v", err)
	}
}

// TestSetupCreateStillDiscardsKeyFileBeforePush keeps the other half of §11:
// while nothing has been published, a generated key file no vault uses is
// still removed.
func TestSetupCreateStillDiscardsKeyFileBeforePush(t *testing.T) {
	f := newFixture(t)
	keyPath := filepath.Join(f.dirs.Config, "key")
	f.cfg.Key = config.KeyConfig{Source: config.KeyFile, File: config.FileKey{Path: keyPath}}
	if err := f.cfg.Save(f.dirs.ConfigFile()); err != nil {
		t.Fatal(err)
	}
	a := f.load(t)
	useRemote(a, &fakeRemote{pushErr: remote.ErrNetwork})
	if _, err := a.Setup(context.Background(), nil, nil); !errors.Is(err, remote.ErrNetwork) {
		t.Fatalf("want ErrNetwork, got %v", err)
	}
	if _, statErr := os.Lstat(keyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("key file kept although the push failed: %v", statErr)
	}
}

// TestOpenWorksWithUnreachableRemote covers the offline bug: spec §2.1 marks
// status, projects list, trash and unlock as "network? no", but Open made a
// failing remote.Prepare (git fetch origin) fatal for every keyed command.
func TestOpenWorksWithUnreachableRemote(t *testing.T) {
	prepareErrors := map[string]error{
		"network":      remote.ErrNetwork,
		"auth":         remote.ErrAuth,
		"unclassified": errors.New("fatal: '/nonexistent/repo.git' does not appear to be a git repository"),
	}
	for name, prepareErr := range prepareErrors {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			s, _ := f.setup(t, "correct horse")
			id := s.Vault.ID()
			s.Close()

			a := f.load(t)
			rem := &fakeRemote{onPrepare: func() error { return prepareErr }}
			useRemote(a, rem)
			var warnings []string
			o, err := a.OpenWith(context.Background(), &fixedPrompter{pass: "correct horse"},
				OpenOptions{Warn: func(w string) { warnings = append(warnings, w) }})
			if err != nil {
				t.Fatalf("Open must fall back to the local vault copy: %v", err)
			}
			defer o.Close()
			if o.Vault.ID() != id {
				t.Fatalf("opened %s, want %s", o.Vault.ID(), id)
			}
			joined := strings.Join(warnings, "\n")
			if !strings.Contains(joined, "prepare remote fake") || !strings.Contains(joined, "local vault copy") {
				t.Fatalf("the unreachable remote should be a warning, got %q", joined)
			}
			if o.Warn == nil {
				t.Fatal("session has no Warn")
			}
		})
	}

	t.Run("plain Open too", func(t *testing.T) {
		f := newFixture(t)
		s, _ := f.setup(t, "correct horse")
		s.Close()
		a := f.load(t)
		useRemote(a, &fakeRemote{onPrepare: func() error { return remote.ErrNetwork }})
		o, err := a.Open(context.Background(), &fixedPrompter{pass: "correct horse"})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		o.Close()
	})

	t.Run("no local vault keeps the remote error", func(t *testing.T) {
		f := newFixture(t)
		a := f.load(t)
		useRemote(a, &fakeRemote{onPrepare: func() error { return remote.ErrNetwork }})
		p := &fixedPrompter{pass: "correct horse"}
		_, err := a.Open(context.Background(), p)
		if !errors.Is(err, remote.ErrNetwork) || !strings.Contains(err.Error(), "prepare remote fake") {
			t.Fatalf("without a local vault the remote failure must stand: %v", err)
		}
		if p.calls != 0 {
			t.Fatalf("prompted %d times, want 0", p.calls)
		}
	})
}

// TestOpenNoRemoteSkipsPrepare: --no-remote means no remote traffic at all.
func TestOpenNoRemoteSkipsPrepare(t *testing.T) {
	f := newFixture(t)
	s, _ := f.setup(t, "correct horse")
	id := s.Vault.ID()
	s.Close()

	a := f.load(t)
	rem := &fakeRemote{onPrepare: func() error { return errors.New("remote must not be touched") }}
	useRemote(a, rem)
	o, err := a.OpenWith(context.Background(), &fixedPrompter{pass: "correct horse"}, OpenOptions{NoRemote: true})
	if err != nil {
		t.Fatalf("OpenWith(NoRemote): %v", err)
	}
	defer o.Close()
	if o.Vault.ID() != id {
		t.Fatalf("opened %s, want %s", o.Vault.ID(), id)
	}
	if rem.prepared != 0 || rem.fetches != 0 || len(rem.pushes) != 0 {
		t.Fatalf("--no-remote touched the remote: prepared=%d fetches=%d pushes=%v", rem.prepared, rem.fetches, rem.pushes)
	}
}
