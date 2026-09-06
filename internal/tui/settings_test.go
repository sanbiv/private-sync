package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/sanbiv/private-sync/internal/config"
)

func TestSplitCSV(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"a", []string{"a"}},
		{"a, b,c ,, d", []string{"a", "b", "c", "d"}},
	}
	for _, tt := range tests {
		got := splitCSV(tt.in)
		if len(got) == 0 && len(tt.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("splitCSV(%q) = %#v, want %#v", tt.in, got, tt.want)
		}
	}
}

func TestJoinCSVRoundTrip(t *testing.T) {
	in := []string{"*.env", "*.yaml", "*.pem"}
	joined := joinCSV(in)
	if joined != "*.env, *.yaml, *.pem" {
		t.Errorf("joinCSV(%v) = %q", in, joined)
	}
	back := splitCSV(joined)
	if !reflect.DeepEqual(back, in) {
		t.Errorf("splitCSV(joinCSV(%v)) = %v, want it back unchanged", in, back)
	}
}

// --- save() builds on a copy and validates before installing it ------------

func TestSettingsSaveAppliesValidChanges(t *testing.T) {
	s := newTestSession(t)
	m := newSettingsModel(context.Background(), s)
	m.machineName = "new-machine"
	m.includeCSV = "*.secret"

	if err := m.save(); err != nil {
		t.Fatalf("save(): %v", err)
	}
	if s.Config.Machine.Name != "new-machine" {
		t.Fatalf("Machine.Name = %q, want %q", s.Config.Machine.Name, "new-machine")
	}
	if len(s.Config.Scan.Include) != 1 || s.Config.Scan.Include[0] != "*.secret" {
		t.Fatalf("Scan.Include = %v, want [*.secret]", s.Config.Scan.Include)
	}
}

// TestSettingsSaveRejectsInvalidConfigWithoutMutatingSession is the
// regression for save() mutating the live *config.Config field by field: a
// validation failure (git remote with no URL) must leave s.Config exactly as
// it was, not half-updated with the rejected values.
func TestSettingsSaveRejectsInvalidConfigWithoutMutatingSession(t *testing.T) {
	s := newTestSession(t)
	before := *s.Config

	m := newSettingsModel(context.Background(), s)
	m.machineName = "should-not-stick"
	m.remoteType = string(config.RemoteGit)
	m.gitURL = "" // invalid: git requires a URL

	if err := m.save(); err == nil {
		t.Fatalf("save() with an empty git URL should fail validation")
	}
	if !reflect.DeepEqual(before, *s.Config) {
		t.Fatalf("save() failure must leave s.Config unchanged:\nbefore=%+v\nafter=%+v", before, *s.Config)
	}
}

// TestSettingsUpdateFormRebuildsOnSaveError is the regression for huh v1's
// Form.Update becoming a permanent no-op once State != StateNormal: leaving
// m.form as the just-completed (dead) form after a failed save() left the
// settings screen stuck showing the error with no field the user could fix,
// and only esc (discarding every typed change) to get out.
func TestSettingsUpdateFormRebuildsOnSaveError(t *testing.T) {
	s := newTestSession(t)
	m := newSettingsModel(context.Background(), s)
	m.remoteType = string(config.RemoteGit)
	m.gitURL = "" // invalid: git requires a URL, so save() will fail
	m.machineName = "still-typed"

	oldForm := m.form
	m.form.State = huh.StateCompleted // simulate the form having just finished

	back, _ := m.updateForm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")})
	if back {
		t.Fatalf("a failed save should not leave the settings screen")
	}
	if m.err == "" {
		t.Fatalf("a failed save should set m.err")
	}
	if m.form == oldForm {
		t.Fatalf("updateForm must rebuild m.form after a failed save, or huh v1's Form.Update becomes a permanent no-op from here on")
	}
	if m.form.State != huh.StateNormal {
		t.Fatalf("the rebuilt form should start at StateNormal (editable), got %v", m.form.State)
	}
	// buildForm binds its fields to the model's own m.* pointers, so the
	// typed value must survive the rebuild.
	if m.machineName != "still-typed" {
		t.Fatalf("machineName = %q, want the typed value preserved across the rebuild", m.machineName)
	}
}

// --- startRekey also updates the configured key source ----------------------

func TestStartRekeyUpdatesKeyFileOnFileSource(t *testing.T) {
	s := newTestSession(t)
	keyPath := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyPath, []byte("old-passphrase\n"), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	s.Config.Key.Source = config.KeyFile
	s.Config.Key.File.Path = keyPath

	m := &settingsModel{ctx: context.Background(), s: s}
	msg := m.startRekey([]byte("new-passphrase-xyz"))()
	done, ok := msg.(rekeyDoneMsg)
	if !ok {
		t.Fatalf("startRekey() = %#v, want a rekeyDoneMsg", msg)
	}
	if done.err != nil {
		t.Fatalf("startRekey: %v", done.err)
	}
	data, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key file: %v", err)
	}
	if strings.TrimSpace(string(data)) != "new-passphrase-xyz" {
		t.Fatalf("key file content = %q, want the new passphrase", data)
	}
}

