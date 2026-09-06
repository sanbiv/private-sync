package tui

import (
	"context"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
)

// typeInto feeds a literal into a rekeyPrompt one rune at a time.
func typeInto(p *rekeyPrompt, s string) {
	for _, r := range s {
		p.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func TestSecretInputTypeBackspaceAndMask(t *testing.T) {
	var s secretInput
	s.insertRunes([]rune("pä§s"))
	if string(s.Bytes()) != "pä§s" {
		t.Fatalf("Bytes() = %q, want the typed text", s.Bytes())
	}
	if got, want := s.View(), strings.Repeat("•", 4); got != want {
		t.Fatalf("View() = %q, want %q (one bullet per rune, never the text)", got, want)
	}
	s.backspace()
	if string(s.Bytes()) != "pä§" {
		t.Fatalf("after backspace Bytes() = %q, want a whole rune removed", s.Bytes())
	}
	s.backspace()
	if string(s.Bytes()) != "pä" {
		t.Fatalf("after backspace Bytes() = %q", s.Bytes())
	}
	// Backspacing an empty field is a no-op, not a panic.
	var empty secretInput
	empty.backspace()
	if len(empty.Bytes()) != 0 {
		t.Fatalf("backspace on an empty field changed it")
	}
}

// TestSecretInputZeroWipesTheBuffer is the core of the regression: the TUI
// rekey used to bind the new passphrase to huh string inputs, whose bodies
// cannot be overwritten, leaving the plaintext recoverable from a core dump
// or swap. A secretInput must actually wipe what it held, including the
// buffer it grew out of.
func TestSecretInputZeroWipesTheBuffer(t *testing.T) {
	var s secretInput
	s.insertRunes([]rune("correct horse battery staple"))
	held := s.Bytes()[:cap(s.Bytes())]

	s.Zero()

	for i, b := range held {
		if b != 0 {
			t.Fatalf("byte %d of the passphrase survived Zero(): %q", i, held)
		}
	}
	if s.Bytes() != nil {
		t.Fatalf("Zero() should release the buffer, got %v", s.Bytes())
	}

	var g secretInput
	g.insertRunes([]rune("short"))
	first := g.Bytes()[:cap(g.Bytes())]
	g.insertRunes([]rune(strings.Repeat("x", 4096)))
	if &first[0] == &g.Bytes()[0] {
		t.Skip("no reallocation happened; nothing to check")
	}
	for i, b := range first {
		if b != 0 {
			t.Fatalf("byte %d of the old buffer survived the grow: %q", i, first)
		}
	}
}

// TestSettingsRekeyModelHoldsNoPassphraseStrings pins the shape of the rekey
// flow: no field of settingsModel may be a string the passphrase is typed
// into (that was exactly the pass1/pass2 defect).
func TestSettingsRekeyModelHoldsNoPassphraseStrings(t *testing.T) {
	typ := reflect.TypeOf(settingsModel{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type.Kind() != reflect.String {
			continue
		}
		name := strings.ToLower(f.Name)
		if strings.Contains(name, "pass") && !strings.Contains(name, "err") && !strings.Contains(name, "warning") {
			t.Fatalf("settingsModel.%s is a string: a passphrase in a Go string cannot be zeroed", f.Name)
		}
	}
	if got := reflect.TypeOf(settingsModel{}.rekey).String(); got != "*tui.rekeyPrompt" {
		t.Fatalf("settingsModel.rekey = %s, want the []byte-backed prompt", got)
	}
}

func TestRekeyPromptCollectsAndZeroesBothFields(t *testing.T) {
	s := newTestSession(t)
	m := newSettingsModel(context.Background(), s)
	m.step = settingsStepRekeyForm
	m.rekey = newRekeyPrompt()

	typeInto(m.rekey, "new-passphrase-xyz")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // move to the confirm field
	typeInto(m.rekey, "new-passphrase-xyz")

	// The masked view must never leak the text itself.
	if view := m.View(); strings.Contains(view, "new-passphrase-xyz") {
		t.Fatalf("the rekey prompt rendered the passphrase in clear:\n%s", view)
	}

	buf := m.rekey.first()[:cap(m.rekey.first())]
	back, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if back {
		t.Fatalf("submitting the rekey prompt should not leave the settings screen")
	}
	if cmd == nil {
		t.Fatalf("a matching pair should start the rekey")
	}
	if m.step != settingsStepRekeying {
		t.Fatalf("step = %v, want settingsStepRekeying", m.step)
	}
	if m.rekey != nil {
		t.Fatalf("the prompt should be released once the passphrase is handed over")
	}
	for i, b := range buf {
		if b != 0 {
			t.Fatalf("byte %d of the typed passphrase survived submission: %q", i, buf)
		}
	}

	// The command still performs a real rekey with what was typed.
	msg, ok := cmd().(rekeyDoneMsg)
	if !ok {
		t.Fatalf("cmd() = %#v, want a rekeyDoneMsg", msg)
	}
	if msg.err != nil {
		t.Fatalf("rekey: %v", msg.err)
	}
}

func TestRekeyPromptRejectsMismatchAndBlank(t *testing.T) {
	m := &settingsModel{step: settingsStepRekeyForm, rekey: newRekeyPrompt()}

	typeInto(m.rekey, "one")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	typeInto(m.rekey, "two")
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Fatalf("a mismatch must not start the rekey")
	}
	if !strings.Contains(m.rekeyErr, "do not match") {
		t.Fatalf("rekeyErr = %q, want a mismatch message", m.rekeyErr)
	}
	if len(m.rekey.first()) != 0 || len(m.rekey.second()) != 0 {
		t.Fatalf("a mismatch should clear both fields for the retry")
	}
	if !strings.Contains(m.View(), "do not match") {
		t.Fatalf("the mismatch should be visible on screen")
	}

	// Blank (whitespace only) is refused too.
	typeInto(m.rekey, "  ")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	typeInto(m.rekey, "  ")
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Fatalf("a blank passphrase must not start the rekey")
	}
	if !strings.Contains(m.rekeyErr, "empty") {
		t.Fatalf("rekeyErr = %q, want an empty-passphrase message", m.rekeyErr)
	}
}

func TestRekeyPromptEscCancelsAndWipes(t *testing.T) {
	m := &settingsModel{step: settingsStepRekeyForm, rekey: newRekeyPrompt()}
	typeInto(m.rekey, "typed-then-abandoned")
	buf := m.rekey.first()[:cap(m.rekey.first())]

	back, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if back {
		t.Fatalf("esc on the rekey prompt should not leave the whole settings screen")
	}
	if m.step != settingsStepDone {
		t.Fatalf("step = %v, want settingsStepDone", m.step)
	}
	if m.rekey != nil {
		t.Fatalf("the prompt should be released on cancel")
	}
	for i, b := range buf {
		if b != 0 {
			t.Fatalf("byte %d of the abandoned passphrase survived esc: %q", i, buf)
		}
	}
}

// TestSettingsFormWantRekeyOpensThePrompt keeps the wiring from the main form
// to the byte-backed prompt covered.
func TestSettingsFormWantRekeyOpensThePrompt(t *testing.T) {
	s := newTestSession(t)
	m := newSettingsModel(context.Background(), s)
	m.wantRekey = true
	m.form.State = huh.StateCompleted

	back, _ := m.updateForm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")})
	if back {
		t.Fatalf("finishing the form with wantRekey should stay on the settings screen")
	}
	if m.step != settingsStepRekeyForm || m.rekey == nil {
		t.Fatalf("step = %v, rekey = %v; want the rekey prompt open", m.step, m.rekey)
	}
}
