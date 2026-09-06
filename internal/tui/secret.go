package tui

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sanbiv/private-sync/internal/crypto"
)

// secretInput is a minimal masked single-line field that keeps what the user
// types in a []byte it can overwrite.
//
// It exists because huh's inputs (like bubbles' textinput underneath them)
// bind to a Go string, and a string body cannot be zeroed: a passphrase typed
// into one stays readable in the heap — and so in a core dump, a swap file or
// a hibernation image — until the garbage collector happens to reuse that
// memory. Every other passphrase path in this codebase is rigorous about
// this (ui.Prompter.Password returns []byte, crypto.DeriveKEK zeroes its
// input, vault.Open/Create/Rekey all defer crypto.Zero), so the TUI's rekey
// form is held to the same rule.
type secretInput struct {
	buf []byte
}

// grow makes room for n more bytes. It never uses append: append's own
// reallocation would leave the previous array — with the passphrase bytes
// still in it — behind for the GC, unreachable and therefore unzeroable.
func (s *secretInput) grow(n int) {
	if cap(s.buf)-len(s.buf) >= n {
		return
	}
	size := cap(s.buf)*2 + n
	if size < 64 {
		size = 64
	}
	next := make([]byte, len(s.buf), size)
	copy(next, s.buf)
	crypto.Zero(s.buf[:cap(s.buf)])
	s.buf = next
}

// insertRunes appends runes to the buffer without building a Go string.
func (s *secretInput) insertRunes(rs []rune) {
	for _, r := range rs {
		s.grow(utf8.UTFMax)
		s.buf = utf8.AppendRune(s.buf, r)
	}
}

// backspace drops the last rune, wiping the bytes it occupied.
func (s *secretInput) backspace() {
	if len(s.buf) == 0 {
		return
	}
	_, size := utf8.DecodeLastRune(s.buf)
	if size == 0 {
		size = 1
	}
	crypto.Zero(s.buf[len(s.buf)-size:])
	s.buf = s.buf[:len(s.buf)-size]
}

// Bytes returns the live buffer. The caller must not retain it past Zero.
func (s *secretInput) Bytes() []byte { return s.buf }

// Zero overwrites everything typed so far, including the unused capacity a
// grow may have already copied into, and releases the buffer.
func (s *secretInput) Zero() {
	if s.buf != nil {
		crypto.Zero(s.buf[:cap(s.buf)])
	}
	s.buf = nil
}

// View renders one bullet per rune typed, never the bytes themselves.
func (s *secretInput) View() string {
	return strings.Repeat("•", utf8.RuneCount(s.buf))
}

// rekeyPromptResult is what one key press did to a rekeyPrompt.
type rekeyPromptResult int

const (
	rekeyPromptContinue rekeyPromptResult = iota
	rekeyPromptSubmit
	rekeyPromptCancel
)

// rekeyPromptLabels names the two fields, in order.
var rekeyPromptLabels = [2]string{"New passphrase", "Confirm passphrase"}

// rekeyPrompt collects the new vault passphrase twice, in secretInputs, as
// the replacement for the huh form that used to bind it to two strings.
type rekeyPrompt struct {
	fields [2]secretInput
	idx    int
}

func newRekeyPrompt() *rekeyPrompt { return &rekeyPrompt{} }

// first and second are the two entries, in field order.
func (p *rekeyPrompt) first() []byte  { return p.fields[0].Bytes() }
func (p *rekeyPrompt) second() []byte { return p.fields[1].Bytes() }

// match reports whether the two entries are identical.
func (p *rekeyPrompt) match() bool { return bytes.Equal(p.first(), p.second()) }

// blank reports whether the first entry is empty or only whitespace.
func (p *rekeyPrompt) blank() bool { return len(bytes.TrimSpace(p.first())) == 0 }

// reset wipes both entries and returns to the first field, for a retry after
// a mismatch.
func (p *rekeyPrompt) reset() {
	p.Zero()
	p.idx = 0
}

// Zero wipes both entries.
func (p *rekeyPrompt) Zero() {
	for i := range p.fields {
		p.fields[i].Zero()
	}
}

// update applies one message. Only key presses matter.
func (p *rekeyPrompt) update(msg tea.Msg) rekeyPromptResult {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return rekeyPromptContinue
	}
	switch key.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		return rekeyPromptCancel
	case tea.KeyEnter:
		if p.idx < len(p.fields)-1 {
			p.idx++
			return rekeyPromptContinue
		}
		return rekeyPromptSubmit
	case tea.KeyTab, tea.KeyDown:
		if p.idx < len(p.fields)-1 {
			p.idx++
		}
	case tea.KeyShiftTab, tea.KeyUp:
		if p.idx > 0 {
			p.idx--
		}
	case tea.KeyBackspace, tea.KeyDelete:
		p.fields[p.idx].backspace()
	case tea.KeyCtrlU:
		p.fields[p.idx].Zero()
	case tea.KeyRunes:
		p.fields[p.idx].insertRunes(key.Runes)
	case tea.KeySpace:
		if len(key.Runes) > 0 {
			p.fields[p.idx].insertRunes(key.Runes)
		} else {
			p.fields[p.idx].insertRunes([]rune{' '})
		}
	}
	return rekeyPromptContinue
}

// View renders both masked fields plus the key help.
func (p *rekeyPrompt) View() string {
	var b strings.Builder
	b.WriteString(styles.Subtitle.Render("Change passphrase") + "\n\n")
	for i := range p.fields {
		cursor := "  "
		if i == p.idx {
			cursor = "> "
		}
		fmt.Fprintf(&b, "%s%-21s %s\n", cursor, rekeyPromptLabels[i]+":", p.fields[i].View())
	}
	b.WriteString("\n")
	b.WriteString(styles.Help.Render("enter: next/confirm  tab/shift+tab: move  ctrl+u: clear field  esc: cancel"))
	return b.String()
}
