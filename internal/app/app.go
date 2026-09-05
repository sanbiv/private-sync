// Package app wires the core packages for both front ends (spec §12).
package app

import (
	"context"
	"errors"

	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/execx"
	"github.com/sanbiv/private-sync/internal/identity"
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

// App is the loaded configuration without any key material.
type App struct {
	Dirs       paths.Dirs
	ConfigPath string
	Config     *config.Config
	Machine    state.Machine
	Runner     execx.Runner
}

// Load reads the config (ErrNoConfig when absent) and the machine identity.
func Load(configPath string, dirs paths.Dirs, r execx.Runner) (*App, error) {
	return nil, errors.New("app.Load: not implemented")
}

// Session is an opened vault with its state, remote and engine.
type Session struct {
	*App
	Vault  *vault.Vault
	State  *state.Store
	Remote remote.Remote
	Engine *sync.Engine
}

// Open obtains the key, prepares the remote (no fetch), opens the vault and checks the pin.
func (a *App) Open(ctx context.Context, p ui.Prompter) (*Session, error) {
	return nil, errors.New("app.Open: not implemented")
}

// Setup runs the vault creation protocol (spec §10.1): prepare, fetch, open or create+push+verify.
func (a *App) Setup(ctx context.Context, p ui.Prompter, log func(string)) (*Session, error) {
	return nil, errors.New("app.Setup: not implemented")
}

// SaveConfig writes the config back to ConfigPath.
func (a *App) SaveConfig() error { return a.Config.Save(a.ConfigPath) }

// Close releases key material.
func (s *Session) Close() {}

// ScanOptions builds scanner options from the config (hard excludes, tracked set).
func (s *Session) ScanOptions(projectID string) (scan.Options, error) {
	return scan.Options{}, errors.New("app.ScanOptions: not implemented")
}

// Identify computes the fingerprints of dir and the vault matches.
func (s *Session) Identify(ctx context.Context, dir string) ([]identity.Fingerprint, []identity.Match, error) {
	return nil, nil, errors.New("app.Identify: not implemented")
}

// ProjectID derives the deterministic project id for fingerprints (random when only dir:).
func (s *Session) ProjectID(fps []identity.Fingerprint, forceRandom bool) (string, error) {
	return "", errors.New("app.ProjectID: not implemented")
}

// LinkProject records a project mapping in the config and writes this machine's meta.
func (s *Session) LinkProject(ctx context.Context, id, name, dir string, fps []identity.Fingerprint) error {
	return errors.New("app.LinkProject: not implemented")
}

// UnlinkProject removes the mapping (vault untouched).
func (s *Session) UnlinkProject(id string) error {
	return errors.New("app.UnlinkProject: not implemented")
}

// ResolveProject finds a linked project by id or name.
func (s *Session) ResolveProject(idOrName string) (*config.ProjectConfig, error) {
	return nil, errors.New("app.ResolveProject: not implemented")
}

// UnlinkedProjects lists vault projects not mapped on this machine.
func (s *Session) UnlinkedProjects() ([]vault.Project, error) { return nil, nil }
