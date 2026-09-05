// Package remote moves the vault directory to and from a remote (spec §10).
package remote

import (
	"context"
	"errors"

	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/execx"
)

// Remote is a transport for the vault directory.
//
// Transport invariant: Push transfers only this machine's own files
// (machines/<id>, projects/*/meta/<id>, projects/*/state/<id>), blobs and the
// vault bootstrap files; Fetch never overwrites this machine's own files.
type Remote interface {
	Name() string
	Prepare(ctx context.Context, log func(string)) error
	Fetch(ctx context.Context, log func(string)) error
	// Push uploads; written lists vault-relative paths written since open.
	Push(ctx context.Context, written []string, log func(string)) error
}

// Options passed to every backend.
type Options struct {
	MachineID   string
	MachineName string
	Runner      execx.Runner
}

// ErrUnsupported is returned for unknown remote types.
var ErrUnsupported = errors.New("unsupported remote type")

// ErrAuth is returned (wrapped) when the remote needs interactive authentication.
var ErrAuth = errors.New("remote authentication required")

// New builds the remote for the config.
func New(cfg config.RemoteConfig, vaultDir string, o Options) (Remote, error) {
	switch cfg.Type {
	case config.RemoteNone, "":
		return None{}, nil
	case config.RemoteGit:
		return NewGit(cfg.Git, vaultDir, o)
	case config.RemoteRclone:
		return NewRclone(cfg.Rclone, vaultDir, o)
	}
	return nil, ErrUnsupported
}

// None is the no-op remote (vault inside a desktop-synced folder, or local only).
type None struct{}

func (None) Name() string                                       { return "none" }
func (None) Prepare(context.Context, func(string)) error        { return nil }
func (None) Fetch(context.Context, func(string)) error          { return nil }
func (None) Push(context.Context, []string, func(string)) error { return nil }

// NewGit builds the git backend.
func NewGit(cfg config.GitRemote, vaultDir string, o Options) (Remote, error) {
	return nil, errors.New("remote.NewGit: not implemented")
}

// NewRclone builds the rclone backend.
func NewRclone(cfg config.RcloneRemote, vaultDir string, o Options) (Remote, error) {
	return nil, errors.New("remote.NewRclone: not implemented")
}

// OwnFiles returns the vault-relative glob/paths that belong to a machine.
func OwnFiles(machineID string) []string {
	return []string{
		"machines/" + machineID + ".json.enc",
		"projects/*/meta/" + machineID + ".json.enc",
		"projects/*/state/" + machineID + ".json.enc",
	}
}
