package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/sync"
)

func syncedItem() sync.Item { return sync.Item{Action: sync.ActionInSync} }

func TestDashboardBadge(t *testing.T) {
	tests := []struct {
		name  string
		pp    *sync.ProjectPlan
		label string
	}{
		{"nil plan", nil, "unknown"},
		{"missing path", &sync.ProjectPlan{Missing: true, Items: []sync.Item{syncedItem()}}, "path missing"},
		{"duplicate", &sync.ProjectPlan{DuplicateOf: "abcd1234"}, "duplicate of abcd1234"},
		{"empty is synced", &sync.ProjectPlan{}, "synced"},
		{"all in sync", &sync.ProjectPlan{Items: []sync.Item{syncedItem(), syncedItem()}}, "synced"},
		{
			"local changes",
			&sync.ProjectPlan{Items: []sync.Item{{Action: sync.ActionUpload}}},
			"local changes",
		},
		{
			"remote changes",
			&sync.ProjectPlan{Items: []sync.Item{{Action: sync.ActionDownload}}},
			"remote changes",
		},
		{
			"local and remote changes both present",
			&sync.ProjectPlan{Items: []sync.Item{{Action: sync.ActionUpload}, {Action: sync.ActionDownload}}},
			"local changes",
		},
		{
			"conflicts win over changes",
			&sync.ProjectPlan{Items: []sync.Item{{Action: sync.ActionUpload}, {Action: sync.ActionConflict}}},
			"conflicts",
		},
		{
			// A rollback is a remote-side event (the vault head moved
			// backward relative to the base), not a content conflict
			// between two edits — it must not share the "conflicts" badge
			// with sum.Conflicts (spec §2.2; see dashboardBadge's doc).
			"rollback surfaces as remote changes, not conflicts",
			&sync.ProjectPlan{Items: []sync.Item{{Action: sync.ActionRollback, Original: sync.ActionDownload}}},
			"remote changes",
		},
		{
			"pending wins over changes",
			&sync.ProjectPlan{Items: []sync.Item{{Action: sync.ActionUpload}, {Action: sync.ActionPending}}},
			"pending",
		},
		{
			// ActionMissingLocal is one tracked file deleted locally with
			// the vault head unchanged — a local-side change awaiting an
			// explicit decision, not "the project directory is gone"
			// (that's pp.Missing, tested separately above).
			"missing locally reported as local changes, not path missing",
			&sync.ProjectPlan{Items: []sync.Item{{Action: sync.ActionMissingLocal}}},
			"local changes",
		},
		{
			"push/pull report-only still counts as a change",
			&sync.ProjectPlan{Items: []sync.Item{{Action: sync.ActionReportOnly, Original: sync.ActionDownload}}},
			"remote changes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			label, style := dashboardBadge(tt.pp)
			if label != tt.label {
				t.Errorf("dashboardBadge() label = %q, want %q", label, tt.label)
			}
			if style.Render("x") == "" {
				t.Errorf("dashboardBadge() returned a zero style")
			}
		})
	}
}

func TestDashboardBadgeMissingBeatsDuplicate(t *testing.T) {
	pp := &sync.ProjectPlan{Missing: true, DuplicateOf: "other"}
	label, _ := dashboardBadge(pp)
	if label != "path missing" {
		t.Errorf("dashboardBadge() = %q, want %q (missing must be checked first)", label, "path missing")
	}
}

func TestComputeDashboardRowsEmptyVault(t *testing.T) {
	s := newTestSession(t)
	rows, err := computeDashboardRows(context.Background(), s)
	if err != nil {
		t.Fatalf("computeDashboardRows: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("computeDashboardRows() = %d rows, want 0 for an empty vault", len(rows))
	}
}

// --- vault + remote summary header (spec §2.2 item 2) -----------------------

func TestDescribeRemote(t *testing.T) {
	tests := []struct {
		name string
		r    config.RemoteConfig
		want string
	}{
		{"none", config.RemoteConfig{Type: config.RemoteNone}, "none"},
		{"unset defaults to none", config.RemoteConfig{}, "none"},
		{"git", config.RemoteConfig{Type: config.RemoteGit, Git: config.GitRemote{URL: "git@host:repo.git", Branch: "main"}}, "git git@host:repo.git (main)"},
		{"rclone", config.RemoteConfig{Type: config.RemoteRclone, Rclone: config.RcloneRemote{Remote: "gdrive", Path: "vault"}}, "rclone gdrive:vault"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := describeRemote(tt.r); got != tt.want {
				t.Errorf("describeRemote(%+v) = %q, want %q", tt.r, got, tt.want)
			}
		})
	}
}

