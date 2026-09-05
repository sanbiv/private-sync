package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/paths"
)

func TestSetupDefaultsUsesExistingConfig(t *testing.T) {
	existing := &config.Config{
		Version: config.CurrentVersion,
		Machine: config.MachineConfig{Name: "laptop"},
		Vault:   config.VaultConfig{Path: "~/vault"},
		Projects: []config.ProjectConfig{
			{ID: "abc", Name: "demo", Path: "~/code/demo"},
		},
	}
	got := setupDefaults(paths.Dirs{}, existing)
	if got.Machine.Name != "laptop" {
		t.Errorf("Machine.Name = %q, want %q", got.Machine.Name, "laptop")
	}
	if len(got.Projects) != 1 || got.Projects[0].ID != "abc" {
		t.Errorf("Projects = %v, want the existing project preserved", got.Projects)
	}
	// Mutating the copy must not affect the caller's config.
	got.Machine.Name = "changed"
	if existing.Machine.Name != "laptop" {
		t.Errorf("setupDefaults should return a copy, not alias the existing config")
	}
}

func TestSetupDefaultsFirstRun(t *testing.T) {
	dirs := paths.Dirs{Config: "/tmp/psv-config", State: "/tmp/psv-state"}
	got := setupDefaults(dirs, nil)
	if got.Vault.Path == "" {
		t.Errorf("first-run defaults should set a vault path")
	}
	if got.Key.Source != config.KeyPrompt {
		t.Errorf("Key.Source = %v, want KeyPrompt by default", got.Key.Source)
	}
}

func TestRequiredField(t *testing.T) {
	v := requiredField("vault path")
	if err := v(""); err == nil {
		t.Errorf("requiredField should reject an empty value")
	}
	if err := v("   "); err == nil {
		t.Errorf("requiredField should reject a whitespace-only value")
	}
	if err := v("ok"); err != nil {
		t.Errorf("requiredField should accept a non-empty value: %v", err)
	}
}

func TestOrPlaceholder(t *testing.T) {
	if got := orPlaceholder("", "default"); got != "default" {
		t.Errorf("orPlaceholder(empty) = %q, want %q", got, "default")
	}
	if got := orPlaceholder("set", "default"); got != "set" {
		t.Errorf("orPlaceholder(set) = %q, want %q", got, "set")
	}
}

func TestSetupSummaryMentionsEachRemoteAndKeyKind(t *testing.T) {
	git := setupSummary("~/vault", "m1", string(config.RemoteGit), "git@host:repo.git", "main", "", "", string(config.KeyPrompt), "", "", "")
	if !strings.Contains(git, "git@host:repo.git") {
		t.Errorf("git summary = %q, want it to mention the URL", git)
	}
	rclone := setupSummary("~/vault", "m1", string(config.RemoteRclone), "", "", "gdrive", "vault", string(config.KeyFile), "~/key", "", "")
	if !strings.Contains(rclone, "gdrive:vault") {
		t.Errorf("rclone summary = %q, want it to mention remote:path", rclone)
	}
	bw := setupSummary("~/vault", "m1", string(config.RemoteNone), "", "", "", "", string(config.KeyBitwarden), "", "vault-item", "password")
	if !strings.Contains(bw, "vault-item") {
		t.Errorf("bitwarden summary = %q, want it to mention the item", bw)
	}
}

// --- review note DescriptionFunc binding (medium finding) ------------------
//
// huh's Eval.shouldUpdate only recomputes a Note's DescriptionFunc when
// hashstructure.Hash(bindings) changes between the field receiving one of
// huh's internal "updateFieldMsg" refreshes (sent whenever the group holding
// the field processes any message at all). Binding it to a constant string,
// as RunSetup's groupConfirm used to, hashes the same forever, so the review
// freezes at whatever it first evaluated to; binding it to the live field
// values (RunSetup's `bindings` struct of pointers) makes the hash — and so
// the rendered text — track every edit. driveHuhCmd below is huh's own
// doAllUpdates test pattern (see huh_test.go), adapted to run outside a real
// tea.Program: it repeatedly executes the tea.Cmd a Form.Update returns and
// feeds the resulting message back in, unwrapping tea.Batch along the way.

func driveHuhCmd(f *huh.Form, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, sub := range msg {
			driveHuhCmd(f, sub)
		}
	default:
		_, result := f.Update(msg)
		driveHuhCmd(f, result)
	}
}

// TestReviewNoteDescriptionFuncTracksLiveBindings is the regression for the
// fix: binding a Note's DescriptionFunc to a pointer struct over the live
// setup values (mirroring RunSetup's groupConfirm) must pick up a later
// change to those values once the group re-evaluates its fields.
func TestReviewNoteDescriptionFuncTracksLiveBindings(t *testing.T) {
	vaultPath := "/initial/vault"
	bindings := &struct{ VaultPath *string }{&vaultPath}
	note := huh.NewNote().Title("Review").DescriptionFunc(func() string {
		return vaultPath
	}, bindings)
	confirmed := false
	form := huh.NewForm(huh.NewGroup(note, huh.NewConfirm().Value(&confirmed)))

	// Any message reaching the active group causes it to re-evaluate every
	// Func-bound field; a WindowSizeMsg is a convenient side-effect-free one
	// (Form.Update sizes the layout from it, then falls through to the
	// normal per-field update path).
	_, cmd := form.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	driveHuhCmd(form, cmd)
	if view := form.View(); !strings.Contains(view, "/initial/vault") {
		t.Fatalf("review note = %q, want it to mention the initial value %q", view, "/initial/vault")
	}

	vaultPath = "/changed/vault"
	_, cmd = form.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	driveHuhCmd(form, cmd)
	if view := form.View(); !strings.Contains(view, "/changed/vault") {
		t.Fatalf("review note after the bound value changed = %q, want it to mention %q (bindings must track live values)", view, "/changed/vault")
	}
}

// TestReviewNoteDescriptionFuncConstantBindingNeverUpdates documents the bug
// being fixed: a constant-string binding (what groupConfirm used to pass)
// hashes identically on every re-evaluation, so the description is computed
// once and never again, however many times the bound value changes.
func TestReviewNoteDescriptionFuncConstantBindingNeverUpdates(t *testing.T) {
	vaultPath := "/initial/vault"
	note := huh.NewNote().Title("Review").DescriptionFunc(func() string {
		return vaultPath
	}, "setup-review")
	confirmed := false
	form := huh.NewForm(huh.NewGroup(note, huh.NewConfirm().Value(&confirmed)))

	_, cmd := form.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	driveHuhCmd(form, cmd)
	if view := form.View(); !strings.Contains(view, "/initial/vault") {
		t.Fatalf("setup: review note should show the initial value once first evaluated, got %q", view)
	}

	vaultPath = "/changed/vault"
	_, cmd = form.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	driveHuhCmd(form, cmd)
	if view := form.View(); strings.Contains(view, "/changed/vault") {
		t.Fatalf("a constant DescriptionFunc binding picked up the changed value %q; this was expected to reproduce the bug the fix addresses", view)
	}
}
