package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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
