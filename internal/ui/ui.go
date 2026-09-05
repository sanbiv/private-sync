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

// ErrNonInteractive is returned when a prompt is needed but no terminal is available.
var ErrNonInteractive = &nonInteractiveError{}

type nonInteractiveError struct{}

func (*nonInteractiveError) Error() string {
	return "a prompt is required but the session is non-interactive (set PRIVATE_SYNC_PASSPHRASE or use key.source: file)"
}