func TestDashboardViewShowsVaultAndRemoteSummary(t *testing.T) {
	s := newTestSession(t)
	m := newDashboardModel(context.Background(), s)
	view := m.View()
	for _, want := range []string{s.Config.Machine.Name, "remote none", "vault"} {
		if !strings.Contains(view, want) {
			t.Errorf("View() = %q, want it to contain %q", view, want)
		}
	}
}

// --- Update-driven key handling (spec §2.2 item 2) --------------------------

func newTestDashboard(t *testing.T) *dashboardModel {
	t.Helper()
	s := newTestSession(t)
	m := newDashboardModel(context.Background(), s)
	m.rows = []projectRow{
		{linked: true, id: "p1", name: "proj1", path: "/tmp/proj1", badge: "synced", badgeStyle: styles.BadgeGood},
	}
	return m
}

func TestDashboardHandleKeyQuit(t *testing.T) {
	m := newTestDashboard(t)
	action, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if action.kind != dashActionQuit {
		t.Fatalf("q -> action.kind = %v, want dashActionQuit", action.kind)
	}
}

func TestDashboardHandleKeyAdd(t *testing.T) {
	m := newTestDashboard(t)
	action, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if action.kind != dashActionAdd {
		t.Fatalf("a -> action.kind = %v, want dashActionAdd", action.kind)
	}
}

func TestDashboardHandleKeySettings(t *testing.T) {
	m := newTestDashboard(t)
	action, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	if action.kind != dashActionSettings {
		t.Fatalf("c -> action.kind = %v, want dashActionSettings", action.kind)
	}
}

func TestDashboardCursorMovement(t *testing.T) {
	m := newTestDashboard(t)
	m.rows = append(m.rows, projectRow{linked: true, id: "p2", name: "proj2"})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.cursor != 1 {
		t.Fatalf("cursor after down = %d, want 1", m.cursor)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown}) // clamped at the last row
	if m.cursor != 1 {
		t.Fatalf("cursor after a second down = %d, want 1 (clamped)", m.cursor)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.cursor != 0 {
		t.Fatalf("cursor after up = %d, want 0", m.cursor)
	}
}

func TestDashboardHandleEnterOnLinkedRowOpensDetail(t *testing.T) {
	m := newTestDashboard(t)
	action, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if action.kind != dashActionDetail || action.projectID != "p1" {
		t.Fatalf("enter on a linked row = %+v, want dashActionDetail for p1", action)
	}
}

func TestDashboardHandleEnterOnUnlinkedRowOpensLinkPrompt(t *testing.T) {
	m := newTestDashboard(t)
	m.rows = []projectRow{{linked: false, id: "p2", name: "proj2"}}
	action, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if action.kind != dashActionNone {
		t.Fatalf("enter on an unlinked row should not request a screen change yet, got %v", action.kind)
	}
	if m.linking == nil {
		t.Fatalf("enter on an unlinked row should open the link prompt")
	}
	if cmd == nil {
		t.Fatalf("expected a textinput.Blink command")
	}
}

func TestDashboardLinkPromptEscCancels(t *testing.T) {
	m := newTestDashboard(t)
	m.rows = []projectRow{{linked: false, id: "p2", name: "proj2"}}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.linking == nil {
		t.Fatalf("setup: expected the link prompt open")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.linking != nil {
		t.Fatalf("esc should cancel the link prompt")
	}
}

func TestDashboardLinkPromptRejectsMissingPath(t *testing.T) {
	m := newTestDashboard(t)
	m.rows = []projectRow{{linked: false, id: "p2", name: "proj2"}}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	// The prompt's input is left empty: submitting it should stay put with
	// an error rather than trying to link an empty path.
	action, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if action.kind != dashActionNone || m.linking == nil {
		t.Fatalf("submitting an empty path should stay on the link prompt")
	}
	if m.linking.err == "" {
		t.Fatalf("expected an error asking for a local path")
	}
}

// TestDashboardStartSyncKeysCallInit is the regression for s/u/d/r starting a
// syncView but never calling its Init(): without the returned command the
// pipeline's first stage (Fetch/Plan) never actually runs, so the spinner
// spins forever and nothing happens.
func TestDashboardStartSyncKeysCallInit(t *testing.T) {
	for _, key := range []string{"s", "u", "d", "r"} {
		t.Run(key, func(t *testing.T) {
			m := newTestDashboard(t)
			_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
			if !m.svActive || m.sv == nil {
				t.Fatalf("%q should start a syncView", key)
			}
			if cmd == nil {
				t.Fatalf("%q should return the syncView's Init() command, not nil", key)
			}
		})
	}
}
