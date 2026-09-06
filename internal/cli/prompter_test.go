package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sanbiv/private-sync/internal/ui"
)

func TestTerminalPrompterNonInteractive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows prompter uses os.Stdin")
	}
	dir := t.TempDir()
	regular := filepath.Join(dir, "not-a-tty")
	if err := os.WriteFile(regular, []byte("y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		p    *TerminalPrompter
	}{
		{"missing device", &TerminalPrompter{TTY: filepath.Join(dir, "missing")}},
		{"regular file is not a terminal", &TerminalPrompter{TTY: regular}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if _, err := tt.p.Password(ctx, "Vault passphrase"); !errors.Is(err, ui.ErrNonInteractive) {
				t.Fatalf("Password err = %v, want ErrNonInteractive", err)
			}
			if _, err := tt.p.Confirm(ctx, "continue?", true); !errors.Is(err, ui.ErrNonInteractive) {
				t.Fatalf("Confirm err = %v, want ErrNonInteractive", err)
			}
			var pi ui.Prompter = tt.p // satisfies the interface
			if pi == nil {
				t.Fatal("nil prompter")
			}
		})
	}
}

func TestTerminalPrompterAssumeYes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows prompter uses os.Stdin")
	}
	missing := filepath.Join(t.TempDir(), "missing")
	p := &TerminalPrompter{AssumeYes: true, TTY: missing}
	ctx := context.Background()
	for _, def := range []bool{true, false} {
		ok, err := p.Confirm(ctx, "continue?", def)
		if err != nil || !ok {
			t.Fatalf("Confirm(def=%v) = %v, %v; want true without a terminal", def, ok, err)
		}
	}
	// Password still needs the terminal: --yes never invents a passphrase.
	if _, err := p.Password(ctx, "Vault passphrase"); !errors.Is(err, ui.ErrNonInteractive) {
		t.Fatalf("Password err = %v, want ErrNonInteractive", err)
	}
}

func TestTerminalPrompterNilAndCancelled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows prompter uses os.Stdin")
	}
	var p *TerminalPrompter
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Password(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Password err = %v", err)
	}
	if _, err := p.Confirm(ctx, "x", false); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Confirm err = %v", err)
	}
	// A nil prompter with a live context falls back to /dev/tty; when the test
	// runs without a controlling terminal that is ErrNonInteractive, and with
	// one we must not block, so only the cancelled paths are exercised here.
	q := &TerminalPrompter{TTY: filepath.Join(t.TempDir(), "missing")}
	if _, err := q.Confirm(context.Background(), "x", false); !errors.Is(err, ui.ErrNonInteractive) {
		t.Fatalf("missing tty err = %v", err)
	}
}

func TestCtxErr(t *testing.T) {
	if err := ctxErr(nil); err != nil { //nolint:staticcheck // nil context is tolerated on purpose
		t.Fatalf("nil ctx err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ctxErr(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestConfirmErrUsesConfirmationRemedy(t *testing.T) {
	t.Run("plain sentinel", func(t *testing.T) {
		got := confirmErr(ui.ErrNonInteractive)
		if !errors.Is(got, ui.ErrNonInteractiveConfirm) {
			t.Fatalf("confirmErr(%v) = %v, want the confirmation sentinel", ui.ErrNonInteractive, got)
		}
		if strings.Contains(got.Error(), "PRIVATE_SYNC_PASSPHRASE") {
			t.Errorf("confirmation error mentions the passphrase: %q", got)
		}
		if !strings.Contains(got.Error(), "--yes") {
			t.Errorf("confirmation error does not name --yes: %q", got)
		}
	})
	t.Run("keeps the detail", func(t *testing.T) {
		wrapped := fmt.Errorf("%w (/dev/tty is not a terminal)", ui.ErrNonInteractive)
		got := confirmErr(wrapped)
		if !errors.Is(got, ui.ErrNonInteractiveConfirm) {
			t.Fatalf("confirmErr(%v) = %v, want the confirmation sentinel", wrapped, got)
		}
		if !strings.Contains(got.Error(), "/dev/tty is not a terminal") {
			t.Errorf("detail lost: %q", got)
		}
	})
	t.Run("other errors pass through", func(t *testing.T) {
		other := errors.New("boom")
		if got := confirmErr(other); got != other {
			t.Errorf("confirmErr(%v) = %v, want it unchanged", other, got)
		}
		if confirmErr(nil) != nil {
			t.Error("confirmErr(nil) != nil")
		}
	})
}

func TestTerminalPrompterConfirmNonInteractive(t *testing.T) {
	p := &TerminalPrompter{openTTY: func() (*ttyHandle, error) { return nil, ui.ErrNonInteractive }}
	_, err := p.Confirm(context.Background(), "delete everything?", false)
	if !errors.Is(err, ui.ErrNonInteractiveConfirm) {
		t.Fatalf("Confirm error = %v, want the confirmation sentinel", err)
	}
	if !errors.Is(err, ui.ErrNonInteractive) {
		t.Errorf("Confirm error no longer matches the general sentinel: %v", err)
	}
}
