package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// stubTTY is a pipe-backed terminal: the test writes answers to in, prompts
// land in out; readPassword reads a line from the same pipe.
type stubTTY struct {
	in     *os.File // write end of the pipe
	out    bytes.Buffer
	closed bool
	h      *ttyHandle
}

func newStubTTY(t *testing.T) (*TerminalPrompter, *stubTTY) {
	t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	s := &stubTTY{in: pw}
	s.h = newTTYHandle(-1, &s.out, pr, func() { s.closed = true })
	s.h.readPassword = func() ([]byte, error) {
		b, err := s.h.readLine()
		return bytes.TrimRight(b, "\r\n"), err
	}
	t.Cleanup(func() {
		_ = pw.Close()
		_ = pr.Close() // unblocks a read that was abandoned by a cancellation
	})
	p := &TerminalPrompter{openTTY: func() (*ttyHandle, error) { return s.h, nil }}
	return p, s
}

func TestTerminalPrompterConfirmAnswers(t *testing.T) {
	tests := []struct {
		name    string
		input   string // closed without a newline at the end → EOF
		def     bool
		want    bool
		wantErr string
		wantOut string
	}{
		{"yes", "y\n", false, true, "", "continue? [y/N] "},
		{"YES", "YES\n", false, true, "", ""},
		{"no", "n\n", true, false, "", "continue? [Y/n] "},
		{"No with spaces", "  No  \n", true, false, "", ""},
		{"empty takes the default (true)", "\n", true, true, "", ""},
		{"empty takes the default (false)", "\n", false, false, "", ""},
		{"eof takes the default", "", true, true, "", ""},
		{"retry then answer", "maybe\nno\n", true, false, "", "please answer y or n"},
		{"three bad answers", "a\nb\nc\n", false, false, "unrecognised answer", ""},
		{"eof after a bad answer", "what", false, false, "unrecognised answer", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, s := newStubTTY(t)
			if _, err := io.WriteString(s.in, tt.input); err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(tt.input, "\n") {
				_ = s.in.Close()
			}
			got, err := p.Confirm(context.Background(), "continue?", tt.def)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil || got != tt.want {
				t.Fatalf("Confirm = %v, %v; want %v", got, err, tt.want)
			}
			if tt.wantOut != "" && !strings.Contains(s.out.String(), tt.wantOut) {
				t.Fatalf("prompt output %q lacks %q", s.out.String(), tt.wantOut)
			}
			if !s.closed {
				t.Fatal("terminal not closed after the answer")
			}
		})
	}
}

func TestTerminalPrompterPasswordReadsALine(t *testing.T) {
	p, s := newStubTTY(t)
	if _, err := io.WriteString(s.in, "s3cret\n"); err != nil {
		t.Fatal(err)
	}
	got, err := p.Password(context.Background(), "Vault passphrase")
	if err != nil || string(got) != "s3cret" {
		t.Fatalf("Password = %q, %v", got, err)
	}
	if !strings.HasPrefix(s.out.String(), "Vault passphrase: ") || !strings.HasSuffix(s.out.String(), "\n") {
		t.Fatalf("prompt output = %q", s.out.String())
	}
	if !s.closed {
		t.Fatal("terminal not closed after the answer")
	}

	// EOF before any input is an error, not an empty passphrase.
	p, s = newStubTTY(t)
	_ = s.in.Close()
	if _, err := p.Password(context.Background(), "Vault passphrase"); err == nil {
		t.Fatal("EOF accepted as a passphrase")
	}
}

// TestTerminalPrompterCancelledWhileBlocked: a prompt blocked in a terminal
// read returns ctx.Err() when the context is cancelled (ctrl+c) instead of
// waiting for Enter; the abandoned terminal is not closed.
func TestTerminalPrompterCancelledWhileBlocked(t *testing.T) {
	for _, name := range []string{"Confirm", "Password"} {
		t.Run(name, func(t *testing.T) {
			p, s := newStubTTY(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				if name == "Confirm" {
					_, err = p.Confirm(ctx, "continue?", false)
				} else {
					_, err = p.Password(ctx, "Vault passphrase")
				}
				done <- err
			}()
			select {
			case err := <-done:
				t.Fatalf("prompt returned before the cancellation: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("err = %v, want context.Canceled", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the prompt did not return after the cancellation")
			}
			if s.closed {
				t.Fatal("the abandoned terminal was closed")
			}
			if !strings.HasSuffix(s.out.String(), "\n") {
				t.Fatalf("no newline after the abandoned prompt: %q", s.out.String())
			}
		})
	}
}

// TestTerminalPrompterAnswerRacingCancellation: an answer that arrives
// together with the cancellation is still used by Confirm.
func TestTerminalPrompterAnswerRacingCancellation(t *testing.T) {
	p, s := newStubTTY(t)
	if _, err := io.WriteString(s.in, "y\n"); err != nil {
		t.Fatal(err)
	}
	ok, err := p.Confirm(context.Background(), "continue?", false)
	if err != nil || !ok {
		t.Fatalf("Confirm = %v, %v", ok, err)
	}
	// A cancelled context is checked before anything is read.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Confirm(ctx, "continue?", false); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}