func TestStartRekeyWarnsForBitwardenSource(t *testing.T) {
	s := newTestSession(t)
	s.Config.Key.Source = config.KeyBitwarden
	s.Config.Key.Bitwarden.Item = "my-vault-item"

	m := &settingsModel{ctx: context.Background(), s: s}
	msg := m.startRekey([]byte("new-passphrase-xyz"))()
	done, ok := msg.(rekeyDoneMsg)
	if !ok {
		t.Fatalf("startRekey() = %#v, want a rekeyDoneMsg", msg)
	}
	if done.err != nil {
		t.Fatalf("startRekey: %v", done.err)
	}
	if !strings.Contains(done.warning, "my-vault-item") {
		t.Fatalf("warning = %q, want it to name the Bitwarden item", done.warning)
	}
}

// TestStartRekeyFailsCleanlyWhenKeyFileMissing is the regression for the
// settings screen silently rewrapping the vault key without touching
// key.source=file's key file: it must at least report the failure instead of
// leaving the user locked out with no explanation.
func TestStartRekeyFailsCleanlyWhenKeyFileMissing(t *testing.T) {
	s := newTestSession(t)
	s.Config.Key.Source = config.KeyFile
	s.Config.Key.File.Path = filepath.Join(t.TempDir(), "does-not-exist")

	m := &settingsModel{ctx: context.Background(), s: s}
	msg := m.startRekey([]byte("new-passphrase-xyz"))()
	done, ok := msg.(rekeyDoneMsg)
	if !ok {
		t.Fatalf("startRekey() = %#v, want a rekeyDoneMsg", msg)
	}
	if done.err == nil {
		t.Fatalf("startRekey should fail when the key file cannot be found/written")
	}
	if !strings.Contains(done.err.Error(), "key file") {
		t.Fatalf("error = %v, want it to mention the key file", done.err)
	}
}

func TestStartRekeyPromptSourceNeedsNoFileUpdate(t *testing.T) {
	s := newTestSession(t) // config.Default leaves Key.Source at KeyPrompt
	m := &settingsModel{ctx: context.Background(), s: s}
	msg := m.startRekey([]byte("new-passphrase-xyz"))()
	done, ok := msg.(rekeyDoneMsg)
	if !ok {
		t.Fatalf("startRekey() = %#v, want a rekeyDoneMsg", msg)
	}
	if done.err != nil {
		t.Fatalf("startRekey: %v", done.err)
	}
	if done.warning != "" {
		t.Fatalf("warning = %q, want none for key.source=prompt", done.warning)
	}
}

// --- Update-driven stage transitions ----------------------------------------

func TestUpdateRekeyingSuccessStartsPush(t *testing.T) {
	s := newTestSession(t)
	m := newSettingsModel(context.Background(), s)
	m.step = settingsStepRekeying
	_, cmd := m.updateRekeying(rekeyDoneMsg{})
	if m.sv == nil {
		t.Fatalf("a successful rekey should start a push syncView")
	}
	if cmd == nil {
		t.Fatalf("expected the syncView's Init() command")
	}
}

func TestUpdateRekeyingErrorGoesToDone(t *testing.T) {
	m := &settingsModel{step: settingsStepRekeying}
	if back, _ := m.updateRekeying(rekeyDoneMsg{err: errors.New("boom")}); back {
		t.Fatalf("an error should not leave the screen")
	}
	if m.step != settingsStepDone {
		t.Fatalf("step = %v, want settingsStepDone", m.step)
	}
	if m.err == "" {
		t.Fatalf("expected err to be set")
	}
}

func TestUpdateRekeyingWarningCarriesThrough(t *testing.T) {
	s := newTestSession(t)
	m := newSettingsModel(context.Background(), s)
	m.step = settingsStepRekeying
	m.updateRekeying(rekeyDoneMsg{warning: "update bitwarden"})
	if m.rekeyWarning != "update bitwarden" {
		t.Fatalf("rekeyWarning = %q, want %q", m.rekeyWarning, "update bitwarden")
	}
}

func TestSettingsUpdateFormEscLeaves(t *testing.T) {
	s := newTestSession(t)
	m := newSettingsModel(context.Background(), s)
	if back, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc}); !back {
		t.Fatalf("esc on the settings form should leave the screen")
	}
}

func TestSettingsUpdateDoneEnterLeaves(t *testing.T) {
	m := &settingsModel{step: settingsStepDone}
	if back, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter}); !back {
		t.Fatalf("enter on settingsStepDone should leave the screen")
	}
}

func TestSettingsUpdateRekeyFormEscGoesToDoneWithoutLeaving(t *testing.T) {
	m := &settingsModel{step: settingsStepRekeyForm}
	if back, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc}); back {
		t.Fatalf("esc on the rekey form should not leave the whole settings screen")
	}
	if m.step != settingsStepDone {
		t.Fatalf("step = %v, want settingsStepDone", m.step)
	}
}
