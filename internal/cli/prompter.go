package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"golang.org/x/term"

	"github.com/sanbiv/private-sync/internal/keysource"
	"github.com/sanbiv/private-sync/internal/ui"
)

// DefaultTTY is the terminal device TerminalPrompter opens on Unix.
const DefaultTTY = "/dev/tty"

// TerminalPrompter asks for passphrases and confirmations on the controlling
// terminal (spec §11). The device is opened directly so that prompts work
// while stdin is piped; on Windows os.Stdin is used instead. Without a
// terminal every prompt fails with ui.ErrNonInteractive.
//
// A terminal read only returns when the user presses Enter (with ISIG on,
// ctrl+c raises SIGINT and does not interrupt the read), so both prompts
// watch ctx while the read runs: once ctx is cancelled they return ctx.Err()
// at once, restore the terminal state and leave the read behind.
//
// A nil *TerminalPrompter behaves like the zero value.
type TerminalPrompter struct {
	// AssumeYes makes Confirm answer true without prompting (--yes).
	// Password still prompts when a terminal exists.
	AssumeYes bool
	// TTY is the terminal device to open; empty means DefaultTTY (os.Stdin on Windows).
	TTY string

	// openTTY replaces the terminal (tests: a pipe-backed stub).
	openTTY func() (*ttyHandle, error)
}

// Password prints the title to the terminal and reads a line without echo.
func (p *TerminalPrompter) Password(ctx context.Context, title string) ([]byte, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	t, err := p.open()
	if err != nil {
		return nil, err
	}
	defer t.release()
	fmt.Fprintf(t.w, "%s: ", title)
	pass, err := t.await(ctx, t.readPassword)
	if t.abandoned {
		return nil, err
	}
	fmt.Fprintln(t.w)
	if err != nil {
		keysource.Zero(pass)
		return nil, fmt.Errorf("read passphrase: %w", err)
	}
	if err := ctxErr(ctx); err != nil {
		keysource.Zero(pass)
		return nil, err
	}
	return pass, nil
}

// Confirm prints "title [y/N]" (or "[Y/n]" when def is true) and reads a
// line; an empty answer selects the default. With AssumeYes it returns true
// without prompting.
func (p *TerminalPrompter) Confirm(ctx context.Context, title string, def bool) (bool, error) {
	if p != nil && p.AssumeYes {
		return true, nil
	}
	if err := ctxErr(ctx); err != nil {
		return false, err
	}
	t, err := p.open()
	if err != nil {
		return false, err
	}
	defer t.release()
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	for attempt := 0; ; attempt++ {
		fmt.Fprintf(t.w, "%s %s ", title, hint)
		raw, err := t.await(ctx, t.readLine)
		if t.abandoned {
			return false, err
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return false, fmt.Errorf("read answer: %w", err)
		}
		answer := strings.ToLower(strings.TrimSpace(string(raw)))
		switch answer {
		case "":
			if err != nil { // EOF without a newline: keep the terminal tidy
				fmt.Fprintln(t.w)
			}
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		if err != nil || attempt >= 2 {
			return false, fmt.Errorf("unrecognised answer %q", answer)
		}
		fmt.Fprintln(t.w, "please answer y or n")
	}
}

// ttyHandle is an open terminal: a file descriptor for ReadPassword, a
// writer for prompts and a reader for answers.
type ttyHandle struct {
	fd    int // -1 without a descriptor (test stubs)
	w     io.Writer
	r     *bufio.Reader
	close func()
	// readPassword reads one line without echo (term.ReadPassword on fd).
	readPassword func() ([]byte, error)
	// abandoned is set once a read was left behind after a cancellation.
	// The descriptor is then never closed: the read goroutine may still use
	// it and a reused descriptor number would hand it another file's input.
	abandoned bool
}

// newTTYHandle wraps an open terminal.
func newTTYHandle(fd int, w io.Writer, r io.Reader, close func()) *ttyHandle {
	return &ttyHandle{
		fd:           fd,
		w:            w,
		r:            bufio.NewReader(r),
		close:        close,
		readPassword: func() ([]byte, error) { return term.ReadPassword(fd) },
	}
}

// readLine reads one line, newline included.
func (t *ttyHandle) readLine() ([]byte, error) {
	s, err := t.r.ReadString('\n')
	return []byte(s), err
}

// await runs read in a goroutine and returns its result, or ctx.Err() as
// soon as ctx is cancelled. On cancellation the terminal state saved before
// the read is restored (ReadPassword turns echo off and only restores it
// when the read returns), a newline ends the prompt line and the read is
// abandoned: the handle must not be closed afterwards (see abandoned).
func (t *ttyHandle) await(ctx context.Context, read func() ([]byte, error)) ([]byte, error) {
	if ctx == nil || ctx.Done() == nil {
		return read()
	}
	var saved *term.State
	if t.fd >= 0 {
		saved, _ = term.GetState(t.fd)
	}
	type outcome struct {
		b   []byte
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		b, err := read()
		ch <- outcome{b, err}
	}()
	select {
	case o := <-ch:
		return o.b, o.err
	case <-ctx.Done():
	}
	select { // a result that raced with the cancellation is still an answer
	case o := <-ch:
		return o.b, o.err
	default:
	}
	t.abandoned = true
	if saved != nil {
		_ = term.Restore(t.fd, saved)
	}
	fmt.Fprintln(t.w)
	go func() { // whatever the abandoned read returns later is secret: wipe it
		keysource.Zero((<-ch).b)
	}()
	return nil, ctx.Err()
}

// release closes the terminal unless a read was abandoned on it.
func (t *ttyHandle) release() {
	if t.abandoned || t.close == nil {
		return
	}
	t.close()
}

// open returns the terminal to prompt on, or ui.ErrNonInteractive.
func (p *TerminalPrompter) open() (*ttyHandle, error) {
	path := ""
	if p != nil {
		if p.openTTY != nil {
			return p.openTTY()
		}
		path = strings.TrimSpace(p.TTY)
	}
	if path == "" && runtime.GOOS == "windows" {
		fd := int(os.Stdin.Fd())
		if !term.IsTerminal(fd) {
			return nil, ui.ErrNonInteractive
		}
		return newTTYHandle(fd, os.Stderr, os.Stdin, func() {}), nil
	}
	if path == "" {
		path = DefaultTTY
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", ui.ErrNonInteractive, err)
	}
	fd := int(f.Fd())
	if !term.IsTerminal(fd) {
		_ = f.Close()
		return nil, fmt.Errorf("%w (%s is not a terminal)", ui.ErrNonInteractive, path)
	}
	return newTTYHandle(fd, f, f, func() { _ = f.Close() }), nil
}

// ctxErr returns the context error, tolerating a nil context.
func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
