// Package ui holds the tiny interfaces front ends implement for the core packages.
package ui

import "context"

// Prompter asks the user for secrets and confirmations. It blocks and must only
// be used before a Bubble Tea program is running (CLI path / setup phase).
type Prompter interface {
	Password(ctx context.Context, title string) ([]byte, error)
	Confirm(ctx context.Context, title string, def bool) (bool, error)
}

// Silent is a Prompter that fails every prompt (non-interactive runs).
type Silent struct{}

func (Silent) Password(context.Context, string) ([]byte, error) {
	return nil, ErrNonInteractive
}

func (Silent) Confirm(_ context.Context, _ string, def bool) (bool, error) {
	return def, nil
}

// ErrNonInteractive is returned when a prompt is needed but no terminal is
// available. Its message points at the passphrase remedies, so it belongs to
// key acquisition (Prompter.Password); a missing yes/no answer must use
// ErrNonInteractiveConfirm instead.
var ErrNonInteractive = &nonInteractiveError{}

// ErrNonInteractiveConfirm is returned when a yes/no confirmation is needed but
// no terminal is available. Confirmations have nothing to do with the vault key,
// so it names the remedy that actually applies: --yes.
//
// It reports itself as an ErrNonInteractive to errors.Is, so callers that key
// off the general sentinel (exit-code mapping, "is this run interactive?"
// fallbacks) keep matching without change.
var ErrNonInteractiveConfirm = &nonInteractiveConfirmError{}

type nonInteractiveError struct{}

func (*nonInteractiveError) Error() string {
	return "a prompt is required but the session is non-interactive (set PRIVATE_SYNC_PASSPHRASE or use key.source: file)"
}

type nonInteractiveConfirmError struct{}

func (*nonInteractiveConfirmError) Error() string {
	return "a confirmation is required but the session is non-interactive; pass --yes to accept it"
}

// Is makes ErrNonInteractiveConfirm match ErrNonInteractive as well as itself.
func (*nonInteractiveConfirmError) Is(target error) bool {
	return target == ErrNonInteractive
}
