// Package remote moves the vault directory to and from a remote (spec §10).
package remote

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

// ErrNetwork is returned (wrapped) when the remote could not be reached.
var ErrNetwork = errors.New("remote unreachable")

// ErrVaultConflict is returned (wrapped in a *VaultConflict) when the local
// vault.json belongs to a different vault than the one on the remote (§10.1).
var ErrVaultConflict = errors.New("vault id conflict")

// VaultConflict reports that the local vault directory already holds a
// vault.json whose id differs from the remote's. It unwraps to ErrVaultConflict.
type VaultConflict struct {
	Dir      string // local vault directory
	LocalID  string // id in <Dir>/vault.json
	RemoteID string // id in the remote's vault.json
}

func (e *VaultConflict) Error() string {
	return fmt.Sprintf("%v: %s already contains vault %q but the remote holds vault %q; "+
		"move the local vault directory aside to open the remote vault, or point the remote at an empty location to keep the local one",
		ErrVaultConflict, e.Dir, e.LocalID, e.RemoteID)
}

// Unwrap lets errors.Is(err, ErrVaultConflict) succeed.
func (e *VaultConflict) Unwrap() error { return ErrVaultConflict }

// RebaseConflictError is returned by the git backend when integrating the
// remote history conflicted; the rebase has already been aborted and the
// repository is back in its pre-fetch state.
type RebaseConflictError struct {
	Files []string // conflicting vault-relative paths (never empty)
	Cause error    // the underlying git error
}

func (e *RebaseConflictError) Error() string {
	return "git rebase conflict in " + strings.Join(e.Files, ", ") + " (rebase aborted, local history kept)"
}

// Unwrap exposes the underlying git error.
func (e *RebaseConflictError) Unwrap() error { return e.Cause }

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

// OwnFiles returns the vault-relative glob/paths that belong to a machine.
func OwnFiles(machineID string) []string {
	return []string{
		"machines/" + machineID + ".json.enc",
		"projects/*/meta/" + machineID + ".json.enc",
		"projects/*/state/" + machineID + ".json.enc",
	}
}
