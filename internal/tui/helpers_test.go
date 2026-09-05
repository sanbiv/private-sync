package tui

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/sanbiv/private-sync/internal/app"
	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/crypto"
	"github.com/sanbiv/private-sync/internal/execx"
	"github.com/sanbiv/private-sync/internal/paths"
	"github.com/sanbiv/private-sync/internal/remote"
	"github.com/sanbiv/private-sync/internal/state"
	"github.com/sanbiv/private-sync/internal/sync"
	"github.com/sanbiv/private-sync/internal/vault"
)

// failRunner refuses every subprocess: the fixtures below never need one
// (remote is "none" and no test drives real git), so any call is a bug.
func failRunner(t *testing.T) execx.Runner {
	t.Helper()
	return execx.Fake(func(c execx.Cmd) (execx.Result, error) {
		return execx.Result{}, errors.New("unexpected subprocess in test: " + c.Name)
	})
}

// newTestSession builds a real, fully local *app.Session (vault + state +
// engine, remote "none") rooted at t.TempDir(), with no network and no
// $HOME/XDG writes. It is used by tests that need Session-backed helpers
// (computeDashboardRows, computeWillRestore, ...).
func newTestSession(t *testing.T) *app.Session {
	t.Helper()

	root := t.TempDir()
	vaultDir := filepath.Join(root, "vault")
	stateDir := filepath.Join(root, "state")
	configDir := filepath.Join(root, "config")

	dirs := paths.Dirs{Config: configDir, State: stateDir}
	cfg := config.Default(dirs)
	cfg.Vault.Path = vaultDir
	cfg.Vault.Remote.Type = config.RemoteNone
	cfg.Machine.Name = "test-machine"

	machine, err := state.LoadMachine(stateDir)
	if err != nil {
		t.Fatalf("state.LoadMachine: %v", err)
	}

	params, err := crypto.DefaultKDFParams()
	if err != nil {
		t.Fatalf("crypto.DefaultKDFParams: %v", err)
	}
	v, err := vault.Create(vaultDir, []byte("correct horse battery staple"), params, machine.ID)
	if err != nil {
		t.Fatalf("vault.Create: %v", err)
	}
	t.Cleanup(v.Close)

	st, err := state.Open(stateDir, v.ID())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	rem := remote.None{}
	eng := sync.New(v, st, cfg, rem, machine)

	a := &app.App{
		Dirs:       dirs,
		ConfigPath: filepath.Join(configDir, "config.yaml"),
		Config:     cfg,
		Machine:    machine,
		Runner:     failRunner(t),
	}
	return &app.Session{App: a, Vault: v, State: st, Remote: rem, Engine: eng, Warn: func(string) {}}
}
