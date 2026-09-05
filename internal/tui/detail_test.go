package tui

import (
	"context"
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
