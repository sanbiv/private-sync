package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sanbiv/private-sync/internal/app"
	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/sync"
)

// newTestDetail builds a detailModel over a real (but otherwise empty)
// session, with a project registered in config and one plan item set
// directly (bypassing Init/loadPlanCmd's real Engine.Plan for a fast,
// hermetic model to drive with Update).
func newTestDetail(t *testing.T) (*detailModel, *app.Session) {
	t.Helper()
	s := newTestSession(t)
	dir := t.TempDir()
	s.Config.AddProject(config.ProjectConfig{ID: "proj1", Name: "Proj One", Path: dir})
	m, err := newDetailModel(context.Background(), s, "proj1", "Proj One")
	if err != nil {
		t.Fatalf("newDetailModel: %v", err)
	}
	m.setPlan(&sync.Plan{Projects: []sync.ProjectPlan{{
		ID: "proj1",
		Items: []sync.Item{
			{Key: sync.ItemKey{Project: "proj1", Path: "a.env"}, Action: sync.ActionInSync},
		},
	}}})
	return m, s
}

func TestDetailHandleKeyEscLeaves(t *testing.T) {
	m, _ := newTestDetail(t)
	if back, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc}); !back {
		t.Fatalf("esc should leave the detail screen")
	}
}

func TestDetailHandleKeyAOpensRescan(t *testing.T) {
	m, _ := newTestDetail(t)
	back, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if back {
		t.Fatalf("a should not leave the detail screen")
	}
	if !m.rescanning || m.rescan == nil {
		t.Fatalf("a should open the rescan wizard")
	}
}

func TestDetailHandleKeySStartsSync(t *testing.T) {
	m, _ := newTestDetail(t)
	back, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if back {
		t.Fatalf("s should not leave the detail screen")
	}
	if !m.svActive || m.sv == nil {
		t.Fatalf("s should start a syncView")
	}
	if cmd == nil {
		t.Fatalf("s should return the syncView's Init() command")
	}
}

