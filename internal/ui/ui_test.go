package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Regression: a missing yes/no confirmation used to be reported with the
// passphrase sentinel, so `private-sync restore myapp </dev/null` in a session
// without a controlling terminal told the user to set PRIVATE_SYNC_PASSPHRASE
// or switch to key.source: file — neither of which supplies the confirmation.
// The remedy is --yes, and the confirmation sentinel must be the one that says
// so while still matching the general ErrNonInteractive check.
func TestErrNonInteractiveConfirmNamesTheRightRemedy(t *testing.T) {
	msg := ErrNonInteractiveConfirm.Error()
	if !strings.Contains(msg, "--yes") {
		t.Errorf("confirm error = %q, want it to mention --yes", msg)
	}
	for _, wrong := range []string{"PRIVATE_SYNC_PASSPHRASE", "key.source", "passphrase"} {
		if strings.Contains(msg, wrong) {
			t.Errorf("confirm error = %q, must not mention %q: the passphrase is not the problem", msg, wrong)
		}
	}
	if !strings.Contains(msg, "non-interactive") {
		t.Errorf("confirm error = %q, want it to say the session is non-interactive", msg)
	}
}

// The key-acquisition sentinel keeps its own text: there --yes is useless and
// the passphrase remedies are the real ones.
func TestErrNonInteractiveKeepsPassphraseRemedies(t *testing.T) {
	msg := ErrNonInteractive.Error()
	if !strings.Contains(msg, "PRIVATE_SYNC_PASSPHRASE") || !strings.Contains(msg, "key.source: file") {
		t.Errorf("password error = %q, want the passphrase remedies", msg)
	}
	if strings.Contains(msg, "--yes") {
		t.Errorf("password error = %q, must not suggest --yes", msg)
	}
	if _, err := (Silent{}).Password(context.Background(), "Vault passphrase"); !errors.Is(err, ErrNonInteractive) {
		t.Errorf("Silent.Password err = %v, want ErrNonInteractive", err)
	}
}

// The two sentinels must stay distinguishable in one direction and compatible
// in the other, including through wrapping, so front ends can special-case the
// confirmation while existing errors.Is(err, ErrNonInteractive) checks (exit
// codes, interactivity fallbacks) keep matching.
func TestNonInteractiveSentinelsIs(t *testing.T) {
	wrapped := fmt.Errorf("%w (open /dev/tty: device not configured)", ErrNonInteractiveConfirm)
	tests := []struct {
		name   string
		err    error
		target error
		want   bool
	}{
		{"confirm is itself", ErrNonInteractiveConfirm, ErrNonInteractiveConfirm, true},
		{"confirm is non-interactive", ErrNonInteractiveConfirm, ErrNonInteractive, true},
		{"password is itself", ErrNonInteractive, ErrNonInteractive, true},
		{"password is not confirm", ErrNonInteractive, ErrNonInteractiveConfirm, false},
		{"wrapped confirm is confirm", wrapped, ErrNonInteractiveConfirm, true},
		{"wrapped confirm is non-interactive", wrapped, ErrNonInteractive, true},
		{"unrelated", errors.New("boom"), ErrNonInteractiveConfirm, false},
		{"confirm is not unrelated", ErrNonInteractiveConfirm, errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errors.Is(tt.err, tt.target); got != tt.want {
				t.Fatalf("errors.Is(%v, %v) = %v, want %v", tt.err, tt.target, got, tt.want)
			}
		})
	}
	if !strings.Contains(wrapped.Error(), "--yes") {
		t.Errorf("wrapped confirm error = %q, want it to still mention --yes", wrapped.Error())
	}
}