func TestDetailHandleKeyXOpensConfirmThenNCancels(t *testing.T) {
	m, _ := newTestDetail(t)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m.confirm == nil {
		t.Fatalf("x should open a confirm prompt")
	}
	if !strings.Contains(m.confirm.message, "a.env") {
		t.Fatalf("confirm message = %q, want it to name the file", m.confirm.message)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if m.confirm != nil {
		t.Fatalf("n should cancel the confirm prompt")
	}
	if m.svActive {
		t.Fatalf("cancelling should not have started a syncView")
	}
}

func TestDetailHandleKeyDOpensConfirmMentioningPath(t *testing.T) {
	m, _ := newTestDetail(t)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	if m.confirm == nil {
		t.Fatalf("D should open a confirm prompt")
	}
	if !strings.Contains(m.confirm.message, "a.env") || !strings.Contains(m.confirm.message, "everywhere") {
		t.Fatalf("confirm message = %q, want it to mention the file and \"everywhere\"", m.confirm.message)
	}
}

func TestDetailHandleKeyROpensRestoreConfirm(t *testing.T) {
	m, _ := newTestDetail(t)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	if m.confirm == nil {
		t.Fatalf("R should open a confirm prompt")
	}
	if !strings.Contains(m.confirm.message, "Restore") {
		t.Fatalf("confirm message = %q, want it to mention restoring", m.confirm.message)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.confirm != nil {
		t.Fatalf("esc should cancel the confirm prompt like n")
	}
}

// TestDetailHandleKeyRStartsAtFetch is the regression for restore skipping
// the fetch: restore is network=yes (spec §2.1), so confirming R must start
// the pipeline at stageFetch, not stagePlan against a possibly-stale local
// vault copy.
func TestDetailHandleKeyRStartsAtFetch(t *testing.T) {
	m, _ := newTestDetail(t)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	if m.confirm == nil {
		t.Fatalf("R should open a confirm prompt")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if !m.svActive || m.sv == nil {
		t.Fatalf("confirming R should start a syncView")
	}
	if m.sv.stage != stageFetch {
		t.Fatalf("sv.stage = %v, want stageFetch (restore must fetch first)", m.sv.stage)
	}
	if m.sv.opts.Mode != sync.ModeRestore {
		t.Fatalf("sv.opts.Mode = %v, want ModeRestore", m.sv.opts.Mode)
	}
}

// TestDetailHandleKeyDTrashesAndRemovesLocalFile is the regression for "D"
// promising "trashed locally, removed from the vault" while only removing it
// from the vault: Engine.DeleteEverywhere alone leaves the plaintext file on
// disk (internal/sync/ops.go), so confirming D must also move the local copy
// to the encrypted trash and delete it, exactly like the CLI's `files delete`
// (internal/cli/cmd_files.go).
func TestDetailHandleKeyDTrashesAndRemovesLocalFile(t *testing.T) {
	s := newTestSession(t)
	dir := t.TempDir()
	s.Config.AddProject(config.ProjectConfig{ID: "proj1", Name: "Proj One", Path: dir})

	// Track a.env in the vault (writeTrackedFiles, addproject_test.go) so
	// Engine.DeleteEverywhere's "tracked" precondition holds, and create the
	// matching local file so there is something for trashAndRemoveLocal to
	// move to the trash.
	writeTrackedFiles(t, s, "proj1", map[string]string{"a.env": "SECRET=1"})
	full := filepath.Join(dir, "a.env")
	if err := os.WriteFile(full, []byte("SECRET=1"), 0o600); err != nil {
		t.Fatalf("write local file: %v", err)
	}

	m, err := newDetailModel(context.Background(), s, "proj1", "Proj One")
	if err != nil {
		t.Fatalf("newDetailModel: %v", err)
	}
	m.setPlan(&sync.Plan{Projects: []sync.ProjectPlan{{
		ID: "proj1",
		Items: []sync.Item{
			{Key: sync.ItemKey{Project: "proj1", Path: "a.env"}, Action: sync.ActionInSync},
		},
	}}})

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	if m.confirm == nil {
		t.Fatalf("D should open a confirm prompt")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if m.err != "" {
		t.Fatalf("unexpected error after confirming D: %s", m.err)
	}
	if cmd == nil {
		t.Fatalf("confirming D should return the syncView's Init() command")
	}
	if _, err := os.Stat(full); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local copy should have been removed after D, stat err = %v", err)
	}
	entries, err := s.State.TrashList()
	if err != nil {
		t.Fatalf("TrashList: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Project == "proj1" && e.Path == "a.env" {
			found = true
		}
	}
	if !found {
		t.Fatalf("trash entries = %+v, want one for proj1/a.env", entries)
	}
}

// TestDetailTrashAndRemoveLocalMissingFileIsNoop covers the case where the
// local file is already gone by the time D runs: nothing to trash, and no
// error should be raised.
func TestDetailTrashAndRemoveLocalMissingFileIsNoop(t *testing.T) {
	m, _ := newTestDetail(t)
	m.trashAndRemoveLocal("does-not-exist.env")
	if m.err != "" {
		t.Fatalf("trashAndRemoveLocal on a missing file should not set m.err, got %q", m.err)
	}
}

// TestDetailHandleKeyDashUnlinkYesRemovesProject drives the full "-" unlink
// confirm round trip against a real config/state, the way a user pressing y
// would: run() calls the real UnlinkProject, its cmd delivers unlinkedMsg,
// and the screen reports back=true once that arrives.
func TestDetailHandleKeyDashUnlinkYesRemovesProject(t *testing.T) {
	m, s := newTestDetail(t)

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("-")})
	if m.confirm == nil {
		t.Fatalf("- should open a confirm prompt")
	}

	back, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if back {
		t.Fatalf("y itself should not report back yet — it returns a cmd that must be fed through Update")
	}
	if cmd == nil {
		t.Fatalf("y should return a command")
	}
	msg := cmd()
	back, _ = m.Update(msg)
	if !back {
		t.Fatalf("unlinkedMsg should leave the detail screen")
	}
	if _, ok := s.Config.Project("proj1"); ok {
		t.Fatalf("UnlinkProject should have removed the project from config")
	}
}

func TestDetailPlanMsgSetsErrorAndItems(t *testing.T) {
	m, _ := newTestDetail(t)
	m.Update(detailPlanMsg{err: context.DeadlineExceeded})
	if m.err == "" {
		t.Fatalf("a detailPlanMsg error should set m.err")
	}

	plan := &sync.Plan{Projects: []sync.ProjectPlan{{ID: "proj1", Items: []sync.Item{
		{Key: sync.ItemKey{Project: "proj1", Path: "b.env"}, Action: sync.ActionUpload},
	}}}}
	m.Update(detailPlanMsg{plan: plan})
	if len(m.items) != 1 || m.items[0].Key.Path != "b.env" {
		t.Fatalf("items = %+v, want the single b.env item from the plan", m.items)
	}
}

func TestDetailUpdateSyncViewTerminalEnterReloadsPlan(t *testing.T) {
	m, _ := newTestDetail(t)
	m.svActive = true
	m.sv = newSyncView(context.Background(), nil, sync.Options{}, stageDone, false, false)
	back, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if back {
		t.Fatalf("finishing a syncView should not itself leave the detail screen")
	}
	if m.svActive || m.sv != nil {
		t.Fatalf("enter on a terminal syncView should clear svActive/sv")
	}
	if cmd == nil {
		t.Fatalf("expected the plan-reload command")
	}
}
